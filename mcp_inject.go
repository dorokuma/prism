package main

import (
	"encoding/json"
	"log"
	"os"
)

var mcpInjectTools []map[string]any

func loadMCPTools(path string) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("mcp_inject: failed to read %s: %v", path, err)
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
		log.Printf("mcp_inject: failed to parse %s: %v", path, err)
		return
	}
	for _, ns := range namespaces {
		for _, t := range ns.Tools {
			prefixed := ns.Namespace + "__" + t.Name
			registerToolNamespace(t.Name, ns.Namespace)
			fnObj := map[string]any{"name": prefixed}
			if t.Description != "" {
				fnObj["description"] = t.Description
			}
			if len(t.Parameters) > 0 {
				fnObj["parameters"] = simplifyJSONSchema(t.Parameters)
			} else {
				fnObj["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			mcpInjectTools = append(mcpInjectTools, map[string]any{
				"type":     "function",
				"function": fnObj,
			})
		}
	}
	log.Printf("mcp_inject: loaded %d tools from %d namespaces", len(mcpInjectTools), len(namespaces))
}
