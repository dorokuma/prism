package config

import (
	"bytes"
	"fmt"
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

// SetProviderSchemaForTest sets the unexported providerSchema map on a Config
// so tests can exercise EffortSchema/provider-aware lookups without going
// through LoadConfig (which derives the schema from account base_url hosts).
func SetProviderSchemaForTest(c *Config, schema map[string]string) {
	c.providerSchema = schema
}

// TestReloadConfig_DebugChangedNoWarning pins item 5: debug is
// hot-reloadable (the SIGHUP main flow re-applies it via util.DebugMode;
// LogLevelHook only adjusts the logger level), so a debug change must NOT
// emit the old "restart required" warning.
func TestReloadConfig_DebugChangedNoWarning(t *testing.T) {
	content1 := `
debug: false
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

	content2 := `
debug: true
accounts:
  - name: acc1
    key: key1
    base_url: https://api1.example.com
`
	if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}

	warnings, err := ReloadConfig(holder, f.Name())
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "debug") {
			t.Errorf("unexpected debug restart warning after hot-reloadable change: %q", w)
		}
	}
	// The new value is live in the holder.
	if got := holder.Load().Debug; !got {
		t.Error("Debug = false after reload, want true (hot-reloadable)")
	}
}

func TestLookupModelMetadata_FallbackToDefault(t *testing.T) {
	cfg := &Config{
		ModelMetadata: ModelMetadataMap{
			"glm-5.2": {ContextWindow: intPtr(1000000)},
		},
	}
	// No per-provider layer: lookup for any provider falls back to default.
	meta, ok := cfg.LookupModelMetadata("ollama-cloud", "glm-5.2")
	if !ok {
		t.Fatalf("expected glm-5.2 to be found via default fallback")
	}
	if meta.ContextWindow == nil || *meta.ContextWindow != 1000000 {
		t.Fatalf("ContextWindow = %v, want 1000000", meta.ContextWindow)
	}
	// Unknown model: not found.
	if _, ok := cfg.LookupModelMetadata("ollama-cloud", "nope"); ok {
		t.Fatalf("expected unknown model to be not found")
	}
}

func TestLookupModelMetadata_PerProviderReplacesDefault(t *testing.T) {
	cfg := &Config{
		ModelMetadata: ModelMetadataMap{
			// default layer: deepseek-v4-pro = 1M context
			"deepseek-v4-pro": {ContextWindow: intPtr(1000000)},
		},
		ModelMetadataPerProvider: map[string]ModelMetadataMap{
			"ollama-cloud": {
				// per-provider override: entry present but context_window omitted
				// (nil) → full replacement of the default entry. The nil
				// ContextWindow must NOT become the default 1M.
				"deepseek-v4-pro": {Reasoning: boolPtr(true)},
			},
		},
	}

	// ollama-cloud: per-provider entry fully replaces default → no ContextWindow.
	meta, ok := cfg.LookupModelMetadata("ollama-cloud", "deepseek-v4-pro")
	if !ok {
		t.Fatalf("expected per-provider deepseek-v4-pro to be found")
	}
	if meta.ContextWindow != nil {
		t.Fatalf("per-provider ContextWindow = %v, want nil (full replace, not merge)", *meta.ContextWindow)
	}
	if meta.Reasoning == nil || *meta.Reasoning != true {
		t.Fatalf("per-provider Reasoning = %v, want true", meta.Reasoning)
	}

	// opencode-go: no per-provider entry → default layer wins (1M).
	meta2, ok := cfg.LookupModelMetadata("opencode-go", "deepseek-v4-pro")
	if !ok {
		t.Fatalf("expected default deepseek-v4-pro to be found for opencode-go")
	}
	if meta2.ContextWindow == nil || *meta2.ContextWindow != 1000000 {
		t.Fatalf("opencode-go ContextWindow = %v, want 1000000 (default fallback)", meta2.ContextWindow)
	}
}

func intPtr(i int) *int    { return &i }
func boolPtr(b bool) *bool { return &b }

func TestEffortSchema_Ollama(t *testing.T) {
	cfg := loadProviderSchemaCfg(t)
	if got := cfg.EffortSchema("ollama-cloud"); got != "ollama" {
		t.Errorf("EffortSchema(ollama-cloud)=%q, want ollama", got)
	}
}

func TestEffortSchema_Opencode(t *testing.T) {
	cfg := loadProviderSchemaCfg(t)
	if got := cfg.EffortSchema("opencode-go"); got != "" {
		t.Errorf("EffortSchema(opencode-go)=%q, want empty (opencode)", got)
	}
}

func TestEffortSchema_EmptyProvider(t *testing.T) {
	cfg := loadProviderSchemaCfg(t)
	if got := cfg.EffortSchema(""); got != "" {
		t.Errorf("EffortSchema(\"\")=%q, want empty", got)
	}
}

func loadProviderSchemaCfg(t *testing.T) *Config {
	t.Helper()
	content := `
providers:
  ollama-cloud:
    accounts:
      - name: ollama-acc
        key: test-key-12345
        base_url: https://ollama.com/v1
  opencode-go:
    accounts:
      - name: opencode-acc
        key: test-key-12345
        base_url: https://opencode.ai/zen
`
	f, err := os.CreateTemp("", "cfg-*.yaml")
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
	return cfg
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

// TestAccountConfigHeadersProbePathYAML verifies that the account-level
// headers / auth_header / probe_path / skip_pi_sync fields parse from YAML
// (both the providers block and the flat accounts block) and survive the
// providers→accounts flattening.
func TestAccountConfigHeadersProbePathYAML(t *testing.T) {
	content := `
providers:
  agentrouter-anthropic:
    accounts:
      - name: agentrouter-ant-1
        base_url: https://gw.example.com/
        skip_pi_sync: true
        probe_path: disabled
        headers:
          User-Agent: "claude-cli/1.0.0 (external, cli)"
          anthropic-beta: "claude-code-20250219,interleaved-thinking-20250219"
          x-app: cli
  agentrouter-openai:
    accounts:
      - name: agentrouter-oai-1
        base_url: https://gw.example.com/v1
        skip_pi_sync: true
        probe_path: /custom-probe
        auth_header: x-api-key
        headers:
          Originator: codex_cli_rs
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

	// Keys must be present (env fallback would fail otherwise).
	t.Setenv("LB_KEY_AGENTROUTER_ANT_1", "test-key-ant-1")
	t.Setenv("LB_KEY_AGENTROUTER_OAI_1", "test-key-oai-1")

	cfg, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(cfg.Accounts))
	}

	var ant, oai *AccountConfig
	for i := range cfg.Accounts {
		switch cfg.Accounts[i].Name {
		case "agentrouter-ant-1":
			ant = &cfg.Accounts[i]
		case "agentrouter-oai-1":
			oai = &cfg.Accounts[i]
		}
	}
	if ant == nil || oai == nil {
		t.Fatalf("expected both accounts, got %v", cfg.Accounts)
	}

	if ant.Provider != "agentrouter-anthropic" {
		t.Errorf("ant provider = %q, want agentrouter-anthropic", ant.Provider)
	}
	if !ant.SkipPISync {
		t.Error("ant skip_pi_sync = false, want true")
	}
	if ant.ProbePath != "disabled" {
		t.Errorf("ant probe_path = %q, want disabled", ant.ProbePath)
	}
	if ant.AuthHeader != "" {
		t.Errorf("ant auth_header = %q, want empty", ant.AuthHeader)
	}
	if len(ant.Headers) != 3 {
		t.Fatalf("ant headers = %v, want 3 entries", ant.Headers)
	}
	if ant.Headers["User-Agent"] != "claude-cli/1.0.0 (external, cli)" {
		t.Errorf("ant User-Agent = %q", ant.Headers["User-Agent"])
	}
	if ant.Headers["anthropic-beta"] != "claude-code-20250219,interleaved-thinking-20250219" {
		t.Errorf("ant anthropic-beta = %q", ant.Headers["anthropic-beta"])
	}
	if ant.Headers["x-app"] != "cli" {
		t.Errorf("ant x-app = %q", ant.Headers["x-app"])
	}

	if oai.Provider != "agentrouter-openai" {
		t.Errorf("oai provider = %q, want agentrouter-openai", oai.Provider)
	}
	if !oai.SkipPISync {
		t.Error("oai skip_pi_sync = false, want true")
	}
	if oai.ProbePath != "/custom-probe" {
		t.Errorf("oai probe_path = %q, want /custom-probe", oai.ProbePath)
	}
	if oai.AuthHeader != "x-api-key" {
		t.Errorf("oai auth_header = %q, want x-api-key", oai.AuthHeader)
	}
	if oai.Headers["Originator"] != "codex_cli_rs" {
		t.Errorf("oai Originator = %q", oai.Headers["Originator"])
	}
}

