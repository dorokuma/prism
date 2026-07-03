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
	ModelTiers    map[string]string `yaml:"model_tiers"`
	DefaultTier   string            `yaml:"default_tier"`
	Debug         bool              `yaml:"debug"`
	MCPToolsJSON  string            `yaml:"mcp_tools_json"`
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
	if cfg.ModelTiers == nil {
		cfg.ModelTiers = map[string]string{}
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

// RemapModel resolves a virtual model name to an upstream model via tier lookup.
// virtual model → tier (model_remap) → upstream model (model_tiers).
// Falls back to default_tier → model_tiers, then returns the input unchanged.
func (c *Config) RemapModel(model string) string {
	if c.ModelRemap != nil {
		if tier, ok := c.ModelRemap[model]; ok {
			if upstream, ok := c.ModelTiers[tier]; ok && upstream != "" {
				return upstream
			}
		}
	}
	if c.DefaultTier != "" {
		if upstream, ok := c.ModelTiers[c.DefaultTier]; ok && upstream != "" {
			return upstream
		}
	}
	return model
}

// ReverseRemapModel maps an upstream model name back to a virtual model.
// Scans model_remap keys that resolve to the same upstream via model_tiers.
// Returns the first match or the model name unchanged.
func (c *Config) ReverseRemapModel(upstream string) string {
	if c.ModelTiers == nil || c.ModelRemap == nil {
		return upstream
	}
	for virtual, tier := range c.ModelRemap {
		if t, ok := c.ModelTiers[tier]; ok && t == upstream {
			return virtual
		}
	}
	return upstream
}

// VirtualModels returns the list of virtual model names exposed to clients.
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
