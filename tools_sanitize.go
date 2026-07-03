package main

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
)

// splitNamespaceTool splits a prefixed tool name like "mcp__codegraph__search"
// into (namespace, tool) using the last "__" as the boundary.
// Namespace: "mcp__codegraph", Tool: "search".
// Falls back to ("", name) if no "__" is found.
func splitNamespaceTool(prefixed string) (namespace, tool string) {
	if idx := strings.LastIndex(prefixed, "__"); idx >= 0 {
		return prefixed[:idx], prefixed[idx+2:]
	}
	return "", prefixed
}

// toolNamespaceCache maps short tool names to their namespace.
// Populated from namespace bundles in requests and from mcp_tools.json.
var (
	toolNamespaceCache   = map[string]string{}
	toolNamespaceCacheMu sync.RWMutex
)

// registerToolNamespace records the namespace for an un-prefixed tool name.
func registerToolNamespace(shortName, namespace string) {
	toolNamespaceCacheMu.Lock()
	defer toolNamespaceCacheMu.Unlock()
	toolNamespaceCache[shortName] = namespace
}

// lookupToolNamespace returns the namespace for an un-prefixed tool name.
func lookupToolNamespace(shortName string) string {
	toolNamespaceCacheMu.RLock()
	defer toolNamespaceCacheMu.RUnlock()
	return toolNamespaceCache[shortName]
}
// NamespaceForTool returns the namespace for a prefixed tool name via string parsing.
func NamespaceForTool(prefixedName string) string {
	ns, _ := splitNamespaceTool(prefixedName)
	if ns != "" {
		return ns
	}
	return lookupToolNamespace(prefixedName)
}

// ResolveNamespaceTool returns the original tool name from a prefixed name.
func ResolveNamespaceTool(name string) string {
	_, tool := splitNamespaceTool(name)
	return tool
}

// PrefixNamespaceTool reconstructs the prefixed name from namespace + tool name.
// Used when converting Codex function_call history back to Chat Completions format.
// If the name is already prefixed, returns as-is.
// The caller in responseItemToMessage should concatenate namespace + "__" + name
// when both are available from the Codex request.
func PrefixNamespaceTool(name string) string {
	return name
}

// sanitizeToolsForChatCompletions converts Responses API tools to Chat Completions format.
// Filters out Codex-internal types that have no Chat Completions equivalent.
// Namespace bundles are flattened with prefixed names to preserve routing info.
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
	// Namespace / MCP bundle: { name, description, tools: [...] }
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
				// Prefix with bundle name to preserve namespace routing.
				// Resolution is done via string splitting at the last "__",
				// so no mapping table is needed.
				if bundleName != "" && subName != "" {
					prefixed := bundleName + "__" + subName
					if fnObj, ok := t["function"].(map[string]any); ok {
						fnObj["name"] = prefixed
					}
					registerToolNamespace(subName, bundleName)
				}
				out = append(out, t)
			}
		}
		return out
	}
	// tool_search: deferred MCP tool discovery (Codex v0.142.5+)
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
		result = append(result, mcpInjectTools...)
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
