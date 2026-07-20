package main

import "time"

const (
	// Probe
	maxProbeAttempts = 3
	probeRetryDelay  = 2 * time.Second
	probeTimeout     = 30 * time.Second

	// Rate limiting
	rateLimitPerSecond = 60
	rateLimitBurst     = 100
	rateLimitIdleTTL   = 10 * time.Minute

	// Upstream / proxy
	upstreamTimeout      = 10 * time.Minute
	streamMaxDuration    = 1 * time.Hour
	upstreamRetryDelay   = 200 * time.Millisecond
	accountSelectTimeout = 30 * time.Second
	maxErrorBodyBytes    = 1 << 20
	redactJSONMaxDepth   = 20

	// MCP cache
	mcpCacheTTL = 30 * time.Minute

	// System prompt
	systemPromptMaxRunes = 12000

	// Stream scanner
	streamScannerInitialBuf = 64 * 1024
	streamScannerMaxBuf     = 4 * 1024 * 1024
)
