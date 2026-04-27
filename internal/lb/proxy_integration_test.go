package lb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestProxyRewritesCodexAppsForBackendAccounts(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	token := testJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-a"},
		"https://api.openai.com/profile": map[string]any{"email": "a@example.com"},
	})
	home := filepath.Join(tmp, "acc-a")
	writeAuthFile(t, home, token, "acct-a")

	var hitPath string
	var hitAuth string
	var hitAccountID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/wham/usage" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":10}}}`)
			return
		}
		hitPath = r.URL.RequestURI()
		hitAuth = r.Header.Get("Authorization")
		hitAccountID = r.Header.Get("ChatGPT-Account-Id")
		if r.URL.Path != "/backend-api/wham/apps" {
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","result":{"ok":true},"id":1}`)
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Accounts = []Account{
			{ID: "a", Alias: "a", HomeDir: home, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/api/codex/apps?session=abc", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post to proxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if got, want := hitPath, "/backend-api/wham/apps?session=abc"; got != want {
		t.Fatalf("upstream request uri mismatch: got %q want %q", got, want)
	}
	if got, want := hitAuth, "Bearer "+token; got != want {
		t.Fatalf("authorization mismatch: got %q want %q", got, want)
	}
	if got, want := hitAccountID, "acct-a"; got != want {
		t.Fatalf("account id mismatch: got %q want %q", got, want)
	}
}

func TestProxyReturnsAggregatedUsageForProxyOnlyRuntimeAuth(t *testing.T) {
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

	var usageHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/wham/usage" {
			usageHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":99},"secondary_window":{"used_percent":99}}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	now := time.Now()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Accounts = []Account{
			{
				ID:      "a",
				Alias:   "a",
				HomeDir: homeA,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:    100,
					DailyUsed:     20,
					DailyResetAt:  now.Add(3 * time.Hour).Unix(),
					WeeklyLimit:   100,
					WeeklyUsed:    40,
					WeeklyResetAt: now.Add(5 * 24 * time.Hour).Unix(),
					LastSyncAt:    now.UnixMilli(),
					Source:        "manual",
				},
			},
			{
				ID:      "b",
				Alias:   "b",
				HomeDir: homeB,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:    100,
					DailyUsed:     60,
					DailyResetAt:  now.Add(2 * time.Hour).Unix(),
					WeeklyLimit:   100,
					WeeklyUsed:    20,
					WeeklyResetAt: now.Add(4 * 24 * time.Hour).Unix(),
					LastSyncAt:    now.UnixMilli(),
					Source:        "manual",
				},
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	req, err := http.NewRequest(http.MethodGet, proxySrv.URL+"/backend-api/wham/usage", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+buildProxyOnlyAccessToken(proxyOnlyRuntimeProfile{}))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET usage: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var payload usageResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode usage payload: %v", err)
	}
	if usageHits != 0 {
		t.Fatalf("expected proxy-only aggregated usage to avoid upstream usage endpoint, got %d hits", usageHits)
	}
	if got := payload.RateLimit.PrimaryWindow.UsedPercent; got != 40 {
		t.Fatalf("expected aggregated primary used_percent 40, got %v", got)
	}
	if payload.PlanType != "plus" {
		t.Fatalf("expected plan_type plus, got %q", payload.PlanType)
	}
	if payload.Email != "proxy-only@codexlb.internal" {
		t.Fatalf("expected proxy-only email, got %q", payload.Email)
	}
	if !payload.RateLimit.Allowed || payload.RateLimit.LimitReached {
		t.Fatalf("expected rate limit to remain allowed, got %+v", payload.RateLimit)
	}
	if got := payload.RateLimit.PrimaryWindow.LimitWindowSeconds; got != 5*60*60 {
		t.Fatalf("expected primary limit window to be 5h, got %d", got)
	}
	if got := payload.RateLimit.SecondaryWindow.LimitWindowSeconds; got != 7*24*60*60 {
		t.Fatalf("expected secondary limit window to be 7d, got %d", got)
	}
	if got := payload.RateLimit.SecondaryWindow.UsedPercent; got != 30 {
		t.Fatalf("expected aggregated secondary used_percent 30, got %v", got)
	}
	if got := payload.RateLimit.PrimaryWindow.ResetsAt; got != now.Add(2*time.Hour).Unix() {
		t.Fatalf("expected earliest primary reset, got %d", got)
	}
	if got := payload.RateLimit.SecondaryWindow.ResetsAt; got != now.Add(4*24*time.Hour).Unix() {
		t.Fatalf("expected earliest secondary reset, got %d", got)
	}
}

