package lb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenStoreCreatesConfigToml(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	_ = store

	path := ConfigPath(root)
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	text := string(bytes)
	if !strings.Contains(text, "[proxy]") {
		t.Fatalf("expected [proxy] section in config.toml: %s", text)
	}
	if !strings.Contains(text, "upstream_base_url") {
		t.Fatalf("expected upstream_base_url in config.toml: %s", text)
	}
	if !strings.Contains(text, "[commands]") {
		t.Fatalf("expected [commands] section in config.toml: %s", text)
	}
}

func TestOpenStoreLoadsConfigTomlValues(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	custom := `
[proxy]
listen = "127.0.0.1:9999"
max_attempts = 7
usage_timeout_ms = 1234
cooldown_default_seconds = 9

proxy_url = "http://127.0.0.1:19000"

[policy]
mode = "sticky"
delta_percent = 5

[policy.weights]
daily = 70
weekly = 30

[quota]
refresh_interval_minutes = 3
refresh_interval_messages = 2
cache_ttl_minutes = 8

[commands]
login = ["login", "--device-code"]
run = ["exec", "--yolo"]

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
	snap := store.Snapshot()
	if snap.Settings.Proxy.Listen != "127.0.0.1:9999" {
		t.Fatalf("listen not loaded from config: %s", snap.Settings.Proxy.Listen)
	}
	if snap.Settings.Proxy.MaxAttempts != 7 {
		t.Fatalf("max_attempts not loaded: %d", snap.Settings.Proxy.MaxAttempts)
	}
	if snap.Settings.Policy.Mode != PolicySticky {
		t.Fatalf("policy mode not loaded: %s", snap.Settings.Policy.Mode)
	}
	if snap.Settings.Quota.RefreshIntervalMinutes != 3 {
		t.Fatalf("quota refresh_interval_minutes not loaded: %d", snap.Settings.Quota.RefreshIntervalMinutes)
	}
	if len(snap.Settings.Commands.Login) != 2 || snap.Settings.Commands.Login[1] != "--device-code" {
		t.Fatalf("commands.login not loaded: %#v", snap.Settings.Commands.Login)
	}
	if len(snap.Settings.Commands.Run) != 2 || snap.Settings.Commands.Run[1] != "--yolo" {
		t.Fatalf("commands.run not loaded: %#v", snap.Settings.Commands.Run)
	}
	if snap.Settings.ProxyURL != "http://127.0.0.1:19000" {
		t.Fatalf("proxy_url not loaded: %s", snap.Settings.ProxyURL)
	}
	if snap.Settings.Run.InheritShell {
		t.Fatalf("run.inherit_shell not loaded")
	}
}

func TestOpenStoreDoesNotInheritMissingRunProxyURLFromStoreJSON(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	legacy := map[string]any{
		"version":    1,
		"updated_at": "2026-01-01T00:00:00Z",
		"settings": map[string]any{
			"run": map[string]any{
				"proxy_url": "codexlb.internal",
			},
		},
		"state": map[string]any{
			"active_index": 0,
		},
		"accounts": []any{},
	}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "store.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write store.json: %v", err)
	}

	config := `
[proxy]
listen = "127.0.0.1:8765"
`
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	if got := store.Snapshot().Settings.ProxyURL; got != "" {
		t.Fatalf("expected proxy_url to come from config/defaults, got %q", got)
	}
	if !store.Snapshot().Settings.Run.InheritShell {
		t.Fatalf("expected default run.inherit_shell=true")
	}
}

func TestOpenStoreLoadsLegacyRunProxyURLFromConfigToml(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	custom := `
[run]
proxy_url = "http://legacy-proxy.local"
`
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(custom), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if got := store.Snapshot().Settings.ProxyURL; got != "http://legacy-proxy.local" {
		t.Fatalf("expected legacy run.proxy_url to be loaded, got %q", got)
	}
}
