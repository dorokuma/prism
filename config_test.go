package main

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	content := `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	f, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != ":18790" {
		t.Errorf("default listen = %q, want :18790", cfg.Listen)
	}
	if cfg.ProbeInterval != 10*time.Minute {
		t.Errorf("default probe interval = %v, want 10m", cfg.ProbeInterval)
	}
	if cfg.WireAPI != "both" {
		t.Errorf("default wire_api = %q, want both", cfg.WireAPI)
	}
	if cfg.ProbeModel != "deepseek-chat" {
		t.Errorf("default probe_model = %q, want deepseek-chat", cfg.ProbeModel)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(cfg.Accounts))
	}
	if cfg.Accounts[0].Name != "test-acc" {
		t.Errorf("account name = %q, want test-acc", cfg.Accounts[0].Name)
	}
}

func TestLoadConfigKeyFromEnv(t *testing.T) {
	os.Setenv("LB_KEY_TEST_ACC", "env-key-value")
	defer os.Unsetenv("LB_KEY_TEST_ACC")

	content := `
accounts:
  - name: test-acc
    base_url: https://api.example.com
`
	f, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Accounts[0].Key != "env-key-value" {
		t.Errorf("key = %q, want env-key-value", cfg.Accounts[0].Key)
	}
}

func TestLoadConfigMissingKeyError(t *testing.T) {
	content := `
accounts:
  - name: test-acc
    base_url: https://api.example.com
`
	f, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	_, err = LoadConfig(f.Name())
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
}

func TestConfigRemapModel(t *testing.T) {
	cfg := &Config{
		ModelRemap: map[string]string{
			"gpt-4":    "premium",
			"gpt-3.5":  "standard",
		},
		ModelTiers: map[string]string{
			"premium":  "gpt-4-turbo",
			"standard": "gpt-3.5-turbo",
		},
		DefaultTier: "standard",
	}

	tests := []struct {
		input    string
		expected string
	}{
		{"gpt-4", "gpt-4-turbo"},
		{"gpt-3.5", "gpt-3.5-turbo"},
		{"unknown-model", "unknown-model"},
		{"gpt-4-turbo", "gpt-4-turbo"}, // pass-through for real model names
	}
	for _, tc := range tests {
		got := cfg.RemapModel(tc.input)
		if got != tc.expected {
			t.Errorf("RemapModel(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestConfigRemapModelFallback(t *testing.T) {
	cfg := &Config{
		ModelRemap: map[string]string{
			"gpt-4":   "premium",
		},
		ModelTiers: map[string]string{
			"standard": "gpt-3.5-turbo",
		},
		DefaultTier: "standard",
	}
	// premium tier has no upstream mapping, falls back to default_tier
	got := cfg.RemapModel("gpt-4")
	if got != "gpt-3.5-turbo" {
		t.Errorf("RemapModel(gpt-4) = %q, want gpt-3.5-turbo (fallback)", got)
	}
}

func TestConfigAllModels(t *testing.T) {
	cfg := &Config{
		ModelRemap: map[string]string{
			"gpt-4":   "premium",
			"gpt-3.5": "standard",
		},
		ModelTiers: map[string]string{
			"premium":  "gpt-4-turbo",
			"standard": "gpt-3.5-turbo",
		},
	}
	models := cfg.AllModels()
	if len(models) != 4 {
		t.Fatalf("AllModels len = %d, want 4", len(models))
	}
	seen := make(map[string]bool)
	for _, m := range models {
		seen[m] = true
	}
	for _, want := range []string{"gpt-4", "gpt-3.5", "gpt-4-turbo", "gpt-3.5-turbo"} {
		if !seen[want] {
			t.Errorf("AllModels missing %q", want)
		}
	}
}
