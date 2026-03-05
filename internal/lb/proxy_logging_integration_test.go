package lb

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyWritesSwitchAndRequestLogs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	token := testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"},
	})
	home := filepath.Join(root, "acc-a")
	writeAuthFile(t, home, token, "acct-a")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = io.WriteString(w, `{"rate_limit":{"primary_window":{"used_percent":50},"secondary_window":{"used_percent":50}}}`)
		case "/backend-api/codex/responses":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	if err := store.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.UpstreamBaseURL = upstream.URL + "/backend-api"
		sf.Accounts = []Account{{ID: "a", Alias: "a", HomeDir: home, BaseURL: sf.Settings.Proxy.UpstreamBaseURL, Enabled: true}}
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	events, err := OpenEventLogger(root)
	if err != nil {
		t.Fatalf("OpenEventLogger: %v", err)
	}
	proxySrv := httptest.NewServer(NewProxyServer(store, nil, events))
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/responses", "application/json", bytes.NewBufferString(`{"input":"hello"}`))
	if err != nil {
		t.Fatalf("post proxy: %v", err)
	}
	_ = resp.Body.Close()
	_ = events.Close()

	data, err := os.ReadFile(filepath.Join(root, "logs", "proxy.current.jsonl"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `"event":"request.account_selected"`) {
		t.Fatalf("missing request.account_selected event: %s", text)
	}
	if !strings.Contains(text, `"event":"request.completed"`) {
		t.Fatalf("missing request.completed event: %s", text)
	}
}
