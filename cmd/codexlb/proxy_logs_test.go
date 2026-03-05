package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyLogsCommandTail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/logs" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("tail"); got != "2" {
			t.Fatalf("expected tail=2, got %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "500" {
			t.Fatalf("expected limit=500, got %q", got)
		}
		w.Header().Set("X-Next-Offset", "123")
		_, _ = w.Write([]byte("line1\nline2\n"))
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"proxy", "logs", "--root", t.TempDir(), "--proxy-url", server.URL, "--tail", "2"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d output=%s", code, out)
	}
	if strings.TrimSpace(out) != "line1\nline2" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestProxyLogsCommandOffset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/logs" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("offset"); got != "42" {
			t.Fatalf("expected offset=42, got %q", got)
		}
		_, _ = w.Write([]byte(""))
	}))
	defer server.Close()

	_, code := captureStdout(func() int {
		return run([]string{"proxy", "logs", "--root", t.TempDir(), "--proxy-url", server.URL, "--offset", "42"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}
