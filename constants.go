package main

import (
	"github.com/dorokuma/prism/internal/config"
)

var (
	// Probe
	maxProbeAttempts = config.MaxProbeAttempts
	probeRetryDelay  = config.ProbeRetryDelay
)

const (
	// Concurrency limits for DeepSeek v4 (official × 90% safety margin)
	deepseekV4ProConcurrency   = config.DeepseekV4ProConcurrency
	deepseekV4FlashConcurrency = config.DeepseekV4FlashConcurrency
	defaultConcurrencyRatio    = config.DefaultConcurrencyRatio

	probeTimeout = config.ProbeTimeout

	// Rate limiting
	rateLimitPerSecond = config.RateLimitPerSecond
	rateLimitBurst     = config.RateLimitBurst
	rateLimitIdleTTL   = config.RateLimitIdleTTL

	// Upstream / proxy
	upstreamTimeout      = config.UpstreamTimeout
	streamMaxDuration    = config.StreamMaxDuration
	upstreamRetryDelay   = config.UpstreamRetryDelay
	accountSelectTimeout = config.AccountSelectTimeout
	maxErrorBodyBytes    = config.MaxErrorBodyBytes
	redactJSONMaxDepth   = config.RedactJSONMaxDepth

	// MCP cache
	mcpCacheTTL = config.McpCacheTTL

	// System prompt
	systemPromptMaxRunes = config.SystemPromptMaxRunes
	truncationSuffix     = config.TruncationSuffix

	// Stream scanner
	streamScannerInitialBuf = config.StreamScannerInitialBuf
	streamScannerMaxBuf     = config.StreamScannerMaxBuf
)
