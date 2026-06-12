package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultOptionsPath = "/data/options.json"

// Config is the unified config struct for both production and dev modes.
type Config struct {
	HomeAssistant struct {
		URL   string `json:"url"   yaml:"url"`
		Token string `json:"token" yaml:"token"`
	} `json:"homeassistant" yaml:"homeassistant"`

	ScriptsDir string `json:"scripts_dir" yaml:"scripts_dir"`
	Database   string `json:"database"    yaml:"database"`
	LogLevel   string `json:"log_level"   yaml:"log_level"`

	StateHistory struct {
		RetentionDays int    `json:"retention_days" yaml:"retention_days"`
		PurgeInterval string `json:"purge_interval"  yaml:"purge_interval"`
	} `json:"state_history" yaml:"state_history"`

	Debug struct {
		PprofAddr string `json:"pprof_addr" yaml:"pprof_addr"`
	} `json:"debug" yaml:"debug"`
}

// PurgeInterval parses and returns the purge interval duration.
func (c *Config) PurgeInterval() (time.Duration, error) {
	s := c.StateHistory.PurgeInterval
	if s == "" {
		return time.Hour, nil
	}
	return time.ParseDuration(s)
}

// Defaults fills in zero-value fields with sensible defaults.
func (c *Config) Defaults() {
	if c.ScriptsDir == "" {
		c.ScriptsDir = "/addon_config/scripts"
	}
	if c.Database == "" {
		c.Database = "/data/ha-lua.db"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.StateHistory.RetentionDays == 0 {
		c.StateHistory.RetentionDays = 2
	}
	if c.StateHistory.PurgeInterval == "" {
		c.StateHistory.PurgeInterval = "1h"
	}
}

// Load reads config from path (dev mode, YAML). If path is empty, add-on
// mode is assumed: user options come from /data/options.json and the
// connection details are not options at all — the token comes from
// $SUPERVISOR_TOKEN and the URL is the fixed Supervisor proxy endpoint.
// This is the single production config channel; run.sh passes no flags.
func Load(path string) (*Config, error) {
	addon := path == ""
	if addon {
		path = defaultOptionsPath
	}
	return load(path, addon)
}

func load(path string, addon bool) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	// Try JSON first (production options.json), then YAML (dev config).
	if err := json.Unmarshal(data, &cfg); err != nil {
		if err2 := yaml.Unmarshal(data, &cfg); err2 != nil {
			return nil, fmt.Errorf("parse config (JSON: %v, YAML: %v)", err, err2)
		}
	}
	cfg.Defaults()
	if addon {
		cfg.HomeAssistant.URL = "ws://supervisor/core/websocket"
		cfg.HomeAssistant.Token = os.Getenv("SUPERVISOR_TOKEN")
	}
	return &cfg, nil
}
