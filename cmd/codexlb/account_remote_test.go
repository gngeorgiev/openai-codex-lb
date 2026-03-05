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
