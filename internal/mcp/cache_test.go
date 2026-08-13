package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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

// TestMCPCache_UnauthenticatedBucketIsReadOnly pins the auth-disabled
// isolation: with no authenticated API key the request identity is the
// fixed mcp.UnauthenticatedIdentity, whose bucket is READ-ONLY —
// cacheMCPTool refuses to write it and getTenantMCPTools returns only the
// shared admin-injected tools. Different local clients can therefore never
// pollute each other's cached tools when auth is disabled.
func TestMCPCache_UnauthenticatedBucketIsReadOnly(t *testing.T) {
	snap := cacheSnapshot()
	ClearMCPCache()
	defer restoreCache(snap)

	cacheAdminTool(toolWithName("admin__global"))
	// Request-path writes under the unauthenticated identity are dropped.
	cacheMCPTool(UnauthenticatedIdentity, toolWithName("anon__leak"))
	cacheMCPTool("", toolWithName("empty__leak")) // empty → "default", NOT unauthenticated

	// The unauthenticated identity sees ONLY the admin bucket.
	got := toolNames(getTenantMCPTools(UnauthenticatedIdentity))
	if len(got) != 1 || !got["admin__global"] {
		t.Errorf("unauthenticated identity must see only the admin tools, got %v", got)
	}

	// A second unauthenticated "client" sees exactly the same admin-only
	// set — nothing the first client cached can leak to it.
	got2 := toolNames(getTenantMCPTools(UnauthenticatedIdentity))
	if len(got2) != 1 || !got2["admin__global"] {
		t.Errorf("second unauthenticated identity must see only the admin tools, got %v", got2)
	}

	// The dropped write must not leak into ANY writable bucket either.
	for _, id := range []string{"default", "key-a"} {
		names := toolNames(getTenantMCPTools(id))
		if names["anon__leak"] {
			t.Errorf("identity %q must not see the dropped unauthenticated write", id)
		}
	}
}

