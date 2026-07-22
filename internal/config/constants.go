package config

import "time"

var (
	// MaxProbeAttempts is the maximum number of probe attempts per account.
	MaxProbeAttempts = 3

	// ProbeRetryDelay is the delay between probe retries.
	ProbeRetryDelay = 2 * time.Second
)

const (
	// DeepseekV4ProConcurrency is the concurrency limit for DeepSeek v4 (official × 90% safety margin).
	DeepseekV4ProConcurrency = 500

	// DeepseekV4FlashConcurrency is the concurrency limit for DeepSeek v4 flash.
	DeepseekV4FlashConcurrency = 2500

	// DefaultConcurrencyRatio is the default concurrency ratio (90%).
	DefaultConcurrencyRatio = 90

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

	// AccountSelectTimeout is the timeout for account selection.
	AccountSelectTimeout = 30 * time.Second

	// MaxErrorBodyBytes is the maximum bytes to read from an error response body.
	MaxErrorBodyBytes = 1 << 20

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
