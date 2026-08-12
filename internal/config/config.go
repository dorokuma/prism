package config

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// ModelCost defines per-model pricing rates.
type ModelCost struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}

// ModelMetadata defines optional per-model metadata that prism returns
// via /v1/models for agent/tool auto-discovery. All fields are optional;
// agents use what they understand and ignore the rest.
type ModelMetadata struct {
	ContextWindow    *int               `yaml:"context_window,omitempty"`
	MaxTokens        *int               `yaml:"max_tokens,omitempty"`
	Reasoning        *bool              `yaml:"reasoning,omitempty"`
	Input            []string           `yaml:"input,omitempty"`
	Cost             *ModelCost         `yaml:"cost,omitempty"`
	ThinkingLevelMap map[string]*string `yaml:"thinking_level_map,omitempty"`
	Extra            map[string]any     `yaml:"extra,omitempty"`
}

// ModelMetadataMap is a convenience alias for map[string]ModelMetadata.
type ModelMetadataMap map[string]ModelMetadata

// LogLevelHook is a package-level hook for setting the log level at runtime.
// It is set by the main package during initialization to enable hot-reload
// of the log level via ReloadConfig.
var LogLevelHook func(level string)

// maxAPIKeyTokenBytes is the longest API key token the constant-time auth
// comparison supports (must match middleware.authPadLen). Tokens longer than
// this would be truncated by the fixed-length pad, which could let a prefix
// match pass; LoadConfig rejects them.
const maxAPIKeyTokenBytes = 256

// UsageConfig mirrors internal/usage.Config for the yaml `usage` section.
// The mapping to internal/usage.Config happens in the wiring stage
// (cmd/prism); internal/usage deliberately does not depend on this package.
//
// enabled defaults to false: token usage recording is opt-in so existing
// deployments behave exactly as before until they configure it.
// db_path defaults to /var/lib/prism/usage.db, next to the model cache
// directory. db_path is NOT hot-reloadable: changing it requires a restart
// (ReloadConfig warns).
//
// Costing note: prices come from model_metadata[].cost (USD per 1M tokens)
// and are resolved per request by the wiring stage; no conversion is done
// here.
//
// Defaults applied in LoadConfig: enabled=false, db_path=/var/lib/prism/usage.db,
// retention_days=30 (only when the field is ABSENT — an explicit 0 is kept
// and means "never delete"; see UnmarshalYAML), channel_size=4096,
// batch_size=50, batch_flush_ms=200, default_key_id="anonymous" (an empty
// value — absent or explicit "" — falls back to "anonymous"). Other
// zero-valued fields are replaced by these defaults (LoadConfig style).
type UsageConfig struct {
	Enabled       bool   `yaml:"enabled"`
	DBPath        string `yaml:"db_path"`
	RetentionDays int    `yaml:"retention_days"`
	ChannelSize   int    `yaml:"channel_size"`
	BatchSize     int    `yaml:"batch_size"`
	BatchFlushMS  int    `yaml:"batch_flush_ms"`

	// DefaultKeyID is the key_id recorded for requests that were not
	// authenticated with a named api_keys entry (auth disabled, or the
	// request was rejected before key attribution). Defaults to
	// "anonymous". prism deliberately does not hard-code any client name:
	// a deployment that wants its single client attributed under a
	// specific label sets this explicitly.
	DefaultKeyID string `yaml:"default_key_id"`

	// retentionDaysSet records whether the yaml contained an explicit
	// retention_days key. yaml unmarshalling cannot distinguish "0" from
	// "absent" on a plain int, but the two must behave differently: absent
	// → default 30, explicit 0 → "keep forever, disable cleanup".
	retentionDaysSet bool
}

// UnmarshalYAML decodes the usage section and remembers whether
// retention_days was explicitly present, so LoadConfig can tell "user
// configured 0" apart from "user did not configure the field".
func (u *UsageConfig) UnmarshalYAML(value *yaml.Node) error {
	type plain UsageConfig // avoids recursion into this method
	if err := value.Decode((*plain)(u)); err != nil {
		return err
	}
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "retention_days" {
				u.retentionDaysSet = true
				break
			}
		}
	}
	return nil
}

