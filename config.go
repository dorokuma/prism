package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type AccountConfig struct {
	Name    string `yaml:"name"`
	Key     string `yaml:"key,omitempty"`
	BaseURL string `yaml:"base_url"`
}

type Config struct {
	Listen        string              `yaml:"listen"`
	ProbeInterval time.Duration       `yaml:"probe_interval"`
	WireAPI       string              `yaml:"wire_api"`
	Accounts      []AccountConfig     `yaml:"accounts"`
	ModelRemap    map[string]string   `yaml:"model_remap"`
	ModelTiers    map[string]string   `yaml:"model_tiers"`
	DefaultTier   string              `yaml:"default_tier"`
	StripFields   map[string][]string `yaml:"strip_fields"`
	Debug         bool                `yaml:"debug"`
	MCPToolsJSON  string              `yaml:"mcp_tools_json"`
	ProbeModel     string              `yaml:"probe_model"`
	AuthToken      string              `yaml:"auth_token,omitempty"`
	TLSCertFile    string              `yaml:"tls_cert_file,omitempty"`
	TLSKeyFile     string              `yaml:"tls_key_file,omitempty"`
	TrustedProxies []string            `yaml:"trusted_proxies,omitempty"`
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
		cfg.Listen = "127.0.0.1:18790"
	}
	if cfg.ProbeInterval == 0 {
		cfg.ProbeInterval = 10 * time.Minute
	}
	if cfg.ProbeInterval > 0 && cfg.ProbeInterval < time.Second {
		log.Printf("WARNING: probe_interval %v is too small (< 1s), falling back to 10m", cfg.ProbeInterval)
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
	if len(cfg.Accounts) == 0 {
		return nil, fmt.Errorf("no accounts configured")
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
	// AuthToken fallback to env var
	if cfg.AuthToken == "" {
		cfg.AuthToken = os.Getenv("PRISM_AUTH_TOKEN")
	}
	// TLS cert/key fallback to env vars
	if cfg.TLSCertFile == "" {
		cfg.TLSCertFile = os.Getenv("PRISM_TLS_CERT")
	}
	if cfg.TLSKeyFile == "" {
		cfg.TLSKeyFile = os.Getenv("PRISM_TLS_KEY")
	}
	// Validate trusted proxies CIDRs
	for _, s := range cfg.TrustedProxies {
		if _, _, err := net.ParseCIDR(s); err != nil {
			return nil, fmt.Errorf("trusted_proxies: invalid CIDR %q: %v", s, err)
		}
	}
	// Startup validation: warn if GLM/z-ai upstreams lack prompt_cache_retention in strip_fields
	for tier, upstream := range cfg.ModelTiers {
		upstreamLower := strings.ToLower(upstream)
		if strings.Contains(upstreamLower, "glm") || strings.Contains(upstreamLower, "z-ai") {
			fields := cfg.StripFields[tier]
			hasPromptCacheRetention := false
			for _, f := range fields {
				if f == "prompt_cache_retention" {
					hasPromptCacheRetention = true
					break
				}
			}
			if !hasPromptCacheRetention {
				log.Printf("WARNING: tier %q upstream %q looks like GLM/z-ai but strip_fields for this tier is missing prompt_cache_retention; add it to avoid 400 errors", tier, upstream)
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

// ParseTrustedProxies parses a list of CIDR strings into *net.IPNet values.
// This is a helper for main.go to use after loading config.
func ParseTrustedProxies(proxies []string) ([]*net.IPNet, error) {
	if len(proxies) == 0 {
		return nil, nil
	}
	parsed := make([]*net.IPNet, 0, len(proxies))
	for _, s := range proxies {
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, cidr)
	}
	return parsed, nil
}
