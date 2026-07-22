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

func cacheMCPTool(tenantID string, tool map[string]any) {
	if tenantID == "" {
		tenantID = "default"
	}
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()

	tc, ok := mcpCache[tenantID]
	if !ok {
		tc = &tenantCache{}
		mcpCache[tenantID] = tc
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

func getTenantMCPTools(tenantID string) []map[string]any {
	if tenantID == "" {
		tenantID = "default"
	}
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()

	tc, ok := mcpCache[tenantID]
	if !ok {
		return nil
	}
	tc.lastAccess = time.Now()
	out := make([]map[string]any, len(tc.tools))
	copy(out, tc.tools)
	return out
}
