package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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

// writeConfig writes content to a temp yaml file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

// TestLoadConfig_RejectsUnsafeProviderNames pins item 10 at the config
// stage: a provider name that would escape the model cache directory (path
// separators, "..", absolute paths) fails configuration loading early.
func TestLoadConfig_RejectsUnsafeProviderNames(t *testing.T) {
	bad := []string{"../evil", "a/b", "a\\b", "/abs", "..", "."}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, "accounts:\n  - name: t\n    key: k\n    base_url: https://api.example.com\n    provider: "+name+"\n")
			if _, err := LoadConfig(path); err == nil {
				t.Fatalf("provider %q must be rejected, got nil error", name)
			}
		})
	}
	// A normal provider name still loads.
	path := writeConfig(t, "accounts:\n  - name: t\n    key: k\n    base_url: https://api.example.com\n    provider: opencode-go\n")
	if _, err := LoadConfig(path); err != nil {
		t.Errorf("normal provider name must load: %v", err)
	}
	// A provider-less account is now a LOAD ERROR (audit round 6, item 3):
	// it can never be selected by business requests, so loading it silently
	// would hide a dead account. The error names the account and the fix.
	path = writeConfig(t, "accounts:\n  - name: t\n    key: k\n    base_url: https://api.example.com\n")
	if _, err := LoadConfig(path); err == nil {
		t.Error("provider-less account must be rejected at load (it can never be selected)")
	}
}

// TestLoadConfig_RejectsProviderlessAccount pins audit round 6 item 3: an
// account without a provider can never be selected (business requests route
// by provider via X-Prism-Provider/default_provider, model-cache fetches
// select by provider too), so loading must fail fast instead of silently
// keeping a dead account in the pool. The providers-block form and an
// explicit account.provider both load.
func TestLoadConfig_RejectsProviderlessAccount(t *testing.T) {
	// Bare top-level accounts (no provider field) → load error.
	path := writeConfig(t, "accounts:\n  - name: t\n    key: k\n    base_url: https://api.example.com\n")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("provider-less account must be rejected at load")
	}
	// Explicit account.provider → loads.
	path = writeConfig(t, "accounts:\n  - name: t\n    key: k\n    base_url: https://api.example.com\n    provider: p\n")
	if _, err := LoadConfig(path); err != nil {
		t.Errorf("explicit account.provider must load: %v", err)
	}
	// providers block → loads (provider is inherited from the block).
	path = writeConfig(t, "providers:\n  p:\n    accounts:\n      - name: t\n        key: k\n        base_url: https://api.example.com\n")
	if _, err := LoadConfig(path); err != nil {
		t.Errorf("providers-block account must load: %v", err)
	}
}