func TestProxyReturnsAggregatedUsageForCodexStatusWithRealRuntimeAuth(t *testing.T) {
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

	var usageHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/wham/usage" || r.URL.Path == "/api/codex/usage" {
			usageHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":99},"secondary_window":{"used_percent":99}}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	now := time.Now()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Accounts = []Account{
			{
				ID:      "a",
				Alias:   "a",
				HomeDir: homeA,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:    100,
					DailyUsed:     20,
					DailyResetAt:  now.Add(3 * time.Hour).Unix(),
					WeeklyLimit:   100,
					WeeklyUsed:    40,
					WeeklyResetAt: now.Add(5 * 24 * time.Hour).Unix(),
					LastSyncAt:    now.UnixMilli(),
					Source:        "manual",
				},
			},
			{
				ID:      "b",
				Alias:   "b",
				HomeDir: homeB,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:    100,
					DailyUsed:     60,
					DailyResetAt:  now.Add(2 * time.Hour).Unix(),
					WeeklyLimit:   100,
					WeeklyUsed:    20,
					WeeklyResetAt: now.Add(4 * 24 * time.Hour).Unix(),
					LastSyncAt:    now.UnixMilli(),
					Source:        "manual",
				},
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	req, err := http.NewRequest(http.MethodGet, proxySrv.URL+"/api/codex/usage", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenA)
	req.Header.Set("ChatGPT-Account-Id", "acct-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET usage: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var payload usageResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode usage payload: %v", err)
	}
	if usageHits != 0 {
		t.Fatalf("expected aggregated Codex status usage to avoid upstream usage endpoint, got %d hits", usageHits)
	}
	if got := payload.RateLimit.PrimaryWindow.UsedPercent; got != 40 {
		t.Fatalf("expected aggregated primary used_percent 40, got %v", got)
	}
	if got := payload.RateLimit.SecondaryWindow.UsedPercent; got != 30 {
		t.Fatalf("expected aggregated secondary used_percent 30, got %v", got)
	}
}

func TestProxyReturnsAggregatedUsageForDirectWhamUsagePath(t *testing.T) {
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

	var usageHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/wham/usage" || r.URL.Path == "/wham/usage" {
			usageHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":99},"secondary_window":{"used_percent":99}}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	now := time.Now()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Accounts = []Account{
			{
				ID:      "a",
				Alias:   "a",
				HomeDir: homeA,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:    100,
					DailyUsed:     20,
					DailyResetAt:  now.Add(3 * time.Hour).Unix(),
					WeeklyLimit:   100,
					WeeklyUsed:    40,
					WeeklyResetAt: now.Add(5 * 24 * time.Hour).Unix(),
					LastSyncAt:    now.UnixMilli(),
					Source:        "manual",
				},
			},
			{
				ID:      "b",
				Alias:   "b",
				HomeDir: homeB,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:    100,
					DailyUsed:     60,
					DailyResetAt:  now.Add(2 * time.Hour).Unix(),
					WeeklyLimit:   100,
					WeeklyUsed:    20,
					WeeklyResetAt: now.Add(4 * 24 * time.Hour).Unix(),
					LastSyncAt:    now.UnixMilli(),
					Source:        "manual",
				},
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	req, err := http.NewRequest(http.MethodGet, proxySrv.URL+"/wham/usage", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenA)
	req.Header.Set("ChatGPT-Account-Id", "acct-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET usage: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var payload usageResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode usage payload: %v", err)
	}
	if usageHits != 0 {
		t.Fatalf("expected direct /wham/usage to avoid upstream usage endpoint, got %d hits", usageHits)
	}
	if got := payload.RateLimit.PrimaryWindow.UsedPercent; got != 40 {
		t.Fatalf("expected aggregated primary used_percent 40, got %v", got)
	}
	if got := payload.RateLimit.SecondaryWindow.UsedPercent; got != 30 {
		t.Fatalf("expected aggregated secondary used_percent 30, got %v", got)
	}
}

