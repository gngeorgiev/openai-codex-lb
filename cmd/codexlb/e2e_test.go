package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gngeorgiev/openai-codex-lb/internal/lb"
)

func TestE2EWrapperLoginAndRun(t *testing.T) {
	root := t.TempDir()
	fakeLog := filepath.Join(root, "fake-codex.log")
	fakeBin := filepath.Join(root, "codex")
	writeFakeCodex(t, fakeBin)

	t.Setenv("CODEXLB_CODEX_BIN", fakeBin)
	t.Setenv("FAKE_LOG", fakeLog)

	t.Setenv("FAKE_TOKEN", testJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-a"},
		"https://api.openai.com/profile": map[string]any{"email": "a@example.com"},
	}))
	t.Setenv("FAKE_ACCOUNT_ID", "acct-a")
	if code := run([]string{"account", "login", "--root", root, "alice"}); code != 0 {
		t.Fatalf("account login alice failed: %d", code)
	}

	t.Setenv("FAKE_TOKEN", testJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-b"},
		"https://api.openai.com/profile": map[string]any{"email": "b@example.com"},
	}))
	t.Setenv("FAKE_ACCOUNT_ID", "acct-b")
	if code := run([]string{"account", "login", "--root", root, "bob"}); code != 0 {
		t.Fatalf("account login bob failed: %d", code)
	}

	if code := run([]string{"account", "list", "--root", root}); code != 0 {
		t.Fatalf("account list failed: %d", code)
	}

	runtimeHome := filepath.Join(root, "runtime-home")
	if code := run([]string{"run", "--root", root, "--proxy-url", "http://127.0.0.1:9876", "--codex-home", runtimeHome, "exec", "--json", "ping"}); code != 0 {
		t.Fatalf("wrapper run failed: %d", code)
	}

	data, err := os.ReadFile(fakeLog)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	logLine := string(data)
	if !strings.Contains(logLine, "OPENAI_BASE_URL=http://127.0.0.1:9876") {
		t.Fatalf("missing OPENAI_BASE_URL in log: %s", logLine)
	}
	if !strings.Contains(logLine, "OPENAI_API_KEY=codex-lb-local-key") {
		t.Fatalf("missing OPENAI_API_KEY in log: %s", logLine)
	}
	if !strings.Contains(logLine, "CODEX_HOME="+runtimeHome) {
		t.Fatalf("missing CODEX_HOME override in log: %s", logLine)
	}
	if _, err := os.Stat(filepath.Join(runtimeHome, "auth.json")); err != nil {
		t.Fatalf("expected runtime auth.json to be seeded: %v", err)
	}
}

func TestE2EWrapperRunSeedsRuntimeConfigFromUserCodexHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatalf("mkdir user codex home: %v", err)
	}
	wantConfig := []byte("model = \"gpt-5.4\"\n")
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), wantConfig, 0o600); err != nil {
		t.Fatalf("write user config.toml: %v", err)
	}

	fakeLog := filepath.Join(root, "fake-codex.log")
	fakeBin := filepath.Join(root, "codex")
	writeFakeCodex(t, fakeBin)

	t.Setenv("HOME", home)
	t.Setenv("CODEXLB_CODEX_BIN", fakeBin)
	t.Setenv("FAKE_LOG", fakeLog)
	t.Setenv("FAKE_TOKEN", testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"},
	}))
	t.Setenv("FAKE_ACCOUNT_ID", "acct-a")

	if code := run([]string{"account", "login", "--root", root, "alice"}); code != 0 {
		t.Fatalf("account login alice failed: %d", code)
	}

	runtimeHome := filepath.Join(root, "runtime-home")
	if code := run([]string{"run", "--root", root, "--proxy-url", "http://127.0.0.1:9876", "--codex-home", runtimeHome, "exec", "--json", "ping"}); code != 0 {
		t.Fatalf("wrapper run failed: %d", code)
	}

	gotConfig, err := os.ReadFile(filepath.Join(runtimeHome, "config.toml"))
	if err != nil {
		t.Fatalf("read runtime config.toml: %v", err)
	}
	if string(gotConfig) != string(wantConfig) {
		t.Fatalf("runtime config.toml = %q, want %q", string(gotConfig), string(wantConfig))
	}
}