// TestMCPCache_WriteSnapshotsCallerMap pins the write-path snapshot: the
// cache must store an INDEPENDENT copy of the caller's tool map, not the
// map by reference. Mutating the original map (or its nested "function"
// schema) AFTER caching must not change the cached entry — otherwise a
// caller's later edits would race with cache readers and could grow the
// cached tool past the serialized-size gate that was passed at write time.
func TestMCPCache_WriteSnapshotsCallerMap(t *testing.T) {
	snap := cacheSnapshot()
	ClearMCPCache()
	defer restoreCache(snap)

	// A tool with a nested schema, cached near the size cap.
	tool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "snap_tool",
			"description": strings.Repeat("d", MaxToolSerializedBytes-2048),
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
			},
		},
	}
	cacheMCPTool("key-a", tool)

	// Mutate the caller's map after the write: rename the function, add a
	// nested property, and grow the description beyond the size cap.
	fn := tool["function"].(map[string]any)
	fn["name"] = "snap_mutated"
	fn["description"] = strings.Repeat("x", MaxToolSerializedBytes+4096)
	params := fn["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	props["sneaky"] = map[string]any{"type": "string"}

	cached := getTenantMCPTools("key-a")
	if len(cached) != 1 {
		t.Fatalf("cached tools = %d, want 1", len(cached))
	}
	cachedFn, ok := cached[0]["function"].(map[string]any)
	if !ok {
		t.Fatal("cached tool has no function object")
	}
	if name, _ := cachedFn["name"].(string); name != "snap_tool" {
		t.Errorf("cached function name = %q, want %q (the cache must hold a snapshot, not the caller's map)", name, "snap_tool")
	}
	if desc, _ := cachedFn["description"].(string); len(desc) != MaxToolSerializedBytes-2048 {
		t.Errorf("cached description length = %d, want %d (later caller edits must not change the cached entry)", len(desc), MaxToolSerializedBytes-2048)
	}
	cachedParams, ok := cachedFn["parameters"].(map[string]any)
	if !ok {
		t.Fatal("cached tool has no parameters object")
	}
	cachedProps, ok := cachedParams["properties"].(map[string]any)
	if !ok {
		t.Fatal("cached tool has no properties object")
	}
	if _, ok := cachedProps["sneaky"]; ok {
		t.Error("nested schema edits after the write leaked into the cache (shallow copy of the outer map is not enough)")
	}
	if _, ok := cachedProps["query"]; !ok {
		t.Error("the original nested schema must survive the snapshot")
	}

	// The cached entry must stay under the serialized-size cap even though
	// the caller grew its map far past it after the write.
	raw, err := json.Marshal(cached[0])
	if err != nil {
		t.Fatalf("marshal cached tool: %v", err)
	}
	if len(raw) > MaxToolSerializedBytes {
		t.Errorf("cached tool size = %d, want <= %d (caller edits after the write must not push the cache past the gate)", len(raw), MaxToolSerializedBytes)
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

// TestMCPCache_SingleToolSizeLimit pins the per-tool serialized-size gate: a
// tool whose JSON representation exceeds MaxToolSerializedBytes is NOT
// cached (the per-tenant count cap alone cannot bound memory when one tool
// carries a huge description/schema), smaller tools are unaffected, and the
// oversized tool must not be visible to any identity.
func TestMCPCache_SingleToolSizeLimit(t *testing.T) {
	snap := cacheSnapshot()
	ClearMCPCache()
	defer restoreCache(snap)

	// A tool over the 1 MiB cap (big description).
	huge := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "huge_tool",
			"description": strings.Repeat("a", MaxToolSerializedBytes+1024),
		},
	}
	cacheMCPTool("key-a", huge)
	// A tool just under the cap still caches (exercises the boundary).
	near := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "near_tool",
			"description": strings.Repeat("b", MaxToolSerializedBytes-2048),
		},
	}
	cacheMCPTool("key-a", near)
	cacheMCPTool("key-a", toolWithName("small_tool"))

	a := toolNames(getTenantMCPTools("key-a"))
	if a["huge_tool"] {
		t.Error("the oversized tool must NOT be cached")
	}
	if !a["near_tool"] {
		t.Error("a tool under the size cap must still be cached")
	}
	if !a["small_tool"] {
		t.Error("a small tool must still be cached")
	}
	// The oversized tool must not leak into any other identity either.
	if names := toolNames(getTenantMCPTools("key-b")); names["huge_tool"] {
		t.Error("the oversized tool must not be visible to other identities")
	}
}

// TestMCPCache_ConcurrentWrites pins the concurrency behavior of the
// lock-scoped cacheTool: N goroutines caching distinct tools into one
// bucket concurrently must all land (count == N, no lost updates — the
// dedupe/cap/append all happen under the lock), and concurrent duplicate
// writes of the SAME tool name must dedupe to exactly one entry. Run with
// -race to prove the marshal-outside-the-lock refactor introduced no data
// race.
func TestMCPCache_ConcurrentWrites(t *testing.T) {
	snap := cacheSnapshot()
	ClearMCPCache()
	defer restoreCache(snap)

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("conc_tool_%02d", i)
			cacheMCPTool("key-a", toolWithName(name))
			// Concurrent duplicate of the same name: must dedupe to one.
			cacheMCPTool("key-a", toolWithName(name))
		}(i)
	}
	wg.Wait()

	tools := getTenantMCPTools("key-a")
	if len(tools) != n {
		t.Fatalf("tools after %d concurrent writers = %d, want %d (no lost updates)", n, len(tools), n)
	}
	names := toolNames(tools)
	for i := 0; i < n; i++ {
		if !names[fmt.Sprintf("conc_tool_%02d", i)] {
			t.Errorf("tool conc_tool_%02d missing after concurrent writes", i)
		}
	}

	// Concurrent oversized writes are dropped without corrupting the bucket.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			huge := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "conc_huge",
					"description": strings.Repeat("x", MaxToolSerializedBytes+1024),
				},
			}
			cacheMCPTool("key-a", huge)
		}()
	}
	wg.Wait()
	if got := len(getTenantMCPTools("key-a")); got != n {
		t.Errorf("tools after concurrent oversized writes = %d, want %d (oversized tools must not consume slots)", got, n)
	}
}

