package main

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
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
var (
	mcpCache   []map[string]any
	mcpCacheMu sync.Mutex
)

func cacheMCPTool(tool map[string]any) {
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()
	for _, existing := range mcpCache {
		if fn, ok := existing["function"].(map[string]any); ok {
			if nf, ok := tool["function"].(map[string]any); ok {
				if fn["name"] == nf["name"] {
					return // already cached
				}
			}
		}
	}
	mcpCache = append(mcpCache, tool)
}

func clearMCPCache() {
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()
	mcpCache = nil
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

// PrefixNamespaceTool is a no-op; namespace reconstruction is handled
// by responseItemToMessage which reads the namespace field from Codex requests.
func PrefixNamespaceTool(name string) string {
	return name
}

// sanitizeToolsForChatCompletions converts Responses API tools to Chat Completions format.
func sanitizeToolsForChatCompletions(raw json.RawMessage) any {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return jsonRawToAny(raw)
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, flattenToolEntry(item)...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func flattenToolEntry(item json.RawMessage) []map[string]any {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(item, &m); err != nil {
		return nil
	}
	typ, _ := rawStringField(m, "type")
	switch typ {
	case "code_interpreter", "file_search", "computer_use":
		log.Printf("tools_sanitize: dropping unsupported tool type %q", typ)
		return nil
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
		}}
	}
	// Namespace / MCP bundle: extract tools and cache for tool_search
	if nested, ok := m["tools"]; ok && len(nested) > 0 && string(nested) != "null" {
		bundleName, _ := rawStringField(m, "name")
		var sub []json.RawMessage
		if err := json.Unmarshal(nested, &sub); err != nil {
			return nil
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
				cacheMCPTool(t)
				out = append(out, t)
			}
		}
		return out
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
		mcpCacheMu.Lock()
		result = append(result, mcpCache...)
		mcpCacheMu.Unlock()
		return result
	}
	if t := asFunctionTool(m); t != nil {
		return []map[string]any{t}
	}
	return nil
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
