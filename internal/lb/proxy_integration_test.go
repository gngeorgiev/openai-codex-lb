package lb

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProxySelectsAccountByUsage(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	tokenA := testJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-a"},
		"https://api.openai.com/profile": map[string]any{"email": "a@example.com"},
	})
	tokenB := testJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-b"},
		"https://api.openai.com/profile": map[string]any{"email": "b@example.com"},
	})

	homeA := filepath.Join(tmp, "acc-a")
	homeB := filepath.Join(tmp, "acc-b")
	writeAuthFile(t, homeA, tokenA, "acct-a")
	writeAuthFile(t, homeB, tokenB, "acct-b")

	var mu sync.Mutex
	hits := []string{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			w.Header().Set("Content-Type", "application/json")
			if token == tokenA {
				_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":90},"secondary_window":{"used_percent":90}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":20}}}`)
			return
		case "/backend-api/codex/responses":
			mu.Lock()
			hits = append(hits, token+"|"+r.Header.Get("ChatGPT-Account-Id"))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Accounts = []Account{
			{ID: "a", Alias: "a", HomeDir: homeA, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true},
			{ID: "b", Alias: "b", HomeDir: homeB, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("post to proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d, body=%s", resp.StatusCode, string(body))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 1 {
		t.Fatalf("expected 1 upstream hit, got %d", len(hits))
	}
	if !strings.HasPrefix(hits[0], tokenB+"|acct-b") {
		t.Fatalf("expected account B to be selected, hit=%s", hits[0])
	}
}

func TestProxyFailoverOn429(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	tokenA := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"}})
	tokenB := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-b"}})
	homeA := filepath.Join(tmp, "acc-a")
	homeB := filepath.Join(tmp, "acc-b")
	writeAuthFile(t, homeA, tokenA, "acct-a")
	writeAuthFile(t, homeB, tokenB, "acct-b")

	var mu sync.Mutex
	order := []string{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":50},"secondary_window":{"used_percent":50}}}`)
			return
		case "/backend-api/codex/responses":
			mu.Lock()
			order = append(order, token)
			mu.Unlock()
			if token == tokenA {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":"rate"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Settings.Proxy.MaxAttempts = 3
		sf.Settings.Policy.Mode = PolicySticky
		sf.Accounts = []Account{
			{ID: "a", Alias: "a", HomeDir: homeA, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true},
			{ID: "b", Alias: "b", HomeDir: homeB, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("post to proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d, body=%s", resp.StatusCode, string(body))
	}

	mu.Lock()
	if len(order) != 2 || order[0] != tokenA || order[1] != tokenB {
		t.Fatalf("unexpected request order: %v", order)
	}
	mu.Unlock()

	snap := store.Snapshot()
	if snap.Accounts[0].CooldownUntilMS <= time.Now().UnixMilli() {
		t.Fatalf("expected account a cooldown to be set")
	}
}

func TestProxyDisablesAccountOn401(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	tokenA := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"}})
	tokenB := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-b"}})
	homeA := filepath.Join(tmp, "acc-a")
	homeB := filepath.Join(tmp, "acc-b")
	writeAuthFile(t, homeA, tokenA, "acct-a")
	writeAuthFile(t, homeB, tokenB, "acct-b")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":50},"secondary_window":{"used_percent":50}}}`)
			return
		case "/backend-api/codex/responses":
			if token == tokenA {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Settings.Proxy.MaxAttempts = 3
		sf.Settings.Policy.Mode = PolicySticky
		sf.Accounts = []Account{
			{ID: "a", Alias: "a", HomeDir: homeA, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true},
			{ID: "b", Alias: "b", HomeDir: homeB, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("post to proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d, body=%s", resp.StatusCode, string(body))
	}

	snap := store.Snapshot()
	if snap.Accounts[0].Enabled {
		t.Fatalf("expected account a to be disabled")
	}
	if snap.Accounts[0].DisabledReason != "http-401" {
		t.Fatalf("unexpected disable reason: %s", snap.Accounts[0].DisabledReason)
	}
}

func TestProxyRefreshesAccountOn401AndRetries(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	oldTokenA := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"}})
	newTokenA := testJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-a"},
		"https://api.openai.com/profile": map[string]any{"email": "a@example.com"},
	})
	tokenB := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-b"}})
	homeA := filepath.Join(tmp, "acc-a")
	homeB := filepath.Join(tmp, "acc-b")
	writeAuthTokensFile(t, homeA, oldTokenA, "refresh-a-1", "acct-a")
	writeAuthFile(t, homeB, tokenB, "acct-b")

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if got := r.Form.Get("refresh_token"); got != "refresh-a-1" {
			t.Fatalf("unexpected refresh token: %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  newTokenA,
			"refresh_token": "refresh-a-2",
			"id_token":      testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"}}),
		})
	}))
	defer authSrv.Close()

	var mu sync.Mutex
	hits := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":50},"secondary_window":{"used_percent":50}}}`)
			return
		case "/backend-api/codex/responses":
			mu.Lock()
			hits = append(hits, token)
			mu.Unlock()
			if token == oldTokenA {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
				return
			}
			if token == newTokenA {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"ok":true}`)
				return
			}
			t.Fatalf("unexpected token routed upstream: %q", token)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Settings.Proxy.MaxAttempts = 3
		sf.Settings.Policy.Mode = PolicySticky
		sf.Accounts = []Account{
			{ID: "a", Alias: "a", HomeDir: homeA, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true},
			{ID: "b", Alias: "b", HomeDir: homeB, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxy := NewProxyServer(store, nil, nil)
	proxy.authTokenURL = authSrv.URL
	proxy.authClientID = "client-123"

	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("post to proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d, body=%s", resp.StatusCode, string(body))
	}

	mu.Lock()
	if len(hits) != 2 || hits[0] != oldTokenA || hits[1] != newTokenA {
		t.Fatalf("unexpected upstream hits: %v", hits)
	}
	mu.Unlock()

	snap := store.Snapshot()
	if !snap.Accounts[0].Enabled {
		t.Fatalf("expected account a to remain enabled")
	}
	if snap.Accounts[0].DisabledReason != "" {
		t.Fatalf("expected account a disable reason to clear, got %q", snap.Accounts[0].DisabledReason)
	}

	authInfo, err := LoadAuth(homeA)
	if err != nil {
		t.Fatalf("LoadAuth after refresh: %v", err)
	}
	if authInfo.AccessToken != newTokenA {
		t.Fatalf("expected refreshed access token to be persisted")
	}
	if authInfo.RefreshToken != "refresh-a-2" {
		t.Fatalf("expected rotated refresh token, got %q", authInfo.RefreshToken)
	}
}

func TestProxyStatusRefreshesDisabled401Account(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	oldToken := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"}})
	newToken := testJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-a"},
		"https://api.openai.com/profile": map[string]any{"email": "a@example.com"},
	})
	home := filepath.Join(tmp, "acc-a")
	writeAuthTokensFile(t, home, oldToken, "refresh-a-1", "acct-a")

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if got := r.Form.Get("refresh_token"); got != "refresh-a-1" {
			t.Fatalf("unexpected refresh token: %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  newToken,
			"refresh_token": "refresh-a-2",
			"id_token":      testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"}}),
		})
	}))
	defer authSrv.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != newToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
			return
		}
		_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":25},"secondary_window":{"used_percent":30}}}`)
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Accounts = []Account{
			{
				ID:             "a",
				Alias:          "a",
				HomeDir:        home,
				BaseURL:        sf.Settings.Proxy.UpstreamBaseURL,
				Enabled:        false,
				DisabledReason: "http-401",
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxy := NewProxyServer(store, nil, nil)
	proxy.authTokenURL = authSrv.URL
	proxy.authClientID = "client-123"

	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	resp, err := http.Get(proxySrv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	snap := store.Snapshot()
	if !snap.Accounts[0].Enabled {
		t.Fatalf("expected account to be re-enabled")
	}
	if snap.Accounts[0].DisabledReason != "" {
		t.Fatalf("expected cleared disabled reason, got %q", snap.Accounts[0].DisabledReason)
	}
	if snap.Accounts[0].Quota.Source != "openai_usage_api" {
		t.Fatalf("expected usage quota refresh source, got %q", snap.Accounts[0].Quota.Source)
	}
	authInfo, err := LoadAuth(home)
	if err != nil {
		t.Fatalf("LoadAuth after status refresh: %v", err)
	}
	if authInfo.RefreshToken != "refresh-a-2" {
		t.Fatalf("expected rotated refresh token after status refresh, got %q", authInfo.RefreshToken)
	}
}

func TestProxyDoesNotDisableAccountOn403ForNonAccountPath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	tokenA := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"}})
	homeA := filepath.Join(tmp, "acc-a")
	writeAuthFile(t, homeA, tokenA, "acct-a")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":50},"secondary_window":{"used_percent":50}}}`)
			return
		case "/backend-api/models":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"forbidden"}`)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Settings.Proxy.MaxAttempts = 3
		sf.Settings.Policy.Mode = PolicySticky
		sf.Accounts = []Account{
			{ID: "a", Alias: "a", HomeDir: homeA, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	req, err := http.NewRequest(http.MethodGet, proxySrv.URL+"/models", nil)
	if err != nil {
		t.Fatalf("build GET /models: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 403, got %d body=%s", resp.StatusCode, string(body))
	}

	snap := store.Snapshot()
	if !snap.Accounts[0].Enabled {
		t.Fatalf("expected account to remain enabled")
	}
	if snap.Accounts[0].DisabledReason != "" {
		t.Fatalf("expected empty disabled reason, got %q", snap.Accounts[0].DisabledReason)
	}
}

func TestProxyRootEndpointIsLocalHealth(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	resp, err := http.Get(proxySrv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "codexlb-proxy") {
		t.Fatalf("expected local root response body, got %q", string(body))
	}
}

func TestProxyReloadedPolicyAppliesWithoutRestart(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	tokenA := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"}})
	tokenB := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-b"}})
	homeA := filepath.Join(tmp, "acc-a")
	homeB := filepath.Join(tmp, "acc-b")
	writeAuthFile(t, homeA, tokenA, "acct-a")
	writeAuthFile(t, homeB, tokenB, "acct-b")

	var mu sync.Mutex
	hits := []string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/responses":
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			mu.Lock()
			hits = append(hits, token)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		case "/backend-api/wham/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":50},"secondary_window":{"used_percent":50}}}`)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}))
	defer upstream.Close()

	nowMS := time.Now().UnixMilli()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Settings.Policy.Mode = PolicySticky
		sf.Accounts = []Account{
			{
				ID:      "a",
				Alias:   "a",
				HomeDir: homeA,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:   100,
					DailyUsed:    90,
					WeeklyLimit:  100,
					WeeklyUsed:   90,
					LastSyncAt:   nowMS,
					Source:       "manual",
					DailyResetAt: nowMS + 3600_000,
				},
			},
			{
				ID:      "b",
				Alias:   "b",
				HomeDir: homeB,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:   100,
					DailyUsed:    10,
					WeeklyLimit:  100,
					WeeklyUsed:   20,
					LastSyncAt:   nowMS,
					Source:       "manual",
					DailyResetAt: nowMS + 3600_000,
				},
			},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	if err := store.PersistSettingsToConfig(); err != nil {
		t.Fatalf("PersistSettingsToConfig: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartConfigReloader(ctx, store, nil, nil, 20*time.Millisecond)

	resp1, err := http.Post(proxySrv.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"first"}`))
	if err != nil {
		t.Fatalf("first post to proxy: %v", err)
	}
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected first request 200, got %d", resp1.StatusCode)
	}

	cfg := store.Snapshot().Settings
	cfg.Policy.Mode = PolicyUsageBalanced
	if err := WriteSettingsConfig(tmp, cfg); err != nil {
		t.Fatalf("WriteSettingsConfig: %v", err)
	}
	if !waitFor(t, 2*time.Second, 20*time.Millisecond, func() bool {
		return store.Snapshot().Settings.Policy.Mode == PolicyUsageBalanced
	}) {
		t.Fatalf("timed out waiting for policy reload")
	}

	resp2, err := http.Post(proxySrv.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"second"}`))
	if err != nil {
		t.Fatalf("second post to proxy: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected second request 200, got %d", resp2.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hits) < 2 {
		t.Fatalf("expected at least 2 upstream hits, got %d", len(hits))
	}
	if hits[0] != tokenA {
		t.Fatalf("expected sticky policy to use account a first, got token=%s", hits[0])
	}
	if hits[1] != tokenB {
		t.Fatalf("expected usage_balanced policy to switch to account b, got token=%s", hits[1])
	}
}

func writeAuthFile(t *testing.T, home, accessToken, accountID string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}
	payload := map[string]any{
		"tokens": map[string]any{
			"access_token": accessToken,
			"account_id":   accountID,
		},
	}
	b, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), b, 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
}

func writeAuthTokensFile(t *testing.T, home, accessToken, refreshToken, accountID string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}
	payload := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": refreshToken,
			"account_id":    accountID,
		},
	}
	b, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), b, 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
}
