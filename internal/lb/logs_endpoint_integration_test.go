package lb

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyLogsEndpointTailAndOffset(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	logPath := filepath.Join(logDir, "proxy.current.jsonl")
	content := "l1\nl2\nl3\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	srv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/logs?tail=2")
	if err != nil {
		t.Fatalf("GET /logs tail: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if got := string(body); got != "l2\nl3\n" {
		t.Fatalf("unexpected tail body: %q", got)
	}
	if got := strings.TrimSpace(resp.Header.Get("X-Next-Offset")); got == "" {
		t.Fatalf("expected X-Next-Offset header")
	}

	resp2, err := http.Get(srv.URL + "/logs?offset=3&limit=10")
	if err != nil {
		t.Fatalf("GET /logs offset: %v", err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp2.StatusCode, string(body2))
	}
	if got := string(body2); got != "l2\nl3\n" {
		t.Fatalf("unexpected offset body: %q", got)
	}
}
