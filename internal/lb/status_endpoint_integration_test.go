package lb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestProxyStatusEndpoint(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	nowMS := time.Now().UnixMilli()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Policy.Mode = PolicyUsageBalanced
		sf.Accounts = []Account{
			{
				Alias:   "alice",
				ID:      "openai:alice",
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:  100,
					DailyUsed:   60,
					WeeklyLimit: 100,
					WeeklyUsed:  70,
					LastSyncAt:  nowMS,
				},
			},
			{
				Alias:   "bob",
				ID:      "openai:bob",
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:  100,
					DailyUsed:   20,
					WeeklyLimit: 100,
					WeeklyUsed:  30,
					LastSyncAt:  nowMS,
				},
			},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	srv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var status ProxyStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.SelectedAccountID != "openai:bob" {
		t.Fatalf("expected selected openai:bob, got %q", status.SelectedAccountID)
	}
	if len(status.Accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(status.Accounts))
	}
}

func TestProxyStatusEndpointForceRefreshesQuotaEvenWhenNotDue(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	token := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-f"}})
	home := filepath.Join(root, "acc-f")
	writeAuthFile(t, home, token, "acct-f")

	usageCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/wham/usage" {
			usageCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":12},"secondary_window":{"used_percent":34}}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	nowMS := time.Now().UnixMilli()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Settings.Quota.RefreshIntervalMinutes = 999
		sf.Settings.Quota.RefreshIntervalMessages = 999
		sf.Accounts = []Account{
			{
				Alias:   "f",
				ID:      "openai:f",
				HomeDir: home,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:             100,
					DailyUsed:              0,
					WeeklyLimit:            100,
					WeeklyUsed:             0,
					LastSyncAt:             nowMS,
					LastSyncMessageCounter: 0,
				},
			},
		}
		sf.State.ActiveIndex = 0
		sf.State.MessageCounter = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	srv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if usageCalls == 0 {
		t.Fatalf("expected /status to force a quota refresh call")
	}

	snap := store.Snapshot()
	if snap.Accounts[0].Quota.Source != "openai_usage_api" {
		t.Fatalf("expected quota source to be refreshed, got %q", snap.Accounts[0].Quota.Source)
	}
}

func TestProxyStatusEndpointAutoRecoversDisabledAccount(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	token := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-f"}})
	home := filepath.Join(root, "acc-f")
	writeAuthFile(t, home, token, "acct-f")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/wham/usage" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":5},"secondary_window":{"used_percent":10}}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	nowMS := time.Now().UnixMilli()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Settings.Quota.RefreshIntervalMinutes = 999
		sf.Settings.Quota.RefreshIntervalMessages = 999
		sf.Accounts = []Account{
			{
				Alias:            "f",
				ID:               "openai:f",
				HomeDir:          home,
				BaseURL:          sf.Settings.Proxy.UpstreamBaseURL,
				Enabled:          false,
				DisabledReason:   "http-403",
				LastSwitchReason: "http-403",
				Quota: QuotaState{
					DailyLimit:             100,
					DailyUsed:              99,
					WeeklyLimit:            100,
					WeeklyUsed:             99,
					LastSyncAt:             nowMS,
					LastSyncMessageCounter: 0,
				},
			},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	srv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	snap := store.Snapshot()
	if !snap.Accounts[0].Enabled {
		t.Fatalf("expected account to auto-recover and be enabled")
	}
	if snap.Accounts[0].DisabledReason != "" {
		t.Fatalf("expected disabled reason to be cleared, got %q", snap.Accounts[0].DisabledReason)
	}
	if snap.Accounts[0].LastSwitchReason != "quota-refresh-recovered" {
		t.Fatalf("expected recovery marker, got %q", snap.Accounts[0].LastSwitchReason)
	}
}

func TestProxyStatusEndpointRefreshesQuotaWhenRequestContextIsCanceled(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	token := testJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-c"}})
	home := filepath.Join(root, "acc-c")
	writeAuthFile(t, home, token, "acct-c")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/wham/usage" {
			time.Sleep(25 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":7},"secondary_window":{"used_percent":11}}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	nowMS := time.Now().UnixMilli()
	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Accounts = []Account{
			{
				Alias:   "c",
				ID:      "openai:c",
				HomeDir: home,
				BaseURL: sf.Settings.Proxy.UpstreamBaseURL,
				Enabled: true,
				Quota: QuotaState{
					DailyLimit:             100,
					DailyUsed:              0,
					WeeklyLimit:            100,
					WeeklyUsed:             0,
					LastSyncAt:             nowMS,
					LastSyncMessageCounter: 0,
				},
			},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/status", nil).WithContext(reqCtx)
	rec := httptest.NewRecorder()

	NewProxyServer(store, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	snap := store.Snapshot()
	if snap.Accounts[0].Quota.Source != "openai_usage_api" {
		t.Fatalf("expected quota source to be refreshed, got %q", snap.Accounts[0].Quota.Source)
	}
	if snap.Accounts[0].Quota.DailyUsed != 7 {
		t.Fatalf("expected daily used to be refreshed, got %v", snap.Accounts[0].Quota.DailyUsed)
	}
	if snap.Accounts[0].Quota.WeeklyUsed != 11 {
		t.Fatalf("expected weekly used to be refreshed, got %v", snap.Accounts[0].Quota.WeeklyUsed)
	}
}
