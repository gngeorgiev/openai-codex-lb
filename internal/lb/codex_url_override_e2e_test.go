package lb

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRealCodexUsesOPENAI_BASE_URL(t *testing.T) {
	if os.Getenv("CODEXLB_RUN_REAL_CODEX_TEST") != "1" {
		t.Skip("set CODEXLB_RUN_REAL_CODEX_TEST=1 to run real Codex URL override check")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not found")
	}

	var mu sync.Mutex
	hits := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "404 page not found")
	}))
	defer server.Close()

	tmpHome := t.TempDir()
	codexHome := filepath.Join(tmpHome, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("mkdir codex home: %v", err)
	}
	cmd := exec.CommandContext(context.Background(), "codex", "exec", "--skip-git-repo-check", "--sandbox", "read-only", "--json", "ping")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"HOME="+tmpHome,
		"CODEX_HOME="+codexHome,
		"OPENAI_API_KEY=dummy",
		"OPENAI_BASE_URL="+server.URL,
	)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("timed out waiting for codex command")
	case <-done:
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hits) == 0 {
		t.Fatal("expected mock server to receive at least one request")
	}
	matched := false
	for _, hit := range hits {
		if strings.Contains(hit, " /responses") {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("expected at least one /responses request, got: %v", hits)
	}
}
