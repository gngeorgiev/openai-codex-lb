package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	source := t.TempDir()
	auth := `{"tokens":{"access_token":"` + testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-import"},
	}) + `","account_id":"acct-import"}}`
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("model = \"gpt-5\"\n"), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/account/import" {
			http.NotFound(w, r)
			return
		}
		var req lb.AdminImportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Alias != "alice" || req.FromHome != "" {
			t.Fatalf("unexpected request: %+v", req)
		}
		if len(req.Auth) == 0 || !json.Valid(req.Auth) {
			t.Fatalf("expected auth payload to be uploaded")
		}
		if strings.TrimSpace(req.Config) == "" {
			t.Fatalf("expected config payload to be uploaded")
		}
		_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true, Total: 2})
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"account", "import", "--into", "proxy", "--proxy-url", server.URL, "--from", source, "alice"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if !strings.Contains(out, "imported account alice (total=2)") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestAccountImportRemoteDefaultsSourceAndAlias(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".codex")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	token := testJWT(map[string]any{})
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"tokens":{"access_token":"`+token+`"}}`), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("profile = \"remote-work\"\n"), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	listCalls := 0
	importCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/accounts":
			listCalls++
			_ = json.NewEncoder(w).Encode(lb.AdminAccountsResponse{})
		case r.Method == http.MethodPost && r.URL.Path == "/admin/account/import":
			importCalls++
			var req lb.AdminImportRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Alias != "remote-work" || req.FromHome != "" {
				t.Fatalf("unexpected request: %+v", req)
			}
			if len(req.Auth) == 0 || !json.Valid(req.Auth) {
				t.Fatalf("expected uploaded auth payload")
			}
			_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true, Total: 2})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"account", "import", "--into", "proxy", "--proxy-url", server.URL})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if listCalls != 1 {
		t.Fatalf("expected one remote list call, got %d", listCalls)
	}
	if importCalls != 1 {
		t.Fatalf("expected one remote import call, got %d", importCalls)
	}
	if !strings.Contains(out, "imported account remote-work (total=2)") {
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

func TestAccountLoginRemoteRunsLocallyAndImports(t *testing.T) {
	root := t.TempDir()
	fakeLog := filepath.Join(root, "fake-codex.log")
	fakeBin := filepath.Join(root, "codex")
	writeFakeCodex(t, fakeBin)

	t.Setenv("CODEXLB_CODEX_BIN", fakeBin)
	t.Setenv("FAKE_LOG", fakeLog)
	t.Setenv("FAKE_TOKEN", testJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-a"},
		"https://api.openai.com/profile": map[string]any{"email": "alice@example.com"},
	}))
	t.Setenv("FAKE_ACCOUNT_ID", "acct-a")

	importCalls := 0
	loginCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/account/login":
			loginCalls++
			http.NotFound(w, r)
			return
		case "/admin/account/import":
		default:
			http.NotFound(w, r)
			return
		}
		importCalls++
		var req lb.AdminImportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Alias != "alice" {
			t.Fatalf("unexpected alias: %q", req.Alias)
		}
		if len(req.Auth) == 0 || !json.Valid(req.Auth) {
			t.Fatalf("expected auth payload to be uploaded")
		}
		var auth map[string]any
		if err := json.Unmarshal(req.Auth, &auth); err != nil {
			t.Fatalf("unmarshal auth payload: %v", err)
		}
		tokens, _ := auth["tokens"].(map[string]any)
		if got, _ := tokens["account_id"].(string); got != "acct-a" {
			t.Fatalf("unexpected uploaded account id: %q", got)
		}
		_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true, Total: 3})
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"account", "login", "--root", root, "--proxy-url", server.URL, "alice"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if importCalls != 1 {
		t.Fatalf("expected one remote import call, got %d", importCalls)
	}
	if loginCalls != 0 {
		t.Fatalf("expected no remote login calls, got %d", loginCalls)
	}
	if !strings.Contains(out, "registered account alice (total=3)") {
		t.Fatalf("unexpected output: %q", out)
	}
	data, err := os.ReadFile(fakeLog)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	if !strings.Contains(string(data), "LOGIN_ARGS=login") {
		t.Fatalf("expected local codex login to run, got: %s", string(data))
	}
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store after remote login: %v", err)
	}
	if got := len(store.Snapshot().Accounts); got != 0 {
		t.Fatalf("expected no local accounts to be registered, got %d", got)
	}
}

func TestAccountLoginDefaultsToConfiguredProxyURL(t *testing.T) {
	root := t.TempDir()
	fakeLog := filepath.Join(root, "fake-codex.log")
	fakeBin := filepath.Join(root, "codex")
	writeFakeCodex(t, fakeBin)

	t.Setenv("CODEXLB_CODEX_BIN", fakeBin)
	t.Setenv("FAKE_LOG", fakeLog)
	t.Setenv("FAKE_TOKEN", testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"},
	}))
	t.Setenv("FAKE_ACCOUNT_ID", "acct-a")

	importCalls := 0
	loginCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/account/login":
			loginCalls++
			http.NotFound(w, r)
			return
		case "/admin/account/import":
		default:
			http.NotFound(w, r)
			return
		}
		importCalls++
		var req lb.AdminImportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Alias != "alice" || len(req.Auth) == 0 {
			t.Fatalf("unexpected request: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true, Total: 3})
	}))
	defer server.Close()

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
	if importCalls != 1 {
		t.Fatalf("expected one remote import call, got %d", importCalls)
	}
	if loginCalls != 0 {
		t.Fatalf("expected zero remote login calls, got %d", loginCalls)
	}
	if !strings.Contains(out, "registered account alice (total=3)") {
		t.Fatalf("unexpected output: %q", out)
	}
	data, err := os.ReadFile(fakeLog)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	if !strings.Contains(string(data), "LOGIN_ARGS=login") {
		t.Fatalf("expected local codex login to run, got: %s", string(data))
	}
}

func TestAccountImportDefaultsToConfiguredProxyURL(t *testing.T) {
	source := t.TempDir()
	auth := `{"tokens":{"access_token":"` + testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-import"},
	}) + `","account_id":"acct-import"}}`
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

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
		if req.Alias != "alice" || req.FromHome != "" || len(req.Auth) == 0 {
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
		return run([]string{"account", "import", "--root", root, "--into", "proxy", "--from", source, "alice"})
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

func TestAccountImportDefaultsToLocalEvenWhenProxyConfigured(t *testing.T) {
	source := t.TempDir()
	auth := `{"tokens":{"access_token":"` + testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-local"},
	}) + `","account_id":"acct-local"}}`
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
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
		return run([]string{"account", "import", "--root", root, "--from", source, "alice"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if calls != 0 {
		t.Fatalf("expected no remote calls, got %d", calls)
	}
	if !strings.Contains(out, "imported account alice (total=1)") {
		t.Fatalf("unexpected output: %q", out)
	}
	if _, err := os.Stat(filepath.Join(root, "accounts", "alice", "auth.json")); err != nil {
		t.Fatalf("expected local imported auth: %v", err)
	}
}

func TestAccountImportIntoProxyRequiresProxyURL(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"tokens":{"access_token":"token"}}`), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEXLB_ROOT", "")
	t.Setenv("CODEXLB_PROXY_URL", "")

	errOut, code := captureStderr(func() int {
		return run([]string{"account", "import", "--into", "proxy", "--from", source, "alice"})
	})
	if code != 2 {
		t.Fatalf("expected exit 2, got %d", code)
	}
	if !strings.Contains(errOut, "--into=proxy requires") {
		t.Fatalf("unexpected stderr: %q", errOut)
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

func TestUseDefaultsToConfiguredProxyURL(t *testing.T) {
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
