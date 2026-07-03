package main

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"strings"
	"time"
)

type AccountConfig struct {
	Name    string `yaml:"name"`
	Key     string `yaml:"key,omitempty"`
	BaseURL string `yaml:"base_url"`
}

type Config struct {
	Listen        string            `yaml:"listen"`
	ProbeInterval time.Duration     `yaml:"probe_interval"`
	WireAPI       string            `yaml:"wire_api"`
	Accounts      []AccountConfig   `yaml:"accounts"`
	ModelRemap    map[string]string `yaml:"model_remap"`
	DefaultModel  string            `yaml:"default_model"`
	Debug         bool              `yaml:"debug"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.Listen == "" {
		cfg.Listen = ":18790"
	}
	if cfg.ProbeInterval == 0 {
		cfg.ProbeInterval = 10 * time.Minute
	}
	if cfg.WireAPI == "" {
		cfg.WireAPI = "both"
	}
	if _, err := ParseWireAPIMode(cfg.WireAPI); err != nil {
		return nil, err
	}
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Key == "" {
			envVar := "LB_KEY_" + strings.ToUpper(strings.ReplaceAll(cfg.Accounts[i].Name, "-", "_"))
			cfg.Accounts[i].Key = os.Getenv(envVar)
			if cfg.Accounts[i].Key == "" {
				return nil, fmt.Errorf("account %s: key not set in config and env var %s is empty", cfg.Accounts[i].Name, envVar)
			}
		}
	}
	return cfg, nil
}

// RemapModel translates a model name using the configured remap table.
// If no mapping exists and a default_model is set, returns the default.
// If neither is set, returns the original model name.
func (c *Config) RemapModel(model string) string {
	if c.ModelRemap != nil {
		if mapped, ok := c.ModelRemap[model]; ok {
			return mapped
		}
	}
	if c.DefaultModel != "" {
		return c.DefaultModel
	}
	return model
}
