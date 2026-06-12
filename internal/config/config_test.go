package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAddonMode(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "super-secret")
	path := writeFile(t, "options.json", `{
		"log_level": "debug",
		"state_history": {"retention_days": 5, "purge_interval": "30m"}
	}`)

	cfg, err := load(path, true)
	if err != nil {
		t.Fatal(err)
	}

	// Connection details are not options: fixed URL, token from the env.
	if cfg.HomeAssistant.URL != "ws://supervisor/core/websocket" {
		t.Errorf("url: got %q", cfg.HomeAssistant.URL)
	}
	if cfg.HomeAssistant.Token != "super-secret" {
		t.Errorf("token: got %q", cfg.HomeAssistant.Token)
	}
	if cfg.ScriptsDir != "/addon_config/scripts" {
		t.Errorf("scripts_dir: got %q", cfg.ScriptsDir)
	}
	if cfg.Database != "/data/ha-lua.db" {
		t.Errorf("database: got %q", cfg.Database)
	}
	if cfg.LogLevel != "debug" || cfg.StateHistory.RetentionDays != 5 {
		t.Errorf("options not applied: %+v", cfg)
	}
}

func TestLoadAddonIgnoresConnectionOptions(t *testing.T) {
	t.Setenv("SUPERVISOR_TOKEN", "env-token")
	path := writeFile(t, "options.json", `{
		"homeassistant": {"url": "ws://evil:1234", "token": "stale"}
	}`)

	cfg, err := load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HomeAssistant.URL != "ws://supervisor/core/websocket" {
		t.Errorf("url must be forced in add-on mode, got %q", cfg.HomeAssistant.URL)
	}
	if cfg.HomeAssistant.Token != "env-token" {
		t.Errorf("token must come from the env in add-on mode, got %q", cfg.HomeAssistant.Token)
	}
}

func TestLoadDevYAML(t *testing.T) {
	path := writeFile(t, "config.dev.yaml", `
homeassistant:
  url: "ws://homeassistant.local:8123/api/websocket"
  token: "dev-token"
scripts_dir: "./scripts"
database: "./ha-lua.db"
`)

	cfg, err := load(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HomeAssistant.URL != "ws://homeassistant.local:8123/api/websocket" {
		t.Errorf("url: got %q", cfg.HomeAssistant.URL)
	}
	if cfg.HomeAssistant.Token != "dev-token" {
		t.Errorf("token: got %q", cfg.HomeAssistant.Token)
	}
	if cfg.ScriptsDir != "./scripts" {
		t.Errorf("scripts_dir: got %q", cfg.ScriptsDir)
	}
	// Unset fields still get defaults.
	if cfg.LogLevel != "info" || cfg.StateHistory.RetentionDays != 2 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := load(filepath.Join(t.TempDir(), "nope.json"), false); err == nil {
		t.Fatal("expected error for missing config file")
	}
}
