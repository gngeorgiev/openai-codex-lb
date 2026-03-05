package lb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEventLoggerWritesJSONL(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	logger, err := OpenEventLogger(root)
	if err != nil {
		t.Fatalf("OpenEventLogger: %v", err)
	}
	defer logger.Close()

	logger.Log("request.completed", map[string]any{"status": 200, "account_id": "a"})
	if err := logger.Close(); err != nil {
		t.Fatalf("close logger: %v", err)
	}

	path := filepath.Join(root, "logs", "proxy.current.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 1 {
		t.Fatalf("expected at least one log line")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatalf("invalid json line: %v", err)
	}
	if rec["event"] != "request.completed" {
		t.Fatalf("unexpected event: %#v", rec["event"])
	}
}
