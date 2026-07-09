package main

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
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
	ModelTiers    map[string]string `yaml:"model_tiers"`
	DefaultTier   string            `yaml:"default_tier"`
	Debug         bool              `yaml:"debug"`
	MCPToolsJSON  string            `yaml:"mcp_tools_json"`
	ProbeModel    string            `yaml:"probe_model"`
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
	if cfg.ProbeModel == "" {
		cfg.ProbeModel = "deepseek-chat"
	}
	if _, err := ParseWireAPIMode(cfg.WireAPI); err != nil {
		return nil, err
	}
	if cfg.ModelTiers == nil {
		cfg.ModelTiers = map[string]string{}
	}
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Key == "" {
			envVar := "LB_KEY_" + strings.ToUpper(strings.ReplaceAll(cfg.Accounts[i].Name, "-", "_"))
			// Try systemd LoadCredential first, then env var
			key := getCredential(envVar)
			if key == "" {
				key = os.Getenv(envVar)
			}
			cfg.Accounts[i].Key = key
			if cfg.Accounts[i].Key == "" {
				return nil, fmt.Errorf("account %s: key not set in config and credential/env var %s is empty", cfg.Accounts[i].Name, envVar)
			}
		}
	}
	return cfg, nil
}

// RemapModel resolves a virtual model name to its upstream model via
// model_remap → model_tiers. Models NOT in model_remap (real upstream names)
// pass through unchanged. Models IN model_remap whose tier has no upstream
// mapping fall back to default_tier.
func (c *Config) RemapModel(model string) string {
	if c.ModelRemap != nil {
		if tier, ok := c.ModelRemap[model]; ok {
			if upstream, ok := c.ModelTiers[tier]; ok && upstream != "" {
				return upstream
			}
			// Virtual model found but its tier has no upstream → fallback
			if c.DefaultTier != "" {
				if upstream, ok := c.ModelTiers[c.DefaultTier]; ok && upstream != "" {
					return upstream
				}
			}
		}
	}
	return model
}

// AllModels returns both virtual model names (model_remap keys) and real
// upstream model names (model_tiers values) for /v1/models.
func (c *Config) AllModels() []string {
	seen := make(map[string]bool)
	var out []string
	for k := range c.ModelRemap {
		seen[k] = true
		out = append(out, k)
	}
	for _, upstream := range c.ModelTiers {
		if upstream != "" && !seen[upstream] {
			seen[upstream] = true
			out = append(out, upstream)
		}
	}
	return out
}

func (c *Config) VirtualModels() []string {
	if c.ModelRemap == nil {
		return nil
	}
	models := make([]string, 0, len(c.ModelRemap))
	for k := range c.ModelRemap {
		models = append(models, k)
	}
	return models
}

// getCredential reads a credential file from the systemd LoadCredential
// directory (CREDENTIALS_DIRECTORY). Returns the trimmed contents on success,
// or "" if CREDENTIALS_DIRECTORY is unset or the file cannot be read.
func getCredential(name string) string {
	credDir := os.Getenv("CREDENTIALS_DIRECTORY")
	if credDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(credDir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
