package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gngeorgiev/openai-codex-lb/internal/lb"
)

func TestStatusCommandPrintsTable(t *testing.T) {
	status := lb.ProxyStatus{
		ProxyName:         "main",
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		Policy:            lb.PolicyConfig{Mode: lb.PolicyUsageBalanced},
		SelectedAccountID: "openai:alice",
		State:             lb.RuntimeState{PinnedAccountID: "openai:alice"},
		SelectionReason:   "usage-stay",
		Accounts: []lb.AccountStatus{
			{ProxyName: "main", Alias: "alice", ID: "openai:alice", Email: "a@example.com", Active: true, Healthy: true, Enabled: true, DailyLeftPct: 80, DailyResetAt: 1710000000, WeeklyLeftPct: 70, WeeklyResetAt: 1710600000, Score: 0.75},
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
	if !strings.Contains(out, "proxy=main") {
		t.Fatalf("expected proxy name in output: %s", out)
	}
	if !strings.Contains(out, "pinned=alice") {
		t.Fatalf("expected pinned alias in output: %s", out)
	}
	if !regexp.MustCompile(`(?m)^\*\s+P\s+main\s+alice\s+openai:alice`).MatchString(out) {
		t.Fatalf("expected pinned marker in account row: %s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Fatalf("expected account row in output: %s", out)
	}
	if !strings.Contains(out, "2024-03-09T16:00:00Z") {
		t.Fatalf("expected daily reset timestamp in output: %s", out)
	}
	if !strings.Contains(out, "2024-03-16T14:40:00Z") {
		t.Fatalf("expected weekly reset timestamp in output: %s", out)
	}
	if !strings.Contains(out, "aggregate usage left: daily=80.0% weekly=70.0%") {
		t.Fatalf("expected aggregate usage line in output: %s", out)
	}
}

func TestStatusCommandAggregateUsageLeftAveragesAccounts(t *testing.T) {
	status := lb.ProxyStatus{
		ProxyName:   "main",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Policy:      lb.PolicyConfig{Mode: lb.PolicyUsageBalanced},
		Accounts: []lb.AccountStatus{
			{ProxyName: "main", Alias: "a", ID: "openai:a", Enabled: true, DailyLeftPct: 100, WeeklyLeftPct: 80},
			{ProxyName: "main", Alias: "b", ID: "openai:b", Enabled: true, DailyLeftPct: 70, WeeklyLeftPct: 50},
			{ProxyName: "main", Alias: "c", ID: "openai:c", Enabled: true, DailyLeftPct: 40, WeeklyLeftPct: 20},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"status", "--root", t.TempDir(), "--proxy-url", server.URL})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d output=%s", code, out)
	}
	if !strings.Contains(out, "aggregate usage left: daily=70.0% weekly=50.0%") {
		t.Fatalf("unexpected aggregate usage line: %s", out)
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
		ProxyName:       "main",
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		Policy:          lb.PolicyConfig{Mode: lb.PolicyUsageBalanced},
		SelectionReason: "usage-stay",
		Accounts: []lb.AccountStatus{
			{ProxyName: "main", Alias: "alice", ID: "openai:alice", Active: true},
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

func TestStatusCommandShortUsesActiveChildProxy(t *testing.T) {
	status := lb.ProxyStatus{
		ProxyName:         "main",
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		Policy:            lb.PolicyConfig{Mode: lb.PolicyUsageBalanced},
		SelectedProxyName: "child-b",
		SelectionReason:   "usage-stay",
		ChildProxies: []lb.ChildProxyStatus{
			{Name: "child-b", URL: "http://child-b.internal", Active: true, Healthy: true, Reachable: true, Score: 0.9},
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
	if line != "lb=child-b reason=usage-stay mode=usage_balanced" {
		t.Fatalf("unexpected short status line: %q", line)
	}
}

func TestStatusCommandPrintsChildProxyTable(t *testing.T) {
	status := lb.ProxyStatus{
		ProxyName:         "main",
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		Policy:            lb.PolicyConfig{Mode: lb.PolicyUsageBalanced},
		SelectedProxyURL:  "http://child-b.internal",
		SelectedProxyName: "child-b",
		SelectionReason:   "usage-stay",
		Accounts: []lb.AccountStatus{
			{ProxyName: "child-b", Alias: "bob", ID: "openai:bob", Active: true, Healthy: true, Enabled: true, Score: 0.9},
		},
		ChildProxies: []lb.ChildProxyStatus{
			{
				Name:            "child-b",
				URL:             "http://child-b.internal",
				Active:          true,
				Healthy:         true,
				Reachable:       true,
				Score:           0.9,
				SelectedTarget:  "openai:bob",
				SelectionReason: "usage-stay",
			},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"status", "--root", t.TempDir(), "--proxy-url", server.URL})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d output=%s", code, out)
	}
	if !strings.Contains(out, "selected=child-b") {
		t.Fatalf("expected selected child proxy in output: %s", out)
	}
	if !strings.Contains(out, "child-b") {
		t.Fatalf("expected child proxy name in output: %s", out)
	}
	if !strings.Contains(out, "http://child-b.internal") {
		t.Fatalf("expected child proxy row in output: %s", out)
	}
	if !strings.Contains(out, "openai:bob") {
		t.Fatalf("expected child selected target in output: %s", out)
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

func TestStatusCommandDefaultsToRunProxyURL(t *testing.T) {
	status := lb.ProxyStatus{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()

	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Snapshot().Settings
	cfg.ProxyURL = server.URL
	cfg.Proxy.Listen = "127.0.0.1:1"
	if err := lb.WriteSettingsConfig(root, cfg); err != nil {
		t.Fatalf("write settings config: %v", err)
	}

	_, code := captureStdout(func() int {
		return run([]string{"status", "--root", root})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestStatusCommandUsesCODEXLBProxyURL(t *testing.T) {
	status := lb.ProxyStatus{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()

	t.Setenv("CODEXLB_PROXY_URL", server.URL)

	_, code := captureStdout(func() int {
		return run([]string{"status", "--root", t.TempDir()})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestStatusCommandUsesCODEXLBRoot(t *testing.T) {
	status := lb.ProxyStatus{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()

	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Snapshot().Settings
	cfg.ProxyURL = server.URL
	cfg.Proxy.Listen = "127.0.0.1:1"
	if err := lb.WriteSettingsConfig(root, cfg); err != nil {
		t.Fatalf("write settings config: %v", err)
	}

	t.Setenv("CODEXLB_ROOT", root)

	_, code := captureStdout(func() int {
		return run([]string{"status"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestParseProxyURLListEnv(t *testing.T) {
	got := parseProxyURLListEnv(" http://child-a.internal/,\nhttp://child-b.internal/base  http://child-a.internal/ ")
	if len(got) != 2 {
		t.Fatalf("expected 2 child proxy urls, got %#v", got)
	}
	if got[0] != "http://child-a.internal" || got[1] != "http://child-b.internal/base" {
		t.Fatalf("unexpected parsed urls: %#v", got)
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