// TestMCPCache_SizeLimitDoesNotReplaceCountCap guards the interaction with
// the per-tenant count cap: the size gate drops oversized tools, the count
// cap (100 tools per tenant) still applies to the surviving tools, and an
// oversized tool is simply skipped (count not consumed).
func TestMCPCache_SizeLimitDoesNotReplaceCountCap(t *testing.T) {
	snap := cacheSnapshot()
	ClearMCPCache()
	defer restoreCache(snap)

	// Fill 100 small tools → the 101st is dropped by the COUNT cap.
	for i := 0; i < 100; i++ {
		cacheMCPTool("key-a", toolWithName(fmt.Sprintf("t%02d", i)))
	}
	cacheMCPTool("key-a", toolWithName("t101"))
	if got := len(getTenantMCPTools("key-a")); got != 100 {
		t.Fatalf("tools after count cap = %d, want 100 (count cap must still apply)", got)
	}

	// An oversized tool does not consume a count slot either: the bucket
	// still holds exactly the 100 small tools.
	huge := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "huge_tool",
			"description": strings.Repeat("a", MaxToolSerializedBytes+1024),
		},
	}
	cacheMCPTool("key-a", huge)
	if got := len(getTenantMCPTools("key-a")); got != 100 {
		t.Errorf("tools after oversized write = %d, want 100 (oversized tool must not consume a slot)", got)
	}
}

// TestMCPCache_SameNameReplacesSnapshot pins same-name writes: a second
// cache of the same function name must replace the stored snapshot (a later
// parameter schema is visible), not keep the first write forever. A replace
// still works when the bucket is already at the 100-tool cap.
func TestMCPCache_SameNameReplacesSnapshot(t *testing.T) {
	snap := cacheSnapshot()
	ClearMCPCache()
	defer restoreCache(snap)

	cacheMCPTool("key-a", map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "echo",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"old": map[string]any{"type": "string"}},
			},
		},
	})
	cacheMCPTool("key-a", map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "echo",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"new": map[string]any{"type": "string"}},
			},
		},
	})

	tools := getTenantMCPTools("key-a")
	if len(tools) != 1 {
		t.Fatalf("tools after same-name rewrite = %d, want 1", len(tools))
	}
	fn, ok := tools[0]["function"].(map[string]any)
	if !ok {
		t.Fatal("cached tool has no function object")
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatal("cached tool has no parameters object")
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("cached tool has no properties object")
	}
	if _, ok := props["new"]; !ok {
		t.Error("second write's schema (property \"new\") must be visible after same-name replace")
	}
	if _, ok := props["old"]; ok {
		t.Error("first write's schema (property \"old\") must not survive a same-name replace")
	}

	ClearMCPCache()
	for i := 0; i < 100; i++ {
		cacheMCPTool("key-a", toolWithName(fmt.Sprintf("t%02d", i)))
	}
	cacheMCPTool("key-a", map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":   "t00",
			"schema": "v2",
		},
	})
	got := getTenantMCPTools("key-a")
	if len(got) != 100 {
		t.Fatalf("tools after in-cap replace = %d, want 100", len(got))
	}
	replaced := false
	for _, tool := range got {
		if toolFunctionName(tool) != "t00" {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			t.Fatal("t00 has no function object")
		}
		if fn["schema"] != "v2" {
			t.Errorf("t00 schema = %v, want v2 (100-cap must not block a same-name replace)", fn["schema"])
		}
		replaced = true
	}
	if !replaced {
		t.Fatal("t00 missing after same-name replace at the 100-tool cap")
	}
}