// TestAccountsEqualIncludesHeaders verifies accountsEqual detects changes in
// the account-level metadata fields (headers / probe_path / auth_header /
// skip_pi_sync / provider) so hot reload warns instead of silently ignoring
// them.
func TestAccountsEqualIncludesHeaders(t *testing.T) {
	base := []AccountConfig{{Name: "a", Key: "k", BaseURL: "https://x.com/v1", Provider: "p"}}

	if !accountsEqual(base, []AccountConfig{{Name: "a", Key: "k", BaseURL: "https://x.com/v1", Provider: "p"}}) {
		t.Error("identical accounts should be equal")
	}

	cases := []struct {
		name string
		b    AccountConfig
	}{
		{"headers added", AccountConfig{Name: "a", Key: "k", BaseURL: "https://x.com/v1", Provider: "p", Headers: map[string]string{"User-Agent": "ua"}}},
		{"header value changed", AccountConfig{Name: "a", Key: "k", BaseURL: "https://x.com/v1", Provider: "p", Headers: map[string]string{"User-Agent": "other"}}},
		{"probe_path changed", AccountConfig{Name: "a", Key: "k", BaseURL: "https://x.com/v1", Provider: "p", ProbePath: "/custom"}},
		{"auth_header changed", AccountConfig{Name: "a", Key: "k", BaseURL: "https://x.com/v1", Provider: "p", AuthHeader: "x-api-key"}},
		{"skip_pi_sync changed", AccountConfig{Name: "a", Key: "k", BaseURL: "https://x.com/v1", Provider: "p", SkipPISync: true}},
		{"provider changed", AccountConfig{Name: "a", Key: "k", BaseURL: "https://x.com/v1", Provider: "other"}},
	}
	for _, tc := range cases {
		if accountsEqual(base, []AccountConfig{tc.b}) {
			t.Errorf("accountsEqual should be false when %s", tc.name)
		}
	}
}

