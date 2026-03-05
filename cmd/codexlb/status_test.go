package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gngeorgiev/openai-codex-lb/internal/lb"
)

func TestStatusCommandPrintsTable(t *testing.T) {
	status := lb.ProxyStatus{
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		Policy:            lb.PolicyConfig{Mode: lb.PolicyUsageBalanced},
		SelectedAccountID: "openai:alice",
		SelectionReason:   "usage-stay",
		Accounts: []lb.AccountStatus{
			{Alias: "alice", ID: "openai:alice", Email: "a@example.com", Active: true, Healthy: true, Enabled: true, DailyLeftPct: 80, WeeklyLeftPct: 70, Score: 0.75},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"status", "--root", t.TempDir(), "--proxy-url", server.URL})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d output=%s", code, out)
	}
	if !strings.Contains(out, "policy=usage_balanced") {
		t.Fatalf("expected policy line in output: %s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Fatalf("expected account row in output: %s", out)
	}
}

func TestStatusCommandJSON(t *testing.T) {
	status := lb.ProxyStatus{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Accounts: []lb.AccountStatus{{Alias: "a", ID: "id-a"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"status", "--root", t.TempDir(), "--proxy-url", server.URL, "--json"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(out, `"accounts"`) {
		t.Fatalf("expected json output, got: %s", out)
	}
}

func TestStatusCommandShort(t *testing.T) {
	status := lb.ProxyStatus{
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		Policy:          lb.PolicyConfig{Mode: lb.PolicyUsageBalanced},
		SelectionReason: "usage-stay",
		Accounts: []lb.AccountStatus{
			{Alias: "alice", ID: "openai:alice", Active: true},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"status", "--root", t.TempDir(), "--proxy-url", server.URL, "--short"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d output=%s", code, out)
	}
	line := strings.TrimSpace(out)
	if line != "lb=alice reason=usage-stay mode=usage_balanced" {
		t.Fatalf("unexpected short status line: %q", line)
	}
}

func TestStatusCommandJSONAndShortMutuallyExclusive(t *testing.T) {
	errOut, code := captureStderr(func() int {
		return run([]string{"status", "--root", t.TempDir(), "--json", "--short"})
	})
	if code != 2 {
		t.Fatalf("expected exit 2, got %d", code)
	}
	if !strings.Contains(errOut, "mutually exclusive") {
		t.Fatalf("expected mutual exclusion error, got: %s", errOut)
	}
}

func captureStdout(fn func() int) (string, int) {
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := fn()
	_ = w.Close()
	os.Stdout = orig
	buf := &bytes.Buffer{}
	_, _ = io.Copy(buf, r)
	_ = r.Close()
	return buf.String(), code
}

func captureStderr(fn func() int) (string, int) {
	orig := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	code := fn()
	_ = w.Close()
	os.Stderr = orig
	buf := &bytes.Buffer{}
	_, _ = io.Copy(buf, r)
	_ = r.Close()
	return buf.String(), code
}
