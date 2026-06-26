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
	Listen        string          `yaml:"listen"`
	ProbeInterval time.Duration   `yaml:"probe_interval"`
	Accounts      []AccountConfig `yaml:"accounts"`
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
