package mcp

import (
	"context"
	"sync"
	"time"

	"github.com/dorokuma/prism/internal/config"
)

// tenantCache stores MCP tool definitions discovered from Codex namespace bundles.
// Populated automatically each time a namespace bundle passes through flattenToolEntry.
// No disk file needed — rebuilt from requests after restart.
type tenantCache struct {
	tools      []map[string]any
	lastAccess time.Time
}

var (
	mcpCache          = make(map[string]*tenantCache)
	mcpCacheMu        sync.Mutex
	mcpCacheCtxCancel context.CancelFunc
)

func init() {
	var ctx context.Context
	ctx, mcpCacheCtxCancel = context.WithCancel(context.Background())
	go mcpCacheEvictLoop(ctx)
}

func mcpCacheEvictLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
		}
		mcpCacheMu.Lock()
		now := time.Now()
		for tenantID, tc := range mcpCache {
			if now.Sub(tc.lastAccess) > config.McpCacheTTL {
				delete(mcpCache, tenantID)
			}
		}
		mcpCacheMu.Unlock()
	}
}

// StopMCPCache stops the background cache eviction goroutine.
// Safe to call multiple times.
func StopMCPCache() {
	if mcpCacheCtxCancel != nil {
		mcpCacheCtxCancel()
	}
}

// cacheMCPTool caches a tool under the given REQUEST identity (the
// authenticated API key NAME — see proxy.getTenantID — stable and
// non-secret, never the raw token). An empty identity maps to the "default"
// anonymous bucket; an identity equal to the reserved admin key
// (config.McpAdminIdentity) is dropped outright — the request path must
// NEVER write into the shared admin-injected bucket (config validation
// forbids such key names; this guard covers programmatically built key
// sets). Tools cached under one identity are never returned to another
// (per-identity isolation).
func cacheMCPTool(identity string, tool map[string]any) {
	if identity == "" {
		identity = "default"
	}
	if identity == config.McpAdminIdentity {
		return
	}
	cacheTool(identity, tool)
}

// cacheAdminTool caches a tool in the shared admin-injected bucket
// (config.McpAdminIdentity, populated by LoadMCPTools from mcp_tools.json).
// Only the admin configuration path calls it; request-path identities can
// never write here (see cacheMCPTool).
func cacheAdminTool(tool map[string]any) {
	cacheTool(config.McpAdminIdentity, tool)
}

// cacheTool is the shared bucket-write implementation: it deduplicates by
// function name and caps each bucket at 100 tools to prevent memory
// exhaustion. Must be called with the resolved cache key (never empty).
func cacheTool(key string, tool map[string]any) {
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()

	tc, ok := mcpCache[key]
	if !ok {
		tc = &tenantCache{}
		mcpCache[key] = tc
	}
	tc.lastAccess = time.Now()

	if len(tc.tools) >= 100 {
		return // limit to 100 tools per tenant to prevent memory exhaustion
	}

	for _, existing := range tc.tools {
		if fn, ok := existing["function"].(map[string]any); ok {
			if nf, ok := tool["function"].(map[string]any); ok {
				if fn["name"] == nf["name"] {
					return // already cached
				}
			}
		}
	}
	tc.tools = append(tc.tools, tool)
}

// ClearMCPCache clears all cached MCP tools for all tenants.
func ClearMCPCache() {
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()
	mcpCache = make(map[string]*tenantCache)
}

// getTenantMCPTools returns the MCP tools visible to the given identity:
// the shared admin-injected bucket (config.McpAdminIdentity, populated by
// LoadMCPTools from mcp_tools.json) merged with the identity's own
// request-cached tools. Per-identity tools are isolated — tools cached
// under one identity are never returned to another — while the
// admin-injected definitions stay visible to every identity. On a name
// collision the identity's own tool wins over the shared one. The "default"
// identity is a plain per-client bucket (the anonymous fallback for empty
// identities); it has no special relationship to the admin bucket.
func getTenantMCPTools(identity string) []map[string]any {
	if identity == "" {
		identity = "default"
	}
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()

	shared := snapshotTenantToolsLocked(config.McpAdminIdentity)
	own := snapshotTenantToolsLocked(identity)
	if len(own) == 0 {
		return shared
	}
	if len(shared) == 0 {
		return own
	}
	out := make([]map[string]any, 0, len(shared)+len(own))
	seen := make(map[string]bool, len(shared)+len(own))
	// The identity's OWN tools win on a name collision: mark them first and
	// append them before the shared tools, then append the shared tools
	// that are not shadowed.
	for _, t := range own {
		out = append(out, t)
		if n := toolFunctionName(t); n != "" {
			seen[n] = true
		}
	}
	for _, t := range shared {
		n := toolFunctionName(t)
		if n != "" && seen[n] {
			continue
		}
		out = append(out, t)
		if n != "" {
			seen[n] = true
		}
	}
	return out
}

// snapshotTenantToolsLocked copies the tool slice of one cache bucket,
// refreshing its lastAccess (the eviction loop removes buckets idle past
// McpCacheTTL). Must be called with mcpCacheMu held.
func snapshotTenantToolsLocked(identity string) []map[string]any {
	tc, ok := mcpCache[identity]
	if !ok {
		return nil
	}
	tc.lastAccess = time.Now()
	out := make([]map[string]any, len(tc.tools))
	copy(out, tc.tools)
	return out
}

// toolFunctionName returns the "function.name" of a cached tool entry, or
// "" when the entry has no function name (defensive: malformed entries are
// skipped by the dedupe, never returned twice).
func toolFunctionName(tool map[string]any) string {
	if fn, ok := tool["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok {
			return name
		}
	}
	return ""
}
