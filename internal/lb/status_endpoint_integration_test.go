package lb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
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
		sf.Settings.Proxy.Name = "edge-a"
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
	if status.ProxyName != "edge-a" {
		t.Fatalf("expected proxy name edge-a, got %q", status.ProxyName)
	}
	if len(status.Accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(status.Accounts))
	}
	for _, account := range status.Accounts {
		if account.ProxyName != "edge-a" {
			t.Fatalf("expected account proxy name edge-a, got %+v", account)
		}
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

	usageStarted := make(chan struct{}, 1)
	releaseUsage := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/wham/usage" {
			select {
			case usageStarted <- struct{}{}:
			default:
			}
			<-releaseUsage
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

	done := make(chan struct{})
	var resp *http.Response
	go func() {
		resp, err = http.Get(srv.URL + "/status")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("expected /status to return before quota refresh completed")
	}
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	select {
	case <-usageStarted:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected /status to trigger a quota refresh call")
	}
	close(releaseUsage)

	if !waitFor(t, 2*time.Second, 20*time.Millisecond, func() bool {
		return store.Snapshot().Accounts[0].Quota.Source == "openai_usage_api"
	}) {
		t.Fatalf("expected quota source to be refreshed")
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

	if !waitFor(t, 2*time.Second, 20*time.Millisecond, func() bool {
		return store.Snapshot().Accounts[0].Enabled
	}) {
		t.Fatalf("expected account to auto-recover and be enabled")
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

	usageStarted := make(chan struct{}, 1)
	releaseUsage := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/wham/usage" {
			select {
			case usageStarted <- struct{}{}:
			default:
			}
			<-releaseUsage
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

	select {
	case <-usageStarted:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected canceled /status request to still start a quota refresh")
	}
	close(releaseUsage)

	if !waitFor(t, 2*time.Second, 20*time.Millisecond, func() bool {
		snap := store.Snapshot()
		return snap.Accounts[0].Quota.Source == "openai_usage_api" &&
			snap.Accounts[0].Quota.DailyUsed == 7 &&
			snap.Accounts[0].Quota.WeeklyUsed == 11
	}) {
		snap := store.Snapshot()
		t.Fatalf("expected quota refresh to complete after canceled request, got %+v", snap.Accounts[0].Quota)
	}
}

func TestProxyStatusEndpointUsesCachedChildProxyStatusWhenRefreshIsSlow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	delayRefresh := make(chan struct{}, 1)
	refreshStarted := make(chan struct{}, 1)
	releaseRefresh := make(chan struct{})
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		select {
		case <-delayRefresh:
			select {
			case refreshStarted <- struct{}{}:
			default:
			}
			<-releaseRefresh
		default:
		}
		_ = json.NewEncoder(w).Encode(ProxyStatus{
			ProxyName:       "child-a",
			GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
			SelectionReason: "usage-stay",
			Accounts: []AccountStatus{
				{ProxyName: "child-a", Alias: "a", ID: "a", Active: true, Healthy: true, Enabled: true, Score: 0.9},
			},
		})
	}))
	defer child.Close()
	defer close(releaseRefresh)

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "main"
		sf.Settings.Policy.Mode = PolicyUsageBalanced
		sf.Settings.Proxy.ChildProxyURLs = []string{child.URL}
		sf.Accounts = nil
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxy := NewProxyServer(store, nil, nil)
	now := time.Now()
	snapshot := store.Snapshot()
	sel, target, err := proxy.selectChildProxy(context.Background(), snapshot, now, true)
	if err != nil {
		t.Fatalf("warm child proxy status: %v", err)
	}
	proxy.markChildProxySuccess(sel, target.URL, now)

	delayRefresh <- struct{}{}
	srv := httptest.NewServer(proxy)
	defer srv.Close()

	done := make(chan struct{})
	var resp *http.Response
	go func() {
		resp, err = http.Get(srv.URL + "/status")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("expected /status to return before child proxy refresh completed")
	}
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
	if status.SelectedProxyURL != child.URL {
		t.Fatalf("expected selected proxy %q, got %q", child.URL, status.SelectedProxyURL)
	}
	if status.SelectedProxyName != "child-a" {
		t.Fatalf("expected selected proxy name child-a, got %q", status.SelectedProxyName)
	}
	if len(status.Accounts) != 1 || status.Accounts[0].ID != "a" || !status.Accounts[0].Active {
		t.Fatalf("expected cached child account to stay visible, got %+v", status.Accounts)
	}

	select {
	case <-refreshStarted:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected /status to refresh child proxy status in the background")
	}
}

func TestProxyStatusEndpointUsesCachedChildProxyStatusOnTransientRefreshFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	var failRefresh atomic.Bool
	child := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if failRefresh.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `temporary failure`)
			return
		}
		_ = json.NewEncoder(w).Encode(ProxyStatus{
			ProxyName:       "child-a",
			GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
			SelectionReason: "usage-stay",
			Accounts: []AccountStatus{
				{ProxyName: "child-a", Alias: "a", ID: "a", Active: true, Healthy: true, Enabled: true, Score: 0.9},
			},
		})
	}))
	defer child.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "main"
		sf.Settings.Policy.Mode = PolicyUsageBalanced
		sf.Settings.Proxy.ChildProxyURLs = []string{child.URL}
		sf.Accounts = nil
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	proxy := NewProxyServer(store, nil, nil)
	now := time.Now()
	snapshot := store.Snapshot()
	sel, target, err := proxy.selectChildProxy(context.Background(), snapshot, now, true)
	if err != nil {
		t.Fatalf("warm child proxy status: %v", err)
	}
	proxy.markChildProxySuccess(sel, target.URL, now)
	failRefresh.Store(true)

	srv := httptest.NewServer(proxy)
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
	if status.SelectedProxyURL != child.URL {
		t.Fatalf("expected selected proxy %q, got %q", child.URL, status.SelectedProxyURL)
	}
	if status.SelectedProxyName != "child-a" {
		t.Fatalf("expected selected proxy name child-a, got %q", status.SelectedProxyName)
	}
	if len(status.Accounts) != 1 || status.Accounts[0].ID != "a" || !status.Accounts[0].Active {
		t.Fatalf("expected cached child account to stay visible, got %+v", status.Accounts)
	}
	if len(status.ChildProxies) != 1 {
		t.Fatalf("expected one child proxy, got %+v", status.ChildProxies)
	}
	if !status.ChildProxies[0].Reachable || !status.ChildProxies[0].Healthy {
		t.Fatalf("expected child proxy to stay reachable and healthy, got %+v", status.ChildProxies[0])
	}
}