func TestLoadConfigDefaultProviderValid(t *testing.T) {
	content := `
providers:
  agentrouter-anthropic:
    accounts:
      - name: ant-1
        key: k
        base_url: https://api.example.com
      - name: ant-2
        key: k
        base_url: https://api.example.com
default_provider: agentrouter-anthropic
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
		t.Fatalf("LoadConfig with valid default_provider: %v", err)
	}
	if cfg.DefaultProvider != "agentrouter-anthropic" {
		t.Errorf("DefaultProvider = %q, want agentrouter-anthropic", cfg.DefaultProvider)
	}
	if len(cfg.Accounts) != 2 {
		t.Errorf("accounts = %d, want 2", len(cfg.Accounts))
	}
}

func TestLoadConfigDefaultProviderUnknown(t *testing.T) {
	content := `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
    provider: opencode-go
default_provider: no-such-provider
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
		t.Fatal("LoadConfig with unknown default_provider: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "default_provider") || !strings.Contains(err.Error(), "no-such-provider") {
		t.Errorf("error = %q, want it to mention default_provider no-such-provider", err)
	}
}

// TestLoadConfig_UsageDefaults: a config without a usage section must get the
// documented defaults (enabled=false → opt-in, db_path next to the model
// cache dir, tuned channel/batch/flush, 30-day retention).
func TestLoadConfig_UsageDefaults(t *testing.T) {
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
	if cfg.Usage.Enabled {
		t.Error("usage.enabled default must be false (opt-in: existing deployments unchanged)")
	}
	if cfg.Usage.DBPath != "/var/lib/prism/usage.db" {
		t.Errorf("usage.db_path default = %q, want /var/lib/prism/usage.db", cfg.Usage.DBPath)
	}
	if cfg.Usage.RetentionDays != 30 {
		t.Errorf("usage.retention_days default = %d, want 30", cfg.Usage.RetentionDays)
	}
	if cfg.Usage.ChannelSize != 4096 {
		t.Errorf("usage.channel_size default = %d, want 4096", cfg.Usage.ChannelSize)
	}
	if cfg.Usage.BatchSize != 50 {
		t.Errorf("usage.batch_size default = %d, want 50", cfg.Usage.BatchSize)
	}
	if cfg.Usage.BatchFlushMS != 200 {
		t.Errorf("usage.batch_flush_ms default = %d, want 200", cfg.Usage.BatchFlushMS)
	}
}

