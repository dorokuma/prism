package mcp

import (
	"encoding/json"
	"log/slog"
	"os"

	"github.com/dorokuma/prism/internal/sanitize"
)

// LoadMCPTools reads mcp_tools.json at startup and populates
// the runtime MCP cache so tool_search responses include real definitions
// even before the first namespace bundle arrives from Codex.
func LoadMCPTools(path string) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("mcp_inject failed to read file", "path", path, "error", err)
		return
	}
	var namespaces map[string]struct {
		Namespace string `json:"namespace"`
		Tools     []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &namespaces); err != nil {
		slog.Warn("mcp_inject failed to parse file", "path", path, "error", err)
		return
	}
	count := 0
	for _, ns := range namespaces {
		for _, t := range ns.Tools {
			prefixed := ns.Namespace + "__" + t.Name
			fnObj := map[string]any{"name": prefixed}
			if t.Description != "" {
				fnObj["description"] = t.Description
			}
			if len(t.Parameters) > 0 {
				// Depth-bounded schema simplification (mcp_tools.json is
				// admin-provided, but a pathological file must fail this tool
				// loudly — never cache the unsafe original).
				params, err := sanitize.SimplifyJSONSchemaLimited(t.Parameters, sanitize.MaxJSONSchemaDepth)
				if err != nil {
					slog.Warn("mcp_inject tool schema too deep, skipping tool", "tool", ns.Namespace+"__"+t.Name, "error", err)
					continue
				}
				fnObj["parameters"] = params
			} else {
				fnObj["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			cacheAdminTool(map[string]any{
				"type":     "function",
				"function": fnObj,
			})
			count++
		}
	}
	slog.Info("mcp_inject loaded tools", "count", count, "namespaces", len(namespaces))
}
