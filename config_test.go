package main

import (
	"bytes"
	"log"
	"os"
	"strings"
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
			"gpt-4":   "premium",
			"gpt-3.5": "standard",
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
			"gpt-4": "premium",
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

func TestLoadConfigGLMWarning(t *testing.T) {
	t.Run("glm tier without strip_fields triggers warning", func(t *testing.T) {
		content := `
model_tiers:
  glm-test: glm-5.2
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

		var buf bytes.Buffer
		oldWriter := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(oldWriter)

		cfg, err := LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if !strings.Contains(buf.String(), "WARNING") {
			t.Errorf("expected WARNING for GLM tier without strip_fields, got: %s", buf.String())
		}
		if cfg.ModelTiers["glm-test"] != "glm-5.2" {
			t.Errorf("expected glm-test tier, got %v", cfg.ModelTiers)
		}
	})

	t.Run("glm tier with prompt_cache_retention does not warn", func(t *testing.T) {
		content := `
model_tiers:
  glm-test: glm-5.2
strip_fields:
  glm-test:
    - prompt_cache_retention
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

		var buf bytes.Buffer
		oldWriter := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(oldWriter)

		cfg, err := LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if strings.Contains(buf.String(), "WARNING") {
			t.Errorf("unexpected WARNING for GLM tier WITH prompt_cache_retention, got: %s", buf.String())
		}
		if cfg.StripFields["glm-test"] == nil || len(cfg.StripFields["glm-test"]) == 0 {
			t.Errorf("expected strip_fields for glm-test, got %v", cfg.StripFields)
		}
	})

	t.Run("glm tier with other fields but missing prompt_cache_retention warns", func(t *testing.T) {
		content := `
model_tiers:
  glm-test: glm-5.2
strip_fields:
  glm-test:
    - some_other_field
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

		var buf bytes.Buffer
		oldWriter := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(oldWriter)

		cfg, err := LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if !strings.Contains(buf.String(), "WARNING") {
			t.Errorf("expected WARNING for GLM tier with other fields but no prompt_cache_retention, got: %s", buf.String())
		}
		if len(cfg.StripFields["glm-test"]) != 1 || cfg.StripFields["glm-test"][0] != "some_other_field" {
			t.Errorf("expected strip_fields for glm-test with some_other_field, got %v", cfg.StripFields)
		}
	})

	t.Run("non-glm tier does not warn", func(t *testing.T) {
		content := `
model_tiers:
  standard: deepseek-v4-flash
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

		var buf bytes.Buffer
		oldWriter := log.Writer()
		log.SetOutput(&buf)
		defer log.SetOutput(oldWriter)

		cfg, err := LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if strings.Contains(buf.String(), "WARNING") {
			t.Errorf("unexpected WARNING for non-GLM tier, got: %s", buf.String())
		}
		if cfg.ModelTiers["standard"] != "deepseek-v4-flash" {
			t.Errorf("expected standard tier, got %v", cfg.ModelTiers)
		}
	})
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