// AccountConfig holds configuration for a single upstream API account.
type AccountConfig struct {
	Name     string `yaml:"name"`
	Key      string `yaml:"key,omitempty"`
	Provider string `yaml:"provider,omitempty"`
	BaseURL  string `yaml:"base_url"`
	// Headers are account-level custom headers applied to every upstream
	// request (chat/messages forwards and probe requests). They override
	// same-named client headers, but can never set the credential header
	// (Authorization, or the account's auth_header) — that always comes
	// from the account key. Nil/omitted = no extra headers (backward compat).
	Headers map[string]string `yaml:"headers,omitempty"`
	// AuthHeader overrides the credential header name for this account.
	// Semantics (implemented in pool.ApplyAuthHeader, used by both upstream
	// forwards and probe requests):
	//   - empty/omitted, or canonical form == "Authorization":
	//     write "Authorization: Bearer <key>" (identical to current behavior)
	//   - any other value (e.g. "x-api-key"):
	//     write "<auth_header>: <key>" (raw key, NO Bearer prefix) and do
	//     NOT write an Authorization header at all
	AuthHeader string `yaml:"auth_header,omitempty"`
	// ProbePath overrides the health-check GET path for this account.
	// Empty or "default" = "/v1/models"; an explicit path (e.g. "/models")
	// is used as-is (joined with base_url); "-", "disabled" or "none"
	// disables probing entirely (exhausted accounts are optimistically
	// marked healthy each probe cycle, no HTTP request is sent).
	ProbePath string `yaml:"probe_path,omitempty"`
	// SkipPISync excludes the provider from prism-managed pi models.json
	// sync only: its pi metadata is hand-maintained and must not be
	// overwritten by prism. It does NOT affect upstream model fetching —
	// the model cache still fetches this provider like any other.
	SkipPISync bool `yaml:"skip_pi_sync,omitempty"`
}

// APIKey defines a single client authentication credential for prism's
// /v1/* endpoints. Name is the human-readable key identifier recorded in
// audit logs; Token is the secret itself and must never be logged.
type APIKey struct {
	Name  string `yaml:"name"`
	Token string `yaml:"token"`
}

// McpAdminIdentity is the reserved cache identity of the shared
// admin-injected MCP tool bucket (mcp_tools.json, see internal/mcp
// LoadMCPTools). It is deliberately NOT a usable API key name: LoadConfig
// rejects any api_keys entry whose name equals this value with an explicit
// error, so an authenticated request identity can never collide with the
// admin bucket (the request path additionally refuses to write to it — see
// internal/mcp cacheMCPTool). It is defined here (not in internal/mcp)
// because mcp already imports config and config must not import mcp.
const McpAdminIdentity = "__prism_admin__"

