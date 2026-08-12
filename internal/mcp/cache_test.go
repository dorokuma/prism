package mcp

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/sanitize"
)

// mustMarshal serializes v to a JSON string, failing the test on error.
func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// cacheSnapshot returns a deep-enough copy of the current mcpCache so a test
// can restore it afterwards (mirrors inject_test.go's save/restore pattern).
func cacheSnapshot() map[string]*tenantCache {
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()
	snap := make(map[string]*tenantCache, len(mcpCache))
	for k, v := range mcpCache {
		copied := &tenantCache{
			tools:      make([]map[string]any, len(v.tools)),
			lastAccess: v.lastAccess,
		}
		copy(copied.tools, v.tools)
		snap[k] = copied
	}
	return snap
}

func restoreCache(snap map[string]*tenantCache) {
	mcpCacheMu.Lock()
	mcpCache = snap
	mcpCacheMu.Unlock()
}

func toolWithName(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": name,
		},
	}
}

func toolNames(tools []map[string]any) map[string]bool {
	names := make(map[string]bool, len(tools))
	for _, t := range tools {
		if n := toolFunctionName(t); n != "" {
			names[n] = true
		}
	}
	return names
}

// TestMCPCache_IdentityIsolation pins item 2: tools cached under one API key
// identity are never visible to another identity (dual-tenant isolation).
func TestMCPCache_IdentityIsolation(t *testing.T) {
	snap := cacheSnapshot()
	ClearMCPCache()
	defer restoreCache(snap)

	cacheMCPTool("key-a", toolWithName("ns_a__secret_a"))
	cacheMCPTool("key-b", toolWithName("ns_b__secret_b"))

	a := toolNames(getTenantMCPTools("key-a"))
	if !a["ns_a__secret_a"] {
		t.Error("key-a must see its own cached tool")
	}
	if a["ns_b__secret_b"] {
		t.Error("key-a must NOT see key-b's cached tool (isolation)")
	}
	b := toolNames(getTenantMCPTools("key-b"))
	if !b["ns_b__secret_b"] {
		t.Error("key-b must see its own cached tool")
	}
	if b["ns_a__secret_a"] {
		t.Error("key-b must NOT see key-a's cached tool (isolation)")
	}
	c := toolNames(getTenantMCPTools("key-c"))
	if len(c) != 0 {
		t.Errorf("an identity that never cached anything must see no per-identity tools, got %v", c)
	}
}

// TestMCPCache_SharedAdminToolsVisibleToAllIdentities pins the
// compatibility half of the admin-bucket fix: the shared admin-injected
// bucket (config.McpAdminIdentity, populated by LoadMCPTools from
// mcp_tools.json via cacheAdminTool) stays visible to EVERY identity, while
// per-identity tools stay isolated. On a name collision the identity's own
// tool wins over the shared one.
func TestMCPCache_SharedAdminToolsVisibleToAllIdentities(t *testing.T) {
	snap := cacheSnapshot()
	ClearMCPCache()
	defer restoreCache(snap)

	// LoadMCPTools caches admin tools under the reserved admin identity.
	cacheAdminTool(toolWithName("admin__global"))
	cacheMCPTool("key-a", toolWithName("ns_a__own"))
	// Name collision: key-a's own tool must win over the shared one.
	cacheAdminTool(toolWithName("shared__dup"))
	cacheMCPTool("key-a", map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "shared__dup",
			"own":  true,
		},
	})

	a := toolNames(getTenantMCPTools("key-a"))
	if !a["admin__global"] {
		t.Error("admin-injected tools must stay visible to key-a")
	}
	if !a["ns_a__own"] {
		t.Error("key-a must see its own tools")
	}
	b := toolNames(getTenantMCPTools("key-b"))
	if !b["admin__global"] {
		t.Error("admin-injected tools must stay visible to key-b")
	}
	if b["ns_a__own"] {
		t.Error("key-b must NOT see key-a's tools")
	}

	// The collision: exactly one shared__dup, and it is key-a's own.
	dupCount := 0
	own := false
	for _, t := range getTenantMCPTools("key-a") {
		if n := toolFunctionName(t); n == "shared__dup" {
			dupCount++
			if fn, ok := t["function"].(map[string]any); ok {
				if v, ok := fn["own"].(bool); ok && v {
					own = true
				}
			}
		}
	}
	if dupCount != 1 || !own {
		t.Errorf("shared__dup must appear exactly once with key-a's own entry, got count=%d own=%v", dupCount, own)
	}
}

