package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMCPTools(t *testing.T) {
	// Save and restore the cache between tests
	origCache := make(map[string]*tenantCache)
	mcpCacheMu.Lock()
	for k, v := range mcpCache {
		copied := &tenantCache{
			tools:      make([]map[string]any, len(v.tools)),
			lastAccess: v.lastAccess,
		}
		copy(copied.tools, v.tools)
		origCache[k] = copied
	}
	mcpCache = make(map[string]*tenantCache)
	mcpCacheMu.Unlock()

	defer func() {
		mcpCacheMu.Lock()
		mcpCache = origCache
		mcpCacheMu.Unlock()
	}()

	t.Run("empty path", func(t *testing.T) {
		clearMCPCache()
		loadMCPTools("")
		// Cache should still be empty
		tools := getTenantMCPTools("default")
		if len(tools) != 0 {
			t.Errorf("expected 0 tools for empty path, got %d", len(tools))
		}
	})

	t.Run("file not found", func(t *testing.T) {
		clearMCPCache()
		loadMCPTools("/nonexistent/mcp_tools_does_not_exist.json")
		// Should not panic, cache empty
		tools := getTenantMCPTools("default")
		if len(tools) != 0 {
			t.Errorf("expected 0 tools for missing file, got %d", len(tools))
		}
	})

	t.Run("valid json with tools", func(t *testing.T) {
		clearMCPCache()
		dir := t.TempDir()
		path := filepath.Join(dir, "mcp_tools.json")
		content := `{
  "codegraph": {
    "namespace": "codegraph",
    "tools": [
      {
        "name": "search",
        "description": "Search the codebase",
        "parameters": {
          "type": "object",
          "properties": {
            "query": {"type": "string", "description": "Search query"},
            "justification": {"type": "string"}
          },
          "required": ["query"]
        }
      },
      {
        "name": "list",
        "description": "List files",
        "parameters": {
          "type": "object",
          "properties": {}
        }
      }
    ]
  }
}`
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write temp file: %v", err)
		}

		loadMCPTools(path)

		tools := getTenantMCPTools("default")
		if len(tools) != 2 {
			t.Fatalf("expected 2 tools, got %d", len(tools))
		}

		// First tool should be codegraph__search
		fn1, ok := tools[0]["function"].(map[string]any)
		if !ok {
			t.Fatal("tool[0] missing function")
		}
		if fn1["name"] != "codegraph__search" {
			t.Errorf("tool[0] name = %q, want codegraph__search", fn1["name"])
		}
		if fn1["description"] != "Search the codebase" {
			t.Errorf("tool[0] description mismatch")
		}
		// Parameters should have been simplified (justification stripped)
		params1, ok := fn1["parameters"].(map[string]any)
		if !ok {
			t.Fatal("tool[0] missing parameters")
		}
		props1, _ := params1["properties"].(map[string]any)
		if _, hasJustification := props1["justification"]; hasJustification {
			t.Error("justification should have been stripped from parameters")
		}

		// Second tool should be codegraph__list
		fn2, ok := tools[1]["function"].(map[string]any)
		if !ok {
			t.Fatal("tool[1] missing function")
		}
		if fn2["name"] != "codegraph__list" {
			t.Errorf("tool[1] name = %q, want codegraph__list", fn2["name"])
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		clearMCPCache()
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte("this is not json {{{"), 0644); err != nil {
			t.Fatalf("write temp file: %v", err)
		}

		loadMCPTools(path)
		// Should not panic, cache empty
		tools := getTenantMCPTools("default")
		if len(tools) != 0 {
			t.Errorf("expected 0 tools for malformed JSON, got %d", len(tools))
		}
	})

	t.Run("empty json object", func(t *testing.T) {
		clearMCPCache()
		dir := t.TempDir()
		path := filepath.Join(dir, "empty.json")
		if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
			t.Fatalf("write temp file: %v", err)
		}

		loadMCPTools(path)
		tools := getTenantMCPTools("default")
		if len(tools) != 0 {
			t.Errorf("expected 0 tools for empty JSON, got %d", len(tools))
		}
	})

	t.Run("valid json with empty namespaces", func(t *testing.T) {
		clearMCPCache()
		dir := t.TempDir()
		path := filepath.Join(dir, "no_tools.json")
		content := `{"ns1": {"namespace": "ns1", "tools": []}}`
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write temp file: %v", err)
		}

		loadMCPTools(path)
		tools := getTenantMCPTools("default")
		if len(tools) != 0 {
			t.Errorf("expected 0 tools for namespace with no tools, got %d", len(tools))
		}
	})

	t.Run("tool without description", func(t *testing.T) {
		clearMCPCache()
		dir := t.TempDir()
		path := filepath.Join(dir, "no_desc.json")
		content := `{
  "ns": {
    "namespace": "ns",
    "tools": [
      {
        "name": "bare_tool",
        "parameters": {}
      }
    ]
  }
}`
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write temp file: %v", err)
		}

		loadMCPTools(path)

		tools := getTenantMCPTools("default")
		if len(tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(tools))
		}

		fn, _ := tools[0]["function"].(map[string]any)
		if _, hasDesc := fn["description"]; hasDesc {
			t.Error("tool without description should not have description key")
		}
	})

	t.Run("tool without parameters", func(t *testing.T) {
		clearMCPCache()
		dir := t.TempDir()
		path := filepath.Join(dir, "no_params.json")
		content := `{
  "ns": {
    "namespace": "ns",
    "tools": [
      {
        "name": "no_param_tool",
        "description": "A tool with no parameters"
      }
    ]
  }
}`
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write temp file: %v", err)
		}

		loadMCPTools(path)

		tools := getTenantMCPTools("default")
		if len(tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(tools))
		}

		fn, _ := tools[0]["function"].(map[string]any)
		params, ok := fn["parameters"].(map[string]any)
		if !ok {
			t.Fatal("tool without parameters should get default empty params")
		}
		if params["type"] != "object" {
			t.Errorf("default params type = %q, want object", params["type"])
		}
	})

	t.Run("multiple namespaces", func(t *testing.T) {
		clearMCPCache()
		dir := t.TempDir()
		path := filepath.Join(dir, "multi_ns.json")
		content := `{
  "ns_a": {
    "namespace": "ns_a",
    "tools": [{"name": "tool_a", "parameters": {}}]
  },
  "ns_b": {
    "namespace": "ns_b",
    "tools": [{"name": "tool_b", "parameters": {}}, {"name": "tool_c", "parameters": {}}]
  }
}`
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write temp file: %v", err)
		}

		loadMCPTools(path)

		tools := getTenantMCPTools("default")
		if len(tools) != 3 {
			t.Fatalf("expected 3 tools across 2 namespaces, got %d", len(tools))
		}

		names := make(map[string]bool)
		for _, tool := range tools {
			fn, _ := tool["function"].(map[string]any)
			names[fn["name"].(string)] = true
		}
		for _, want := range []string{"ns_a__tool_a", "ns_b__tool_b", "ns_b__tool_c"} {
			if !names[want] {
				t.Errorf("expected tool %q not found", want)
			}
		}
	})
}