// TestLoadConfig_UsageExplicit: a populated usage section maps field-for-field
// onto UsageConfig (the wiring stage converts it to internal/usage.Config).
func TestLoadConfig_UsageExplicit(t *testing.T) {
	content := `
usage:
  enabled: true
  db_path: /tmp/custom-usage.db
  retention_days: 7
  channel_size: 512
  batch_size: 10
  batch_flush_ms: 50
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
	if !cfg.Usage.Enabled {
		t.Error("usage.enabled = false, want true")
	}
	if cfg.Usage.DBPath != "/tmp/custom-usage.db" {
		t.Errorf("usage.db_path = %q, want /tmp/custom-usage.db", cfg.Usage.DBPath)
	}
	if cfg.Usage.RetentionDays != 7 {
		t.Errorf("usage.retention_days = %d, want 7", cfg.Usage.RetentionDays)
	}
	if cfg.Usage.ChannelSize != 512 {
		t.Errorf("usage.channel_size = %d, want 512", cfg.Usage.ChannelSize)
	}
	if cfg.Usage.BatchSize != 10 {
		t.Errorf("usage.batch_size = %d, want 10", cfg.Usage.BatchSize)
	}
	if cfg.Usage.BatchFlushMS != 50 {
		t.Errorf("usage.batch_flush_ms = %d, want 50", cfg.Usage.BatchFlushMS)
	}
}

// TestLoadConfig_UsageRetentionDaysZeroExplicit: an EXPLICIT retention_days:
// 0 must be preserved (it means "keep forever, disable cleanup"), while an
// absent field defaults to 30. yaml zero values cannot distinguish the two
// on a plain int, which is why UsageConfig tracks explicit presence.
func TestLoadConfig_UsageRetentionDaysZeroExplicit(t *testing.T) {
	content := `
usage:
  enabled: true
  retention_days: 0
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	cfg := loadCfgString(t, content)
	if cfg.Usage.RetentionDays != 0 {
		t.Errorf("usage.retention_days = %d, want 0 (explicit 0 must be preserved)", cfg.Usage.RetentionDays)
	}
}

