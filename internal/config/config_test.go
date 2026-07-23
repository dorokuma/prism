package config

import (
	"bytes"
	"log/slog"
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
	if cfg.Listen != "127.0.0.1:18790" {
		t.Errorf("default listen = %q, want 127.0.0.1:18790", cfg.Listen)
	}
	if cfg.ProbeInterval != 10*time.Minute {
		t.Errorf("default probe interval = %v, want 10m", cfg.ProbeInterval)
	}
	if cfg.WireAPI != "both" {
		t.Errorf("default wire_api = %q, want both", cfg.WireAPI)
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
		ModelRemapEnabled: true,
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
		ModelRemapEnabled: true,
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
		oldDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		defer slog.SetDefault(oldDefault)

		cfg, err := LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if !strings.Contains(buf.String(), "prompt_cache_retention") {
			t.Errorf("expected warning for GLM tier without strip_fields, got: %s", buf.String())
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
		oldDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		defer slog.SetDefault(oldDefault)

		cfg, err := LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if strings.Contains(buf.String(), "prompt_cache_retention") {
			t.Errorf("unexpected warning for GLM tier WITH prompt_cache_retention, got: %s", buf.String())
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
		oldDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		defer slog.SetDefault(oldDefault)

		cfg, err := LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if !strings.Contains(buf.String(), "prompt_cache_retention") {
			t.Errorf("expected warning for GLM tier with other fields but no prompt_cache_retention, got: %s", buf.String())
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
		oldDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		defer slog.SetDefault(oldDefault)

		cfg, err := LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if strings.Contains(buf.String(), "prompt_cache_retention") {
			t.Errorf("unexpected warning for non-GLM tier, got: %s", buf.String())
		}
		if cfg.ModelTiers["standard"] != "deepseek-v4-flash" {
			t.Errorf("expected standard tier, got %v", cfg.ModelTiers)
		}
	})
}

func TestLoadConfigEmptyAccounts(t *testing.T) {
	content := `
accounts:
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
		t.Fatal("expected error for empty accounts, got nil")
	}
	if !strings.Contains(err.Error(), "no accounts") {
		t.Errorf("error = %q, want containing \"no accounts\"", err.Error())
	}
}

func TestParseTrustedProxies(t *testing.T) {
	_, err := ParseTrustedProxies([]string{"10.0.0.0/8"})
	if err != nil {
		t.Errorf("unexpected error for valid CIDR: %v", err)
	}
	_, err = ParseTrustedProxies([]string{"invalid"})
	if err == nil {
		t.Fatal("expected error for invalid CIDR, got nil")
	}
}

func TestLoadConfigProbeIntervalTooSmall(t *testing.T) {
	content := `
probe_interval: 500ms
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
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(oldDefault)

	cfg, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ProbeInterval != 10*time.Minute {
		t.Errorf("probe_interval = %v, want fallback to 10m", cfg.ProbeInterval)
	}
	if !strings.Contains(buf.String(), "too small") {
		t.Errorf("expected warning for too-small probe_interval, got: %s", buf.String())
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

func TestLoadConfig_NonLoopbackRequiresAuth(t *testing.T) {
	t.Run("non-loopback without auth", func(t *testing.T) {
		content := `
listen: 0.0.0.0:8080
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		f, err := os.CreateTemp("", "config-.yaml")
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
			t.Fatal("expected error for non-loopback listen without auth token, got nil")
		}
	})

	t.Run("loopback 127.0.0.1 without auth", func(t *testing.T) {
		content := `
listen: 127.0.0.1:8080
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		f, err := os.CreateTemp("", "config-.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		f.Close()

		_, err = LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig with loopback should succeed: %v", err)
		}
	})

	t.Run("loopback [::1] without auth", func(t *testing.T) {
		content := `
listen: "[::1]:8080"
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		f, err := os.CreateTemp("", "config-.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		f.Close()

		_, err = LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig with [::1] should succeed: %v", err)
		}
	})

	t.Run("non-loopback with auth token", func(t *testing.T) {
		content := `
listen: 0.0.0.0:8080
auth_token: my-secret-token
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		f, err := os.CreateTemp("", "config-.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		f.Close()

		_, err = LoadConfig(f.Name())
		if err != nil {
			t.Fatalf("LoadConfig with non-loopback and auth token should succeed: %v", err)
		}
	})

	t.Run("empty host without auth", func(t *testing.T) {
		content := `
listen: ":8080"
auth_token: 
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		f, err := os.CreateTemp("", "config-.yaml")
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
			t.Fatal("expected error for empty-host listen without auth token, got nil")
		}
	})
}

func TestReloadConfig_ModelRemapUpdated(t *testing.T) {
	// Write initial config with model_remap={"a":"tier1"}
	content1 := `
model_tiers:
  tier1: upstream-a
  tier2: upstream-b
model_remap_enabled: true
model_remap:
  a: tier1
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
	if _, err := f.Write([]byte(content1)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg1, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	holder := NewConfigHolder(cfg1)

	// Initial remap: a → tier1 → upstream-a
	if got := holder.Load().RemapModel("a"); got != "upstream-a" {
		t.Fatalf("initial RemapModel(a) = %q, want upstream-a", got)
	}

	// Overwrite config with model_remap={"b":"tier2"}
	content2 := `
model_tiers:
  tier1: upstream-a
  tier2: upstream-b
model_remap_enabled: true
model_remap:
  b: tier2
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}

	warnings, err := ReloadConfig(holder, f.Name())
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if len(warnings) != 0 {
		t.Logf("warnings (expected none): %v", warnings)
	}

	// After reload: b → tier2 → upstream-b
	newCfg := holder.Load()
	if got := newCfg.RemapModel("b"); got != "upstream-b" {
		t.Errorf("after reload RemapModel(b) = %q, want upstream-b", got)
	}
	// Old mapping no longer exists
	if got := newCfg.RemapModel("a"); got != "a" {
		t.Errorf("after reload RemapModel(a) = %q, want \"a\" (pass-through)", got)
	}
}

func TestReloadConfig_AccountsChangedWarning(t *testing.T) {
	content1 := `
accounts:
  - name: acc1
    key: key1
    base_url: https://api1.example.com
`
	f, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write([]byte(content1)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg1, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	holder := NewConfigHolder(cfg1)

	// Change accounts: add a second account
	content2 := `
accounts:
  - name: acc1
    key: key1
    base_url: https://api1.example.com
  - name: acc2
    key: key2
    base_url: https://api2.example.com
`
	if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}

	warnings, err := ReloadConfig(holder, f.Name())
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}

	found := false
	for _, w := range warnings {
		if strings.Contains(w, "accounts") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about accounts change, got: %v", warnings)
	}
}

func TestReloadConfig_ListenChangedWarning(t *testing.T) {
	content1 := `
listen: 127.0.0.1:8080
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
	if _, err := f.Write([]byte(content1)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg1, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	holder := NewConfigHolder(cfg1)

	// Change listen address
	content2 := `
listen: 127.0.0.1:9090
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}

	warnings, err := ReloadConfig(holder, f.Name())
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}

	found := false
	for _, w := range warnings {
		if strings.Contains(w, "listen") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about listen change, got: %v", warnings)
	}
}

func TestReloadConfig_NonLoopbackNoAuthRejected(t *testing.T) {
	content1 := `
listen: 127.0.0.1:8080
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
	if _, err := f.Write([]byte(content1)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg1, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	holder := NewConfigHolder(cfg1)

	// Change to non-loopback without auth – should be rejected by LoadConfig
	content2 := `
listen: 0.0.0.0:8080
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}

	_, err = ReloadConfig(holder, f.Name())
	if err == nil {
		t.Fatal("expected error for non-loopback listen without auth token, got nil")
	}

	// Old config should be preserved
	curCfg := holder.Load()
	if curCfg.Listen != "127.0.0.1:8080" {
		t.Errorf("old config listen = %q, want 127.0.0.1:8080 (should be preserved)", curCfg.Listen)
	}
}

func TestReloadConfig_OldConfigPreservedOnError(t *testing.T) {
	content1 := `
listen: 127.0.0.1:8080
model_tiers:
  tier1: upstream-a
model_remap_enabled: true
model_remap:
  a: tier1
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
	if _, err := f.Write([]byte(content1)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg1, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	holder := NewConfigHolder(cfg1)

	// Write an invalid config (missing accounts)
	content2 := `
listen: 0.0.0.0:8080
auth_token: ""
`
	if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}

	_, err = ReloadConfig(holder, f.Name())
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}

	// Old config model mapping should be preserved
	curCfg := holder.Load()
	if got := curCfg.RemapModel("a"); got != "upstream-a" {
		t.Errorf("after failed reload RemapModel(a) = %q, want upstream-a (old config preserved)", got)
	}
	if curCfg.Listen != "127.0.0.1:8080" {
		t.Errorf("after failed reload listen = %q, want 127.0.0.1:8080 (old config preserved)", curCfg.Listen)
	}
}
