package lb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func TestResolveCodexInvocationUsesRunProxyURLFromConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	custom := `
[proxy]
listen = "127.0.0.1:8765"

proxy_url = "http://127.0.0.1:19000"

[run]
inherit_shell = false
`
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(custom), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	_, _, _, env, inheritShell := resolveCodexInvocation(store, "", "", "", nil)
	if env["OPENAI_BASE_URL"] != "http://127.0.0.1:19000" {
		t.Fatalf("OPENAI_BASE_URL = %q, want %q", env["OPENAI_BASE_URL"], "http://127.0.0.1:19000")
	}
	if env["CODEX_REFRESH_TOKEN_URL_OVERRIDE"] != "http://127.0.0.1:19000/oauth/token" {
		t.Fatalf("CODEX_REFRESH_TOKEN_URL_OVERRIDE = %q", env["CODEX_REFRESH_TOKEN_URL_OVERRIDE"])
	}
	if inheritShell {
		t.Fatalf("inheritShell = true, want false")
	}
}

func TestSeedRuntimeAuthIfMissingCreatesProxyOnlyAuthWithoutAccounts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	runtimeHome := filepath.Join(root, "runtime-proxy-only")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	if err := seedRuntimeAuthIfMissing(store, runtimeHome, ""); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}

	if _, err := os.Stat(filepath.Join(runtimeHome, "auth.json")); err != nil {
		t.Fatalf("expected runtime auth.json: %v", err)
	}
	auth, err := LoadAuth(runtimeHome)
	if err != nil {
		t.Fatalf("LoadAuth(runtime): %v", err)
	}
	if auth.AccessToken == "" {
		t.Fatalf("expected runtime access token")
	}
	if auth.ChatGPTAccountID != "proxy-only" {
		t.Fatalf("expected proxy-only account id, got %q", auth.ChatGPTAccountID)
	}
	raw, err := os.ReadFile(filepath.Join(runtimeHome, "auth.json"))
	if err != nil {
		t.Fatalf("read runtime auth.json: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal runtime auth: %v", err)
	}
	tokens, _ := parsed["tokens"].(map[string]any)
	idToken, _ := tokens["id_token"].(string)
	if idToken == "" {
		t.Fatalf("expected proxy-only id_token in runtime auth")
	}
	refreshToken, _ := tokens["refresh_token"].(string)
	if refreshToken == "" {
		t.Fatalf("expected proxy-only refresh_token in runtime auth")
	}
}

func TestEnsureRuntimeAuthRewritesStoreRuntimeHome(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	runtimeHome := store.RuntimeDir()
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	writeAuthForTest(t, runtimeHome, "acct-old", "old@example.com")

	if err := EnsureRuntimeAuth(store, ""); err != nil {
		t.Fatalf("EnsureRuntimeAuth: %v", err)
	}

	auth, err := LoadAuth(runtimeHome)
	if err != nil {
		t.Fatalf("LoadAuth(runtime): %v", err)
	}
	if auth.ChatGPTAccountID != "proxy-only" {
		t.Fatalf("expected proxy-only account id, got %q", auth.ChatGPTAccountID)
	}
	if auth.UserEmail != "proxy-only@codexlb.internal" {
		t.Fatalf("expected proxy-only email, got %q", auth.UserEmail)
	}
}

