package lb

import (
	"bytes"
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
