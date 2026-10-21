package store

import (
	"os"
	"path/filepath"
	"testing"
)

// The published example must stay loadable: users copy it to /etc/ts3panel and
// expect the panel to start. Loading a copy also exercises secret generation.
func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatalf("read config.example.toml: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfgStore, created, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("config.example.toml does not load: %v", err)
	}
	if created {
		t.Fatal("existing example must not be reported as newly generated")
	}
	cfg := cfgStore.Snapshot()
	if cfg.Listen != "127.0.0.1:8899" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
	if cfg.InstanceRoot != "/srv/ts3" {
		t.Fatalf("instance_root = %q", cfg.InstanceRoot)
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("example must not ship providers, got %d", len(cfg.Providers))
	}
	if cfg.SessionSecret == "" || cfg.MasterKey == "" {
		t.Fatal("missing secrets were not generated")
	}
}

func TestLoadConfigPortProbeDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgStore, created, err := LoadConfig(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !created {
		t.Fatal("expected a generated config.toml")
	}
	cfg := cfgStore.Snapshot()
	if cfg.Ports.ExternalCheck {
		t.Fatal("external check must default to disabled")
	}
	if cfg.Ports.CheckCacheSeconds != 60 {
		t.Fatalf("check_cache_seconds = %d, want 60", cfg.Ports.CheckCacheSeconds)
	}
	if cfg.Backup.Enabled {
		t.Fatal("backup plan must default to disabled")
	}
}

func TestConfigValidatesPortProbeCache(t *testing.T) {
	dir := t.TempDir()
	cfgStore, _, err := LoadConfig(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []int{9, 3601} {
		if err := cfgStore.Update(func(cfg *Config) error {
			cfg.Ports.CheckCacheSeconds = bad
			return nil
		}); err == nil {
			t.Fatalf("check_cache_seconds=%d was accepted", bad)
		}
	}
	if err := cfgStore.Update(func(cfg *Config) error {
		cfg.Ports.CheckCacheSeconds = 120
		cfg.Ports.ExternalCheck = true
		return nil
	}); err != nil {
		t.Fatalf("valid port settings rejected: %v", err)
	}
	cfg := cfgStore.Snapshot()
	if !cfg.Ports.ExternalCheck || cfg.Ports.CheckCacheSeconds != 120 {
		t.Fatalf("saved port settings = %+v", cfg.Ports)
	}
}

func TestConfigRejectsInvalidBackupPlan(t *testing.T) {
	dir := t.TempDir()
	cfgStore, _, err := LoadConfig(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []func(*Config) error{
		func(cfg *Config) error { cfg.Backup.Mode = "weekly"; return nil },
		func(cfg *Config) error { cfg.Backup.DailyAt = "4:30"; return nil },
		func(cfg *Config) error { cfg.Backup.IntervalHours = -1; return nil },
		func(cfg *Config) error { cfg.Backup.Retention = -1; return nil },
	}
	for i, mutate := range cases {
		if err := cfgStore.Update(mutate); err == nil {
			t.Fatalf("case %d was accepted", i)
		}
	}
}
