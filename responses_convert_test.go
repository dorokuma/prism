package main

import (
	"encoding/json"
	"testing"
)

func TestResponsesToChatCompletions(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4-pro",
		"stream": true,
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"reasoning": {"effort": "high"},
		"tools": [{"type":"function","name":"shell","parameters":{"type":"object"}}]
	}`)
	chat, stream, _, err := responsesToChatCompletions(body, "test-tenant")
	if err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Fatal("expected stream")
	}
	var m map[string]any
	if err := json.Unmarshal(chat, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "deepseek-v4-pro" {
		t.Fatalf("model: %v", m["model"])
	}
	if m["reasoning_effort"] != "high" {
		t.Fatalf("effort: %v", m["reasoning_effort"])
	}
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages: %v", m["messages"])
	}
}

func TestConvertResponsesUsage(t *testing.T) {
	// Construct a Chat API completion response with detailed usage.
	chatBody := `{
		"model": "gpt-4",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "Hello"
			}
		}],
		"usage": {
			"prompt_tokens": 200,
			"completion_tokens": 50,
			"total_tokens": 250,
			"prompt_cache_hit_tokens": 100,
			"prompt_cache_miss_tokens": 50,
			"prompt_tokens_details": {"cached_tokens": 100},
			"completion_tokens_details": {"reasoning_tokens": 30}
		}
	}`
	out, err := chatCompletionToResponse([]byte(chatBody), "gpt-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	usage, ok := resp["usage"].(map[string]any)
	if !ok {
		t.Fatal("missing usage")
	}
	// prompt_cache_hit_tokens
	hit, _ := usage["prompt_cache_hit_tokens"].(float64)
	if hit != 100 {
		t.Fatalf("prompt_cache_hit_tokens = %v, want 100", hit)
	}
	// prompt_cache_miss_tokens
	miss, _ := usage["prompt_cache_miss_tokens"].(float64)
	if miss != 50 {
		t.Fatalf("prompt_cache_miss_tokens = %v, want 50", miss)
	}
	// completion_tokens_details.reasoning_tokens
	ctd, ok := usage["completion_tokens_details"].(map[string]any)
	if !ok {
		t.Fatal("missing completion_tokens_details")
	}
	rt, _ := ctd["reasoning_tokens"].(float64)
	if rt != 30 {
		t.Fatalf("reasoning_tokens = %v, want 30", rt)
	}
	// prompt_tokens
	pt, _ := usage["prompt_tokens"].(float64)
	if pt != 200 {
		t.Fatalf("prompt_tokens = %v, want 200", pt)
	}
}

func TestMapThoughtLevel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"low to high", "low", "high"},
		{"medium to high", "medium", "high"},
		{"high to high", "high", "high"},
		{"xhigh to max", "xhigh", "max"},
		{"LOW uppercase to high", "LOW", "high"},
		{"unknown passes through", "unknown", "unknown"},
		{"empty string passes through", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapThoughtLevel(tt.input)
			if got != tt.want {
				t.Errorf("mapThoughtLevel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}