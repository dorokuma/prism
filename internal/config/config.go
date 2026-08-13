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

// QuotaConfig is the yaml `quota` section: upstream plan-window snapshots
// (OpenCode Go rolling/weekly/monthly). Independent of UsageConfig.
//
// enabled defaults to true when the field is absent. refresh_interval
// defaults to 120s (minimum 30s). request_timeout defaults to 5s.
type QuotaConfig struct {
	Enabled         bool          `yaml:"enabled"`
	RefreshInterval time.Duration `yaml:"refresh_interval"`
	RequestTimeout  time.Duration `yaml:"request_timeout"`
	enabledSet      bool
}

// UnmarshalYAML records whether enabled was explicitly present so LoadConfig
// can tell "user set false" from "section missing".
func (q *QuotaConfig) UnmarshalYAML(value *yaml.Node) error {
	type plain QuotaConfig
	if err := value.Decode((*plain)(q)); err != nil {
		return err
	}
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "enabled" {
				q.enabledSet = true
				break
			}
		}
	}
	return nil
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
	// disables probing entirely (no HTTP request is sent, and the account
	// state is left untouched — an exhausted account stays exhausted until
	// the operator restores it; "probing disabled" is not "the credential
	// recovered").
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

// McpUnauthenticatedIdentity is the reserved MCP tool-cache identity for
// requests that presented NO authenticated API key (auth disabled, or the
// request bypassed the auth middleware; see internal/mcp cache.go and
// internal/proxy getTenantID). The bucket is deliberately READ-ONLY
// (internal/mcp cacheMCPTool refuses to write it), so with auth disabled
// different local clients can never pollute each other's cached tools. It
// is deliberately NOT a usable API key name: LoadConfig rejects any
// api_keys entry whose name equals this value, so an authenticated key can
// never collide with — or shadow — the unauthenticated identity (an
// authenticated request whose key name equals this label would otherwise
// silently lose its own MCP tool cache and be indistinguishable from an
// unauthenticated one). Defined here (not in internal/mcp) because mcp
// already imports config and config must not import mcp; internal/mcp
// aliases this value as its UnauthenticatedIdentity constant.
const McpUnauthenticatedIdentity = "unauthenticated"

