package lb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
		_ = json.NewEncoder(w).Encode(AdminRuntimeAuthResponse{
			Auth: json.RawMessage(`{"tokens":{"access_token":"remote-access","id_token":"remote-id","account_id":"acct-remote"}}`),
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
	if got, _ := tokens["access_token"].(string); got != "remote-access" {
		t.Fatalf("unexpected access_token: %q", got)
	}
	if got, _ := tokens["id_token"].(string); got != "remote-id" {
		t.Fatalf("unexpected id_token: %q", got)
	}
	if got, _ := tokens["account_id"].(string); got != "acct-remote" {
		t.Fatalf("unexpected account_id: %q", got)
	}
	if got, _ := tokens["refresh_token"].(string); got != "remote-access" {
		t.Fatalf("expected refresh_token to be derived from access_token, got %q", got)
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