func TestEnsureRuntimeAuthStripsPersistedRuntimeRateLimits(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	runtimeHome := store.RuntimeDir()
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	writeAuthForTest(t, runtimeHome, "acct-old", "old@example.com")

	rolloutDir := filepath.Join(runtimeHome, "sessions", "2026", "04", "27")
	if err := os.MkdirAll(rolloutDir, 0o700); err != nil {
		t.Fatalf("mkdir rollout dir: %v", err)
	}
	rolloutPath := filepath.Join(rolloutDir, "rollout.jsonl")
	rollout := strings.Join([]string{
		`{"timestamp":"2026-04-27T14:00:00Z","type":"session_meta","payload":{"id":"thread-1"}}`,
		`{"timestamp":"2026-04-27T14:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":123}},"rate_limits":{"limit_id":"codex","primary":{"used_percent":99.0}}}}`,
		`{"timestamp":"2026-04-27T14:00:02Z","type":"event_msg","payload":{"type":"warning","message":"keep me"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(rolloutPath, []byte(rollout), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	if err := EnsureRuntimeAuth(store, ""); err != nil {
		t.Fatalf("EnsureRuntimeAuth: %v", err)
	}

	raw, err := os.ReadFile(rolloutPath)
	if err != nil {
		t.Fatalf("read rollout: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, `"used_percent":99.0`) {
		t.Fatalf("expected persisted rate limits to be stripped, got %s", text)
	}
	if !strings.Contains(text, `"rate_limits":null`) {
		t.Fatalf("expected persisted rate limits to be nulled, got %s", text)
	}
	if !strings.Contains(text, `"message":"keep me"`) {
		t.Fatalf("expected non-token-count events to remain, got %s", text)
	}
}

func TestSeedRuntimeAuthIfMissingRepairsInvalidRuntimeAuth(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	runtimeHome := filepath.Join(root, "runtime-repair")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeHome, "auth.json"), []byte(`{"tokens":{"access_token":""}}`), 0o600); err != nil {
		t.Fatalf("write invalid runtime auth: %v", err)
	}

	if err := seedRuntimeAuthIfMissing(store, runtimeHome, ""); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}
	auth, err := LoadAuth(runtimeHome)
	if err != nil {
		t.Fatalf("LoadAuth(runtime): %v", err)
	}
	if auth.AccessToken == "" {
		t.Fatalf("expected repaired runtime access token")
	}
}

func TestSeedRuntimeAuthIfMissingRefreshesExistingRuntimeAuthFromSelectedAccount(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	homeA := filepath.Join(root, "acc-a")
	homeB := filepath.Join(root, "acc-b")
	if err := os.MkdirAll(homeA, 0o700); err != nil {
		t.Fatalf("mkdir homeA: %v", err)
	}
	if err := os.MkdirAll(homeB, 0o700); err != nil {
		t.Fatalf("mkdir homeB: %v", err)
	}
	writeAuthForTest(t, homeA, "acct-a", "a@example.com")
	writeAuthForTest(t, homeB, "acct-b", "b@example.com")

	if err := store.Update(func(sf *StoreFile) error {
		sf.Accounts = []Account{
			{Alias: "a", ID: "openai:a", HomeDir: homeA, Enabled: true},
			{Alias: "b", ID: "openai:b", HomeDir: homeB, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		sf.State.PinnedAccountID = "openai:b"
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	runtimeHome := filepath.Join(root, "runtime-refresh")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	writeAuthForTest(t, runtimeHome, "acct-a", "a@example.com")

	if err := seedRuntimeAuthIfMissing(store, runtimeHome, ""); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}
	auth, err := LoadAuth(runtimeHome)
	if err != nil {
		t.Fatalf("LoadAuth(runtime): %v", err)
	}
	if auth.ChatGPTAccountID != "acct-b" {
		t.Fatalf("expected refreshed runtime account acct-b, got %q", auth.ChatGPTAccountID)
	}
}

func TestSeedRuntimeAuthIfMissingMasksRuntimeIDTokenDisplayForRealAccount(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	accountHome := filepath.Join(root, "acc-a")
	if err := os.MkdirAll(accountHome, 0o700); err != nil {
		t.Fatalf("mkdir account home: %v", err)
	}
	writeAuthForTest(t, accountHome, "acct-a", "real@example.com")

	if err := store.Update(func(sf *StoreFile) error {
		sf.Accounts = []Account{
			{Alias: "a", ID: "openai:a", HomeDir: accountHome, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	runtimeHome := filepath.Join(root, "runtime-masked-local")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	if err := seedRuntimeAuthIfMissing(store, runtimeHome, ""); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(runtimeHome, "auth.json"))
	if err != nil {
		t.Fatalf("read runtime auth.json: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal runtime auth payload: %v", err)
	}
	tokens, _ := payload["tokens"].(map[string]any)
	if got, _ := tokens["account_id"].(string); got != "acct-a" {
		t.Fatalf("tokens.account_id = %q, want acct-a", got)
	}
	if got, _ := tokens["refresh_token"].(string); got == "" {
		t.Fatalf("expected runtime refresh_token")
	}
	idToken, _ := tokens["id_token"].(string)
	idClaims, err := decodeJWTPayload(idToken)
	if err != nil {
		t.Fatalf("decode runtime id_token: %v", err)
	}
	if got := stringField(idClaims["email"]); got != "proxy-only@codexlb.internal" {
		t.Fatalf("runtime id_token email = %q", got)
	}
	if got := nestedString(idClaims, "https://api.openai.com/auth", "chatgpt_account_id"); got != "acct-a" {
		t.Fatalf("runtime id_token account id = %q", got)
	}
}

func TestSeedRuntimeAuthIfMissingFetchesFromRemoteProxy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/runtime-auth" {
			http.NotFound(w, r)
			return
		}
		auth, err := proxyOnlyRuntimeAuthPayload(proxyOnlyRuntimeProfile{})
		if err != nil {
			t.Fatalf("proxyOnlyRuntimeAuthPayload: %v", err)
		}
		_ = json.NewEncoder(w).Encode(AdminRuntimeAuthResponse{
			Auth: json.RawMessage(auth),
		})
	}))
	defer server.Close()

	runtimeHome := filepath.Join(root, "runtime-remote")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	if err := seedRuntimeAuthIfMissing(store, runtimeHome, server.URL); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(runtimeHome, "auth.json"))
	if err != nil {
		t.Fatalf("read runtime auth.json: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal runtime auth payload: %v", err)
	}
	tokens, _ := payload["tokens"].(map[string]any)
	accessToken, _ := tokens["access_token"].(string)
	if strings.TrimSpace(accessToken) == "" {
		t.Fatalf("expected proxy-only access_token")
	}
	if got, _ := tokens["id_token"].(string); got != accessToken {
		t.Fatalf("expected proxy-only id_token to match access_token, got %q want %q", got, accessToken)
	}
	if got, _ := tokens["account_id"].(string); got != "proxy-only" {
		t.Fatalf("unexpected account_id: %q", got)
	}
	if got, _ := tokens["refresh_token"].(string); got != proxyRuntimeRefreshToken {
		t.Fatalf("expected refresh_token override %q, got %q", proxyRuntimeRefreshToken, got)
	}
}

func TestSeedRuntimeAuthIfMissingMasksRemoteRuntimeIDTokenDisplayForRealAccount(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	accessToken := testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-remote",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "real@example.com",
		},
	})
	idToken := testJWT(map[string]any{
		"email": "real@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-remote",
			"chatgpt_plan_type":  "plus",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "real@example.com",
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/runtime-auth" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(AdminRuntimeAuthResponse{
			Auth: json.RawMessage(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":"%s","refresh_token":"refresh-remote","id_token":"%s","account_id":"acct-remote"}}`, accessToken, idToken)),
		})
	}))
	defer server.Close()

	runtimeHome := filepath.Join(root, "runtime-remote-masked")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	if err := seedRuntimeAuthIfMissing(store, runtimeHome, server.URL); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(runtimeHome, "auth.json"))
	if err != nil {
		t.Fatalf("read runtime auth.json: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal runtime auth payload: %v", err)
	}
	tokens, _ := payload["tokens"].(map[string]any)
	if got, _ := tokens["account_id"].(string); got != "acct-remote" {
		t.Fatalf("tokens.account_id = %q, want acct-remote", got)
	}
	if got, _ := tokens["refresh_token"].(string); got != proxyRuntimeRefreshToken {
		t.Fatalf("tokens.refresh_token = %q, want %q", got, proxyRuntimeRefreshToken)
	}
	maskedIDToken, _ := tokens["id_token"].(string)
	if maskedIDToken == idToken {
		t.Fatalf("expected remote runtime id_token to be rewritten for display")
	}
	idClaims, err := decodeJWTPayload(maskedIDToken)
	if err != nil {
		t.Fatalf("decode masked remote id_token: %v", err)
	}
	if got := stringField(idClaims["email"]); got != "proxy-only@codexlb.internal" {
		t.Fatalf("masked remote id_token email = %q", got)
	}
	if got := nestedString(idClaims, "https://api.openai.com/auth", "chatgpt_account_id"); got != "acct-remote" {
		t.Fatalf("masked remote id_token account id = %q", got)
	}
}