// Config holds the top-level application configuration loaded from a YAML file.
type Config struct {
	Listen            string              `yaml:"listen"`
	ProbeInterval     time.Duration       `yaml:"probe_interval"`
	WireAPI           string              `yaml:"wire_api"`
	Accounts          []AccountConfig     `yaml:"accounts"`
	ModelRemapEnabled bool                `yaml:"model_remap_enabled"`
	ModelRemap        map[string]string   `yaml:"model_remap"`
	ModelTiers        map[string]string   `yaml:"model_tiers"`
	DefaultTier       string              `yaml:"default_tier"`
	StripFields       map[string][]string `yaml:"strip_fields"`
	Debug             bool                `yaml:"debug"`
	MCPToolsJSON      string              `yaml:"mcp_tools_json"`
	AuthToken         string              `yaml:"auth_token,omitempty"`
	APIKeys           []APIKey            `yaml:"api_keys,omitempty"`
	TLSCertFile       string              `yaml:"tls_cert_file,omitempty"`
	TLSKeyFile        string              `yaml:"tls_key_file,omitempty"`
	// AllowInsecureHTTP explicitly opts a non-loopback listener into
	// plaintext HTTP (no TLS). Default false: LoadConfig fails when a
	// non-loopback listen address has no complete TLS configuration
	// (tls_cert_file + tls_key_file, or their PRISM_TLS_CERT /
	// PRISM_TLS_KEY env-var fallbacks). trusted_proxies does NOT relax this
	// check: it only limits which X-Forwarded-For hops are trusted and can
	// not prevent direct access to the listener. Loopback listeners are
	// always allowed without TLS (local development). Simple bool, default
	// false; removing the field later is a safe no-op for every config that
	// loads today.
	AllowInsecureHTTP        bool                        `yaml:"allow_insecure_http,omitempty"`
	TrustedProxies           []string                    `yaml:"trusted_proxies,omitempty"`
	Tools                    map[string]string           `yaml:"tools,omitempty"`
	ModelMetadata            ModelMetadataMap            `yaml:"model_metadata,omitempty"`
	ModelMetadataPerProvider map[string]ModelMetadataMap `yaml:"model_metadata_per_provider,omitempty"`
	LogLevel                 string                      `yaml:"log_level"`
	MaxConcurrentPerAccount  map[string]int              `yaml:"max_concurrent_per_account"`
	// MaxConcurrentPerAccountTotal is the per-account AGGREGATE concurrency
	// cap across ALL models (the account-wide total of every per-model
	// counter, applied to each account). 0/absent → the backward-compatible
	// default (see ResolveAccountTotalCap: the sum of distinct positive
	// per-model values, which reproduces the old per-max-value-grouped
	// worst case); negative or above math.MaxInt32 is a load error. It is
	// NOT hot-reloadable: the pool is built once at startup (ReloadConfig
	// warns). Per-model max_concurrent_per_account values DO hot-reload
	// (they are resolved per request), but the aggregate bound needs a
	// restart.
	MaxConcurrentPerAccountTotal int `yaml:"max_concurrent_per_account_total,omitempty"`
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

	// Quota is upstream plan-usage snapshots (see QuotaConfig). Independent
	// of Usage: disabling local token recording does not disable quota.
	Quota QuotaConfig `yaml:"quota"`

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
	// Same int32-accounting bound for the account-wide aggregate cap: a
	// value above math.MaxInt32 would truncate in the pool's int32 total
	// counter. Negative values are rejected outright (the per-model entries
	// ignore non-positive values as "no explicit limit", but a negative
	// aggregate is always a typo, not a meaningful choice).
	if cfg.MaxConcurrentPerAccountTotal > math.MaxInt32 {
		return nil, fmt.Errorf("max_concurrent_per_account_total = %d exceeds the maximum supported %d (int32 concurrency accounting)", cfg.MaxConcurrentPerAccountTotal, math.MaxInt32)
	}
	if cfg.MaxConcurrentPerAccountTotal < 0 {
		return nil, fmt.Errorf("max_concurrent_per_account_total must be >= 0, got %d", cfg.MaxConcurrentPerAccountTotal)
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
	// Quota defaults. enabled defaults to true when the key is absent so a
	// deployment with an opencode-go account starts reporting plan windows
	// without extra YAML. Explicit enabled: false stays off.
	if !cfg.Quota.enabledSet {
		cfg.Quota.Enabled = true
	}
	if cfg.Quota.RefreshInterval == 0 {
		cfg.Quota.RefreshInterval = 120 * time.Second
	}
	if cfg.Quota.RefreshInterval < 30*time.Second {
		slog.Warn("quota.refresh_interval too small, falling back to 30s", "refresh_interval", cfg.Quota.RefreshInterval)
		cfg.Quota.RefreshInterval = 30 * time.Second
	}
	if cfg.Quota.RequestTimeout == 0 {
		cfg.Quota.RequestTimeout = 5 * time.Second
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
	// Every account must belong to a provider (audit round 6, item 3). A
	// provider-less account can never be selected: business requests route
	// through SelectByProvider (the provider comes from X-Prism-Provider or
	// default_provider and is non-empty — a missing provider rejects the
	// request with 400), and model-cache fetches select accounts by provider
	// name too. Such an account only occupies the pool and the probe loop
	// while every business request misses it — a silent capacity loss. This
	// USED to load silently (a bare top-level `accounts:` list); that
	// configuration shape must now declare a provider per account (or move
	// the accounts under a `providers:` block). See README changelog for
	// the compatibility note.
	for _, acc := range cfg.Accounts {
		if acc.Provider == "" {
			return nil, fmt.Errorf("account %q: provider is empty; every account must declare a provider (set account.provider or put the account under a providers block) — a provider-less account can never be selected by business requests", acc.Name)
		}
	}
	// Account NAMEs are the audit/usage account label, the expvar key
	// (pool_account_<name>_*), and the seed of the LB_KEY_* credential /
	// env / credstore file name. Validate them at load:
	//   - non-empty, unique, ASCII alnum start, charset [A-Za-z0-9_-], at
	//     most MaxAccountNameLen bytes (the name also seeds systemd
	//     LoadCredential names and shell env names, so the charset excludes
	//     dots — the expvar hierarchy separator — path separators and ".."
	//     — the name seeds file names — spaces and unicode);
	//   - no two names may FOLD to the same LB_KEY_* credential name
	//     ("a-b" and "a_b" both fold to LB_KEY_A_B via
	//     CredentialEnvName): getCredential would silently resolve both
	//     accounts to the same secret, and the generated systemd unit
	//     would emit duplicate LoadCredential lines.
	seenAccountNames := make(map[string]bool, len(cfg.Accounts))
	foldedCredNames := make(map[string]string, len(cfg.Accounts))
	for _, acc := range cfg.Accounts {
		if err := ValidateAccountName(acc.Name); err != nil {
			return nil, fmt.Errorf("account %q: %w", acc.Name, err)
		}
		if seenAccountNames[acc.Name] {
			return nil, fmt.Errorf("accounts: duplicate name %q; every account needs a unique name (it is the audit account label, the expvar key and the LB_KEY_* credential name)", acc.Name)
		}
		seenAccountNames[acc.Name] = true
		envName := CredentialEnvName(acc.Name)
		if prev, ok := foldedCredNames[envName]; ok {
			return nil, fmt.Errorf("accounts %q and %q collide on credential name %s (hyphens fold to underscores); rename one account so the LB_KEY_* names differ", prev, acc.Name, envName)
		}
		foldedCredNames[envName] = acc.Name
	}
	// Every account base_url must be an absolute http(s) URL with a
	// non-empty host. This is a CONFIG-CORRECTNESS check, not an SSRF
	// defense: base_url is operator-controlled (never client-controlled), so
	// there is no SSRF surface here — the validation exists because a
	// malformed URL would fail every upstream request at runtime
	// (http.NewRequest rejects it, and the request path logs the failure as
	// invalid_upstream_url), and a missing host (e.g. "https:///v1") would
	// be silently joined into a nonsense target. The error names the account
	// but never echoes the URL value: a base_url may embed credentials
	// (user:pass@host) that must not reach logs.
	for _, acc := range cfg.Accounts {
		if err := validateBaseURL(acc.Name, acc.BaseURL); err != nil {
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
			envVar := CredentialEnvName(cfg.Accounts[i].Name)
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
	// admin-injected MCP bucket (config.McpAdminIdentity) or with the
	// read-only unauthenticated identity (config.McpUnauthenticatedIdentity,
	// the bucket for requests that present no credential — an
	// authenticated key named like it would silently lose its own tool
	// cache and be indistinguishable from an unauthenticated one). All
	// violations fail loading with an explicit error. The legacy auth_token
	// expansion above legitimately produces the name "default" (a plain
	// per-client bucket, NOT a reserved identity — only McpAdminIdentity
	// and McpUnauthenticatedIdentity are forbidden), and the errors name
	// the offending key but never echo any token.
	seenNames := make(map[string]bool, len(cfg.APIKeys))
	for _, k := range cfg.APIKeys {
		if strings.TrimSpace(k.Name) == "" {
			return nil, fmt.Errorf("api key: name is empty; every key needs a non-empty name (it is the audit key_id and the MCP cache identity)")
		}
		if k.Name == McpAdminIdentity {
			return nil, fmt.Errorf("api key name %q is reserved for the shared admin-injected MCP tool bucket; choose a different name", k.Name)
		}
		if k.Name == McpUnauthenticatedIdentity {
			return nil, fmt.Errorf("api key name %q is reserved for the unauthenticated MCP tool bucket (requests without a credential); choose a different name", k.Name)
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
	if !IsLoopbackListen(cfg.Listen) && len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("non-loopback listen address %q requires auth_token, PRISM_AUTH_TOKEN, or api_keys", cfg.Listen)
	}
	// TLS cert/key fallback to env vars — MUST run BEFORE the TLS
	// completeness check below, so env-provided certificates count as a
	// complete TLS configuration.
	if cfg.TLSCertFile == "" {
		cfg.TLSCertFile = os.Getenv("PRISM_TLS_CERT")
	}
	if cfg.TLSKeyFile == "" {
		cfg.TLSKeyFile = os.Getenv("PRISM_TLS_KEY")
	}
	// Non-loopback listeners must not serve plaintext unless the operator
	// explicitly opts in with allow_insecure_http: true. trusted_proxies
	// does NOT count as safety: it only governs which X-Forwarded-For hops
	// are trusted for rate limiting and does not prevent direct access to
	// the listener — a reverse proxy in front of a plaintext listener is
	// not a security boundary. Hence:
	//   - exactly one of tls_cert_file/tls_key_file → load error ALWAYS
	//     (the server only serves TLS when BOTH are set, so the listener
	//     would silently serve plaintext — a misconfiguration, not an
	//     opt-in);
	//   - neither set → load error unless allow_insecure_http: true makes
	//     the plaintext explicit (a warning is still logged so the exposure
	//     stays visible);
	//   - loopback listeners stay TLS-free (local development).
	if !IsLoopbackListen(cfg.Listen) {
		if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
			return nil, fmt.Errorf("non-loopback listen address %q has an incomplete TLS configuration (exactly one of tls_cert_file/tls_key_file is set): configure both or remove both", cfg.Listen)
		}
		if cfg.TLSCertFile == "" && !cfg.AllowInsecureHTTP {
			return nil, fmt.Errorf("non-loopback listen address %q is served without TLS: configure tls_cert_file and tls_key_file (or PRISM_TLS_CERT/PRISM_TLS_KEY), or explicitly allow plaintext with allow_insecure_http: true", cfg.Listen)
		}
		if cfg.TLSCertFile == "" {
			slog.Warn("non-loopback listen address serves plaintext HTTP (allow_insecure_http: true): traffic is unencrypted and any network observer can read credentials", "listen", cfg.Listen)
		}
	}
	// Validate trusted proxies CIDRs. The 0.0.0.0/0 and ::/0 "trust every
	// network" ranges are rejected outright: they make ipTrusted return true
	// for EVERY address, so the XFF chain walk skips every hop and the
	// resolved client IP collapses to RemoteAddr — rate limiting keys on the
	// proxy's address for every proxied client and the trusted-proxy feature
	// silently degrades to no client-IP resolution at all. X-Real-IP never
	// rescues it either: it is accepted ONLY when it parses as an UNtrusted
	// valid IP, and with every network trusted it always falls back to
	// RemoteAddr too (see ratelimit.GetClientIP). A trusted proxy must be a
	// real, bounded network.
	for _, s := range cfg.TrustedProxies {
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: invalid CIDR %q: %v", s, err)
		}
		if ones, bits := cidr.Mask.Size(); ones == 0 {
			// The text names the actual degradation, not a claim that the
			// XFF chain is bypassable: with every address trusted the walk
			// skips every hop, the real client cannot be located, and IP
			// rate limiting degrades to keying on the proxy's RemoteAddr.
			return nil, fmt.Errorf("trusted_proxies: %q trusts the whole %d-bit address space; refusing to load (every hop is trusted, so the real client cannot be located and IP rate limiting degrades to the proxy's RemoteAddr for all proxied traffic) — configure the actual proxy network instead", s, bits)
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

// MaxAccountNameLen is the maximum accepted upstream account-name length
// (bytes). Account names seed the LB_KEY_* credential/env/credstore names
// and the expvar keys, so a sane bound keeps the derived names bounded and
// readable.
const MaxAccountNameLen = 64

// CredentialEnvName derives the credential/env/credstore name of an
// account from its account name: "LB_KEY_" + the name uppercased with
// hyphens folded to underscores ("a-b" and "a_b" both fold to
// "LB_KEY_A_B"). It is the SINGLE implementation of the conversion,
// shared by LoadConfig (the fold-collision check and the credential/env
// lookup), `prism setup` (the interactive cross-provider conflict check
// and the generated credstore/systemd names) and cmd/prism's wiring.
func CredentialEnvName(accountName string) string {
	return "LB_KEY_" + strings.ToUpper(strings.ReplaceAll(accountName, "-", "_"))
}

// ValidateAccountName validates an upstream account name: the audit/usage
// account label, the expvar key prefix (pool_account_<name>_*), and the
// seed of the LB_KEY_* credential / env / credstore file name. Rules:
//   - non-empty;
//   - at most MaxAccountNameLen bytes;
//   - starts with an ASCII letter or digit;
//   - contains only ASCII letters, digits, '_' and '-' — no dots (the
//     expvar key would create a hierarchy), no path separators or ".."
//     (the name seeds file/credstore names), no spaces or non-ASCII (the
//     name appears in systemd unit and shell environment names).
//
// The same rule is enforced interactively by `prism setup` (promptAccounts)
// so a generated config can never be rejected by LoadConfig.
func ValidateAccountName(name string) error {
	if name == "" {
		return fmt.Errorf("name is empty; every account needs a non-empty name (it is the audit account label and the LB_KEY_* credential name)")
	}
	if len(name) > MaxAccountNameLen {
		return fmt.Errorf("name longer than %d bytes is not supported (it seeds the LB_KEY_* credential name and the expvar keys)", MaxAccountNameLen)
	}
	if c := name[0]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
		return fmt.Errorf("name %q must start with an ASCII letter or digit", name)
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return fmt.Errorf("name %q contains invalid character %q: only ASCII letters, digits, '_' and '-' are allowed", name, c)
		}
	}
	return nil
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

// validateBaseURL checks that an account base_url is an absolute http(s)
// URL with a non-empty host (see the LoadConfig call site for the
// config-correctness rationale — deliberately NOT framed as SSRF
// protection, since base_url is operator-controlled). The error names the
// account but never echoes the URL value: a base_url may embed credentials
// (user:pass@host) that must never reach logs.
func validateBaseURL(name, baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("account %q: base_url must be an absolute http(s) URL with a non-empty host (parse error)", name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("account %q: base_url must be an absolute http(s) URL (scheme %q is not supported)", name, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("account %q: base_url must include a non-empty host", name)
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
	if err != nil {
		return false
	}
	// Hostname() strips the port; Parse already lowercases http(s) hosts,
	// and the explicit ToLower makes the match robust for every other
	// scheme/shape. Only the exact ollama.com domain and its subdomains
	// qualify — a suffix match on the raw host would also accept
	// "evilollama.com" and "ollama.com.evil.example".
	host := strings.ToLower(u.Hostname())
	return host == "ollama.com" || strings.HasSuffix(host, ".ollama.com")
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
//  3. the conservative built-in default (DefaultAccountConcurrency)
//
// The old model-name heuristics ("flash"/"pro" substrings → DeepSeek v4
// tiers) are gone: they misjudged the provider from an arbitrary client-
// supplied model name (any "*-pro" model was capped like a DeepSeek v4
// tier) and could oversubscribe an unrelated upstream. The operator's
// explicit max_concurrent_per_account configuration is the only way to
// raise the cap; an unknown non-empty model logs a warning so the missing
// configuration is visible. An empty model is the internal no-model case
// (model cache fetches) and silently returns the default so background
// fetches do not spam the log.
//
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
	if model != "" {
		slog.Warn("unknown model, using default concurrency", "model", model)
	}
	return DefaultAccountConcurrency
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
//  3. no positive values → the conservative built-in default
//     (DefaultAccountConcurrency), shared with the business-model path.
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
	return DefaultAccountConcurrency
}

// ResolveAccountTotalCap returns the per-account AGGREGATE concurrency cap
// across all models (applied to every account; see
// max_concurrent_per_account_total). Resolution order:
//  1. the explicit max_concurrent_per_account_total when positive;
//  2. otherwise the SUM of the distinct positive max_concurrent_per_account
//     values — the backward-compatible default: the old per-max-value
//     accounting kept one counter per distinct max value, bounding the
//     account at exactly that sum, so an unconfigured deployment sees the
//     same worst-case total while per-model counters still isolate models
//     (same-max models share the aggregate budget, never a counter);
//  3. no positive values → DefaultAccountConcurrency (the old single-shared-
//     counter bound).
//
// The sum is clamped to math.MaxInt32 (the int32 total counter cannot hold
// more; an operator-configurable bound above it is meaningless anyway). The
// per-model entries themselves were validated <= math.MaxInt32 at load.
func ResolveAccountTotalCap(cfg *Config) int {
	if cfg != nil && cfg.MaxConcurrentPerAccountTotal > 0 {
		return cfg.MaxConcurrentPerAccountTotal
	}
	sum := 0
	seen := make(map[int]bool)
	if cfg != nil {
		for _, v := range cfg.MaxConcurrentPerAccount {
			if v > 0 && !seen[v] {
				seen[v] = true
				sum += v
			}
		}
	}
	if sum == 0 {
		return DefaultAccountConcurrency
	}
	if sum > math.MaxInt32 {
		return math.MaxInt32
	}
	return sum
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
	// Accounts are NOT hot-reloaded: the pool is built once at startup, so
	// publishing a config whose Accounts differ from the running pool would
	// split the holder from the pool (new accounts never served, removed
	// accounts still served, model-cache provider set stale). The reload is
	// NOT rejected as a whole — every other field (model remap, tiers,
	// strip_fields, api_keys, log level, debug, ...) still hot-reloads — but
	// the running accounts configuration is KEPT, with a clear warning, so
	// the holder and the pool can never diverge. Account changes apply on
	// restart only.
	if !accountsEqual(oldCfg.Accounts, newCfg.Accounts) {
		warnings = append(warnings, "accounts changed: keeping the running accounts configuration (restart required to apply account changes)")
		newCfg.Accounts = cloneAccounts(oldCfg.Accounts)
		// providerSchema is derived from account base_url hosts: rebuild it
		// from the KEPT accounts so EffortSchema stays consistent with the
		// running pool.
		newCfg.providerSchema = buildProviderSchema(newCfg.Accounts)
		// default_provider must reference a provider that exists in the
		// running accounts; if the new config's default_provider only exists
		// in the (discarded) new accounts, keep the old default_provider and
		// say so — a dangling default would route header-less requests into
		// no_healthy.
		if newCfg.DefaultProvider != "" && !hasProviderIn(newCfg.Accounts, newCfg.DefaultProvider) {
			warnings = append(warnings, fmt.Sprintf(
				"default_provider %q is not among the running accounts: keeping the previous default_provider %q",
				newCfg.DefaultProvider, oldCfg.DefaultProvider))
			newCfg.DefaultProvider = oldCfg.DefaultProvider
		}
	}
	if oldCfg.ProbeInterval != newCfg.ProbeInterval {
		warnings = append(warnings, "probe_interval changed: restart required to take effect")
	}
	// The account AGGREGATE concurrency cap is a pool-construction property
	// (NewPoolWithTotalCap at startup): the per-model max values hot-reload
	// (they are resolved per request), but the total bound does not — warn
	// so a silent divergence between the config and the running pool is
	// visible. (max_concurrent_per_account per-model values themselves stay
	// hot-reloadable; only the aggregate needs a restart.)
	if oldCfg.MaxConcurrentPerAccountTotal != newCfg.MaxConcurrentPerAccountTotal {
		warnings = append(warnings, "max_concurrent_per_account_total changed: restart required to take effect")
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

// cloneAccounts returns a deep-enough copy of an account slice: every
// account is copied by value and the Headers map is copied (the only
// reference field that the running config could mutate via the holder).
func cloneAccounts(accs []AccountConfig) []AccountConfig {
	out := make([]AccountConfig, len(accs))
	for i, a := range accs {
		if a.Headers != nil {
			a.Headers = stringMapClone(a.Headers)
		}
		out[i] = a
	}
	return out
}

// hasProviderIn reports whether any account in accs belongs to the given
// provider name.
func hasProviderIn(accs []AccountConfig, name string) bool {
	for _, a := range accs {
		if a.Provider == name {
			return true
		}
	}
	return false
}

// stringMapClone copies a string map.
func stringMapClone(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
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

// IsLoopbackListen returns true if addr binds to a loopback interface.
// Loopback IPs (127.0.0.0/8, ::1, IPv4-mapped forms) AND the hostname
// "localhost" (any case — "LOCALHOST", "LocalHost", ...) count as
// loopback. Empty hosts, "0.0.0.0", "::", hostnames other than localhost
// and unparseable addresses are NOT loopback (fail-safe: a parse error
// never relaxes a security check). It is the single listen-loopback
// implementation shared by LoadConfig and `prism setup` (setup refuses to
// generate a config for a non-loopback listen).
func IsLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// ParseTrustedProxies parses a list of CIDR strings into *net.IPNet values.
// This is a helper for main.go to use after loading config. The
// whole-address-space ranges 0.0.0.0/0 and ::/0 are rejected (same rule as
// LoadConfig): with every address "trusted" the XFF chain walk finds no
// untrusted client hop and client-IP resolution collapses to RemoteAddr
// (the proxy itself), silently disabling per-client rate limiting for all
// proxied traffic.
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
		if ones, _ := cidr.Mask.Size(); ones == 0 {
			return nil, fmt.Errorf("trusted_proxies: %q trusts the whole address space; refusing (every hop is trusted, so the real client cannot be located and rate limiting degrades to RemoteAddr/proxy-address aggregation)", s)
		}
		parsed = append(parsed, cidr)
	}
	return parsed, nil
}