// TestMCPCache_DefaultIdentityIsPlainBucket pins the legacy-anonymous
// semantics: the "default" identity (the legacy auth_token expansion and
// the empty-name fallback) is a plain per-client bucket — it sees the
// admin-injected tools merged with its own, and its own tools are never
// visible to other identities and never pollute the admin bucket.
func TestMCPCache_DefaultIdentityIsPlainBucket(t *testing.T) {
	snap := cacheSnapshot()
	ClearMCPCache()
	defer restoreCache(snap)

	cacheAdminTool(toolWithName("admin__global"))
	cacheMCPTool("default", toolWithName("legacy__own")) // legacy auth_token / explicit name=default

	d := toolNames(getTenantMCPTools("default"))
	if !d["admin__global"] {
		t.Error("default identity must see the shared admin bucket")
	}
	if !d["legacy__own"] {
		t.Error("default identity must see its own cached tool")
	}
	// The "default" write must NOT leak into the admin bucket: another
	// tenant must never see it.
	other := toolNames(getTenantMCPTools("key-a"))
	if other["legacy__own"] {
		t.Error("default identity's tool must NOT be visible to other identities (it is not an admin tool)")
	}
	if !other["admin__global"] {
		t.Error("admin bucket must stay visible to other identities")
	}
}

// TestMCPCache_RequestPathNeverWritesAdminBucket pins the core isolation
// invariant: cacheMCPTool (the request path) can never write into the
// shared admin bucket — neither the legacy auth_token identity ("default"),
// an explicit empty name, nor an identity that somehow equals the reserved
// admin key (config validation forbids such key names; this is the
// defense-in-depth guard for programmatically built key sets).
func TestMCPCache_RequestPathNeverWritesAdminBucket(t *testing.T) {
	snap := cacheSnapshot()
	ClearMCPCache()
	defer restoreCache(snap)

	cacheAdminTool(toolWithName("admin__global"))
	// Request-path writes under identities that must never pollute admin:
	cacheMCPTool("default", toolWithName("legacy__own"))             // legacy auth_token → name "default"
	cacheMCPTool("", toolWithName("anon__own"))                      // empty name → "default" bucket
	cacheMCPTool(config.McpAdminIdentity, toolWithName("evil__own")) // reserved-name identity → dropped
	cacheMCPTool("key-a", toolWithName("keya__own"))                 // normal identity

	// The admin bucket itself contains ONLY the admin-injected tool.
	admin := toolNames(getTenantMCPTools(config.McpAdminIdentity))
	if len(admin) != 1 || !admin["admin__global"] {
		t.Errorf("admin bucket must contain only admin tools, got %v", admin)
	}
	// No other tenant can see the legacy/anonymous/reserved writes.
	for _, id := range []string{"key-b", "key-c"} {
		names := toolNames(getTenantMCPTools(id))
		if names["legacy__own"] || names["anon__own"] || names["evil__own"] || names["keya__own"] {
			t.Errorf("tenant %q must not see any request-path tool from other identities: %v", id, names)
		}
		if !names["admin__global"] {
			t.Errorf("tenant %q must still see the admin tools: %v", id, names)
		}
	}
	// The reserved-name write was dropped entirely: even the reserved
	// identity itself must not serve it.
	evil := toolNames(getTenantMCPTools(config.McpAdminIdentity))
	if evil["evil__own"] {
		t.Error("a request write under the reserved admin identity must be dropped, never cached")
	}
	// Empty-name writes land in the "default" bucket and stay there.
	def := toolNames(getTenantMCPTools("default"))
	if !def["legacy__own"] || !def["anon__own"] {
		t.Errorf("default bucket must keep the legacy/anonymous tools, got %v", def)
	}
}

// TestSanitizeTools_DeepSchemaFails pins item 5 at the mcp surface: a tool
// parameter schema deeper than the limit fails fast with ErrSchemaTooDeep
// (propagated to the caller → convert → 400 invalid_request) instead of
// recursing unbounded or silently falling back to the unsafe original.
func TestSanitizeTools_DeepSchemaFails(t *testing.T) {
	deep := map[string]any{"type": "object"}
	cur := deep
	for i := 0; i < sanitize.MaxJSONSchemaDepth+10; i++ {
		nested := map[string]any{"type": "object"}
		cur["properties"] = map[string]any{"nested": nested}
		cur = nested
	}
	raw := []byte(`[{"type":"function","name":"deep_tool","parameters":` + mustMarshal(t, deep) + `}]`)

	_, err := SanitizeToolsForChatCompletions(raw, "key-a")
	if !errors.Is(err, sanitize.ErrSchemaTooDeep) {
		t.Fatalf("expected ErrSchemaTooDeep, got %v", err)
	}
}

// TestSanitizeTools_NormalSchemaStillWorks guards the happy path: a normal
// schema (including the blacklist stripping) passes through untouched and
// produces no error.
func TestSanitizeTools_NormalSchemaStillWorks(t *testing.T) {
	raw := []byte(`[{"type":"function","name":"f","parameters":{"type":"object","properties":{"query":{"type":"string"},"justification":{"type":"string"}}}}]`)
	out, err := SanitizeToolsForChatCompletions(raw, "key-a")
	if err != nil {
		t.Fatalf("normal schema must not fail: %v", err)
	}
	items := out.([]map[string]any)
	fn := items[0]["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	if _, ok := props["justification"]; ok {
		t.Error("blacklisted key justification must still be stripped")
	}
	if _, ok := props["query"]; !ok {
		t.Error("legitimate key query must survive")
	}
}
