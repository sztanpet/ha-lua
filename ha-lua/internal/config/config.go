// Package config loads the daemon configuration: /data/options.json in
// add-on mode, YAML via --config in dev mode.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultOptionsPath = "/data/options.json"

// ingressPort is the internal port the HA Supervisor proxies for the
// authenticated sidebar panel. It must match `ingress_port` in config.yaml.
const ingressPort = 8099

// Config is the unified config struct for both production and dev modes.
type Config struct {
	HomeAssistant struct {
		URL   string `json:"url"   yaml:"url"`
		Token string `json:"token" yaml:"token"`
	} `json:"homeassistant" yaml:"homeassistant"`

	ScriptsDir string `json:"scripts_dir" yaml:"scripts_dir"`
	Database   string `json:"database"    yaml:"database"`
	LogLevel   string `json:"log_level"   yaml:"log_level"`
	Timezone   string `json:"timezone"    yaml:"timezone"`
	// LogDir is where the daemon writes its own log file (ha-lua.log), in
	// addition to stderr (the Supervisor add-on log). Empty disables file
	// logging. Not a user option: add-on mode forces /config/ha-lua/logs so
	// the log sits beside the scripts and is readable via the File Editor;
	// dev mode leaves it empty (stderr only) unless set in the YAML.
	LogDir string `json:"log_dir" yaml:"log_dir"`
	// HTTPPort is the LAN port for the script-driven UI server. 0 disables it.
	HTTPPort int `json:"http_port" yaml:"http_port"`
	// IngressPort is the internal port the HA Supervisor proxies for the
	// authenticated sidebar panel. 0 disables the ingress listener. It is not a
	// user option: add-on mode forces it to the manifest value, dev mode leaves
	// it 0. The spec (§5.5) lists it only as a manifest field, but the Go daemon
	// must actually bind it for ingress to work.
	IngressPort int `json:"ingress_port" yaml:"ingress_port"`
	// ExamplesDir is where the bundled reference examples are materialized
	// (overwritten, read-only) on boot. Not a user option: add-on mode forces
	// /config/ha-lua/examples so the set sits beside the scripts dir and is
	// readable in the File Editor; dev mode leaves it empty (no materialization)
	// unless set in the YAML. Empty disables materialization entirely.
	ExamplesDir string `json:"examples_dir" yaml:"examples_dir"`

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
		c.ScriptsDir = "/config/ha-lua/scripts"
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
		// Ingress only exists under the Supervisor. Force the manifest port so
		// the sidebar panel works out of the box; dev mode binds no ingress
		// listener (IngressPort stays 0). Keep this in sync with
		// `ingress_port` in config.yaml.
		cfg.IngressPort = ingressPort
		// Persist the daemon log under the mounted config dir, beside the
		// scripts, so users can open it in the File Editor / Studio Code.
		cfg.LogDir = "/config/ha-lua/logs"
		// Drop the bundled reference examples beside the scripts dir, refreshed
		// to this add-on version on every boot. Read-only reference, never run.
		cfg.ExamplesDir = "/config/ha-lua/examples"
	}
	return &cfg, nil
}