// TestLoadConfig_UsageDefaultKeyID: usage.default_key_id defaults to
// "anonymous" when absent, honors an explicit value, and an explicit empty
// string falls back to "anonymous" (an empty key_id would split one GROUP BY
// group into two and must never be configurable into existence).
func TestLoadConfig_UsageDefaultKeyID(t *testing.T) {
	t.Run("absent defaults to anonymous", func(t *testing.T) {
		content := `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		cfg := loadCfgString(t, content)
		if cfg.Usage.DefaultKeyID != "anonymous" {
			t.Errorf("usage.default_key_id default = %q, want anonymous", cfg.Usage.DefaultKeyID)
		}
	})

	t.Run("explicit value honored", func(t *testing.T) {
		content := `
usage:
  default_key_id: gateway-pi
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		cfg := loadCfgString(t, content)
		if cfg.Usage.DefaultKeyID != "gateway-pi" {
			t.Errorf("usage.default_key_id = %q, want gateway-pi", cfg.Usage.DefaultKeyID)
		}
	})

	t.Run("explicit empty string falls back to anonymous", func(t *testing.T) {
		content := `
usage:
  default_key_id: ""
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
		cfg := loadCfgString(t, content)
		if cfg.Usage.DefaultKeyID != "anonymous" {
			t.Errorf("usage.default_key_id with explicit empty = %q, want anonymous", cfg.Usage.DefaultKeyID)
		}
	})
}

// TestReloadConfig_UsageDBPathChangedWarning: db_path cannot be hot-reloaded
// (the store is opened once at startup) → ReloadConfig must warn.
func TestReloadConfig_UsageDBPathChangedWarning(t *testing.T) {
	content1 := `
usage:
  enabled: true
  db_path: /var/lib/prism/usage.db
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

	content2 := `
usage:
  enabled: true
  db_path: /var/lib/prism/other.db
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
		if strings.Contains(w, "usage.db_path") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about usage.db_path change, got: %v", warnings)
	}
}

// TestReloadConfig_UsageEnabledChangedWarning: toggling usage.enabled cannot
// take effect without restart (the recorder is built once at startup).
func TestReloadConfig_UsageEnabledChangedWarning(t *testing.T) {
	content1 := `
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

	content2 := `
usage:
  enabled: true
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
		if strings.Contains(w, "usage.enabled") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about usage.enabled change, got: %v", warnings)
	}
}

// TestReloadConfig_UsageUnchangedNoWarning: reloading an identical usage
// section must not produce restart warnings.
func TestReloadConfig_UsageUnchangedNoWarning(t *testing.T) {
	content1 := `
usage:
  enabled: true
  db_path: /var/lib/prism/usage.db
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

	content2 := `
usage:
  enabled: true
  db_path: /var/lib/prism/usage.db
  retention_days: 90
  batch_size: 123
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
	for _, w := range warnings {
		if strings.Contains(w, "usage") {
			t.Errorf("unexpected usage warning for tuning-knob change: %q", w)
		}
	}
}

// loadConfigFrom writes content to a temp file and runs LoadConfig on it.
func loadConfigFrom(t *testing.T, content string) (*Config, error) {
	t.Helper()
	f, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return LoadConfig(f.Name())
}

// TestLoadConfig_EmptyAPIKeyTokenRejected guards the empty/whitespace token
// hole: a key with an empty or whitespace-only token would let every
// "Bearer " request authenticate through the all-zero padded comparison.
func TestLoadConfig_EmptyAPIKeyTokenRejected(t *testing.T) {
	base := `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	for name, keys := range map[string]string{
		"empty token": `api_keys:
  - name: bad
    token: ""`,
		"whitespace": `api_keys:
  - name: bad
    token: "   "`,
		"whitespace auth_token": `auth_token: "  "`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfigFrom(t, base+"\n"+keys)
			if err == nil {
				t.Fatal("LoadConfig must reject an empty/whitespace-only api key token")
			}
			if !strings.Contains(err.Error(), "token is empty") {
				t.Errorf("error = %q, want it to mention the empty token", err)
			}
		})
	}
}