func TestE2EAccountImport(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	token := testJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-import"},
		"https://api.openai.com/profile": map[string]any{"email": "import@example.com"},
	})
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"tokens":{"access_token":"`+token+`","account_id":"acct-import"}}`), 0o600); err != nil {
		t.Fatalf("write source auth: %v", err)
	}

	if code := run([]string{"account", "import", "--root", root, "--from", source, "imported"}); code != 0 {
		t.Fatalf("account import failed: %d", code)
	}

	copied := filepath.Join(root, "accounts", "imported", "auth.json")
	if _, err := os.Stat(copied); err != nil {
		t.Fatalf("expected copied auth at %s: %v", copied, err)
	}
}

func TestRunCommandOnlyPrintsWrappedCommand(t *testing.T) {
	root := t.TempDir()
	fakeLog := filepath.Join(root, "fake-codex.log")
	fakeBin := filepath.Join(root, "codex")
	writeFakeCodex(t, fakeBin)

	t.Setenv("CODEXLB_CODEX_BIN", fakeBin)
	t.Setenv("FAKE_LOG", fakeLog)
	t.Setenv("OPENAI_API_KEY", "")

	runtimeHome := filepath.Join(root, "runtime home")
	out, code := captureStdout(func() int {
		return run([]string{"run", "--root", root, "--proxy-url", "http://127.0.0.1:9876", "--codex-home", runtimeHome, "--command", "exec", "--json", "hello world"})
	})
	if code != 0 {
		t.Fatalf("run --command failed: code=%d out=%s", code, out)
	}
	line := strings.TrimSpace(out)
	if !strings.Contains(line, "OPENAI_BASE_URL=http://127.0.0.1:9876") {
		t.Fatalf("expected OPENAI_BASE_URL in command: %s", line)
	}
	if !strings.Contains(line, "OPENAI_API_KEY=codex-lb-local-key") {
		t.Fatalf("expected OPENAI_API_KEY default in command: %s", line)
	}
	if !strings.Contains(line, "CODEX_HOME='"+runtimeHome+"'") {
		t.Fatalf("expected quoted CODEX_HOME in command: %s", line)
	}
	if !strings.Contains(line, "exec --json 'hello world'") {
		t.Fatalf("expected quoted codex args in command: %s", line)
	}

	if _, err := os.Stat(fakeLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected codex binary not to execute, fake log exists: %v", err)
	}
}

func TestNoSubcommandDefaultsToRun(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	fakeLog := filepath.Join(root, "fake-codex.log")
	fakeBin := filepath.Join(root, "codex")
	writeFakeCodex(t, fakeBin)

	t.Setenv("HOME", home)
	t.Setenv("CODEXLB_CODEX_BIN", fakeBin)
	t.Setenv("FAKE_LOG", fakeLog)

	if code := run([]string{}); code != 0 {
		t.Fatalf("default invocation failed: %d", code)
	}

	data, err := os.ReadFile(fakeLog)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	logLine := string(data)
	if !strings.Contains(logLine, "OPENAI_BASE_URL=http://127.0.0.1:8765") {
		t.Fatalf("missing default OPENAI_BASE_URL in log: %s", logLine)
	}
}

func TestAccountPinAndUnpin(t *testing.T) {
	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	sourceA := filepath.Join(root, "source-a")
	if err := os.MkdirAll(sourceA, 0o700); err != nil {
		t.Fatalf("mkdir source-a: %v", err)
	}
	tokenA := testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"},
	})
	if err := os.WriteFile(filepath.Join(sourceA, "auth.json"), []byte(`{"tokens":{"access_token":"`+tokenA+`","account_id":"acct-a"}}`), 0o600); err != nil {
		t.Fatalf("write source-a auth: %v", err)
	}
	if err := lb.ImportAccount(store, "alice", sourceA); err != nil {
		t.Fatalf("import alice: %v", err)
	}

	sourceB := filepath.Join(root, "source-b")
	if err := os.MkdirAll(sourceB, 0o700); err != nil {
		t.Fatalf("mkdir source-b: %v", err)
	}
	tokenB := testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-b"},
	})
	if err := os.WriteFile(filepath.Join(sourceB, "auth.json"), []byte(`{"tokens":{"access_token":"`+tokenB+`","account_id":"acct-b"}}`), 0o600); err != nil {
		t.Fatalf("write source-b auth: %v", err)
	}
	if err := lb.ImportAccount(store, "bob", sourceB); err != nil {
		t.Fatalf("import bob: %v", err)
	}

	if code := run([]string{"account", "pin", "--root", root, "alice"}); code != 0 {
		t.Fatalf("account pin failed: %d", code)
	}
	reloaded, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("reopen store after pin: %v", err)
	}
	if got := reloaded.Snapshot().State.PinnedAccountID; got != "openai:alice" {
		t.Fatalf("expected pinned account openai:alice, got %q", got)
	}

	if code := run([]string{"account", "unpin", "--root", root}); code != 0 {
		t.Fatalf("account unpin failed: %d", code)
	}
	reloaded, err = lb.OpenStore(root)
	if err != nil {
		t.Fatalf("reopen store after unpin: %v", err)
	}
	if got := reloaded.Snapshot().State.PinnedAccountID; got != "" {
		t.Fatalf("expected pinned account to be cleared, got %q", got)
	}
}

func TestAccountPinUnknownAlias(t *testing.T) {
	root := t.TempDir()
	errOut, code := captureStderr(func() int {
		return run([]string{"account", "pin", "--root", root, "missing"})
	})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(errOut, "alias not found") {
		t.Fatalf("expected alias-not-found error, got: %s", errOut)
	}
}

func TestAccountPinPinsAccount(t *testing.T) {
	root := t.TempDir()
	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	sourceA := filepath.Join(root, "source-a")
	if err := os.MkdirAll(sourceA, 0o700); err != nil {
		t.Fatalf("mkdir source-a: %v", err)
	}
	tokenA := testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"},
	})
	if err := os.WriteFile(filepath.Join(sourceA, "auth.json"), []byte(`{"tokens":{"access_token":"`+tokenA+`","account_id":"acct-a"}}`), 0o600); err != nil {
		t.Fatalf("write source-a auth: %v", err)
	}
	if err := lb.ImportAccount(store, "alice", sourceA); err != nil {
		t.Fatalf("import alice: %v", err)
	}

	out, code := captureStdout(func() int {
		return run([]string{"account", "pin", "--root", root, "alice"})
	})
	if code != 0 {
		t.Fatalf("account pin failed: %d output=%s", code, out)
	}
	if !strings.Contains(out, "pinned account alice") {
		t.Fatalf("unexpected output: %q", out)
	}

	reloaded, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("reopen store after pin: %v", err)
	}
	if got := reloaded.Snapshot().State.PinnedAccountID; got != "openai:alice" {
		t.Fatalf("expected pinned account openai:alice, got %q", got)
	}
}

func TestE2EConfiguredLoginCommand(t *testing.T) {
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

	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Update(func(sf *lb.StoreFile) error {
		sf.Settings.Commands.Login = []string{"login", "--yolo"}
		return nil
	}); err != nil {
		t.Fatalf("update store: %v", err)
	}
	if err := store.PersistSettingsToConfig(); err != nil {
		t.Fatalf("persist config: %v", err)
	}

	if code := run([]string{"account", "login", "--root", root, "alice"}); code != 0 {
		t.Fatalf("account login failed: %d", code)
	}

	data, err := os.ReadFile(fakeLog)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	if !strings.Contains(string(data), "LOGIN_ARGS=login --yolo") {
		t.Fatalf("expected configured login command in log, got: %s", string(data))
	}
}

func TestE2EConfiguredRunCommandPrefix(t *testing.T) {
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

	if code := run([]string{"account", "login", "--root", root, "alice"}); code != 0 {
		t.Fatalf("account login failed: %d", code)
	}

	store, err := lb.OpenStore(root)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Update(func(sf *lb.StoreFile) error {
		sf.Settings.Commands.Run = []string{"exec", "--yolo"}
		return nil
	}); err != nil {
		t.Fatalf("update store: %v", err)
	}
	if err := store.PersistSettingsToConfig(); err != nil {
		t.Fatalf("persist config: %v", err)
	}

	runtimeHome := filepath.Join(root, "runtime-home")
	if code := run([]string{"run", "--root", root, "--proxy-url", "http://127.0.0.1:9876", "--codex-home", runtimeHome, "--", "--json", "ping"}); code != 0 {
		t.Fatalf("wrapper run failed: %d", code)
	}

	data, err := os.ReadFile(fakeLog)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	if !strings.Contains(string(data), "ARGS=exec --yolo --json ping") {
		t.Fatalf("expected configured run command prefix in log, got: %s", string(data))
	}
}

func writeFakeCodex(t *testing.T, path string) {
	t.Helper()
	script := `#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "login" ]]; then
  echo "LOGIN_ARGS=$*" >> "${FAKE_LOG:?missing FAKE_LOG}"
  mkdir -p "$CODEX_HOME"
  cat > "$CODEX_HOME/auth.json" <<JSON
{"tokens":{"access_token":"${FAKE_TOKEN:?missing FAKE_TOKEN}","account_id":"${FAKE_ACCOUNT_ID:-}"}}
JSON
  exit 0
fi
{
  echo "OPENAI_BASE_URL=${OPENAI_BASE_URL:-}";
  echo "OPENAI_API_KEY=${OPENAI_API_KEY:-}";
  echo "CODEX_HOME=${CODEX_HOME:-}";
  echo "ARGS=$*";
} >> "${FAKE_LOG:?missing FAKE_LOG}"
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex script: %v", err)
	}
}

func testJWT(payload map[string]any) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	b, _ := json.Marshal(payload)
	body := base64.RawURLEncoding.EncodeToString(b)
	return head + "." + body + ".sig"
}