// Config holds the top-level application configuration loaded from a YAML file.
type Config struct {
	Listen                   string                      `yaml:"listen"`
	ProbeInterval            time.Duration               `yaml:"probe_interval"`
	WireAPI                  string                      `yaml:"wire_api"`
	Accounts                 []AccountConfig             `yaml:"accounts"`
	ModelRemapEnabled        bool                        `yaml:"model_remap_enabled"`
	ModelRemap               map[string]string           `yaml:"model_remap"`
	ModelTiers               map[string]string           `yaml:"model_tiers"`
	DefaultTier              string                      `yaml:"default_tier"`
	StripFields              map[string][]string         `yaml:"strip_fields"`
	Debug                    bool                        `yaml:"debug"`
	MCPToolsJSON             string                      `yaml:"mcp_tools_json"`
	AuthToken                string                      `yaml:"auth_token,omitempty"`
	APIKeys                  []APIKey                    `yaml:"api_keys,omitempty"`
	TLSCertFile              string                      `yaml:"tls_cert_file,omitempty"`
	TLSKeyFile               string                      `yaml:"tls_key_file,omitempty"`
	TrustedProxies           []string                    `yaml:"trusted_proxies,omitempty"`
	Tools                    map[string]string           `yaml:"tools,omitempty"`
	ModelMetadata            ModelMetadataMap            `yaml:"model_metadata,omitempty"`
	ModelMetadataPerProvider map[string]ModelMetadataMap `yaml:"model_metadata_per_provider,omitempty"`
	LogLevel                 string                      `yaml:"log_level"`
	MaxConcurrentPerAccount  map[string]int              `yaml:"max_concurrent_per_account"`
	// MaxUpstreamResponseBytes caps the size of a non-streaming upstream
	// response body (both the legacy chat path and the responses translation
	// path). Bodies larger than the cap are rejected with HTTP 502
	// response_too_large instead of being buffered whole into memory. Zero
	// (absent) → default 32 MiB; negative or above
	// MaxUpstreamResponseBytesLimit (256 MiB) is a load error.
	// Hot-reloadable.
	MaxUpstreamResponseBytes int64 `yaml:"max_upstream_response_bytes,omitempty"`
	// DefaultProvider is the fallback provider used when a request arrives
	// without the X-Prism-Provider header. Empty (default) = reject such
	// requests with HTTP 400 instead of falling back to whole-pool selection
	// (which could route an account to the wrong provider).
	DefaultProvider string `yaml:"default_provider"`

	// Usage is the optional token-usage recording section (see UsageConfig).
	Usage UsageConfig `yaml:"usage"`

	// providerSchema maps a provider name to its effort schema ("ollama" or
	// empty for opencode). Precomputed from account base_url hosts at load time.
	providerSchema map[string]string
}

