package main

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	Enabled   bool     `yaml:"enabled"`
	DryRun    bool     `yaml:"dry_run"`
	Minutes   int      `yaml:"minutes_before_expiry"`
	Poll      int      `yaml:"poll_seconds"`
	Timeout   int      `yaml:"request_timeout_seconds"`
	StateFile string   `yaml:"state_file"`
	Accounts  []string `yaml:"accounts"`
}

func parseConfig(raw []byte) (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	c := Config{DryRun: true, Minutes: 10, Poll: 15, Timeout: 30, StateFile: filepath.Join(home, ".local/state/codex-autoreset/state.json")}
	if err = yaml.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("invalid plugin configuration")
	}
	if c.Minutes < 1 || c.Minutes > 60 || c.Poll < 1 || c.Poll > 60 || c.Timeout < 1 || c.Timeout > 120 {
		return c, fmt.Errorf("minutes must be 1..60, poll 1..60 seconds, timeout 1..120 seconds")
	}
	if !filepath.IsAbs(c.StateFile) {
		return c, fmt.Errorf("state_file must be absolute")
	}
	return c, nil
}
func (c Config) interval() time.Duration { return time.Duration(c.Poll) * time.Second }