func TestSeedRuntimeAuthIfMissingCopiesRemoteRuntimeConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	wantConfig := "model = \"gpt-5.2-codex\"\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/runtime-auth" {
			http.NotFound(w, r)
			return
		}
		auth, err := proxyOnlyRuntimeAuthPayload(proxyOnlyRuntimeProfile{})
		if err != nil {
			t.Fatalf("proxyOnlyRuntimeAuthPayload: %v", err)
		}
		_ = json.NewEncoder(w).Encode(AdminRuntimeAuthResponse{
			Auth:        json.RawMessage(auth),
			Config:      wantConfig,
			SourceAlias: "remote-a",
		})
	}))
	defer server.Close()

	runtimeHome := filepath.Join(root, "runtime-remote-config")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	if err := seedRuntimeAuthIfMissing(store, runtimeHome, server.URL); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}

	gotConfig, err := os.ReadFile(filepath.Join(runtimeHome, "config.toml"))
	if err != nil {
		t.Fatalf("read runtime config.toml: %v", err)
	}
	cfg := parseRuntimeConfigTOML(t, gotConfig)
	if got := stringConfigValue(t, cfg, "model"); got != "gpt-5.2-codex" {
		t.Fatalf("runtime model = %q, want %q", got, "gpt-5.2-codex")
	}
	if got := stringConfigValue(t, cfg, "chatgpt_base_url"); got != server.URL {
		t.Fatalf("runtime chatgpt_base_url = %q, want %q", got, server.URL)
	}
}

