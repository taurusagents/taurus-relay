// Package config manages persistent relay configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// defaultConfigDir is the config directory relative to the user's home,
// used when no override is provided.
const defaultConfigDir = ".config/taurus-relay"
const configFile = "config.json"

// EnvConfigDir is the environment variable that overrides the config directory.
const EnvConfigDir = "TAURUS_RELAY_CONFIG_DIR"

// overrideDir is set from the --config-dir CLI flag and takes precedence
// over both the environment variable and the default location.
var overrideDir string

// SetDir sets an explicit config directory (e.g. from --config-dir).
// An empty string clears the override.
func SetDir(dir string) {
	overrideDir = dir
}

// Dir resolves the config directory: --config-dir flag > TAURUS_RELAY_CONFIG_DIR
// env var > ~/.config/taurus-relay.
func Dir() string {
	if overrideDir != "" {
		return overrideDir
	}
	if env := os.Getenv(EnvConfigDir); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, defaultConfigDir)
}

// Config holds persistent relay credentials and settings.
type Config struct {
	Server       string `json:"server"`
	TargetID     string `json:"target_id,omitempty"`
	JWT          string `json:"jwt,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// Path returns the full config file path.
func Path() string {
	return filepath.Join(Dir(), configFile)
}

// Load reads config from disk. Returns zero Config if file doesn't exist.
func Load() (*Config, error) {
	p := Path()
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// Save writes config to disk.
func (c *Config) Save() error {
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(p, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// HasCredentials returns true if the config has saved credentials.
func (c *Config) HasCredentials() bool {
	return c.JWT != "" && c.TargetID != ""
}