// LoadConfig reads a YAML config file, unmarshals it into Config, applies
// defaults and env-var fallbacks, and validates the result.
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
		slog.Warn("probe_interval too small, falling back to 10m", "probe_interval", cfg.ProbeInterval)
		cfg.ProbeInterval = 10 * time.Minute
	}
	if cfg.WireAPI == "" {
		cfg.WireAPI = "both"
	}
	if cfg.MaxUpstreamResponseBytes < 0 {
		return nil, fmt.Errorf("max_upstream_response_bytes must be >= 0, got %d", cfg.MaxUpstreamResponseBytes)
	}
	if cfg.MaxUpstreamResponseBytes == 0 {
		cfg.MaxUpstreamResponseBytes = MaxUpstreamResponseBytesDefault
	}
	// Hard upper bound: values above 256 MiB are rejected at load time. The
	// bound exists so the read helper's max+1 probe can never overflow
	// int64 (see MaxUpstreamResponseBytesLimit); it also keeps a mis-typed
	// value (e.g. bytes intended as MiB) from silently buffering unbounded
	// responses into memory.
	if cfg.MaxUpstreamResponseBytes > MaxUpstreamResponseBytesLimit {
		return nil, fmt.Errorf("max_upstream_response_bytes %d exceeds the maximum supported %d (%d MiB)", cfg.MaxUpstreamResponseBytes, MaxUpstreamResponseBytesLimit, MaxUpstreamResponseBytesLimit>>20)
	}
	// Upper-bound validation for max_concurrent_per_account: the pool's
	// concurrency accounting is int32 (Account.TryAcquire converts the limit
	// with int32(max)), so a value above math.MaxInt32 would be silently
	// truncated — a huge value can wrap to 0 (every request waits forever)
	// or to a negative number (the cap disabled entirely). Misconfiguration
	// must fail at load time instead of corrupting concurrency control at
	// runtime.
	for model, v := range cfg.MaxConcurrentPerAccount {
		if v > math.MaxInt32 {
			return nil, fmt.Errorf("max_concurrent_per_account[%q] = %d exceeds the maximum supported %d (int32 concurrency accounting)", model, v, math.MaxInt32)
		}
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	// Usage section defaults. Recording is opt-in (enabled=false): an
	// existing deployment without a usage section behaves exactly as before
	// and only the defaults below are filled in (harmless while disabled).
	// db_path sits next to the model cache directory (/var/lib/prism).
	// retention_days defaults to 30 ONLY when the field was absent: an
	// explicit 0 is preserved and means "keep forever, disable cleanup"
	// (internal/usage disables the cleanup loop for RetentionDays <= 0).
	if cfg.Usage.DBPath == "" {
		cfg.Usage.DBPath = "/var/lib/prism/usage.db"
	}
	if !cfg.Usage.retentionDaysSet && cfg.Usage.RetentionDays == 0 {
		cfg.Usage.RetentionDays = 30
	}
	if cfg.Usage.ChannelSize == 0 {
		cfg.Usage.ChannelSize = 4096
	}
	if cfg.Usage.BatchSize == 0 {
		cfg.Usage.BatchSize = 50
	}
	if cfg.Usage.BatchFlushMS == 0 {
		cfg.Usage.BatchFlushMS = 200
	}
	if cfg.Usage.DefaultKeyID == "" {
		cfg.Usage.DefaultKeyID = "anonymous"
	}
	if _, err := ParseWireAPIMode(cfg.WireAPI); err != nil {
		return nil, err
	}
	// Support providers block
	type providersConfig struct {
		Providers map[string]struct {
			Accounts []AccountConfig `yaml:"accounts"`
		} `yaml:"providers"`
	}
	var pc providersConfig
	if err := yaml.Unmarshal(data, &pc); err == nil && len(pc.Providers) > 0 {
		var allAccounts []AccountConfig
		for providerName, providerCfg := range pc.Providers {
			for _, acc := range providerCfg.Accounts {
				acc.Provider = providerName
				allAccounts = append(allAccounts, acc)
			}
		}
		cfg.Accounts = allAccounts
	}
	// Provider names become model-cache file names (provider + ".json" under
	// the cache dir). Reject names that would escape the cache directory
	// (path separators, "." / "..", absolute paths) at load time — a
	// malicious or mis-typed provider could otherwise read or write cache
	// files outside the cache dir.
	for _, acc := range cfg.Accounts {
		if err := validateProviderName(acc.Provider); err != nil {
			return nil, err
		}
	}
	if len(cfg.Accounts) == 0 {
		return nil, fmt.Errorf("no accounts configured")
	}
	// default_provider must reference a provider that actually exists;
	// otherwise requests without X-Prism-Provider would silently break.
	if cfg.DefaultProvider != "" && !cfg.hasProvider(cfg.DefaultProvider) {
		return nil, fmt.Errorf("default_provider %q not found among configured providers", cfg.DefaultProvider)
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
	// Backward-compatible expansion of the legacy single auth_token into the
	// api_keys set:
	//   - api_keys empty + auth_token set  → inject {name: "default", token: auth_token}
	//   - both set                         → merge; if auth_token's value already
	//     appears in an api_keys entry, that entry's name wins and NO extra
	//     "default" entry is injected (avoids duplicate-token ambiguity).
	//   - api_keys set + auth_token empty  → api_keys used as-is.
	if len(cfg.APIKeys) == 0 && cfg.AuthToken != "" {
		cfg.APIKeys = []APIKey{{Name: "default", Token: cfg.AuthToken}}
	} else if len(cfg.APIKeys) > 0 && cfg.AuthToken != "" {
		dup := false
		for _, k := range cfg.APIKeys {
			if k.Token == cfg.AuthToken {
				dup = true
				break
			}
		}
		if !dup {
			cfg.APIKeys = append(cfg.APIKeys, APIKey{Name: "default", Token: cfg.AuthToken})
		}
	}
	// Reject keys with empty or whitespace-only tokens: the fixed-length
	// padded comparison in the auth middleware treats an empty expected
	// value as an all-zero pad, so a key with an empty token would let
	// every "Bearer " request authenticate. The error names the key but
	// never echoes any token.
	for _, k := range cfg.APIKeys {
		if strings.TrimSpace(k.Token) == "" {
			return nil, fmt.Errorf("api key %q: token is empty (empty or whitespace-only tokens are rejected)", k.Name)
		}
	}
	// API key NAMEs are the audit key_id AND the MCP tool-cache identity
	// (internal/proxy getTenantID): they must be stable, non-empty and
	// unique so that (a) per-key MCP cache isolation holds — two keys
	// sharing a name would see each other's cached tools — and (b) an
	// authenticated request identity can never collide with the shared
	// admin-injected MCP bucket (config.McpAdminIdentity). All three
	// violations fail loading with an explicit error. The legacy auth_token
	// expansion above legitimately produces the name "default" (a plain
	// per-client bucket, NOT the admin bucket — only the reserved identity
	// is forbidden), and the errors name the offending key but never echo
	// any token.
	seenNames := make(map[string]bool, len(cfg.APIKeys))
	for _, k := range cfg.APIKeys {
		if strings.TrimSpace(k.Name) == "" {
			return nil, fmt.Errorf("api key: name is empty; every key needs a non-empty name (it is the audit key_id and the MCP cache identity)")
		}
		if k.Name == McpAdminIdentity {
			return nil, fmt.Errorf("api key name %q is reserved for the shared admin-injected MCP tool bucket; choose a different name", k.Name)
		}
		if seenNames[k.Name] {
			return nil, fmt.Errorf("api_keys: duplicate key name %q; every key needs a unique name (it is the MCP cache identity and the audit key_id)", k.Name)
		}
		seenNames[k.Name] = true
	}
	// Reject duplicate tokens within api_keys. The auth loop deliberately
	// keeps the LAST matching name (no early return, constant-time scan), so
	// a duplicated token would silently attribute every request — and every
	// usage row and audit line — to the last key name. The error names the
	// two conflicting keys but never echoes the token itself. This runs
	// AFTER the auth_token expansion above, which already deduplicates the
	// legacy auth_token against api_keys (an auth_token equal to an
	// api_keys token is the known-good "same key, two spellings" case and
	// never reaches this check as a duplicate).
	seenTokens := make(map[string]string, len(cfg.APIKeys))
	for _, k := range cfg.APIKeys {
		if prev, ok := seenTokens[k.Token]; ok {
			return nil, fmt.Errorf("api_keys: keys %q and %q use the same token; every key must have a unique token", prev, k.Name)
		}
		seenTokens[k.Token] = k.Name
	}
	// API key tokens are compared with a fixed-length constant-time pad in
	// the auth middleware (middleware.authPadLen = 256): a token longer than
	// the pad would be truncated and a prefix match could pass. Reject such
	// configurations outright instead of silently weakening auth.
	for _, k := range cfg.APIKeys {
		if len(k.Token) > maxAPIKeyTokenBytes {
			return nil, fmt.Errorf("api key %q: token longer than %d bytes is not supported (constant-time auth compares a fixed %d-byte pad)", k.Name, maxAPIKeyTokenBytes, maxAPIKeyTokenBytes)
		}
	}
	// Reject non-loopback listen without any credential (checked post-expansion).
	if !isLoopbackListen(cfg.Listen) && len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("non-loopback listen address %q requires auth_token, PRISM_AUTH_TOKEN, or api_keys", cfg.Listen)
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
	// Precompute the provider → effort schema map from account base_url hosts.
	cfg.providerSchema = buildProviderSchema(cfg.Accounts)
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
				slog.Warn("tier upstream looks like GLM/z-ai but missing prompt_cache_retention in strip_fields", "tier", tier, "upstream", upstream)
			}
		}
	}
	return cfg, nil
}

