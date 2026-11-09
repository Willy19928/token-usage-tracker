package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"tokenusage/internal/pricing"
)

func TestResolvePathsRejectsNonDedicatedOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKEN_USAGE_HOME", dir)
	if _, err := ResolvePaths(); err == nil {
		t.Fatal("expected non-dedicated state directory rejection")
	}
}

func TestResolvePathsAcceptsEmptyOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOKEN_USAGE_HOME", dir)
	paths, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.StateDir != dir {
		t.Fatalf("state dir mismatch: %q", paths.StateDir)
	}
	if filepath.Base(paths.Database) != "usage.sqlite" {
		t.Fatalf("database name mismatch: %q", paths.Database)
	}
}

func TestPricingOverridesRoundTripWithoutChangingOtherSettings(t *testing.T) {
	stateDir := t.TempDir()
	paths := Paths{StateDir: stateDir, ConfigPath: filepath.Join(stateDir, "config.json")}
	cfg := Config{
		ListenAddress: "127.0.0.1", Port: 45678, ScanIntervalSeconds: 900,
		ExtraCodexHomes: []string{filepath.Join(stateDir, "codex-home")},
		PricingOverrides: map[string]pricing.Override{
			"CODEX-AUTO-REVIEW": {AliasOf: "GPT-5.6-LUNA"},
		},
	}
	if err := Save(paths, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Port != 45678 || loaded.ScanIntervalSeconds != 900 || loaded.ListenAddress != "127.0.0.1" || len(loaded.ExtraCodexHomes) != 1 {
		t.Fatalf("unrelated settings changed: %#v", loaded)
	}
	if got := loaded.PricingOverrides["codex-auto-review"].AliasOf; got != "gpt-5.6-luna" {
		t.Fatalf("unexpected pricing override %q", got)
	}
	data, err := os.ReadFile(paths.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["port"] != float64(45678) || persisted["scan_interval_seconds"] != float64(900) {
		t.Fatalf("persisted settings were overwritten: %#v", persisted)
	}
}

func TestFallbackScanIntervalDefaultsAndNormalizesToTenMinutes(t *testing.T) {
	if got := Default().ScanIntervalSeconds; got != 600 {
		t.Fatalf("default fallback interval=%d want 600", got)
	}
	stateDir := t.TempDir()
	paths := Paths{StateDir: stateDir, ConfigPath: filepath.Join(stateDir, "config.json")}
	if err := os.WriteFile(paths.ConfigPath, []byte(`{"listen_address":"127.0.0.1","port":43189,"scan_interval_seconds":60}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ScanIntervalSeconds != 600 {
		t.Fatalf("legacy fallback interval=%d want 600", loaded.ScanIntervalSeconds)
	}
}

func TestLoadRejectsInvalidPricingOverride(t *testing.T) {
	stateDir := t.TempDir()
	paths := Paths{StateDir: stateDir, ConfigPath: filepath.Join(stateDir, "config.json")}
	data := `{
		"listen_address":"127.0.0.1",
		"port":43189,
		"scan_interval_seconds":60,
		"pricing_overrides":{"internal":{"input_usd_per_million":"-1","cached_input_usd_per_million":"0.1","cache_write_input_usd_per_million":"1","output_usd_per_million":"1"}}
	}`
	if err := os.WriteFile(paths.ConfigPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(paths); err == nil {
		t.Fatal("expected invalid pricing override to be rejected")
	}
}
