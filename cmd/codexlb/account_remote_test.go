package main

import (
	"encoding/json"
	"io"
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

func TestAccountListProxyNameTargetsChildProxy(t *testing.T) {
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/accounts":
			if got := r.Header.Get(adminTargetProxyNameHeader); got != "edge-vpn" {
				t.Fatalf("expected %s=edge-vpn, got %q", adminTargetProxyNameHeader, got)
			}
			_ = json.NewEncoder(w).Encode(lb.AdminAccountsResponse{
				Accounts: []lb.Account{{Alias: "alice", ID: "openai:alice", UserEmail: "a@example.com", Enabled: true}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer main.Close()

	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Snapshot().Settings
	cfg.ProxyURL = main.URL
	if err := lb.WriteSettingsConfig(root, cfg); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	out, code := captureStdout(func() int {
		return run([]string{"account", "list", "--root", root, "--proxy-name", "edge-vpn"})
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

func TestAccountImportProxyNameTargetsChildProxy(t *testing.T) {
	source := t.TempDir()
	auth := `{"tokens":{"access_token":"` + testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-import"},
	}) + `","account_id":"acct-import"}}`
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/admin/account/import":
			if got := r.Header.Get(adminTargetProxyNameHeader); got != "edge-vpn" {
				t.Fatalf("expected %s=edge-vpn, got %q", adminTargetProxyNameHeader, got)
			}
			var req lb.AdminImportRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Alias != "alice" || len(req.Auth) == 0 {
				t.Fatalf("unexpected request: %+v", req)
			}
			_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true, Total: 2})
		default:
			http.NotFound(w, r)
		}
	}))
	defer main.Close()

	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Snapshot().Settings
	cfg.ProxyURL = main.URL
	if err := lb.WriteSettingsConfig(root, cfg); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	out, code := captureStdout(func() int {
		return run([]string{"account", "import", "--root", root, "--into", "proxy", "--proxy-name", "edge-vpn", "--from", source, "alice"})
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

func TestAccountLoginRemoteRunsOnRemoteProxy(t *testing.T) {
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

	loginCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/admin/account/login":
			loginCalls++
			if got := r.URL.Query().Get("stream"); got != "1" {
				t.Fatalf("expected streamed login query, got %q", got)
			}
			var req lb.AdminLoginRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Alias != "alice" {
				t.Fatalf("unexpected alias: %q", req.Alias)
			}
			if req.CodexBin != fakeBin {
				t.Fatalf("unexpected codex bin: %q", req.CodexBin)
			}
			if len(req.LoginArgs) != 0 {
				t.Fatalf("unexpected login args: %#v", req.LoginArgs)
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Add("Trailer", adminLoginStreamExitCodeTrailer)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "Open https://auth.openai.com/codex/device\n")
			w.Header().Set(adminLoginStreamExitCodeTrailer, "0")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"account", "login", "--root", root, "--proxy-url", server.URL, "alice"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if loginCalls != 1 {
		t.Fatalf("expected one remote login call, got %d", loginCalls)
	}
	if !strings.Contains(out, "Open https://auth.openai.com/codex/device") {
		t.Fatalf("unexpected output: %q", out)
	}
	if _, err := os.Stat(fakeLog); !os.IsNotExist(err) {
		t.Fatalf("expected no local codex login execution, stat err=%v", err)
	}
}

func TestAccountLoginProxyNameRunsOnRemoteProxy(t *testing.T) {
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

	loginCalls := 0
	importCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/account/login":
			loginCalls++
			if got := r.URL.Query().Get("stream"); got != "1" {
				t.Fatalf("expected streamed login query, got %q", got)
			}
			if got := r.Header.Get(adminTargetProxyNameHeader); got != "edge-vpn" {
				t.Fatalf("expected %s=edge-vpn, got %q", adminTargetProxyNameHeader, got)
			}
			var req lb.AdminLoginRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Alias != "alice" {
				t.Fatalf("unexpected alias: %q", req.Alias)
			}
			if req.CodexBin != fakeBin {
				t.Fatalf("unexpected codex bin: %q", req.CodexBin)
			}
			if len(req.LoginArgs) != 0 {
				t.Fatalf("unexpected login args: %#v", req.LoginArgs)
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Add("Trailer", adminLoginStreamExitCodeTrailer)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "Open https://auth.openai.com/codex/device\n")
			w.Header().Set(adminLoginStreamExitCodeTrailer, "0")
		case "/admin/account/import":
			importCalls++
			http.Error(w, "unexpected import", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
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
		return run([]string{"account", "login", "--root", root, "--proxy-name", "edge-vpn", "alice"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if loginCalls != 1 {
		t.Fatalf("expected one remote login call, got %d", loginCalls)
	}
	if importCalls != 0 {
		t.Fatalf("expected no remote import calls, got %d", importCalls)
	}
	if !strings.Contains(out, "Open https://auth.openai.com/codex/device") {
		t.Fatalf("unexpected output: %q", out)
	}
	if _, err := os.Stat(fakeLog); !os.IsNotExist(err) {
		t.Fatalf("expected no local codex login execution, stat err=%v", err)
	}
}

func TestAccountLoginProxyNameStripsSeparatorFromArgs(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/account/login" {
			http.NotFound(w, r)
			return
		}
		var req lb.AdminLoginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(req.LoginArgs) != 1 || req.LoginArgs[0] != "--device-auth" {
			t.Fatalf("unexpected login args: %#v", req.LoginArgs)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Add("Trailer", adminLoginStreamExitCodeTrailer)
		w.WriteHeader(http.StatusOK)
		w.Header().Set(adminLoginStreamExitCodeTrailer, "0")
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

	code := run([]string{"account", "login", "--root", root, "--proxy-name", "edge-vpn", "alice", "--", "--device-auth"})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
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

	loginCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/admin/account/login":
			loginCalls++
			if got := r.URL.Query().Get("stream"); got != "1" {
				t.Fatalf("expected streamed login query, got %q", got)
			}
			var req lb.AdminLoginRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Alias != "alice" || req.CodexBin != fakeBin || len(req.LoginArgs) != 0 {
				t.Fatalf("unexpected request: %+v", req)
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Add("Trailer", adminLoginStreamExitCodeTrailer)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "Open https://auth.openai.com/codex/device\n")
			w.Header().Set(adminLoginStreamExitCodeTrailer, "0")
		default:
			http.NotFound(w, r)
		}
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
	if loginCalls != 1 {
		t.Fatalf("expected one remote login call, got %d", loginCalls)
	}
	if !strings.Contains(out, "Open https://auth.openai.com/codex/device") {
		t.Fatalf("unexpected output: %q", out)
	}
	if _, err := os.Stat(fakeLog); !os.IsNotExist(err) {
		t.Fatalf("expected no local codex login execution, stat err=%v", err)
	}
}

func TestAccountLoginAllowsEmailAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/account/login":
			var req lb.AdminLoginRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Alias != "g99517399@gmail.com" {
				t.Fatalf("unexpected alias: %q", req.Alias)
			}
			_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true})
		default:
			http.NotFound(w, r)
		}
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

	code := run([]string{"account", "login", "--root", root, "g99517399@gmail.com"})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
}

