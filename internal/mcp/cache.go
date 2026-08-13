package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
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

// UnauthenticatedIdentity is the MCP tool-cache identity for requests that
// carried NO authenticated API key (auth disabled, or the request bypassed
// the auth middleware). It is a fixed, stable, non-secret label — never
// derived from credentials (there are none) and never from request data that
// a local client could forge to impersonate another client. The bucket is
// deliberately READ-ONLY (cacheMCPTool refuses to write it), so with auth
// disabled different local clients can never pollute each other's cached
// tools: nobody can write, and every unauthenticated request sees only the
// shared admin-injected bucket. It aliases config.McpUnauthenticatedIdentity
// — the reserved string lives in config (which must not import mcp) so
// LoadConfig can reject api_keys entries that would collide with it.
const UnauthenticatedIdentity = config.McpUnauthenticatedIdentity

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
// sets). The unauthenticated identity (UnauthenticatedIdentity) is also
// refused: with auth disabled there is no per-client identity, and caching
// under one shared label would let different local clients pollute each
// other's tool views. Tools cached under one identity are never returned to
// another (per-identity isolation).
func cacheMCPTool(identity string, tool map[string]any) {
	if identity == "" {
		identity = "default"
	}
	if identity == config.McpAdminIdentity {
		return
	}
	if identity == UnauthenticatedIdentity {
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

// MaxToolSerializedBytes caps the serialized size of ONE cached MCP tool
// (JSON). A tool whose definition (description + parameter schema) exceeds
// the cap is NOT cached — the per-tenant tool-count limit (100) alone is not
// a memory bound, because a single tool can carry a huge description or
// schema. Oversized tools are dropped with a size-only warning (the tool
// body itself is never logged).
const MaxToolSerializedBytes = 1 << 20 // 1 MiB

// cacheTool is the shared bucket-write implementation: it replaces an
// existing tool of the same function name (so a later schema is visible),
// caps each bucket at 100 tools, and caps the serialized size of a SINGLE
// tool (MaxToolSerializedBytes — an oversized tool is dropped, never
// truncated, never logged in full). Must be called with the resolved cache
// key (never empty).
//
// Lock discipline: JSON serialization, the size check and the snapshot
// copy all run BEFORE the global cache lock — marshaling a huge tool (giant
// description or schema) is CPU/memory work that must not serialize every
// other cache operation behind mcpCacheMu, and the snapshot copy is part of
// the same pre-lock serialization stage. The lock is held only for the
// final read-modify-write on the bucket (create, same-name replace, 100-cap,
// append), which keeps the concurrency semantics unchanged: the replace and
// capacity decisions always observe the latest bucket state.
//
// The caller's map is NEVER stored by reference: flattenToolEntry (or the
// admin config path) may keep mutating the tool map after this call, which
// would race with cache readers and could grow the cached entry past the
// serialized-size gate that was just passed. The tool is therefore
// snapshotted into an independent map (a JSON round-trip — the same codec
// the size gate measured, so the snapshot is exactly what was checked)
// before anything is appended.
func cacheTool(key string, tool map[string]any) {
	// Single-tool serialized-size gate BEFORE the lock and before anything
	// is stored: a huge tool (giant description or parameter schema) would
	// otherwise defeat the per-tenant count cap — 100 tools × unbounded
	// size is unbounded memory. Serialization also validates that the tool
	// is JSON-safe.
	raw, err := json.Marshal(tool)
	if err != nil {
		slog.Warn("mcp tool not cacheable (serialization failed), not cached", "identity", key, "error", err)
		return
	}
	if len(raw) > MaxToolSerializedBytes {
		// Size-only log: the tool name is safe metadata, the tool BODY
		// (description/schema, potentially huge or sensitive) is never
		// written to the log.
		slog.Warn("mcp tool too large, not cached", "identity", key, "tool", toolFunctionName(tool), "size_bytes", len(raw), "max_bytes", MaxToolSerializedBytes)
		return
	}
	// Snapshot the tool into an independent map (full JSON decode — a
	// shallow copy of the outer map would still share the nested
	// "function" schema with the caller). Still outside the lock: the copy
	// is the same order of work as the marshal above.
	snapshot := make(map[string]any)
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		slog.Warn("mcp tool not cacheable (snapshot failed), not cached", "identity", key, "error", err)
		return
	}

	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()

	tc, ok := mcpCache[key]
	if !ok {
		tc = &tenantCache{}
		mcpCache[key] = tc
	}
	tc.lastAccess = time.Now()

	// Same function name: replace the stored snapshot so a later schema
	// is visible. Do this BEFORE the 100-cap check so an update of an
	// already-cached tool is not dropped when the bucket is full.
	for i, existing := range tc.tools {
		fn, ok1 := existing["function"].(map[string]any)
		nf, ok2 := snapshot["function"].(map[string]any)
		if ok1 && ok2 && fn["name"] == nf["name"] {
			tc.tools[i] = snapshot
			return
		}
	}
	if len(tc.tools) >= 100 {
		return // limit to 100 tools per tenant to prevent memory exhaustion
	}
	tc.tools = append(tc.tools, snapshot)
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
// identities); it has no special relationship to the admin bucket. The
// unauthenticated identity (UnauthenticatedIdentity) sees ONLY the shared
// admin bucket: its bucket is read-only (see cacheMCPTool), so there is
// nothing per-client to merge.
func getTenantMCPTools(identity string) []map[string]any {
	if identity == "" {
		identity = "default"
	}
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()

	if identity == UnauthenticatedIdentity {
		return snapshotTenantToolsLocked(config.McpAdminIdentity)
	}

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
// skipped by the same-name replace, never returned twice).
func toolFunctionName(tool map[string]any) string {
	if fn, ok := tool["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok {
			return name
		}
	}
	return ""
}
