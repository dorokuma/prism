package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dorokuma/prism/internal/sanitize"
	"github.com/dorokuma/prism/internal/util"
)

// splitNamespaceTool splits "mcp__codegraph__search" into ("mcp__codegraph", "search").
// Uses the last "__" as the boundary. Tool names must not contain "__".
func splitNamespaceTool(prefixed string) (namespace, tool string) {
	if idx := strings.LastIndex(prefixed, "__"); idx >= 0 {
		return prefixed[:idx], prefixed[idx+2:]
	}
	return "", prefixed
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

// SanitizeToolsForChatCompletions converts Responses API tools to Chat Completions format.
func SanitizeToolsForChatCompletions(raw json.RawMessage, tenantID string) (any, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return util.JSONRawToAny(raw), nil
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
	typ, _ := util.RawStringField(m, "type")
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
		if extra := util.JSONRawToAny(m["parameters"]); extra != nil {
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
		bundleName, _ := util.RawStringField(m, "name")
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
			subName, _ := util.RawStringField(sm, "name")
			if subName == "" {
				if fnRaw, ok := sm["function"]; ok {
					var fn map[string]json.RawMessage
					if json.Unmarshal(fnRaw, &fn) == nil {
						subName, _ = util.RawStringField(fn, "name")
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
		desc, _ := util.RawStringField(m, "description")
		fnObj := map[string]any{"name": "tool_search"}
		if desc != "" {
			fnObj["description"] = desc
		}
		if len(m["parameters"]) > 0 && string(m["parameters"]) != "null" {
			fnObj["parameters"] = sanitize.SimplifyJSONSchema(util.JSONRawToAny(m["parameters"]))
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
			name, _ = util.RawStringField(fn, "name")
			desc, _ = util.RawStringField(fn, "description")
			params = fn["parameters"]
		}
	}
	if name == "" {
		var ok bool
		name, ok = util.RawStringField(m, "name")
		if !ok || name == "" {
			return nil
		}
	}
	if desc == "" {
		desc, _ = util.RawStringField(m, "description")
	}
	if len(params) == 0 || string(params) == "null" {
		params = m["parameters"]
	}

	fnObj := map[string]any{"name": name}
	if desc != "" {
		fnObj["description"] = desc
	}
	if len(params) > 0 && string(params) != "null" {
		fnObj["parameters"] = sanitize.SimplifyJSONSchema(util.JSONRawToAny(params))
	} else {
		fnObj["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return map[string]any{
		"type":     "function",
		"function": fnObj,
	}
}

// GetSearchToolCache returns a snapshot of cached MCP tools for tool_search interception.
func GetSearchToolCache(tenantID string) []map[string]any {
	return getTenantMCPTools(tenantID)
}
