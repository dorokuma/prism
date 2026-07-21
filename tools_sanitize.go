package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// splitNamespaceTool splits "mcp__codegraph__search" into ("mcp__codegraph", "search").
// Uses the last "__" as the boundary. Tool names must not contain "__".
func splitNamespaceTool(prefixed string) (namespace, tool string) {
	if idx := strings.LastIndex(prefixed, "__"); idx >= 0 {
		return prefixed[:idx], prefixed[idx+2:]
	}
	return "", prefixed
}

// mcpCache stores MCP tool definitions discovered from Codex namespace bundles.
// Populated automatically each time a namespace bundle passes through flattenToolEntry.
// No disk file needed — rebuilt from requests after restart.
type tenantCache struct {
	tools      []map[string]any
	lastAccess time.Time
}

var (
	mcpCache   = make(map[string]*tenantCache)
	mcpCacheMu sync.Mutex
)

var mcpCacheCtxCancel context.CancelFunc

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
			if now.Sub(tc.lastAccess) > mcpCacheTTL {
				delete(mcpCache, tenantID)
			}
		}
		mcpCacheMu.Unlock()
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

func clearMCPCache() {
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
// NamespaceForTool returns the namespace for a prefixed tool name via string parsing.
func NamespaceForTool(prefixedName string) string {
	ns, _ := splitNamespaceTool(prefixedName)
	return ns
}

// ResolveNamespaceTool returns the original tool name from a prefixed name.
func ResolveNamespaceTool(name string) string {
	_, tool := splitNamespaceTool(name)
	return tool
}

// sanitizeToolsForChatCompletions converts Responses API tools to Chat Completions format.
func sanitizeToolsForChatCompletions(raw json.RawMessage, tenantID string) (any, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return jsonRawToAny(raw), nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entries, err := flattenToolEntry(item, tenantID)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func flattenToolEntry(item json.RawMessage, tenantID string) ([]map[string]any, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(item, &m); err != nil {
		return nil, nil
	}
	typ, _ := rawStringField(m, "type")
	switch typ {
	case "code_interpreter", "file_search", "computer_use":
		return nil, fmt.Errorf("tool type %q not supported by prism proxy", typ)
	}
	if typ == "web_search" {
		params := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query"},
			},
			"required": []string{"query"},
		}
		if extra := jsonRawToAny(m["parameters"]); extra != nil {
			if em, ok := extra.(map[string]any); ok {
				if props, ok := em["properties"].(map[string]any); ok {
					for k, v := range props {
						if k != "query" {
							params["properties"].(map[string]any)[k] = v
						}
					}
				}
			}
		}
		return []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "web_search",
				"description": "Search the web for current information on any topic.",
				"parameters":  params,
			},
		}}, nil
	}
	// Namespace / MCP bundle: extract tools and cache for tool_search
	if nested, ok := m["tools"]; ok && len(nested) > 0 && string(nested) != "null" {
		bundleName, _ := rawStringField(m, "name")
		var sub []json.RawMessage
		if err := json.Unmarshal(nested, &sub); err != nil {
			return nil, nil
		}
		var out []map[string]any
		for _, s := range sub {
			var sm map[string]json.RawMessage
			if err := json.Unmarshal(s, &sm); err != nil {
				continue
			}
			subName, _ := rawStringField(sm, "name")
			if subName == "" {
				if fnRaw, ok := sm["function"]; ok {
					var fn map[string]json.RawMessage
					if json.Unmarshal(fnRaw, &fn) == nil {
						subName, _ = rawStringField(fn, "name")
					}
				}
			}
			if t := asFunctionTool(sm); t != nil {
				if bundleName != "" && subName != "" {
					prefixed := bundleName + "__" + subName
					if fnObj, ok := t["function"].(map[string]any); ok {
						fnObj["name"] = prefixed
					}
				}
				cacheMCPTool(tenantID, t)
				out = append(out, t)
			}
		}
		return out, nil
	}
	// tool_search: append cached MCP tools so non-GPT models can see them
	if typ == "tool_search" {
		desc, _ := rawStringField(m, "description")
		fnObj := map[string]any{"name": "tool_search"}
		if desc != "" {
			fnObj["description"] = desc
		}
		if len(m["parameters"]) > 0 && string(m["parameters"]) != "null" {
			fnObj["parameters"] = simplifyJSONSchema(jsonRawToAny(m["parameters"]))
		} else {
			fnObj["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		result := []map[string]any{{
			"type":     "function",
			"function": fnObj,
		}}
		result = append(result, getTenantMCPTools(tenantID)...)
		return result, nil
	}
	if t := asFunctionTool(m); t != nil {
		return []map[string]any{t}, nil
	}
	return nil, nil
}

func asFunctionTool(m map[string]json.RawMessage) map[string]any {
	var name, desc string
	var params json.RawMessage

	if fnRaw, ok := m["function"]; ok && len(fnRaw) > 0 && string(fnRaw) != "null" {
		var fn map[string]json.RawMessage
		if err := json.Unmarshal(fnRaw, &fn); err == nil {
			name, _ = rawStringField(fn, "name")
			desc, _ = rawStringField(fn, "description")
			params = fn["parameters"]
		}
	}
	if name == "" {
		var ok bool
		name, ok = rawStringField(m, "name")
		if !ok || name == "" {
			return nil
		}
	}
	if desc == "" {
		desc, _ = rawStringField(m, "description")
	}
	if len(params) == 0 || string(params) == "null" {
		params = m["parameters"]
	}

	fnObj := map[string]any{"name": name}
	if desc != "" {
		fnObj["description"] = desc
	}
	if len(params) > 0 && string(params) != "null" {
		fnObj["parameters"] = simplifyJSONSchema(jsonRawToAny(params))
	} else {
		fnObj["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return map[string]any{
		"type":     "function",
		"function": fnObj,
	}
}

// getSearchToolCache returns a snapshot of cached MCP tools for tool_search interception.
func getSearchToolCache(tenantID string) []map[string]any {
	return getTenantMCPTools(tenantID)
}