func TestProxyReturnsBackendCompatibleIntegerUsageForCodexStatus(t *testing.T) {
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

	now := time.Now()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = "https://chatgpt.com/backend-api"
		sf.Accounts = []Account{
			{
				ID:      "a",
				Alias:   "a",
				HomeDir: homeA,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:    100,
					DailyUsed:     17,
					DailyResetAt:  now.Add(3 * time.Hour).Unix(),
					WeeklyLimit:   100,
					WeeklyUsed:    31,
					WeeklyResetAt: now.Add(5 * 24 * time.Hour).Unix(),
					LastSyncAt:    now.UnixMilli(),
					Source:        "manual",
				},
			},
			{
				ID:      "b",
				Alias:   "b",
				HomeDir: homeB,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:    100,
					DailyUsed:     18,
					DailyResetAt:  now.Add(2 * time.Hour).Unix(),
					WeeklyLimit:   100,
					WeeklyUsed:    38,
					WeeklyResetAt: now.Add(4 * 24 * time.Hour).Unix(),
					LastSyncAt:    now.UnixMilli(),
					Source:        "manual",
				},
			},
			{
				ID:      "c",
				Alias:   "c",
				HomeDir: homeB,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:    100,
					DailyUsed:     19,
					DailyResetAt:  now.Add(4 * time.Hour).Unix(),
					WeeklyLimit:   100,
					WeeklyUsed:    34,
					WeeklyResetAt: now.Add(6 * 24 * time.Hour).Unix(),
					LastSyncAt:    now.UnixMilli(),
					Source:        "manual",
				},
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	req, err := http.NewRequest(http.MethodGet, proxySrv.URL+"/api/codex/usage", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenA)
	req.Header.Set("ChatGPT-Account-Id", "acct-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET usage: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var payload struct {
		RateLimit struct {
			PrimaryWindow struct {
				UsedPercent int `json:"used_percent"`
			} `json:"primary_window"`
			SecondaryWindow struct {
				UsedPercent int `json:"used_percent"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode backend-compatible usage payload: %v", err)
	}
	if got := payload.RateLimit.PrimaryWindow.UsedPercent; got != 18 {
		t.Fatalf("expected rounded primary used_percent 18, got %d", got)
	}
	if got := payload.RateLimit.SecondaryWindow.UsedPercent; got != 34 {
		t.Fatalf("expected rounded secondary used_percent 34, got %d", got)
	}
}

func TestProxyReturnsAggregatedUsageForProxyOnlyRuntimeAuthAtRootUsagePath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	token := testJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-a"},
		"https://api.openai.com/profile": map[string]any{"email": "a@example.com"},
	})

	home := filepath.Join(tmp, "acc-a")
	writeAuthFile(t, home, token, "acct-a")

	now := time.Now()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = "https://chatgpt.com/backend-api"
		sf.Accounts = []Account{
			{
				ID:      "a",
				Alias:   "a",
				HomeDir: home,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:    100,
					DailyUsed:     25,
					DailyResetAt:  now.Add(2 * time.Hour).Unix(),
					WeeklyLimit:   100,
					WeeklyUsed:    50,
					WeeklyResetAt: now.Add(5 * 24 * time.Hour).Unix(),
					LastSyncAt:    now.UnixMilli(),
					Source:        "manual",
				},
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	req, err := http.NewRequest(http.MethodGet, proxySrv.URL+"/usage", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+buildProxyOnlyAccessToken(proxyOnlyRuntimeProfile{}))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET usage: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var payload usageResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode usage payload: %v", err)
	}
	if payload.PlanType != "plus" {
		t.Fatalf("expected plan_type plus, got %q", payload.PlanType)
	}
	if got := payload.RateLimit.PrimaryWindow.UsedPercent; got != 25 {
		t.Fatalf("expected aggregated primary used_percent 25, got %v", got)
	}
}

func TestProxyForwardsCompactResponsesPath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	token := testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"},
	})
	home := filepath.Join(tmp, "acc-a")
	writeAuthFile(t, home, token, "acct-a")

	var mu sync.Mutex
	hits := []string{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":10}}}`)
			return
		case "/backend-api/codex/responses/compact":
			mu.Lock()
			hits = append(hits, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"detail":"Not Found"}`)
			return
		}
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Accounts = []Account{
			{ID: "a", Alias: "a", HomeDir: home, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxySrv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/responses/compact", "application/json", bytes.NewBufferString(`{"input":"hi"}`))
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
	if len(hits) != 1 || hits[0] != "/backend-api/codex/responses/compact" {
		t.Fatalf("unexpected upstream hits: %v", hits)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
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

func TestProxyDoesNotCooldownOrRetryCanceledRequest(t *testing.T) {
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

	nowMS := time.Now().UnixMilli()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = "https://chatgpt.com/backend-api"
		sf.Settings.Proxy.MaxAttempts = 3
		sf.Settings.Policy.Mode = PolicySticky
		sf.Settings.Quota.RefreshIntervalMinutes = 999
		sf.Settings.Quota.RefreshIntervalMessages = 999
		sf.State.ActiveIndex = 0
		sf.State.MessageCounter = 0
		sf.Accounts = []Account{
			{
				ID:      "a",
				Alias:   "a",
				HomeDir: homeA,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:             100,
					DailyUsed:              10,
					WeeklyLimit:            100,
					WeeklyUsed:             10,
					LastSyncAt:             nowMS,
					LastSyncMessageCounter: 0,
					Source:                 "openai_usage_api",
				},
			},
			{
				ID:      "b",
				Alias:   "b",
				HomeDir: homeB,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:             100,
					DailyUsed:              20,
					WeeklyLimit:            100,
					WeeklyUsed:             20,
					LastSyncAt:             nowMS,
					LastSyncMessageCounter: 0,
					Source:                 "openai_usage_api",
				},
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxy := NewProxyServer(store, nil, nil)
	proxy.requestClient = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if r.URL.Path != "/backend-api/codex/responses" {
				return nil, &url.Error{Op: r.Method, URL: r.URL.String(), Err: errors.New("unexpected path")}
			}
			mu.Lock()
			order = append(order, token)
			mu.Unlock()
			<-r.Context().Done()
			return nil, &url.Error{Op: r.Method, URL: r.URL.String(), Err: r.Context().Err()}
		}),
	}
	reqCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/responses", bytes.NewBufferString(`{"input":"hi"}`)).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		proxy.ServeHTTP(rec, req)
	}()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		attempts := len(order)
		mu.Unlock()
		if attempts >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for upstream request")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled proxy request")
	}

	mu.Lock()
	if len(order) != 1 || order[0] != tokenA {
		t.Fatalf("unexpected upstream order: %v", order)
	}
	mu.Unlock()

	snap := store.Snapshot()
	if snap.Accounts[0].CooldownUntilMS > time.Now().UnixMilli() {
		t.Fatalf("expected account a cooldown to remain clear")
	}
	if snap.Accounts[1].CooldownUntilMS > time.Now().UnixMilli() {
		t.Fatalf("expected account b cooldown to remain clear")
	}
	if snap.Accounts[0].LastSwitchReason == "transport-error" || snap.Accounts[1].LastSwitchReason == "transport-error" {
		t.Fatalf("expected canceled request not to stamp transport-error, got a=%q b=%q", snap.Accounts[0].LastSwitchReason, snap.Accounts[1].LastSwitchReason)
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

func TestProxyRuntimeOAuthTokenRefreshUsesSyntheticRefreshToken(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	oldToken := testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"},
	})
	newToken := testJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-a"},
		"https://api.openai.com/profile": map[string]any{"email": "real@example.com"},
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
			"id_token": testJWT(map[string]any{
				"email": "real@example.com",
				"https://api.openai.com/auth": map[string]any{
					"chatgpt_account_id": "acct-a",
					"chatgpt_plan_type":  "plus",
				},
				"https://api.openai.com/profile": map[string]any{
					"email": "real@example.com",
				},
			}),
		})
	}))
	defer authSrv.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Accounts = []Account{
			{ID: "openai:a", Alias: "a", HomeDir: home, Enabled: true},
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

	resp, err := http.PostForm(proxySrv.URL+"/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {proxyRuntimeRefreshToken},
	})
	if err != nil {
		t.Fatalf("post oauth token refresh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d, body=%s", resp.StatusCode, string(body))
	}

	var payload oauthTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode oauth refresh payload: %v", err)
	}
	if payload.AccessToken != newToken {
		t.Fatalf("unexpected access token in refresh response")
	}
	if payload.RefreshToken != proxyRuntimeRefreshToken {
		t.Fatalf("expected synthetic refresh token %q, got %q", proxyRuntimeRefreshToken, payload.RefreshToken)
	}
	claims, err := decodeJWTPayload(payload.IDToken)
	if err != nil {
		t.Fatalf("decode masked id token: %v", err)
	}
	if got := stringField(claims["email"]); got != "proxy-only@codexlb.internal" {
		t.Fatalf("masked id token email = %q", got)
	}

	authInfo, err := LoadAuth(home)
	if err != nil {
		t.Fatalf("LoadAuth after runtime refresh: %v", err)
	}
	if authInfo.AccessToken != newToken {
		t.Fatalf("expected refreshed access token to persist")
	}
	if authInfo.RefreshToken != "refresh-a-2" {
		t.Fatalf("expected rotated account refresh token, got %q", authInfo.RefreshToken)
	}
}

func TestProxyMarksTerminalRefreshFailureAndClearsPin(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	store, err := OpenStore(tmp)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	oldTokenA := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"}})
	tokenB := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-b"}})
	homeA := filepath.Join(tmp, "acc-a")
	homeB := filepath.Join(tmp, "acc-b")
	writeAuthTokensFile(t, homeA, oldTokenA, "refresh-a-dead", "acct-a")
	writeAuthFile(t, homeB, tokenB, "acct-b")

	authCalls := 0
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCalls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"refresh token already used","code":"refresh_token_reused"}}`)
	}))
	defer authSrv.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			if token == oldTokenA {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
				return
			}
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":20}}}`)
			return
		case "/backend-api/codex/responses":
			if token == oldTokenA {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"expired"}`)
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
		sf.State.PinnedAccountID = "a"
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

	snap := store.Snapshot()
	if snap.Accounts[0].Enabled {
		t.Fatalf("expected account a to be disabled")
	}
	if snap.Accounts[0].DisabledReason != "refresh-token-reused" {
		t.Fatalf("unexpected disable reason: %s", snap.Accounts[0].DisabledReason)
	}
	if snap.State.PinnedAccountID != "" {
		t.Fatalf("expected pinned account to be cleared, got %q", snap.State.PinnedAccountID)
	}
	if authCalls != 2 {
		t.Fatalf("expected request path to attempt auth refresh twice before terminal disable, got %d", authCalls)
	}

	statusResp, err := http.Get(proxySrv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	_ = statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", statusResp.StatusCode)
	}
	if authCalls != 2 {
		t.Fatalf("expected terminal refresh failure not to be retried on /status, got %d auth calls", authCalls)
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

	if !waitFor(t, 2*time.Second, 20*time.Millisecond, func() bool {
		snap := store.Snapshot()
		return snap.Accounts[0].Enabled &&
			snap.Accounts[0].DisabledReason == "" &&
			snap.Accounts[0].Quota.Source == "openai_usage_api"
	}) {
		t.Fatalf("expected account to be re-enabled after background status refresh")
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

func TestProxyMaintenanceLoopRefreshesIdleAccountAuth(t *testing.T) {
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

	authCalls := 0
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCalls++
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

	usageCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		usageCalls++
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
		sf.Settings.Quota.RefreshIntervalMinutes = 1
		sf.Settings.Quota.RefreshIntervalMessages = 1
		sf.Accounts = []Account{
			{
				ID:      "a",
				Alias:   "a",
				HomeDir: home,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxy := NewProxyServer(store, nil, nil)
	proxy.authTokenURL = authSrv.URL
	proxy.authClientID = "client-123"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxy.StartMaintenanceLoop(ctx, 10*time.Millisecond)

	if !waitFor(t, 2*time.Second, 20*time.Millisecond, func() bool {
		snap := store.Snapshot()
		return snap.Accounts[0].Quota.Source == "openai_usage_api"
	}) {
		t.Fatalf("expected maintenance loop to refresh quota for idle account")
	}

	snap := store.Snapshot()
	if !snap.Accounts[0].Enabled {
		t.Fatalf("expected idle account to remain enabled")
	}
	if snap.Accounts[0].DisabledReason != "" {
		t.Fatalf("expected no disabled reason, got %q", snap.Accounts[0].DisabledReason)
	}
	if authCalls != 1 {
		t.Fatalf("expected one auth refresh call, got %d", authCalls)
	}
	if usageCalls < 2 {
		t.Fatalf("expected usage call to retry after auth refresh, got %d calls", usageCalls)
	}

	authInfo, err := LoadAuth(home)
	if err != nil {
		t.Fatalf("LoadAuth after maintenance refresh: %v", err)
	}
	if authInfo.RefreshToken != "refresh-a-2" {
		t.Fatalf("expected rotated refresh token after maintenance refresh, got %q", authInfo.RefreshToken)
	}
}

func TestProxyMaintenanceLoopRecoversTerminalRefreshFailureAfterAuthFileChanges(t *testing.T) {
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
	writeAuthTokensFile(t, home, oldToken, "refresh-a-dead", "acct-a")

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
		sf.Settings.Quota.RefreshIntervalMinutes = 1
		sf.Settings.Quota.RefreshIntervalMessages = 1
		sf.Accounts = []Account{
			{
				ID:             "a",
				Alias:          "a",
				HomeDir:        home,
				BaseURL:        sf.Settings.Proxy.UpstreamBaseURL,
				Enabled:        false,
				DisabledReason: "refresh-token-reused",
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxy := NewProxyServer(store, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxy.StartMaintenanceLoop(ctx, 10*time.Millisecond)

	writeAuthTokensFile(t, home, newToken, "refresh-a-2", "acct-a")

	if !waitFor(t, 2*time.Second, 20*time.Millisecond, func() bool {
		snap := store.Snapshot()
		return snap.Accounts[0].Enabled &&
			snap.Accounts[0].DisabledReason == "" &&
			snap.Accounts[0].Quota.Source == "openai_usage_api"
	}) {
		t.Fatalf("expected maintenance loop to recover terminal refresh failure after auth file update")
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

func TestProxyBackfillsModelsDisplayName(t *testing.T) {
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
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-5-3","title":"GPT-5.3"}]}`)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Settings.Proxy.MaxAttempts = 1
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

	resp, err := http.Get(proxySrv.URL + "/models")
	if err != nil {
		t.Fatalf("GET /models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}

	var payload struct {
		Models []struct {
			Slug        string `json:"slug"`
			Title       string `json:"title"`
			DisplayName string `json:"display_name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode proxied models payload: %v", err)
	}
	if len(payload.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(payload.Models))
	}
	if payload.Models[0].DisplayName != "GPT-5.3" {
		t.Fatalf("expected display_name backfill, got %+v", payload.Models[0])
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

func TestProxyRoutesViaChildProxyByUsage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	var mu sync.Mutex
	hits := []string{}
	newChild := func(name string, score float64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/status":
				_ = json.NewEncoder(w).Encode(ProxyStatus{
					ProxyName:       "child-" + name,
					GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
					SelectionReason: "usage-stay",
					Accounts: []AccountStatus{
						{ProxyName: "child-" + name, Alias: name, ID: name, Active: true, Healthy: true, Enabled: true, Score: score},
					},
				})
			case "/responses":
				mu.Lock()
				hits = append(hits, name)
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"child":"`+name+`"}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}

	childA := newChild("a", 0.20)
	defer childA.Close()
	childB := newChild("b", 0.90)
	defer childB.Close()

	tokenLocal := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-local"}})
	homeLocalA := filepath.Join(root, "local-a")
	homeLocalB := filepath.Join(root, "local-b")
	writeAuthFile(t, homeLocalA, tokenLocal, "acct-local")
	writeAuthFile(t, homeLocalB, tokenLocal, "acct-local")

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "edge-main"
		sf.Settings.Policy.Mode = PolicyUsageBalanced
		sf.Settings.Proxy.MaxAttempts = 2
		sf.Settings.Proxy.ChildProxyURLs = []string{childA.URL, childB.URL}
		sf.Accounts = []Account{
			{ID: "local-a", Alias: "local-a", HomeDir: homeLocalA, Enabled: true, Quota: QuotaState{DailyLimit: 100, DailyUsed: 95, WeeklyLimit: 100, WeeklyUsed: 95, Source: "manual"}},
			{ID: "local-b", Alias: "local-b", HomeDir: homeLocalB, Enabled: true, Quota: QuotaState{DailyLimit: 100, DailyUsed: 98, WeeklyLimit: 100, WeeklyUsed: 98, Source: "manual"}},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	mainProxy := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer mainProxy.Close()

	resp, err := http.Post(mainProxy.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("post to main proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), `"child":"b"`) {
		t.Fatalf("expected request to route to child b, got body=%s", string(body))
	}

	statusResp, err := http.Get(mainProxy.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer statusResp.Body.Close()
	var status ProxyStatus
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.SelectedProxyURL != childB.URL {
		t.Fatalf("expected selected proxy %q, got %q", childB.URL, status.SelectedProxyURL)
	}
	if status.SelectedProxyName != "child-b" {
		t.Fatalf("expected selected proxy name child-b, got %q", status.SelectedProxyName)
	}
	found := false
	foundLocal := false
	activeCount := 0
	for _, account := range status.Accounts {
		if account.Active {
			activeCount++
		}
		if account.ID == "b" && account.ProxyName == "child-b" {
			found = true
		}
		if account.ID == "local-a" && account.ProxyName == "edge-main" && !account.Active {
			foundLocal = true
		}
	}
	if !found {
		t.Fatalf("expected aggregated account from child-b, got %+v", status.Accounts)
	}
	if !foundLocal {
		t.Fatalf("expected local parent account to remain visible but inactive, got %+v", status.Accounts)
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly one active account on selected route, got %+v", status.Accounts)
	}
	if len(status.ChildProxies) != 2 {
		t.Fatalf("expected 2 child proxies in status, got %d", len(status.ChildProxies))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 1 || hits[0] != "b" {
		t.Fatalf("unexpected child hits: %v", hits)
	}
}

func TestProxyFailsOverAcrossChildProxies(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	var mu sync.Mutex
	order := []string{}
	newChild := func(name string, score float64, statusCode int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/status":
				_ = json.NewEncoder(w).Encode(ProxyStatus{
					ProxyName:       "child-" + name,
					GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
					SelectionReason: "usage-stay",
					Accounts: []AccountStatus{
						{ProxyName: "child-" + name, Alias: name, ID: name, Active: true, Healthy: true, Enabled: true, Score: score},
					},
				})
			case "/responses":
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				if statusCode == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "1")
				}
				w.WriteHeader(statusCode)
				_, _ = io.WriteString(w, `{"child":"`+name+`"}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}

	childA := newChild("a", 0.95, http.StatusTooManyRequests)
	defer childA.Close()
	childB := newChild("b", 0.80, http.StatusOK)
	defer childB.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Policy.Mode = PolicyUsageBalanced
		sf.Settings.Proxy.MaxAttempts = 3
		sf.Settings.Proxy.ChildProxyURLs = []string{childA.URL, childB.URL}
		sf.Accounts = nil
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	mainProxy := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer mainProxy.Close()

	resp, err := http.Post(mainProxy.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("post to main proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), `"child":"b"`) {
		t.Fatalf("expected failover to child b, got body=%s", string(body))
	}

	mu.Lock()
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("unexpected child request order: %v", order)
	}
	mu.Unlock()

	statusResp, err := http.Get(mainProxy.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer statusResp.Body.Close()
	var status ProxyStatus
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(status.ChildProxies) != 2 {
		t.Fatalf("expected 2 child proxies in status, got %d", len(status.ChildProxies))
	}
	if status.ChildProxies[0].CooldownSeconds <= 0 {
		t.Fatalf("expected first child proxy to be in cooldown, got %+v", status.ChildProxies[0])
	}
}

func TestProxyRetriesUsageLimitOnLocalRouteViaChildProxy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	tokenLocal := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-local"}})
	homeLocal := filepath.Join(root, "local")
	writeAuthFile(t, homeLocal, tokenLocal, "acct-local")

	var localHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":5},"secondary_window":{"used_percent":5}}}`)
		case "/backend-api/codex/responses":
			localHits++
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again later."}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	var childHits int
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			_ = json.NewEncoder(w).Encode(ProxyStatus{
				ProxyName:       "child-b",
				GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
				SelectionReason: "usage-stay",
				Accounts: []AccountStatus{
					{ProxyName: "child-b", Alias: "remote", ID: "remote", Active: true, Healthy: true, Enabled: true, Score: 0.20},
				},
			})
		case "/responses":
			childHits++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"source":"child"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer child.Close()

	nowMS := time.Now().UnixMilli()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "edge-main"
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Settings.Proxy.MaxAttempts = 3
		sf.Settings.Proxy.ChildProxyURLs = []string{child.URL}
		sf.Settings.Policy.Mode = PolicySticky
		sf.Settings.Quota.RefreshIntervalMinutes = 999
		sf.Settings.Quota.RefreshIntervalMessages = 999
		sf.Accounts = []Account{
			{
				ID:      "local",
				Alias:   "local",
				HomeDir: homeLocal,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:             100,
					DailyUsed:              5,
					WeeklyLimit:            100,
					WeeklyUsed:             5,
					LastSyncAt:             nowMS,
					LastSyncMessageCounter: 0,
					Source:                 "manual",
				},
			},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	mainProxy := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer mainProxy.Close()

	resp, err := http.Post(mainProxy.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("post to main proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), `"source":"child"`) {
		t.Fatalf("expected failover to child proxy, got body=%s", string(body))
	}
	if localHits != 1 {
		t.Fatalf("expected one local upstream hit, got %d", localHits)
	}
	if childHits != 1 {
		t.Fatalf("expected one child proxy hit, got %d", childHits)
	}

	snap := store.Snapshot()
	if !snap.Accounts[0].Enabled {
		t.Fatalf("expected local account to remain enabled")
	}
	if snap.Accounts[0].DisabledReason != "" {
		t.Fatalf("expected empty disabled reason, got %q", snap.Accounts[0].DisabledReason)
	}
	if snap.Accounts[0].CooldownUntilMS <= time.Now().UnixMilli() {
		t.Fatalf("expected local account cooldown to be set")
	}
	if snap.Accounts[0].LastSwitchReason != "usage-limit" {
		t.Fatalf("expected usage-limit switch reason, got %q", snap.Accounts[0].LastSwitchReason)
	}
}

func TestProxySelectsLocalAccountOverChildProxyWhenScoreIsHigher(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	tokenLocal := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-local"}})
	homeLocal := filepath.Join(root, "local")
	writeAuthFile(t, homeLocal, tokenLocal, "acct-local")

	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			_ = json.NewEncoder(w).Encode(ProxyStatus{
				ProxyName:       "edge-vpn",
				GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
				SelectionReason: "usage-stay",
				Accounts: []AccountStatus{
					{ProxyName: "edge-vpn", Alias: "remote", ID: "remote", Active: true, Healthy: true, Enabled: true, Score: 0.80},
				},
			})
		case "/responses":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"source":"child"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer child.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/responses":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"source":"local"}`)
		case "/backend-api/wham/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":0},"secondary_window":{"used_percent":0}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "edge-main"
		sf.Settings.Policy.Mode = PolicyUsageBalanced
		sf.Settings.Proxy.ChildProxyURLs = []string{child.URL}
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Accounts = []Account{
			{
				ID:      "local",
				Alias:   "f",
				HomeDir: homeLocal,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:  100,
					DailyUsed:   0,
					WeeklyLimit: 100,
					WeeklyUsed:  0,
					Source:      "manual",
				},
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	mainProxy := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer mainProxy.Close()

	resp, err := http.Post(mainProxy.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("post to main proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), `"source":"local"`) {
		t.Fatalf("expected local account to win selection, got body=%s", string(body))
	}

	statusResp, err := http.Get(mainProxy.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer statusResp.Body.Close()
	var status ProxyStatus
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.SelectedAccountID != "local" {
		t.Fatalf("expected local account selected, got %+v", status)
	}
	if status.SelectedProxyURL != "" || status.SelectedProxyName != "" {
		t.Fatalf("expected no selected child proxy when local wins, got %+v", status)
	}
	activeCount := 0
	for _, account := range status.Accounts {
		if account.Active {
			activeCount++
			if account.ID != "local" {
				t.Fatalf("expected only local account to be active, got %+v", status.Accounts)
			}
		}
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly one active account, got %+v", status.Accounts)
	}
}

func TestProxyPrefersLocalRouteWhenChildProxyAverageCapacityIsLower(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	localToken := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-local"}})
	homeLocal := filepath.Join(root, "local")
	writeAuthFile(t, homeLocal, localToken, "acct-local")

	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			_ = json.NewEncoder(w).Encode(ProxyStatus{
				ProxyName:       "edge-vpn",
				GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
				SelectionReason: "usage-stay",
				Accounts: []AccountStatus{
					{ProxyName: "edge-vpn", Alias: "vpn-a", ID: "vpn-a", Active: true, Healthy: true, Enabled: true, Score: 1.0},
					{ProxyName: "edge-vpn", Alias: "vpn-b", ID: "vpn-b", Active: false, Healthy: true, Enabled: true, Score: 0.0},
				},
			})
		case "/responses":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"source":"child"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer child.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/responses":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"source":"local"}`)
		case "/backend-api/wham/usage":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":20},"secondary_window":{"used_percent":20}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "edge-main"
		sf.Settings.Policy.Mode = PolicyUsageBalanced
		sf.Settings.Proxy.ChildProxyURLs = []string{child.URL}
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Accounts = []Account{
			{
				ID:      "local",
				Alias:   "local",
				HomeDir: homeLocal,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:  100,
					DailyUsed:   20,
					WeeklyLimit: 100,
					WeeklyUsed:  20,
					Source:      "manual",
				},
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	mainProxy := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer mainProxy.Close()

	resp, err := http.Post(mainProxy.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("post to main proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), `"source":"local"`) {
		t.Fatalf("expected local account to win selection, got body=%s", string(body))
	}

	statusResp, err := http.Get(mainProxy.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer statusResp.Body.Close()
	var status ProxyStatus
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.SelectedAccountID != "local" {
		t.Fatalf("expected local account selected, got %+v", status)
	}
	if status.SelectedProxyURL != "" || status.SelectedProxyName != "" {
		t.Fatalf("expected no selected child proxy when child average is lower, got %+v", status)
	}
}

func TestProxyChainsChildProxiesRecursively(t *testing.T) {
	t.Parallel()

	grandchild := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			_ = json.NewEncoder(w).Encode(ProxyStatus{
				ProxyName:       "grandchild",
				GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
				SelectionReason: "usage-stay",
				Accounts: []AccountStatus{
					{ProxyName: "grandchild", Alias: "leaf", ID: "leaf", Active: true, Healthy: true, Enabled: true, Score: 0.95},
				},
			})
		case "/responses":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"hop":"grandchild"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer grandchild.Close()

	childRoot := t.TempDir()
	childStore, err := OpenStore(childRoot)
	if err != nil {
		t.Fatalf("OpenStore child: %v", err)
	}
	if err := childStore.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "child"
		sf.Settings.Policy.Mode = PolicyUsageBalanced
		sf.Settings.Proxy.ChildProxyURLs = []string{grandchild.URL}
		sf.Accounts = nil
		return nil
	}); err != nil {
		t.Fatalf("child store update: %v", err)
	}
	childProxy := httptest.NewServer(NewProxyServer(childStore, nil, nil))
	defer childProxy.Close()

	mainRoot := t.TempDir()
	mainStore, err := OpenStore(mainRoot)
	if err != nil {
		t.Fatalf("OpenStore main: %v", err)
	}
	if err := mainStore.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "main"
		sf.Settings.Policy.Mode = PolicyUsageBalanced
		sf.Settings.Proxy.ChildProxyURLs = []string{childProxy.URL}
		sf.Accounts = nil
		return nil
	}); err != nil {
		t.Fatalf("main store update: %v", err)
	}
	mainProxy := httptest.NewServer(NewProxyServer(mainStore, nil, nil))
	defer mainProxy.Close()

	resp, err := http.Post(mainProxy.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("post to main proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), `"hop":"grandchild"`) {
		t.Fatalf("expected recursive chain to reach grandchild, got body=%s", string(body))
	}

	statusResp, err := http.Get(mainProxy.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer statusResp.Body.Close()
	var status ProxyStatus
	if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	foundLeaf := false
	for _, account := range status.Accounts {
		if account.ID == "leaf" && account.ProxyName == "grandchild" {
			foundLeaf = true
		}
	}
	if !foundLeaf {
		t.Fatalf("expected recursive status to include grandchild account origin, got %+v", status.Accounts)
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
