package lb

import (
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
	if err := seedRuntimeAuthIfMissing(store, runtimeHome); err != nil {
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

	if err := seedRuntimeAuthIfMissing(store, runtimeHome); err != nil {
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
