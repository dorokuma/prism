package config

import "time"

var (
	// MaxProbeAttempts is the maximum number of probe attempts per account.
	MaxProbeAttempts = 3

	// ProbeRetryDelay is the delay between probe retries.
	ProbeRetryDelay = 2 * time.Second

	// AccountSelectTimeout is the timeout for account selection.
	// A variable (not a const) so tests can shrink it to milliseconds (see
	// SetAccountSelectTimeoutForTest in internal/proxy/export_test.go);
	// the default 30s is unchanged.
	AccountSelectTimeout = 30 * time.Second

	// QuotaReviveAfter is how long a quota-exhausted account stays
	// exhausted before a successful probe HTTP 200 may MarkHealthy.
	// A variable (not a const) so tests can set it to 0 or a short
	// duration; the default 6h is unchanged.
	QuotaReviveAfter = 6 * time.Hour

	// DefaultModelCacheRefreshInterval is the default periodic interval for
	// model cache background refresh (3 hours).
	DefaultModelCacheRefreshInterval = 3 * time.Hour

	// ModelCacheMinInterval is the minimum accepted non-zero refresh interval (1 minute).
	// Values below this trigger a warning and fall back to the default (3h).
	ModelCacheMinInterval = 1 * time.Minute

	// ModelCacheBackoffInitial is the initial exponential backoff duration for failed background refreshes (30s).
	ModelCacheBackoffInitial = 30 * time.Second

	// ModelCacheBackoffMax is the cap on exponential backoff for failed background refreshes (1h).
	ModelCacheBackoffMax = 1 * time.Hour
)

const (
	// DefaultModelCacheRefreshStrategy is the default refresh strategy ("full").
	DefaultModelCacheRefreshStrategy = "full"

	// ModelCacheRefreshStrategyFull refreshes every unique provider on each tick.
	ModelCacheRefreshStrategyFull = "full"

	// ModelCacheRefreshStrategyStale refreshes only missing or stale providers on each tick.
	ModelCacheRefreshStrategyStale = "stale"
)

const (

	// DeepseekV4ProConcurrency is the concurrency limit for DeepSeek v4 (official × 90% safety margin).
	// Kept for compatibility (exported constant); the built-in DEFAULT per-account
	// concurrency is now the conservative DefaultAccountConcurrency — model-name
	// heuristics no longer guess a provider.
	DeepseekV4ProConcurrency = 500

	// DeepseekV4FlashConcurrency is the concurrency limit for DeepSeek v4 flash.
	// Kept for compatibility (exported constant); see DeepseekV4ProConcurrency.
	DeepseekV4FlashConcurrency = 2500

	// DefaultConcurrencyRatio is the default concurrency ratio (90%).
	// Kept for compatibility (exported constant); see DeepseekV4ProConcurrency.
	DefaultConcurrencyRatio = 90

	// DefaultAccountConcurrency is the conservative built-in per-account
	// concurrency cap used when no max_concurrent_per_account entry matches
	// the model or the "*" wildcard. 8 concurrent requests per account is a
	// safe default for any upstream (including personal API keys with low
	// rate limits); operators that want more MUST configure it explicitly.
	// It deliberately does NOT depend on the model name: guessing a provider
	// or tier from arbitrary model-name substrings misclassified models
	// (e.g. any "*-pro" model was treated as a DeepSeek v4 tier) and could
	// oversubscribe an unrelated upstream.
	DefaultAccountConcurrency = 8

	// ProbeTimeout is the timeout for model probes.
	ProbeTimeout = 30 * time.Second

	// RateLimitPerSecond is the default rate limit (requests per second).
	RateLimitPerSecond = 60

	// RateLimitBurst is the default rate limit burst.
	RateLimitBurst = 100

	// RateLimitIdleTTL is the TTL for idle rate limit entries.
	RateLimitIdleTTL = 10 * time.Minute

	// UpstreamTimeout is the default upstream request timeout.
	UpstreamTimeout = 10 * time.Minute

	// StreamMaxDuration is the maximum duration for a streaming response.
	StreamMaxDuration = 1 * time.Hour

	// UpstreamRetryDelay is the delay between upstream retries.
	UpstreamRetryDelay = 200 * time.Millisecond

	// MaxErrorBodyBytes is the maximum bytes to read from an error response body.
	MaxErrorBodyBytes = 1 << 20

	// MaxUpstreamResponseBytesDefault is the default cap for a non-streaming
	// upstream response body (32 MiB). Responses larger than this are
	// rejected with HTTP 502 response_too_large instead of being buffered
	// whole into memory. Configurable via max_upstream_response_bytes.
	MaxUpstreamResponseBytesDefault = 32 << 20

	// MaxUpstreamResponseBytesLimit is the hard upper bound for
	// max_upstream_response_bytes (256 MiB). LoadConfig rejects values above
	// it. The cap exists so the read helper's max+1 probe can never overflow
	// int64 (values beyond math.MaxInt64-1 would wrap the LimitReader limit
	// into a negative number and silently return an empty body); production
	// values are bounded by this constant, and readResponseBodyLimited also
	// defends the boundary itself.
	MaxUpstreamResponseBytesLimit = 256 << 20

	// RateLimitMaxBuckets is the production default cap on rate-limiter
	// buckets (distinct client IPs). At the cap a new IP is rejected
	// outright (no O(n) eviction scan under the lock); existing buckets are
	// untouched and idle ones are reaped by the cleanup loop.
	RateLimitMaxBuckets = 100000

	// RedactJSONMaxDepth is the maximum depth for JSON redaction.
	RedactJSONMaxDepth = 20

	// McpCacheTTL is the TTL for MCP tool cache.
	McpCacheTTL = 30 * time.Minute

	// SystemPromptMaxRunes is the maximum rune length for system prompts.
	SystemPromptMaxRunes = 12000

	// TruncationSuffix is the suffix appended to truncated content.
	TruncationSuffix = "\n\n[... truncated for upstream compatibility]"

	// StreamScannerInitialBuf is the initial buffer size for the stream scanner.
	StreamScannerInitialBuf = 64 * 1024

	// StreamScannerMaxBuf is the maximum buffer size for the stream scanner.
	StreamScannerMaxBuf = 4 * 1024 * 1024
)