func TestAccountLoginFallsBackToLocalLoginImportWhenRemoteCodexMissing(t *testing.T) {
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

	loginCalls := 0
	importCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/account/login":
			loginCalls++
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Add("Trailer", adminLoginStreamExitCodeTrailer)
			w.Header().Add("Trailer", adminLoginStreamErrorTrailer)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "remote login unavailable\n")
			w.Header().Set(adminLoginStreamExitCodeTrailer, "1")
			w.Header().Set(adminLoginStreamErrorTrailer, `run codex login --device-auth: exec: "codex": executable file not found in $PATH`)
		case "/admin/account/import":
			importCalls++
			var req lb.AdminImportRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode import request: %v", err)
			}
			if req.Alias != "alice" {
				t.Fatalf("unexpected alias: %q", req.Alias)
			}
			if len(req.Auth) == 0 {
				t.Fatalf("expected auth payload")
			}
			authText := string(req.Auth)
			if !strings.Contains(authText, `"account_id":"acct-a"`) {
				t.Fatalf("unexpected auth payload: %s", authText)
			}
			if req.Config != "" {
				t.Fatalf("expected empty config, got %q", req.Config)
			}
			_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true, Message: "imported account alice (total=1)"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	out, code := captureStdout(func() int {
		return run([]string{"account", "login", "--root", root, "--proxy-url", server.URL, "alice"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if loginCalls != 1 {
		t.Fatalf("expected one remote login call, got %d", loginCalls)
	}
	if importCalls != 1 {
		t.Fatalf("expected one remote import call, got %d", importCalls)
	}
	if !strings.Contains(out, "remote codex unavailable; logging in locally and importing auth") {
		t.Fatalf("missing fallback notice: %q", out)
	}
	if !strings.Contains(out, "imported account alice") {
		t.Fatalf("missing import success output: %q", out)
	}
	data, err := os.ReadFile(fakeLog)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	if !strings.Contains(string(data), "LOGIN_ARGS=login --device-auth") {
		t.Fatalf("unexpected fake log: %s", string(data))
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

func TestAccountRemoveProxyNameTargetsGrandchildProxy(t *testing.T) {
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/admin/account/rm":
			if got := r.Header.Get(adminTargetProxyNameHeader); got != "edge-leaf" {
				t.Fatalf("expected %s=edge-leaf, got %q", adminTargetProxyNameHeader, got)
			}
			var req lb.AdminAliasRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Alias != "alice" {
				t.Fatalf("unexpected alias: %q", req.Alias)
			}
			_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer main.Close()

	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Snapshot().Settings
	cfg.ProxyURL = main.URL
	if err := lb.WriteSettingsConfig(root, cfg); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	out, code := captureStdout(func() int {
		return run([]string{"account", "rm", "--root", root, "--proxy-name", "edge-leaf", "alice"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if !strings.Contains(out, "removed account alice") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestAccountRemoveInfersProxyNameFromStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/status":
			_ = json.NewEncoder(w).Encode(lb.ProxyStatus{
				ProxyName: "edge-main",
				Accounts: []lb.AccountStatus{
					{ProxyName: "edge-main", Alias: "f", ID: "openai:f"},
					{ProxyName: "edge-vpn", Alias: "t", ID: "openai:t"},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/admin/account/rm":
			if got := r.Header.Get(adminTargetProxyNameHeader); got != "edge-vpn" {
				t.Fatalf("expected %s=edge-vpn, got %q", adminTargetProxyNameHeader, got)
			}
			var req lb.AdminAliasRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Alias != "t" {
				t.Fatalf("unexpected alias: %q", req.Alias)
			}
			_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true})
		default:
			http.NotFound(w, r)
		}
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
		return run([]string{"account", "rm", "--root", root, "t"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if !strings.Contains(out, "removed account t") {
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

func TestAccountPinInfersProxyNameFromStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/status":
			_ = json.NewEncoder(w).Encode(lb.ProxyStatus{
				ProxyName: "edge-main",
				Accounts: []lb.AccountStatus{
					{ProxyName: "edge-main", Alias: "g", ID: "openai:g"},
					{ProxyName: "edge-vpn", Alias: "usa1", ID: "openai:usa1"},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/admin/account/pin":
			if got := r.Header.Get(adminTargetProxyNameHeader); got != "edge-vpn" {
				t.Fatalf("expected %s=edge-vpn, got %q", adminTargetProxyNameHeader, got)
			}
			var req lb.AdminAliasRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Alias != "usa1" {
				t.Fatalf("unexpected alias: %q", req.Alias)
			}
			_ = json.NewEncoder(w).Encode(lb.AdminMutationResponse{OK: true})
		default:
			http.NotFound(w, r)
		}
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
		return run([]string{"account", "pin", "--root", root, "usa1"})
	})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s", code, out)
	}
	if !strings.Contains(out, "pinned account usa1") {
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