// validateProviderName rejects provider names that cannot be used as a
// model-cache file name: the cache joins provider+".json" under its cache
// directory, so a name with path separators, "." / "..", or an absolute
// path would escape the cache dir (path traversal). Empty is allowed: an
// account without a provider is valid and is simply skipped by the model
// cache (it only manages named providers).
func validateProviderName(name string) error {
	if name == "" {
		return nil
	}
	if strings.ContainsAny(name, `\/`) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("provider name %q must not contain path separators", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("provider name %q is not allowed as a cache file name", name)
	}
	if filepath.IsAbs(name) {
		return fmt.Errorf("provider name %q must not be an absolute path", name)
	}
	return nil
}

func buildProviderSchema(accs []AccountConfig) map[string]string {
	m := map[string]string{}
	for _, a := range accs {
		if a.Provider == "" {
			continue
		}
		if _, ok := m[a.Provider]; ok {
			continue
		}
		if isOllamaHost(a.BaseURL) {
			m[a.Provider] = "ollama"
		} else {
			m[a.Provider] = ""
		}
	}
	return m
}

func isOllamaHost(baseURL string) bool {
	u, err := url.Parse(baseURL)
	return err == nil && strings.HasSuffix(u.Host, "ollama.com")
}

// LookupModelMetadata resolves per-model metadata for (provider, model).
// It first checks the per-provider override layer ModelMetadataPerProvider;
// if an entry exists there it is returned in full (replacing the default
// entry). Otherwise it falls back to the default ModelMetadata layer. The
// boolean reports whether any entry was found. A per-provider entry fully
// replaces the default entry (it is NOT field-merged): this is what keeps a
// model in one provider from inheriting the default layer's fields (which
// would otherwise cause cross-provider crosstalk, e.g. a 1M context_window
// from the default layer leaking into a provider whose upstream reports 512K).
func (c *Config) LookupModelMetadata(provider, model string) (ModelMetadata, bool) {
	if c == nil {
		return ModelMetadata{}, false
	}
	if pp, ok := c.ModelMetadataPerProvider[provider]; ok {
		if meta, ok := pp[model]; ok {
			return meta, true
		}
	}
	meta, ok := c.ModelMetadata[model]
	return meta, ok
}