// TestLoadConfig_ValidAPIKeyTokenAccepted guards the happy path: a normal
// token still loads.
func TestLoadConfig_ValidAPIKeyTokenAccepted(t *testing.T) {
	content := `
api_keys:
  - name: ci-bot
    token: "sk-ci-111"
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	cfg, err := loadConfigFrom(t, content)
	if err != nil {
		t.Fatalf("LoadConfig with a valid key: %v", err)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Token != "sk-ci-111" {
		t.Fatalf("api_keys = %+v, want the single valid key", cfg.APIKeys)
	}
}

// TestLoadConfig_MaxUpstreamResponseBytes covers the max_upstream_response_bytes
// field: absent → default 32 MiB, explicit → honored, negative → load error.
// TestResolveFetchConcurrency pins the model-cache fetch concurrency rule:
// "*" wins when configured; otherwise the smallest positive per-model value
// is used (a fetch holds a slot on the same account as business requests, so
// it must respect every configured model cap); no positive values → the
// built-in default. Non-positive entries are ignored, never treated as a
// zero limit.
func TestResolveFetchConcurrency(t *testing.T) {
	defaultFallback := DeepseekV4ProConcurrency * DefaultConcurrencyRatio / 100
	tests := []struct {
		name string
		m    map[string]int
		want int
	}{
		{"nil map → default", nil, defaultFallback},
		{"empty map → default", map[string]int{}, defaultFallback},
		{"wildcard wins", map[string]int{"*": 3, "m-a": 1, "m-b": 2}, 3},
		{"specific-only → min of positives", map[string]int{"m-a": 5, "m-b": 2}, 2},
		{"min ignores non-positive entries", map[string]int{"m-a": 0, "m-b": -4, "m-c": 7}, 7},
		{"only non-positive entries → default", map[string]int{"m-a": 0, "m-b": -4}, defaultFallback},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{MaxConcurrentPerAccount: tc.m}
			if got := ResolveFetchConcurrency(cfg); got != tc.want {
				t.Errorf("ResolveFetchConcurrency(%v) = %d, want %d", tc.m, got, tc.want)
			}
		})
	}
	// nil config → default (matches ResolveMaxConcurrent's nil-safety).
	if got := ResolveFetchConcurrency(nil); got != defaultFallback {
		t.Errorf("ResolveFetchConcurrency(nil) = %d, want %d", got, defaultFallback)
	}
}

func TestLoadConfig_MaxUpstreamResponseBytes(t *testing.T) {
	base := `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`
	cfg, err := loadConfigFrom(t, base)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MaxUpstreamResponseBytes != MaxUpstreamResponseBytesDefault {
		t.Errorf("default max_upstream_response_bytes = %d, want %d", cfg.MaxUpstreamResponseBytes, MaxUpstreamResponseBytesDefault)
	}

	cfg, err = loadConfigFrom(t, base+"\nmax_upstream_response_bytes: 1048576\n")
	if err != nil {
		t.Fatalf("LoadConfig with explicit cap: %v", err)
	}
	if cfg.MaxUpstreamResponseBytes != 1048576 {
		t.Errorf("explicit max_upstream_response_bytes = %d, want 1048576", cfg.MaxUpstreamResponseBytes)
	}

	if _, err := loadConfigFrom(t, base+"\nmax_upstream_response_bytes: -1\n"); err == nil {
		t.Fatal("LoadConfig must reject a negative max_upstream_response_bytes")
	}

	// Values above the hard cap (256 MiB) are a load error: they would
	// defeat the memory bound and sit dangerously close to the int64
	// overflow of the read helper's max+1 probe.
	if _, err := loadConfigFrom(t, fmt.Sprintf("%s\nmax_upstream_response_bytes: %d\n", base, MaxUpstreamResponseBytesLimit+1)); err == nil {
		t.Fatal("LoadConfig must reject max_upstream_response_bytes above MaxUpstreamResponseBytesLimit")
	}

	// Exactly at the cap is accepted.
	cfg, err = loadConfigFrom(t, fmt.Sprintf("%s\nmax_upstream_response_bytes: %d\n", base, MaxUpstreamResponseBytesLimit))
	if err != nil {
		t.Fatalf("LoadConfig must accept exactly MaxUpstreamResponseBytesLimit: %v", err)
	}
	if cfg.MaxUpstreamResponseBytes != MaxUpstreamResponseBytesLimit {
		t.Errorf("max_upstream_response_bytes at cap = %d, want %d", cfg.MaxUpstreamResponseBytes, MaxUpstreamResponseBytesLimit)
	}
}
