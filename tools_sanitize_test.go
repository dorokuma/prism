package main

import "testing"

func TestSanitizeToolsFlattensNamespaceAndConvertsWebSearch(t *testing.T) {
	raw := []byte(`[
	  {"type":"function","name":"exec_command","parameters":{"type":"object"}},
	  {"type":"web_search"},
	  {"name":"multi_agent_v1","tools":[{"name":"close_agent","parameters":{"type":"object"}}]}
	]`)
	got := sanitizeToolsForChatCompletions(raw).([]map[string]any)
	// web_search is now converted to a function tool (not dropped)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	fn0, _ := got[0]["function"].(map[string]any)
	fn1, _ := got[1]["function"].(map[string]any)
	fn2, _ := got[2]["function"].(map[string]any)
	if fn0["name"] != "exec_command" {
		t.Fatalf("got[0] name=%v, want exec_command", fn0["name"])
	}
	if fn1["name"] != "web_search" {
		t.Fatalf("got[1] name=%v, want web_search", fn1["name"])
	}
	if fn2["name"] != "close_agent" {
		t.Fatalf("got[2] name=%v, want close_agent", fn2["name"])
	}
}
