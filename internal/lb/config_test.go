package lb

import (
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
}