func TestSeedRuntimeAuthIfMissingCopiesUserConfigWhenAccountConfigMissing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", "")

	userCodexHome := filepath.Join(root, ".codex")
	if err := os.MkdirAll(userCodexHome, 0o700); err != nil {
		t.Fatalf("mkdir user codex home: %v", err)
	}
	wantConfig := []byte("model = \"gpt-5.4\"\n")
	if err := os.WriteFile(filepath.Join(userCodexHome, "config.toml"), wantConfig, 0o600); err != nil {
		t.Fatalf("write user config.toml: %v", err)
	}

	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	accountHome := filepath.Join(root, "acc-a")
	if err := os.MkdirAll(accountHome, 0o700); err != nil {
		t.Fatalf("mkdir account home: %v", err)
	}
	writeAuthForTest(t, accountHome, "acct-a", "a@example.com")

	if err := store.Update(func(sf *StoreFile) error {
		sf.Accounts = []Account{
			{Alias: "a", ID: "openai:a", HomeDir: accountHome, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	runtimeHome := filepath.Join(root, "runtime-user-config")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	if err := seedRuntimeAuthIfMissing(store, runtimeHome, ""); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}

	gotConfig, err := os.ReadFile(filepath.Join(runtimeHome, "config.toml"))
	if err != nil {
		t.Fatalf("read runtime config.toml: %v", err)
	}
	cfg := parseRuntimeConfigTOML(t, gotConfig)
	if got := stringConfigValue(t, cfg, "model"); got != "gpt-5.4" {
		t.Fatalf("runtime model = %q, want %q", got, "gpt-5.4")
	}
}

func TestSeedRuntimeAuthIfMissingPrefersAccountConfigOverUserConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", "")

	userCodexHome := filepath.Join(root, ".codex")
	if err := os.MkdirAll(userCodexHome, 0o700); err != nil {
		t.Fatalf("mkdir user codex home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userCodexHome, "config.toml"), []byte("model = \"gpt-5.4\"\n"), 0o600); err != nil {
		t.Fatalf("write user config.toml: %v", err)
	}

	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	accountHome := filepath.Join(root, "acc-a")
	if err := os.MkdirAll(accountHome, 0o700); err != nil {
		t.Fatalf("mkdir account home: %v", err)
	}
	writeAuthForTest(t, accountHome, "acct-a", "a@example.com")
	wantConfig := []byte("model = \"gpt-5.2-codex\"\n")
	if err := os.WriteFile(filepath.Join(accountHome, "config.toml"), wantConfig, 0o600); err != nil {
		t.Fatalf("write account config.toml: %v", err)
	}

	if err := store.Update(func(sf *StoreFile) error {
		sf.Accounts = []Account{
			{Alias: "a", ID: "openai:a", HomeDir: accountHome, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	runtimeHome := filepath.Join(root, "runtime-account-config")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	if err := seedRuntimeAuthIfMissing(store, runtimeHome, ""); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}

	gotConfig, err := os.ReadFile(filepath.Join(runtimeHome, "config.toml"))
	if err != nil {
		t.Fatalf("read runtime config.toml: %v", err)
	}
	cfg := parseRuntimeConfigTOML(t, gotConfig)
	if got := stringConfigValue(t, cfg, "model"); got != "gpt-5.2-codex" {
		t.Fatalf("runtime model = %q, want %q", got, "gpt-5.2-codex")
	}
}

func TestSeedRuntimeAuthIfMissingDoesNotBorrowDifferentAccountConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", "")

	userCodexHome := filepath.Join(root, ".codex")
	if err := os.MkdirAll(userCodexHome, 0o700); err != nil {
		t.Fatalf("mkdir user codex home: %v", err)
	}
	fallbackConfig := []byte("model = \"gpt-5.4\"\n")
	if err := os.WriteFile(filepath.Join(userCodexHome, "config.toml"), fallbackConfig, 0o600); err != nil {
		t.Fatalf("write user config.toml: %v", err)
	}

	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	homeA := filepath.Join(root, "acc-a")
	homeB := filepath.Join(root, "acc-b")
	if err := os.MkdirAll(homeA, 0o700); err != nil {
		t.Fatalf("mkdir homeA: %v", err)
	}
	if err := os.MkdirAll(homeB, 0o700); err != nil {
		t.Fatalf("mkdir homeB: %v", err)
	}
	writeAuthForTest(t, homeA, "acct-a", "a@example.com")
	writeAuthForTest(t, homeB, "acct-b", "b@example.com")
	if err := os.WriteFile(filepath.Join(homeA, "config.toml"), []byte("model = \"wrong-account\"\n"), 0o600); err != nil {
		t.Fatalf("write account A config.toml: %v", err)
	}

	if err := store.Update(func(sf *StoreFile) error {
		sf.Accounts = []Account{
			{Alias: "a", ID: "openai:a", HomeDir: homeA, Enabled: true},
			{Alias: "b", ID: "openai:b", HomeDir: homeB, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		sf.State.PinnedAccountID = "openai:b"
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	runtimeHome := filepath.Join(root, "runtime-no-borrow")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	if err := seedRuntimeAuthIfMissing(store, runtimeHome, ""); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}

	auth, err := LoadAuth(runtimeHome)
	if err != nil {
		t.Fatalf("LoadAuth(runtime): %v", err)
	}
	if auth.ChatGPTAccountID != "acct-b" {
		t.Fatalf("expected runtime account acct-b, got %q", auth.ChatGPTAccountID)
	}

	gotConfig, err := os.ReadFile(filepath.Join(runtimeHome, "config.toml"))
	if err != nil {
		t.Fatalf("read runtime config.toml: %v", err)
	}
	cfg := parseRuntimeConfigTOML(t, gotConfig)
	if got := stringConfigValue(t, cfg, "model"); got != "gpt-5.4" {
		t.Fatalf("runtime model = %q, want %q", got, "gpt-5.4")
	}
}

func TestSeedRuntimeAuthIfMissingSetsProxyChatGPTBaseURLWithoutSourceConfig(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", "")

	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	accountHome := filepath.Join(root, "acc-a")
	if err := os.MkdirAll(accountHome, 0o700); err != nil {
		t.Fatalf("mkdir account home: %v", err)
	}
	writeAuthForTest(t, accountHome, "acct-a", "a@example.com")

	if err := store.Update(func(sf *StoreFile) error {
		sf.Accounts = []Account{
			{Alias: "a", ID: "openai:a", HomeDir: accountHome, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	runtimeHome := filepath.Join(root, "runtime-proxy-config")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	proxyURL := "http://codexlb.internal/"
	if err := seedRuntimeAuthIfMissing(store, runtimeHome, proxyURL); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}

	gotConfig, err := os.ReadFile(filepath.Join(runtimeHome, "config.toml"))
	if err != nil {
		t.Fatalf("read runtime config.toml: %v", err)
	}
	cfg := parseRuntimeConfigTOML(t, gotConfig)
	if got := stringConfigValue(t, cfg, "chatgpt_base_url"); got != "http://codexlb.internal" {
		t.Fatalf("runtime chatgpt_base_url = %q, want %q", got, "http://codexlb.internal")
	}
}

func TestSeedRuntimeAuthIfMissingPreservesRuntimePromptSelections(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", "")

	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	accountHome := filepath.Join(root, "acc-a")
	if err := os.MkdirAll(accountHome, 0o700); err != nil {
		t.Fatalf("mkdir account home: %v", err)
	}
	writeAuthForTest(t, accountHome, "acct-a", "a@example.com")
	sourceConfig := strings.Join([]string{
		`model = "gpt-5.4"`,
		`model_reasoning_effort = "medium"`,
		`[tui]`,
		`status_line = ["model"]`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(accountHome, "config.toml"), []byte(sourceConfig), 0o600); err != nil {
		t.Fatalf("write account config.toml: %v", err)
	}

	if err := store.Update(func(sf *StoreFile) error {
		sf.Accounts = []Account{
			{Alias: "a", ID: "openai:a", HomeDir: accountHome, Enabled: true},
		}
		sf.State.ActiveIndex = 0
		return nil
	}); err != nil {
		t.Fatalf("store update: %v", err)
	}

	runtimeHome := filepath.Join(root, "runtime-persisted-selections")
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		t.Fatalf("mkdir runtime home: %v", err)
	}
	existingRuntimeConfig := strings.Join([]string{
		`model = "gpt-5.5"`,
		`model_reasoning_effort = "high"`,
		`check_for_update_on_startup = false`,
		`[notice]`,
		`hide_gpt5_1_migration_prompt = true`,
		`[notice.model_migrations]`,
		`"gpt-5.4" = "gpt-5.5"`,
		`[projects."/workspace/demo"]`,
		`trust_level = "trusted"`,
		`[tui]`,
		`status_line = ["model-with-reasoning"]`,
		`[tui.model_availability_nux]`,
		`"gpt-5.5" = 1`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(runtimeHome, "config.toml"), []byte(existingRuntimeConfig), 0o600); err != nil {
		t.Fatalf("write existing runtime config.toml: %v", err)
	}

	if err := seedRuntimeAuthIfMissing(store, runtimeHome, "http://codexlb.internal"); err != nil {
		t.Fatalf("seedRuntimeAuthIfMissing: %v", err)
	}

	gotConfig, err := os.ReadFile(filepath.Join(runtimeHome, "config.toml"))
	if err != nil {
		t.Fatalf("read runtime config.toml: %v", err)
	}
	cfg := parseRuntimeConfigTOML(t, gotConfig)
	if got := stringConfigValue(t, cfg, "model"); got != "gpt-5.5" {
		t.Fatalf("runtime model = %q, want %q", got, "gpt-5.5")
	}
	if got := stringConfigValue(t, cfg, "model_reasoning_effort"); got != "high" {
		t.Fatalf("runtime model_reasoning_effort = %q, want %q", got, "high")
	}
	if got, ok := cfg["check_for_update_on_startup"].(bool); !ok || got {
		t.Fatalf("runtime check_for_update_on_startup = %#v, want false", cfg["check_for_update_on_startup"])
	}
	if got := stringConfigValue(t, cfg, "chatgpt_base_url"); got != "http://codexlb.internal" {
		t.Fatalf("runtime chatgpt_base_url = %q, want %q", got, "http://codexlb.internal")
	}

	notice, _ := cfg["notice"].(map[string]any)
	if notice == nil {
		t.Fatalf("expected runtime notice table")
	}
	if got, ok := notice["hide_gpt5_1_migration_prompt"].(bool); !ok || !got {
		t.Fatalf("runtime notice.hide_gpt5_1_migration_prompt = %#v, want true", notice["hide_gpt5_1_migration_prompt"])
	}
	modelMigrations, _ := notice["model_migrations"].(map[string]any)
	if modelMigrations == nil {
		t.Fatalf("expected runtime notice.model_migrations table")
	}
	if got, ok := modelMigrations["gpt-5.4"].(string); !ok || got != "gpt-5.5" {
		t.Fatalf("runtime notice.model_migrations[gpt-5.4] = %#v, want gpt-5.5", modelMigrations["gpt-5.4"])
	}

	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		t.Fatalf("expected runtime projects table")
	}
	project, _ := projects["/workspace/demo"].(map[string]any)
	if project == nil {
		t.Fatalf("expected runtime trusted project entry")
	}
	if got, ok := project["trust_level"].(string); !ok || got != "trusted" {
		t.Fatalf("runtime project trust_level = %#v, want trusted", project["trust_level"])
	}

	tui, _ := cfg["tui"].(map[string]any)
	if tui == nil {
		t.Fatalf("expected runtime tui table")
	}
	statusLine, _ := tui["status_line"].([]any)
	if len(statusLine) != 1 || statusLine[0] != "model" {
		t.Fatalf("runtime tui.status_line = %#v, want source value", tui["status_line"])
	}
	modelAvailability, _ := tui["model_availability_nux"].(map[string]any)
	if modelAvailability == nil {
		t.Fatalf("expected runtime tui.model_availability_nux table")
	}
	if got, ok := modelAvailability["gpt-5.5"].(int64); !ok || got != 1 {
		t.Fatalf("runtime tui.model_availability_nux[gpt-5.5] = %#v, want 1", modelAvailability["gpt-5.5"])
	}
}

func writeAuthForTest(t *testing.T, home, accountID, email string) {
	t.Helper()
	token := testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
		"https://api.openai.com/profile": map[string]any{
			"email": email,
		},
	})
	payload := map[string]any{
		"tokens": map[string]any{
			"access_token": token,
			"account_id":   accountID,
		},
	}
	b, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), b, 0o600); err != nil {
		t.Fatalf("write auth.json for %s: %v", home, err)
	}
}

func parseRuntimeConfigTOML(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := toml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse runtime config.toml: %v", err)
	}
	return cfg
}

func stringConfigValue(t *testing.T, cfg map[string]any, key string) string {
	t.Helper()
	value, ok := cfg[key]
	if !ok {
		t.Fatalf("missing config key %q in %#v", key, cfg)
	}
	s, ok := value.(string)
	if !ok {
		t.Fatalf("config key %q has type %T, want string", key, value)
	}
	return s
}
