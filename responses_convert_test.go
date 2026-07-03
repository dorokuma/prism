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
	chat, stream, _, err := responsesToChatCompletions(body)
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