// TestLoadConfig_MaxConcurrentUpperBound pins item 11: a
// max_concurrent_per_account value above math.MaxInt32 would truncate in
// the pool's int32 concurrency accounting and must fail configuration
// loading clearly; the boundary value itself is accepted.
func TestLoadConfig_MaxConcurrentUpperBound(t *testing.T) {
	base := "accounts:\n  - name: t\n    key: k\n    base_url: https://api.example.com\n    provider: p\n"
	path := writeConfig(t, base+"max_concurrent_per_account:\n  gpt-4: 2147483648\n")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("value above math.MaxInt32 must be rejected, got nil error")
	}

	path = writeConfig(t, base+"max_concurrent_per_account:\n  gpt-4: 2147483647\n")
	if _, err := LoadConfig(path); err != nil {
		t.Errorf("math.MaxInt32 boundary must load: %v", err)
	}

	path = writeConfig(t, base+"max_concurrent_per_account:\n  gpt-4: 100\n")
	if _, err := LoadConfig(path); err != nil {
		t.Errorf("a normal value must load: %v", err)
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

// TestResolveAccountTotalCap pins the account-wide aggregate cap
// resolution (the backward-compatible default): an explicit
// max_concurrent_per_account_total wins; otherwise the SUM of the distinct
// positive per-model values reproduces the old per-max-value-grouped worst
// case (same-max models share the aggregate budget, never a counter); with
// no positive per-model values the built-in default applies; the sum is
// clamped to math.MaxInt32 (int32 accounting).
func TestResolveAccountTotalCap(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want int
	}{
		{"explicit wins",
			&Config{MaxConcurrentPerAccountTotal: 20, MaxConcurrentPerAccount: map[string]int{"a": 10, "b": 10}},
			20},
		{"explicit 0 falls back to default",
			&Config{MaxConcurrentPerAccountTotal: 0, MaxConcurrentPerAccount: map[string]int{"a": 10, "b": 10}},
			10}, // distinct values {10} → 10, not 20: same-max models share the total
		{"sum of distinct values",
			&Config{MaxConcurrentPerAccount: map[string]int{"a": 10, "b": 10, "c": 5, "d": 0, "e": -1}},
			15}, // distinct positive {10,5} → 15 (0/negative ignored)
		{"wildcard only",
			&Config{MaxConcurrentPerAccount: map[string]int{"*": 8}},
			8},
		{"no entries", &Config{}, DefaultAccountConcurrency},
		{"nil map", nil, DefaultAccountConcurrency},
		{"clamped to int32",
			&Config{MaxConcurrentPerAccount: map[string]int{"a": math.MaxInt32, "b": math.MaxInt32}},
			math.MaxInt32},
	}
	for _, tc := range cases {
		if got := ResolveAccountTotalCap(tc.cfg); got != tc.want {
			t.Errorf("%s: ResolveAccountTotalCap = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestLoadConfig_RejectsBadAccountTotalCap: max_concurrent_per_account_total
// is validated at load like the per-model entries — negative values are
// always a typo and values above math.MaxInt32 would truncate in the pool's
// int32 total counter.
func TestLoadConfig_RejectsBadAccountTotalCap(t *testing.T) {
	base := "accounts:\n  - name: t\n    key: k\n    base_url: https://api.example.com\n    provider: p\n"
	if _, err := LoadConfig(writeConfig(t, base+"max_concurrent_per_account_total: -1\n")); err == nil {
		t.Error("negative max_concurrent_per_account_total must be rejected")
	}
	if _, err := LoadConfig(writeConfig(t, base+"max_concurrent_per_account_total: 2147483648\n")); err == nil {
		t.Error("max_concurrent_per_account_total above math.MaxInt32 must be rejected")
	}
	cfg, err := LoadConfig(writeConfig(t, base+"max_concurrent_per_account_total: 12\n"))
	if err != nil {
		t.Fatalf("valid max_concurrent_per_account_total must load: %v", err)
	}
	if cfg.MaxConcurrentPerAccountTotal != 12 {
		t.Errorf("MaxConcurrentPerAccountTotal = %d, want 12", cfg.MaxConcurrentPerAccountTotal)
	}
}

// TestReloadConfig_RejectsProviderlessAndWholeNet pins that the empty-
// provider and whole-address-space-trust validations run on the RELOAD path
// too (ReloadConfig loads through LoadConfig): a reload with a
// provider-less account or a 0.0.0.0/0 trusted_proxies entry fails and the
// OLD config is preserved.
func TestReloadConfig_RejectsProviderlessAndWholeNet(t *testing.T) {
	content1 := `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
    provider: test
`
	path := writeConfig(t, content1)
	cfg1, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	holder := NewConfigHolder(cfg1)

	for name, content2 := range map[string]string{
		"provider-less account": `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
`, // no provider → load error on reload
		"whole-address-space trust": `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
    provider: test
trusted_proxies:
  - "0.0.0.0/0"
`, // /0 → load error on reload
	} {
		if err := os.WriteFile(path, []byte(content2), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReloadConfig(holder, path); err == nil {
			t.Errorf("%s: reload must fail", name)
		}
		if got := holder.Load(); got == nil || len(got.Accounts) != 1 || got.Accounts[0].Provider != "test" {
			t.Errorf("%s: old config must be preserved after a failed reload", name)
		}
		if got := holder.Load(); got != nil && len(got.TrustedProxies) != 0 {
			t.Errorf("%s: old config must keep its trusted_proxies", name)
		}
	}
}

// TestReloadConfig_TotalCapChangedWarning: max_concurrent_per_account_total
// is a pool-construction property (the pool is built once at startup), so a
// reload that changes it must warn that a restart is required.
func TestReloadConfig_TotalCapChangedWarning(t *testing.T) {
	content1 := `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
    provider: test
max_concurrent_per_account_total: 8
`
	path := writeConfig(t, content1)
	cfg1, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	holder := NewConfigHolder(cfg1)

	content2 := `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
    provider: test
max_concurrent_per_account_total: 16
`
	if err := os.WriteFile(path, []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}
	warnings, err := ReloadConfig(holder, path)
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "max_concurrent_per_account_total") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want a max_concurrent_per_account_total restart warning", warnings)
	}
	if got := holder.Load().MaxConcurrentPerAccountTotal; got != 16 {
		t.Errorf("reloaded MaxConcurrentPerAccountTotal = %d, want 16", got)
	}
}

// TestParseTrustedProxiesRejectsWholeNet pins the audit round 6 item 8
// rule: 0.0.0.0/0 and ::/0 ("trust every network") are rejected by the
// parser itself, while bounded IPv4 and IPv6 CIDRs still parse. The error
// text explains the ACTUAL degradation (all hops trusted → the real client
// cannot be located → rate limiting degrades to RemoteAddr/proxy-address
// aggregation), never a claim that arbitrary X-Forwarded-For hops bypass
// the limiter.
func TestParseTrustedProxiesRejectsWholeNet(t *testing.T) {
	for _, bad := range []string{"0.0.0.0/0", "::/0"} {
		_, err := ParseTrustedProxies([]string{bad})
		if err == nil {
			t.Errorf("ParseTrustedProxies(%q) must be rejected (whole-address-space trust)", bad)
			continue
		}
		if !strings.Contains(err.Error(), "real client cannot be located") {
			t.Errorf("ParseTrustedProxies(%q) error = %q, want it to explain the degradation (all hops trusted → no client-IP resolution)", bad, err.Error())
		}
	}
	for _, good := range []string{"10.0.0.0/8", "192.168.1.0/24", "fd00::/8", "2001:db8::/32"} {
		if _, err := ParseTrustedProxies([]string{good}); err != nil {
			t.Errorf("ParseTrustedProxies(%q) = %v, want nil (bounded CIDR must parse)", good, err)
		}
	}
}

// TestLoadConfig_RejectsWholeNetTrustedProxies pins the same rule at the
// config-load stage (fail fast): a trusted_proxies entry covering the whole
// address space makes every address "trusted", so the XFF chain walk finds
// no untrusted client hop and client-IP resolution collapses to RemoteAddr
// — per-client rate limiting silently degrades to keying on the proxy's
// address for all proxied traffic — and X-Real-IP never rescues it (it is
// only accepted when UNtrusted). Loading must fail instead of silently
// trusting every network. The error text describes this degradation, not a
// claim that arbitrary XFF can bypass the limiter. Bounded IPv4/IPv6 ranges
// still load.
func TestLoadConfig_RejectsWholeNetTrustedProxies(t *testing.T) {
	base := "accounts:\n  - name: t\n    key: k\n    base_url: https://api.example.com\n    provider: p\n"
	for _, bad := range []string{"0.0.0.0/0", "::/0"} {
		path := writeConfig(t, base+"trusted_proxies:\n  - \""+bad+"\"\n")
		_, err := LoadConfig(path)
		if err == nil {
			t.Errorf("trusted_proxies %q must fail loading (whole-address-space trust)", bad)
			continue
		}
		if !strings.Contains(err.Error(), "real client cannot be located") {
			t.Errorf("trusted_proxies %q error = %q, want it to explain the degradation (all hops trusted → no client-IP resolution)", bad, err.Error())
		}
	}
	for _, good := range []string{"10.0.0.0/8", "192.168.1.0/24", "fd00::/8", "2001:db8::/32"} {
		path := writeConfig(t, base+"trusted_proxies:\n  - \""+good+"\"\n")
		if _, err := LoadConfig(path); err != nil {
			t.Errorf("trusted_proxies %q must load: %v", good, err)
		}
	}
}

func TestLoadConfigProbeIntervalTooSmall(t *testing.T) {
	content := `
probe_interval: 500ms
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
allow_insecure_http: true
auth_token: my-secret-token
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
  - name: acc2
    key: key2
    base_url: https://api2.example.com
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
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
    provider: test
`
	cfg, err := loadConfigFrom(t, content)
	if err != nil {
		t.Fatalf("LoadConfig with a valid key: %v", err)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Token != "sk-ci-111" {
		t.Fatalf("api_keys = %+v, want the single valid key", cfg.APIKeys)
	}
}

// TestLoadConfig_APIKeyNameValidation pins the MCP identity isolation
// contract: API key NAMEs are the MCP tool-cache identity, so empty names,
// duplicate names and the reserved identities (McpAdminIdentity — the
// shared admin-injected bucket — and McpUnauthenticatedIdentity — the
// read-only bucket for requests without a credential) are explicit load
// errors. The legacy auth_token expansion and an explicit name "default"
// remain valid — "default" is a plain per-client bucket, NOT a reserved
// identity (only McpAdminIdentity and McpUnauthenticatedIdentity are
// forbidden). The errors must name the key but never echo any token.
func TestLoadConfig_APIKeyNameValidation(t *testing.T) {
	base := `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
    provider: test
`
	t.Run("empty name rejected", func(t *testing.T) {
		_, err := loadConfigFrom(t, base+`
api_keys:
  - name: ""
    token: "sk-1"`)
		if err == nil {
			t.Fatal("an empty api key name must be rejected")
		}
		if !strings.Contains(err.Error(), "name is empty") {
			t.Errorf("error = %q, want it to mention the empty name", err)
		}
		if strings.Contains(err.Error(), "sk-1") {
			t.Error("error must never echo the token")
		}
	})
	t.Run("duplicate names rejected", func(t *testing.T) {
		_, err := loadConfigFrom(t, base+`
api_keys:
  - name: dup
    token: "sk-1"
  - name: dup
    token: "sk-2"`)
		if err == nil {
			t.Fatal("duplicate api key names must be rejected (they would share one MCP cache identity)")
		}
		if !strings.Contains(err.Error(), "duplicate key name") {
			t.Errorf("error = %q, want it to mention the duplicate name", err)
		}
		if strings.Contains(err.Error(), "sk-1") || strings.Contains(err.Error(), "sk-2") {
			t.Error("error must never echo any token")
		}
	})
	t.Run("reserved admin identity rejected", func(t *testing.T) {
		_, err := loadConfigFrom(t, base+`
api_keys:
  - name: `+McpAdminIdentity+`
    token: "sk-1"`)
		if err == nil {
			t.Fatalf("the reserved admin identity %q must be rejected as a key name", McpAdminIdentity)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("error = %q, want it to mention the reserved name", err)
		}
		if strings.Contains(err.Error(), "sk-1") {
			t.Error("error must never echo the token")
		}
	})
	t.Run("reserved unauthenticated identity rejected", func(t *testing.T) {
		// The unauthenticated MCP bucket identity must be unusable as an api
		// key name: an authenticated key named like it would silently lose
		// its own MCP tool cache and be indistinguishable from an
		// unauthenticated request. The literal is pinned so a rename of the
		// shared constant breaks this test instead of silently diverging.
		if McpUnauthenticatedIdentity != "unauthenticated" {
			t.Fatalf("McpUnauthenticatedIdentity = %q, want the pinned literal %q", McpUnauthenticatedIdentity, "unauthenticated")
		}
		_, err := loadConfigFrom(t, base+`
api_keys:
  - name: `+McpUnauthenticatedIdentity+`
    token: "sk-1"`)
		if err == nil {
			t.Fatalf("the reserved unauthenticated identity %q must be rejected as a key name", McpUnauthenticatedIdentity)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("error = %q, want it to mention the reserved name", err)
		}
		if strings.Contains(err.Error(), "sk-1") {
			t.Error("error must never echo the token")
		}
	})
	t.Run("legacy auth_token still expands to name default", func(t *testing.T) {
		cfg, err := loadConfigFrom(t, base+`
auth_token: "legacy-secret"`)
		if err != nil {
			t.Fatalf("legacy auth_token must still load: %v", err)
		}
		if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Name != "default" || cfg.APIKeys[0].Token != "legacy-secret" {
			t.Fatalf("auth_token expansion = %+v, want [{default legacy-secret}]", cfg.APIKeys)
		}
	})
	t.Run("explicit name default is a plain client bucket", func(t *testing.T) {
		if _, err := loadConfigFrom(t, base+`
api_keys:
  - name: default
    token: "sk-1"`); err != nil {
			t.Errorf("explicit name \"default\" must load (it is not the reserved admin identity): %v", err)
		}
	})
	t.Run("distinct names still load", func(t *testing.T) {
		if _, err := loadConfigFrom(t, base+`
api_keys:
  - name: ci-bot
    token: "sk-1"
  - name: ci-bot-2
    token: "sk-2"`); err != nil {
			t.Errorf("distinct key names must load: %v", err)
		}
	})
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
	defaultFallback := DefaultAccountConcurrency
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

// TestResolveMaxConcurrent_ConservativeDefault pins the conservative built-in
// default: without an explicit max_concurrent_per_account entry the cap is
// DefaultAccountConcurrency regardless of the model name — the old
// "flash"/"pro" substring heuristics that guessed a DeepSeek tier from an
// arbitrary model name are gone.
func TestResolveMaxConcurrent_ConservativeDefault(t *testing.T) {
	if got := ResolveMaxConcurrent("gpt-5.5-pro", nil); got != DefaultAccountConcurrency {
		t.Errorf("model name containing 'pro' must NOT raise the default: got %d, want %d", got, DefaultAccountConcurrency)
	}
	if got := ResolveMaxConcurrent("deepseek-v4-flash", nil); got != DefaultAccountConcurrency {
		t.Errorf("model name containing 'flash' must NOT raise the default: got %d, want %d", got, DefaultAccountConcurrency)
	}
	if got := ResolveMaxConcurrent("", nil); got != DefaultAccountConcurrency {
		t.Errorf("empty model must use the conservative default: got %d, want %d", got, DefaultAccountConcurrency)
	}
	if got := ResolveMaxConcurrent("any-model", &Config{}); got != DefaultAccountConcurrency {
		t.Errorf("config without entries must use the conservative default: got %d, want %d", got, DefaultAccountConcurrency)
	}
}

// TestResolveMaxConcurrent_ConfigStillWins pins the priority of the existing
// configuration over the built-in default: exact model match first, then the
// "*" wildcard.
func TestResolveMaxConcurrent_ConfigStillWins(t *testing.T) {
	cfg := &Config{MaxConcurrentPerAccount: map[string]int{"gpt-5.5-pro": 2, "*": 5}}
	if got := ResolveMaxConcurrent("gpt-5.5-pro", cfg); got != 2 {
		t.Errorf("exact match = %d, want 2", got)
	}
	if got := ResolveMaxConcurrent("some-other-model", cfg); got != 5 {
		t.Errorf("wildcard = %d, want 5", got)
	}
}

func TestLoadConfig_MaxUpstreamResponseBytes(t *testing.T) {
	base := `
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
    provider: test
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

// TestLoadConfig_BaseURLValidation pins the startup base_url check: every
// account must have an absolute http(s) URL with a non-empty host. Invalid
// schemes, missing hosts and unparseable URLs are load errors; the error
// names the account but never echoes the URL (a base_url may embed
// credentials).
func TestLoadConfig_BaseURLValidation(t *testing.T) {
	keyLine := "    key: test-key-12345\n"
	t.Run("valid https accepted", func(t *testing.T) {
		content := "accounts:\n  - name: a\n" + keyLine + "    base_url: https://api.example.com/v1\n    provider: test\n"
		if _, err := loadConfigFrom(t, content); err != nil {
			t.Fatalf("https base_url must load: %v", err)
		}
	})
	t.Run("valid http accepted", func(t *testing.T) {
		content := "accounts:\n  - name: a\n" + keyLine + "    base_url: http://127.0.0.1:8080\n    provider: test\n"
		if _, err := loadConfigFrom(t, content); err != nil {
			t.Fatalf("http base_url must load: %v", err)
		}
	})
	t.Run("non-http scheme rejected", func(t *testing.T) {
		content := "accounts:\n  - name: a\n" + keyLine + "    base_url: ftp://files.example.com/v1\n    provider: test\n"
		_, err := loadConfigFrom(t, content)
		if err == nil {
			t.Fatal("a non-http(s) base_url must be rejected")
		}
		if !strings.Contains(err.Error(), "a") || strings.Contains(err.Error(), "ftp://") {
			t.Errorf("error must name the account but never echo the URL, got: %q", err.Error())
		}
	})
	t.Run("missing scheme rejected", func(t *testing.T) {
		content := "accounts:\n  - name: a\n" + keyLine + "    base_url: api.example.com/v1\n    provider: test\n"
		if _, err := loadConfigFrom(t, content); err == nil {
			t.Fatal("a base_url without a scheme must be rejected")
		}
	})
	t.Run("empty host rejected", func(t *testing.T) {
		content := "accounts:\n  - name: a\n" + keyLine + "    base_url: https:///v1\n    provider: test\n"
		if _, err := loadConfigFrom(t, content); err == nil {
			t.Fatal("a base_url without a host must be rejected")
		}
	})
	t.Run("unparseable rejected", func(t *testing.T) {
		content := "accounts:\n  - name: a\n" + keyLine + "    base_url: '://bad url with spaces'\n    provider: test\n"
		if _, err := loadConfigFrom(t, content); err == nil {
			t.Fatal("an unparseable base_url must be rejected")
		}
	})
	t.Run("query string rejected", func(t *testing.T) {
		content := "accounts:\n  - name: a\n" + keyLine + "    base_url: 'https://api.example.com/v1?foo=bar'\n    provider: test\n"
		_, err := loadConfigFrom(t, content)
		if err == nil {
			t.Fatal("a base_url with a query string must be rejected")
		}
		if strings.Contains(err.Error(), "foo=bar") {
			t.Errorf("error must never echo the query (it may embed credentials), got: %q", err.Error())
		}
	})
	t.Run("bare trailing question mark rejected", func(t *testing.T) {
		// url.Parse accepts "https://host/v1?" with ForceQuery=true and an
		// empty RawQuery: it corrupts the join exactly like a real query.
		content := "accounts:\n  - name: a\n" + keyLine + "    base_url: 'https://api.example.com/v1?'\n    provider: test\n"
		if _, err := loadConfigFrom(t, content); err == nil {
			t.Fatal("a base_url with a bare trailing '?' must be rejected")
		}
	})
	t.Run("fragment rejected", func(t *testing.T) {
		content := "accounts:\n  - name: a\n" + keyLine + "    base_url: 'https://api.example.com/v1#frag'\n    provider: test\n"
		_, err := loadConfigFrom(t, content)
		if err == nil {
			t.Fatal("a base_url with a fragment must be rejected")
		}
		if strings.Contains(err.Error(), "api.example.com") {
			t.Errorf("error must never echo the URL value, got: %q", err.Error())
		}
	})
	t.Run("query-carrying credentials never echoed", func(t *testing.T) {
		// A query string may embed credentials (?key=secret): the rejection
		// must not leak them into the error.
		content := "accounts:\n  - name: a\n" + keyLine + "    base_url: 'https://api.example.com/v1?key=supersecret'\n    provider: test\n"
		_, err := loadConfigFrom(t, content)
		if err == nil {
			t.Fatal("a base_url with a query string must be rejected")
		}
		if strings.Contains(err.Error(), "supersecret") {
			t.Errorf("error must never echo query credentials, got: %q", err.Error())
		}
	})
	t.Run("credential-carrying URL never echoed", func(t *testing.T) {
		// A base_url embedding credentials must be rejected on the scheme
		// (here "file") without the credentials reaching the error.
		content := "accounts:\n  - name: a\n" + keyLine + "    base_url: file://user:supersecret@/etc/passwd\n    provider: test\n"
		_, err := loadConfigFrom(t, content)
		if err == nil {
			t.Fatal("a non-http(s) base_url must be rejected")
		}
		if strings.Contains(err.Error(), "supersecret") {
			t.Errorf("error must never echo credentials, got: %q", err.Error())
		}
	})
}

// TestLoadConfig_NonLoopbackTLSRequiresOptIn pins the non-loopback TLS
// gate: a non-loopback listener must either have a COMPLETE TLS
// configuration (both files, or both via the PRISM_TLS_CERT/PRISM_TLS_KEY
// env fallbacks — the env fallback runs BEFORE this check) or the explicit
// allow_insecure_http: true opt-in. trusted_proxies does NOT relax the
// check (it cannot prevent direct access to the listener). Exactly one of
// cert/key is a load error regardless of allow_insecure_http. Loopback
// listeners stay TLS-free (local development).
func TestLoadConfig_NonLoopbackTLSRequiresOptIn(t *testing.T) {
	base := `
api_keys:
  - name: ci-bot
    token: sk-ci-111
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
    provider: test
`

	t.Run("loopback without TLS loads", func(t *testing.T) {
		if _, err := loadConfigFrom(t, "listen: 127.0.0.1:18790\n"+base); err != nil {
			t.Fatalf("loopback listen without TLS must load (development): %v", err)
		}
	})
	t.Run("non-loopback with both TLS files loads", func(t *testing.T) {
		if _, err := loadConfigFrom(t, "listen: 0.0.0.0:8080\n"+base+"tls_cert_file: /c.pem\ntls_key_file: /k.pem\n"); err != nil {
			t.Fatalf("non-loopback with complete TLS must load: %v", err)
		}
	})
	t.Run("non-loopback with TLS from env vars loads", func(t *testing.T) {
		// The env fallback runs BEFORE the completeness check: env-provided
		// cert+key must count as a complete TLS configuration.
		t.Setenv("PRISM_TLS_CERT", "/env-c.pem")
		t.Setenv("PRISM_TLS_KEY", "/env-k.pem")
		cfg, err := loadConfigFrom(t, "listen: 0.0.0.0:8080\n"+base)
		if err != nil {
			t.Fatalf("non-loopback with env-provided TLS must load: %v", err)
		}
		if cfg.TLSCertFile != "/env-c.pem" || cfg.TLSKeyFile != "/env-k.pem" {
			t.Errorf("env TLS fallback not applied: cert=%q key=%q", cfg.TLSCertFile, cfg.TLSKeyFile)
		}
	})
	t.Run("non-loopback without TLS and without opt-in rejected", func(t *testing.T) {
		t.Setenv("PRISM_TLS_CERT", "")
		t.Setenv("PRISM_TLS_KEY", "")
		_, err := loadConfigFrom(t, "listen: 0.0.0.0:8080\n"+base)
		if err == nil {
			t.Fatal("non-loopback without TLS must fail loading without allow_insecure_http")
		}
		if !strings.Contains(err.Error(), "allow_insecure_http") {
			t.Errorf("error = %q, want it to name the allow_insecure_http opt-in", err)
		}
	})
	t.Run("trusted_proxies does not relax the TLS check", func(t *testing.T) {
		// trusted_proxies only governs X-Forwarded-For trust; it cannot
		// prevent direct access to the listener, so it must NOT count as
		// TLS safety.
		t.Setenv("PRISM_TLS_CERT", "")
		t.Setenv("PRISM_TLS_KEY", "")
		_, err := loadConfigFrom(t, "listen: 0.0.0.0:8080\n"+base+"trusted_proxies:\n  - 127.0.0.1/32\n")
		if err == nil {
			t.Fatal("trusted_proxies without TLS must still fail loading")
		}
		if !strings.Contains(err.Error(), "allow_insecure_http") {
			t.Errorf("error = %q, want it to name the allow_insecure_http opt-in", err)
		}
	})
	t.Run("non-loopback without TLS with explicit opt-in loads", func(t *testing.T) {
		cfg, err := loadConfigFrom(t, "listen: 0.0.0.0:8080\n"+base+"allow_insecure_http: true\n")
		if err != nil {
			t.Fatalf("non-loopback with allow_insecure_http: true must load: %v", err)
		}
		if !cfg.AllowInsecureHTTP {
			t.Error("AllowInsecureHTTP must be true after load")
		}
	})
	t.Run("cert without key rejected even with opt-in", func(t *testing.T) {
		t.Setenv("PRISM_TLS_CERT", "")
		t.Setenv("PRISM_TLS_KEY", "")
		_, err := loadConfigFrom(t, "listen: 0.0.0.0:8080\n"+base+"allow_insecure_http: true\ntls_cert_file: /c.pem\n")
		if err == nil {
			t.Fatal("cert without key must fail loading regardless of allow_insecure_http")
		}
		if !strings.Contains(err.Error(), "incomplete TLS") {
			t.Errorf("error = %q, want it to mention the incomplete TLS configuration", err)
		}
	})
	t.Run("key without cert rejected even with opt-in", func(t *testing.T) {
		t.Setenv("PRISM_TLS_CERT", "")
		t.Setenv("PRISM_TLS_KEY", "")
		_, err := loadConfigFrom(t, "listen: 0.0.0.0:8080\n"+base+"allow_insecure_http: true\ntls_key_file: /k.pem\n")
		if err == nil {
			t.Fatal("key without cert must fail loading regardless of allow_insecure_http")
		}
		if !strings.Contains(err.Error(), "incomplete TLS") {
			t.Errorf("error = %q, want it to mention the incomplete TLS configuration", err)
		}
	})
	t.Run("incomplete TLS rejected without opt-in too", func(t *testing.T) {
		t.Setenv("PRISM_TLS_CERT", "")
		t.Setenv("PRISM_TLS_KEY", "")
		if _, err := loadConfigFrom(t, "listen: 0.0.0.0:8080\n"+base+"tls_key_file: /k.pem\n"); err == nil {
			t.Fatal("key without cert must fail loading even without allow_insecure_http")
		}
	})
}

// TestReloadConfig_AccountsKept pins the holder/pool consistency rule:
// when a reload changes accounts, the running accounts configuration is
// KEPT (the pool is built once at startup — publishing different accounts
// would split the holder from the pool) with a clear warning, while other
// fields still hot-reload.
func TestReloadConfig_AccountsKept(t *testing.T) {
	content1 := `
accounts:
  - name: acc1
    key: key1
    base_url: https://api1.example.com
    provider: test
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

	// Change accounts (add acc2) AND a hot-reloadable field (model_tiers).
	content2 := `
model_tiers:
  tier1: upstream-b
accounts:
  - name: acc1
    key: key1
    base_url: https://api1.example.com
    provider: test
  - name: acc2
    key: key2
    base_url: https://api2.example.com
    provider: test
`
	if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}

	warnings, err := ReloadConfig(holder, f.Name())
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	cur := holder.Load()

	// The held accounts MUST equal the RUNNING (old) accounts — never the
	// new ones (holder/pool split).
	if len(cur.Accounts) != 1 || cur.Accounts[0].Name != "acc1" || cur.Accounts[0].BaseURL != "https://api1.example.com" {
		t.Errorf("held accounts = %+v, want the running [acc1] (accounts must be kept on reload)", cur.Accounts)
	}
	// The warning must be explicit.
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "keeping the running accounts") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a keeping-accounts warning, got: %v", warnings)
	}
	// Other fields still hot-reload.
	if cur.ModelTiers["tier1"] != "upstream-b" {
		t.Errorf("model_tiers must still hot-reload, got %v", cur.ModelTiers)
	}
}

// TestReloadConfig_AccountsKept_ProviderSchemaConsistent pins that the
// provider → effort schema map is rebuilt from the KEPT accounts, so
// EffortSchema stays consistent with the running pool after an accounts
// change.
func TestReloadConfig_AccountsKept_ProviderSchemaConsistent(t *testing.T) {
	content1 := `
accounts:
  - name: acc1
    key: key1
    base_url: https://ollama.com/v1
    provider: ollama-p
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
	if got := holder.Load().EffortSchema("ollama-p"); got != "ollama" {
		t.Fatalf("initial EffortSchema = %q, want ollama", got)
	}

	// New config: same provider name but a non-ollama host (would resolve to
	// the opencode schema "").
	content2 := `
accounts:
  - name: acc1
    key: key1
    base_url: https://api.example.com/v1
    provider: ollama-p
`
	if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReloadConfig(holder, f.Name()); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	// Accounts were kept → the schema must stay ollama (derived from the
	// KEPT base_url), not flip to the discarded new host.
	if got := holder.Load().EffortSchema("ollama-p"); got != "ollama" {
		t.Errorf("EffortSchema after accounts-changing reload = %q, want ollama (schema must follow the kept accounts)", got)
	}
}

// TestReloadConfig_AccountsKept_DefaultProviderConsistent pins that a new
// default_provider referencing a provider that only exists in the DISCARDED
// new accounts is rolled back to the previous default_provider with a
// warning (a dangling default would route header-less requests into
// no_healthy).
func TestReloadConfig_AccountsKept_DefaultProviderConsistent(t *testing.T) {
	content1 := `
accounts:
  - name: acc1
    key: key1
    base_url: https://api1.example.com
    provider: prov-a
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

	// New config: default_provider prov-b exists only in the new accounts
	// (which are discarded).
	content2 := `
default_provider: prov-b
accounts:
  - name: acc1
    key: key1
    base_url: https://api1.example.com
    provider: prov-a
  - name: acc2
    key: key2
    base_url: https://api2.example.com
    provider: prov-b
`
	if err := os.WriteFile(f.Name(), []byte(content2), 0644); err != nil {
		t.Fatal(err)
	}
	warnings, err := ReloadConfig(holder, f.Name())
	if err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	cur := holder.Load()
	if cur.DefaultProvider != "" {
		t.Errorf("default_provider = %q, want the previous value (empty — prov-b is not among the kept accounts)", cur.DefaultProvider)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "default_provider") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a default_provider rollback warning, got: %v", warnings)
	}
}

// TestIsOllamaHost pins the ollama-host detection: only the exact
// ollama.com domain and its subdomains qualify (case-insensitive, port
// stripped); suffix-matching lookalikes (evilollama.com, ollama.com.evil.example)
// and every other host are rejected.
func TestIsOllamaHost(t *testing.T) {
	tests := []struct {
		baseURL string
		want    bool
	}{
		{"https://ollama.com/v1", true},
		{"https://ollama.com", true},
		{"https://ollama.com:8443/v1", true},
		{"https://sub.ollama.com/v1", true},
		{"https://a.b.ollama.com/v1", true},
		{"https://OLLAMA.COM/v1", true},      // case-normalized
		{"https://Ollama.Com/V1", true},      // case-normalized
		{"https://SUB.OLLAMA.COM/v1", true},  // case-normalized subdomain
		{"http://ollama.com/v1", true},       // http scheme is fine
		{"https://evilollama.com/v1", false}, // suffix lookalike
		{"https://ollama.com.evil.example/v1", false},
		{"https://notollama.com/v1", false},
		{"https://ollama.com.evil.com/v1", false},
		{"https://opencode.ai/zen/v1", false},
		{"https://openai.com/v1", false},
		{"not a url", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isOllamaHost(tc.baseURL); got != tc.want {
			t.Errorf("isOllamaHost(%q) = %v, want %v", tc.baseURL, got, tc.want)
		}
	}
}

// TestIsLoopbackListen pins the listen-loopback classification: loopback
// IPs and the localhost hostname (any case) are loopback; empty hosts,
// wildcard binds, other hostnames and unparseable addresses are not.
func TestIsLoopbackListen(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:18790", true},
		{"127.0.0.1:0", true},
		{"[::1]:18790", true},
		{"localhost:18790", true},
		{"LOCALHOST:18790", true},
		{"LocalHost:18790", true},
		{"localhost", false}, // no port: SplitHostPort fails
		{"", false},
		{":18790", false},        // empty host
		{"0.0.0.0:18790", false}, // wildcard IPv4
		{"[::]:18790", false},    // wildcard IPv6
		{"0.0.0.0", false},       // no port
		{"192.168.1.1:18790", false},
		{"10.0.0.1:8080", false},
		{"example.com:18790", false},
		{"garbage", false},
	}
	for _, tc := range tests {
		if got := IsLoopbackListen(tc.addr); got != tc.want {
			t.Errorf("IsLoopbackListen(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestLoadConfig_LocalhostListenIsLoopback verifies the localhost
// hostname (case-insensitive) counts as a loopback listen for the
// auth/TLS gates: a config listening on LOCALHOST without api_keys and
// without TLS loads fine (loopback listeners stay credential-free).
func TestLoadConfig_LocalhostListenIsLoopback(t *testing.T) {
	content := `
listen: "LOCALHOST:18790"
accounts:
  - name: test-acc
    key: test-key-12345
    base_url: https://api.example.com
    provider: test
`
	cfg, err := loadConfigFrom(t, content)
	if err != nil {
		t.Fatalf("LoadConfig with LOCALHOST listen must succeed (localhost is loopback): %v", err)
	}
	if cfg.Listen != "LOCALHOST:18790" {
		t.Errorf("listen = %q, want the configured value preserved", cfg.Listen)
	}
}

// TestValidateAccountName pins the account-name rule (shared by LoadConfig
// and `prism setup`): non-empty, ASCII alnum start, charset [A-Za-z0-9_-],
// at most MaxAccountNameLen bytes.
func TestValidateAccountName(t *testing.T) {
	valid := []string{
		"a", "1", "account-1", "agentrouter_oai_1", "A-B_c9",
		"go-plan-1", strings.Repeat("a", MaxAccountNameLen),
	}
	for _, name := range valid {
		if err := ValidateAccountName(name); err != nil {
			t.Errorf("ValidateAccountName(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{
		"",
		"-lead", // starts with '-'
		"_lead", // starts with '_'
		".dot",  // starts with '.'
		"with space",
		"with.dot",   // dots are the expvar hierarchy separator
		"with/slash", // path separator (cache/credstore file names)
		"with\\backslash",
		"..", // path traversal
		"中文",
		"emoji😀",
		"a\u0000b",                               // NUL byte
		strings.Repeat("a", MaxAccountNameLen+1), // too long
	}
	for _, name := range invalid {
		if err := ValidateAccountName(name); err == nil {
			t.Errorf("ValidateAccountName(%q) = nil, want an error", name)
		}
	}
}

// TestLoadConfig_AccountNameValidation pins the load-time account-name
// gates: invalid names, duplicate names and names that fold to the same
// LB_KEY_* credential name are all rejected.
func TestLoadConfig_AccountNameValidation(t *testing.T) {
	base := `
accounts:
  - name: %s
    key: test-key-12345
    base_url: https://api.example.com
    provider: test
`
	t.Run("invalid name rejected", func(t *testing.T) {
		_, err := loadConfigFrom(t, fmt.Sprintf(base, "bad name"))
		if err == nil {
			t.Fatal("an account name with a space must be rejected")
		}
		if !strings.Contains(err.Error(), "account") {
			t.Errorf("error = %q, want it to name the account", err)
		}
	})
	t.Run("duplicate names rejected", func(t *testing.T) {
		_, err := loadConfigFrom(t, `
accounts:
  - name: dup
    key: test-key-1
    base_url: https://api1.example.com
    provider: test
  - name: dup
    key: test-key-2
    base_url: https://api2.example.com
    provider: test
`)
		if err == nil {
			t.Fatal("duplicate account names must be rejected")
		}
		if !strings.Contains(err.Error(), "duplicate name") {
			t.Errorf("error = %q, want it to mention the duplicate name", err)
		}
	})
	t.Run("credential folding collision rejected", func(t *testing.T) {
		// "a-b" and "a_b" both fold to LB_KEY_A_B: getCredential would
		// silently resolve both accounts to the same secret.
		_, err := loadConfigFrom(t, `
accounts:
  - name: a-b
    key: test-key-1
    base_url: https://api1.example.com
    provider: test
  - name: a_b
    key: test-key-2
    base_url: https://api2.example.com
    provider: test
`)
		if err == nil {
			t.Fatal("account names folding to the same LB_KEY_* credential name must be rejected")
		}
		if !strings.Contains(err.Error(), "LB_KEY_A_B") {
			t.Errorf("error = %q, want it to name the colliding credential", err)
		}
	})
	t.Run("case-folding collision rejected", func(t *testing.T) {
		// "A-b" and "a-b" fold identically (ToUpper) and are also
		// distinct names that would collide on the credential.
		_, err := loadConfigFrom(t, `
accounts:
  - name: A-b
    key: test-key-1
    base_url: https://api1.example.com
    provider: test
  - name: a-b
    key: test-key-2
    base_url: https://api2.example.com
    provider: test
`)
		if err == nil {
			t.Fatal("account names folding to the same credential must be rejected (case folding)")
		}
	})
}

func TestLoadConfig_QuotaDefaultsAndExplicitOff(t *testing.T) {
	cfg, err := loadConfigFrom(t, `
accounts:
  - name: go-1
    key: test-key-12345
    base_url: https://opencode.ai/zen/go/v1
    provider: opencode-go
`)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Quota.Enabled {
		t.Fatal("quota.enabled default = false, want true")
	}
	if cfg.Quota.RefreshInterval != 120*time.Second {
		t.Fatalf("refresh_interval = %v, want 120s", cfg.Quota.RefreshInterval)
	}
	if cfg.Quota.RequestTimeout != 5*time.Second {
		t.Fatalf("request_timeout = %v, want 5s", cfg.Quota.RequestTimeout)
	}

	off, err := loadConfigFrom(t, `
quota:
  enabled: false
accounts:
  - name: go-1
    key: test-key-12345
    base_url: https://opencode.ai/zen/go/v1
    provider: opencode-go
`)
	if err != nil {
		t.Fatal(err)
	}
	if off.Quota.Enabled {
		t.Fatal("explicit quota.enabled: false must stay false")
	}
}
