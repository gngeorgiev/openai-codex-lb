package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gngeorgiev/openai-codex-lb/internal/lb"
)

func TestAccountListRemote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/accounts" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(lb.AdminAccountsResponse{
			Accounts: []lb.Account{{Alias: "alice", ID: "openai:alice", UserEmail: "a@example.com", Enabled: true}},
		})
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"account", "list", "--proxy-url", server.URL})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if !strings.Contains(out, "alice\topenai:alice\ta@example.com\tready") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestAccountImportRemote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/account/import" {
			http.NotFound(w, r)
			return
		}
		var req lb.AdminImportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Alias != "alice" || req.FromHome != "/srv/codex/alice" {
			t.Fatalf("unexpected request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true, Total: 2})
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"account", "import", "--proxy-url", server.URL, "--from", "/srv/codex/alice", "alice"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if !strings.Contains(out, "imported account alice (total=2)") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestAccountPinRemoteError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/account/pin" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "alias not found: missing"})
	}))
	defer server.Close()

	errOut, code := captureStderr(func() int {
		return run([]string{"account", "pin", "--proxy-url", server.URL, "missing"})
	})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "alias not found: missing") {
		t.Fatalf("unexpected stderr: %q", errOut)
	}
}

func TestAccountLoginDefaultsToConfiguredProxyURL(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/account/login" {
			http.NotFound(w, r)
			return
		}
		calls++
		var req lb.AdminLoginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Alias != "alice" {
			t.Fatalf("unexpected alias: %q", req.Alias)
		}
		_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true, Total: 3})
	}))
	defer server.Close()

	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Snapshot().Settings
	cfg.ProxyURL = server.URL
	if err := lb.WriteSettingsConfig(root, cfg); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	out, code := captureStdout(func() int {
		return run([]string{"account", "login", "--root", root, "alice"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if calls != 1 {
		t.Fatalf("expected one remote login call, got %d", calls)
	}
	if !strings.Contains(out, "registered account alice (total=3)") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestAccountImportDefaultsToConfiguredProxyURL(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/account/import" {
			http.NotFound(w, r)
			return
		}
		calls++
		var req lb.AdminImportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Alias != "alice" || req.FromHome != "/srv/codex/alice" {
			t.Fatalf("unexpected request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true, Total: 4})
	}))
	defer server.Close()

	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Snapshot().Settings
	cfg.ProxyURL = server.URL
	if err := lb.WriteSettingsConfig(root, cfg); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	out, code := captureStdout(func() int {
		return run([]string{"account", "import", "--root", root, "--from", "/srv/codex/alice", "alice"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if calls != 1 {
		t.Fatalf("expected one remote import call, got %d", calls)
	}
	if !strings.Contains(out, "imported account alice (total=4)") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestAccountListDefaultsToConfiguredProxyURL(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/accounts" {
			http.NotFound(w, r)
			return
		}
		calls++
		_ = json.NewEncoder(w).Encode(lb.AdminAccountsResponse{
			Accounts: []lb.Account{{Alias: "alice", ID: "openai:alice", UserEmail: "a@example.com", Enabled: true}},
		})
	}))
	defer server.Close()

	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Snapshot().Settings
	cfg.ProxyURL = server.URL
	if err := lb.WriteSettingsConfig(root, cfg); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	out, code := captureStdout(func() int {
		return run([]string{"account", "list", "--root", root})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if calls != 1 {
		t.Fatalf("expected one remote list call, got %d", calls)
	}
	if !strings.Contains(out, "alice\topenai:alice\ta@example.com\tready") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestAccountRemoveDefaultsToConfiguredProxyURL(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/account/rm" {
			http.NotFound(w, r)
			return
		}
		calls++
		var req lb.AdminAliasRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Alias != "alice" {
			t.Fatalf("unexpected alias: %q", req.Alias)
		}
		_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true})
	}))
	defer server.Close()

	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Snapshot().Settings
	cfg.ProxyURL = server.URL
	if err := lb.WriteSettingsConfig(root, cfg); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	out, code := captureStdout(func() int {
		return run([]string{"account", "rm", "--root", root, "alice"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if calls != 1 {
		t.Fatalf("expected one remote remove call, got %d", calls)
	}
	if !strings.Contains(out, "removed account alice") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestAccountPinDefaultsToConfiguredProxyURL(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/account/pin" {
			http.NotFound(w, r)
			return
		}
		calls++
		var req lb.AdminAliasRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Alias != "g" {
			t.Fatalf("unexpected alias: %q", req.Alias)
		}
		_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true})
	}))
	defer server.Close()

	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Snapshot().Settings
	cfg.ProxyURL = server.URL
	if err := lb.WriteSettingsConfig(root, cfg); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	out, code := captureStdout(func() int {
		return run([]string{"account", "pin", "--root", root, "g"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if calls != 1 {
		t.Fatalf("expected one remote pin call, got %d", calls)
	}
	if !strings.Contains(out, "pinned account g") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestAccountUnpinDefaultsToConfiguredProxyURL(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/account/unpin" {
			http.NotFound(w, r)
			return
		}
		calls++
		_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true})
	}))
	defer server.Close()

	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Snapshot().Settings
	cfg.ProxyURL = server.URL
	if err := lb.WriteSettingsConfig(root, cfg); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	out, code := captureStdout(func() int {
		return run([]string{"account", "unpin", "--root", root})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if calls != 1 {
		t.Fatalf("expected one remote unpin call, got %d", calls)
	}
	if !strings.Contains(out, "unpinned account selection") {
		t.Fatalf("unexpected output: %q", out)
	}
}