// EffortSchema returns the effort-mapping schema for the given provider
// ("ollama" for ollama-cloud hosts, empty string for opencode). An empty or
// unconfigured provider defaults to the opencode schema.
func (c *Config) EffortSchema(provider string) string {
	if c == nil || provider == "" {
		return ""
	}
	return c.providerSchema[provider]
}

// RemapModel resolves a virtual model name to its upstream model via
// model_remap → model_tiers. Models NOT in model_remap (real upstream names)
// pass through unchanged. Models IN model_remap whose tier has no upstream
// mapping fall back to default_tier.
func (c *Config) RemapModel(model string) string {
	if !c.ModelRemapEnabled {
		return model
	}
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

// hasProvider reports whether any account belongs to the given provider name.
func (c *Config) hasProvider(name string) bool {
	for _, acc := range c.Accounts {
		if acc.Provider == name {
			return true
		}
	}
	return false
}

// ProviderNames returns all distinct provider names from account configs.
func (c *Config) ProviderNames() []string {
	seen := make(map[string]bool)
	var out []string
	for _, acc := range c.Accounts {
		if acc.Provider != "" && !seen[acc.Provider] {
			seen[acc.Provider] = true
			out = append(out, acc.Provider)
		}
	}
	return out
}

// GetTier returns the tier name for a virtual model, without resolving to upstream.
func (c *Config) GetTier(model string) string {
	if c.ModelRemap != nil {
		if tier, ok := c.ModelRemap[model]; ok {
			return tier
		}
	}
	return ""
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

// VirtualModels returns the list of virtual model names from model_remap.
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

// ResolveMaxConcurrent returns the maximum concurrent requests per account
// for the given model. Resolution order:
//  1. cfg.MaxConcurrentPerAccount[model]  (exact match)
//  2. cfg.MaxConcurrentPerAccount["*"]   (wildcard default)
//  3. model name contains "flash" → deepseekV4FlashConcurrency * defaultConcurrencyRatio / 100
//  4. model name contains "pro"   → deepseekV4ProConcurrency * defaultConcurrencyRatio / 100
//  5. fallback: deepseekV4ProConcurrency * defaultConcurrencyRatio / 100
//
// An unknown non-empty model logs a warning (the fallback is a guess); an
// empty model is the internal no-model case (model cache fetches) and
// silently returns the fallback so background fetches do not spam the log.
// It is the single concurrency-resolution implementation shared by the
// proxy request path and the model cache fetch path (cache cannot import
// proxy, so the function lives here to avoid a circular dependency).
func ResolveMaxConcurrent(model string, cfg *Config) int {
	if cfg != nil && cfg.MaxConcurrentPerAccount != nil {
		if v, ok := cfg.MaxConcurrentPerAccount[model]; ok && v > 0 {
			return v
		}
		if v, ok := cfg.MaxConcurrentPerAccount["*"]; ok && v > 0 {
			return v
		}
	}
	modelLower := strings.ToLower(model)
	if strings.Contains(modelLower, "flash") {
		return DeepseekV4FlashConcurrency * DefaultConcurrencyRatio / 100
	}
	if strings.Contains(modelLower, "pro") {
		return DeepseekV4ProConcurrency * DefaultConcurrencyRatio / 100
	}
	// Default for unknown models
	if model != "" {
		slog.Warn("unknown model, using default concurrency", "model", model)
	}
	return DeepseekV4ProConcurrency * DefaultConcurrencyRatio / 100
}

// ResolveFetchConcurrency returns the concurrency cap for model-cache
// fetches (internal/cache). A Fetch is not tied to a single business model,
// so the per-model max_concurrent_per_account entries cannot be resolved by
// exact match. The rule is deliberately conservative and explainable:
//  1. a configured "*" wildcard wins — it is the operator's explicit global
//     default for every model;
//  2. otherwise the SMALLEST positive per-model value is used: a fetch
//     holds a concurrency slot on the same account as business requests,
//     and any specific model cap must be respected by a fetch that may run
//     alongside that model's traffic (taking the minimum is the only choice
//     that cannot oversubscribe any configured model);
//  3. no positive values → the same built-in default as the empty-model
//     business resolution (DeepseekV4ProConcurrency *
//     DefaultConcurrencyRatio / 100).
//
// Non-positive entries (0 or negative) are ignored in every branch: they
// mean "no explicit limit", never "limit to zero".
func ResolveFetchConcurrency(cfg *Config) int {
	if cfg != nil && cfg.MaxConcurrentPerAccount != nil {
		if v, ok := cfg.MaxConcurrentPerAccount["*"]; ok && v > 0 {
			return v
		}
		min := 0
		for _, v := range cfg.MaxConcurrentPerAccount {
			if v > 0 && (min == 0 || v < min) {
				min = v
			}
		}
		if min > 0 {
			return min
		}
	}
	return DeepseekV4ProConcurrency * DefaultConcurrencyRatio / 100
}

// getCredential reads a credential file from the systemd LoadCredential
// directory (CREDENTIALS_DIRECTORY). Returns the trimmed contents on success,
// or "" if CREDENTIALS_DIRECTORY is unset or the file cannot be read.
func getCredential(name string) string {
	// 1. systemd LoadCredential
	credDir := os.Getenv("CREDENTIALS_DIRECTORY")
	if credDir != "" {
		data, err := os.ReadFile(filepath.Join(credDir, name))
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	// 2. 环境变量
	if key := os.Getenv(name); key != "" {
		return key
	}
	// 3. 直接读 credstore（setup 生成的 unit 可不声明 LoadCredential）
	data, err := os.ReadFile(filepath.Join("/etc/credstore/prism", name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ConfigHolder provides atomic access to the current *Config, enabling
// safe hot-reloading of read-only configuration fields (model remap,
// model tiers, default tier, strip fields) without disrupting in-flight
// requests.
type ConfigHolder struct {
	ptr atomic.Pointer[Config]
}

// NewConfigHolder creates a ConfigHolder initialized with cfg.
func NewConfigHolder(cfg *Config) *ConfigHolder {
	h := &ConfigHolder{}
	h.ptr.Store(cfg)
	return h
}

// Load returns the current *Config atomically.
func (h *ConfigHolder) Load() *Config {
	return h.ptr.Load()
}

// Store replaces the current *Config atomically.
func (h *ConfigHolder) Store(cfg *Config) {
	h.ptr.Store(cfg)
}

// ReloadConfig loads a new Config from path, validates it, compares
// unsafe-to-reload fields against the current config, atomically swaps
// the new config into holder, and returns any warnings about fields that
// changed but require a process restart to take effect.
//
// On error the old config is preserved.
func ReloadConfig(holder *ConfigHolder, path string) (warnings []string, err error) {
	oldCfg := holder.Load()
	newCfg, err := LoadConfig(path)
	if err != nil {
		return nil, fmt.Errorf("reload config: %w", err)
	}

	// Compare fields that cannot be hot-reloaded and warn if changed.
	if oldCfg.Listen != newCfg.Listen {
		warnings = append(warnings, fmt.Sprintf(
			"listen changed from %q to %q: restart required to take effect",
			oldCfg.Listen, newCfg.Listen))
	}
	// api_keys / auth_token are hot-reloadable: the ConfigHolder atomic pointer
	// swap below publishes the new credential set and the auth middleware reads
	// it per request, so a change takes effect immediately. No restart warning
	// is emitted (the account pool is not rebuilt by this change).
	if !accountsEqual(oldCfg.Accounts, newCfg.Accounts) {
		warnings = append(warnings, "accounts changed: restart required to take effect")
	}
	if oldCfg.ProbeInterval != newCfg.ProbeInterval {
		warnings = append(warnings, "probe_interval changed: restart required to take effect")
	}
	if oldCfg.WireAPI != newCfg.WireAPI {
		warnings = append(warnings, "wire_api changed: restart required to take effect")
	}
	if oldCfg.TLSCertFile != newCfg.TLSCertFile || oldCfg.TLSKeyFile != newCfg.TLSKeyFile {
		warnings = append(warnings, "tls_cert_file/tls_key_file changed: restart required to take effect")
	}
	if !equalStringSlices(oldCfg.TrustedProxies, newCfg.TrustedProxies) {
		warnings = append(warnings, "trusted_proxies changed: restart required to take effect")
	}
	// debug IS hot-reloadable: the SIGHUP flow in cmd/prism re-applies it
	// by setting util.DebugMode directly from the reloaded config — this
	// ReloadConfig never touches util.DebugMode itself, and LogLevelHook
	// (the logger level only) is unrelated to debug, so a debug change
	// takes effect immediately — no restart warning is emitted.
	// The usage store + recorder are built once at startup; db_path (which
	// database file) and enabled (whether recording is on at all) cannot be
	// hot-reloaded. Other usage fields are tuning knobs that also only apply
	// at startup; they are not warned about to keep reload output focused.
	if oldCfg.Usage.DBPath != newCfg.Usage.DBPath {
		warnings = append(warnings, fmt.Sprintf(
			"usage.db_path changed from %q to %q: restart required to take effect",
			oldCfg.Usage.DBPath, newCfg.Usage.DBPath))
	}
	if oldCfg.Usage.Enabled != newCfg.Usage.Enabled {
		warnings = append(warnings, "usage.enabled changed: restart required to take effect")
	}
	if oldCfg.LogLevel != newCfg.LogLevel {
		if LogLevelHook != nil {
			LogLevelHook(newCfg.LogLevel)
		}
	}

	// Atomically swap to the new config.
	holder.Store(newCfg)

	return warnings, nil
}

// accountsEqual compares two account slices by every field that cannot be
// hot-reloaded (name, base_url, key, provider, headers, auth_header,
// probe_path, skip_pi_sync). A change in any of them means the Pool (built
// once at startup) is stale and a restart is required.
func accountsEqual(a, b []AccountConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name ||
			a[i].BaseURL != b[i].BaseURL ||
			a[i].Key != b[i].Key ||
			a[i].Provider != b[i].Provider ||
			a[i].AuthHeader != b[i].AuthHeader ||
			a[i].ProbePath != b[i].ProbePath ||
			a[i].SkipPISync != b[i].SkipPISync ||
			!stringMapsEqual(a[i].Headers, b[i].Headers) {
			return false
		}
	}
	return true
}

// stringMapsEqual compares two string maps for key/value equality.
func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// equalStringSlices compares two string slices for equality.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// isLoopbackListen returns true if addr binds to a loopback interface.
// Uses fail-safe logic: parse errors, empty host, and non-IP hosts are
// treated as non-loopback.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
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
