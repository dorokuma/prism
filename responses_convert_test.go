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



func TestChatCompletionToResponse_FinishReason(t *testing.T) {
	tests := []struct {
		reason string
		status string
	}{
		{"length", "incomplete"},
		{"stop", "completed"},
		{"tool_calls", "completed"},
		{"content_filter", "incomplete"},
		{"", "completed"},
	}
	for _, tc := range tests {
		chatBody := `{"model":"gpt-4","choices":[{"finish_reason":"` + tc.reason + `","message":{"role":"assistant","content":"ok"}}]}`
		out, err := chatCompletionToResponse([]byte(chatBody), "gpt-4", nil)
		if err != nil {
			t.Fatalf("chatCompletionToResponse(%q) error: %v", tc.reason, err)
		}
		var resp map[string]any
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		gotStatus, _ := resp["status"].(string)
		if gotStatus != tc.status {
			t.Errorf("finish_reason=%q status=%q, want %q", tc.reason, gotStatus, tc.status)
		}
	}
}

func TestResponsesToChat_ParallelToolCalls(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4-pro",
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"parallel_tool_calls": true
	}`)
	chat, _, _, err := responsesToChatCompletions(body, "test-tenant")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(chat, &m); err != nil {
		t.Fatal(err)
	}
	ptc, ok := m["parallel_tool_calls"]
	if !ok || ptc != true {
		t.Fatalf("parallel_tool_calls not preserved: %v", ptc)
	}
}

func TestResponsesToChat_ImageInputRejected(t *testing.T) {
	// Content with image_url part should be rejected.
	body := []byte(`{
		"model": "deepseek-v4-pro",
		"input": [{"type":"message","role":"user","content":[{"type":"image_url","image_url":"https://example.com/img.png"}]}]
	}`)
	_, _, _, err := responsesToChatCompletions(body, "test-tenant")
	if err == nil {
		t.Fatal("expected error for image_url content, got nil")
	}
}

func TestResponsesToChat_InputImageRejected(t *testing.T) {
	// Content with input_image part should be rejected.
	body := []byte(`{
		"model": "deepseek-v4-pro",
		"input": [{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.com/img.png"}]}]
	}`)
	_, _, _, err := responsesToChatCompletions(body, "test-tenant")
	if err == nil {
		t.Fatal("expected error for input_image content, got nil")
	}
}

func TestResponsesToChat_TextOnlyOK(t *testing.T) {
	// Pure text content ([]any with text parts) should succeed without error.
	body := []byte(`{
		"model": "deepseek-v4-pro",
		"stream": true,
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"},{"type":"output_text","text":"world"}]}]
	}`)
	chat, _, _, err := responsesToChatCompletions(body, "test-tenant")
	if err != nil {
		t.Fatalf("unexpected error for text-only content: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(chat, &m); err != nil {
		t.Fatal(err)
	}
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages: %v", m["messages"])
	}
	msg := msgs[0].(map[string]any)
	content, ok := msg["content"].(string)
	if !ok || content != "helloworld" {
		t.Fatalf("content: %v (expected 'helloworld')", msg["content"])
	}
}

func TestResponsesToChat_TextStringOK(t *testing.T) {
	// Plain string content should succeed without error.
	body := []byte(`{
		"model": "deepseek-v4-pro",
		"input": "hello world"
	}`)
	chat, _, _, err := responsesToChatCompletions(body, "test-tenant")
	if err != nil {
		t.Fatalf("unexpected error for string content: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(chat, &m); err != nil {
		t.Fatal(err)
	}
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages: %v", m["messages"])
	}
	msg := msgs[0].(map[string]any)
	if msg["content"] != "hello world" {
		t.Fatalf("content: %v", msg["content"])
	}
}

func TestFlattenResponseContentParts_TextOnly(t *testing.T) {
	// Regression: ensure flattenResponseContentParts still handles text parts correctly.
	parts := []map[string]any{
		{"type": "input_text", "text": "hello"},
		{"type": "output_text", "text": "world"},
	}
	result := flattenResponseContentParts(parts)
	arr, ok := result.([]map[string]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("expected 2-element array, got %v", result)
	}
	if arr[0]["type"] != "text" || arr[0]["text"] != "hello" {
		t.Fatalf("part[0]: %v", arr[0])
	}
	if arr[1]["type"] != "text" || arr[1]["text"] != "world" {
		t.Fatalf("part[1]: %v", arr[1])
	}
}

func TestFlattenResponseContentParts_SingleText(t *testing.T) {
	// Single text part should collapse to plain string.
	parts := []map[string]any{
		{"type": "input_text", "text": "solo"},
	}
	result := flattenResponseContentParts(parts)
	s, ok := result.(string)
	if !ok || s != "solo" {
		t.Fatalf("expected 'solo' string, got %v", result)
	}
}

func TestResponsesToChat_PreviousResponseIDRejected(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4-pro",
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"previous_response_id": "resp_abc123"
	}`)
	_, _, _, err := responsesToChatCompletions(body, "test-tenant")
	if err == nil {
		t.Fatal("expected error for previous_response_id, got nil")
	}
}

func TestResponsesToChat_TextFormatJsonSchema(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"text": {
			"format": {
				"type": "json_schema",
				"name": "response",
				"schema": {"type": "object", "properties": {"answer": {"type": "string"}}},
				"strict": true
			}
		}
	}`)
	chat, _, _, err := responsesToChatCompletions(body, "test-tenant")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(chat, &m); err != nil {
		t.Fatal(err)
	}
	rf, ok := m["response_format"]
	if !ok {
		t.Fatal("expected response_format")
	}
	rfMap, ok := rf.(map[string]any)
	if !ok {
		t.Fatalf("response_format not map: %T", rf)
	}
	if rfMap["type"] != "json_schema" {
		t.Fatalf("response_format.type = %v, want json_schema", rfMap["type"])
	}
	if rfMap["name"] != "response" {
		t.Fatalf("response_format.name = %v, want response", rfMap["name"])
	}
	if rfMap["strict"] != true {
		t.Fatalf("response_format.strict = %v, want true", rfMap["strict"])
	}
	if _, ok := rfMap["schema"]; !ok {
		t.Fatal("response_format missing schema")
	}
}

func TestResponsesToChat_TextFormatJsonObject(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"text": {
			"format": {
				"type": "json_object"
			}
		}
	}`)
	chat, _, _, err := responsesToChatCompletions(body, "test-tenant")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(chat, &m); err != nil {
		t.Fatal(err)
	}
	rf, ok := m["response_format"]
	if !ok {
		t.Fatal("expected response_format")
	}
	rfMap, ok := rf.(map[string]any)
	if !ok {
		t.Fatalf("response_format not map: %T", rf)
	}
	if rfMap["type"] != "json_object" {
		t.Fatalf("response_format.type = %v, want json_object", rfMap["type"])
	}
}

func TestResponsesToChat_UserSeedPassthrough(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"user": "user-123",
		"seed": 42
	}`)
	chat, _, _, err := responsesToChatCompletions(body, "test-tenant")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(chat, &m); err != nil {
		t.Fatal(err)
	}
	if m["user"] != "user-123" {
		t.Fatalf("user = %v, want user-123", m["user"])
	}
	if m["seed"] != float64(42) {
		t.Fatalf("seed = %v, want 42", m["seed"])
	}
}

func TestChatCompletionToResponse_CreatedAt(t *testing.T) {
	chatBody := `{
		"model": "gpt-4",
		"created": 1700000000,
		"choices": [{
			"finish_reason": "stop",
			"message": {"role": "assistant", "content": "Hello"}
		}]
	}`
	out, err := chatCompletionToResponse([]byte(chatBody), "gpt-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	ca, ok := resp["created_at"]
	if !ok {
		t.Fatal("missing created_at")
	}
	caFloat, ok := ca.(float64)
	if !ok {
		t.Fatalf("created_at not float64: %T", ca)
	}
	if int64(caFloat) != 1700000000 {
		t.Fatalf("created_at = %v, want 1700000000", int64(caFloat))
	}
}

func TestChatCompletionToResponse_Logprobs(t *testing.T) {
	chatBody := []byte(`{
		"model": "gpt-4",
		"choices": [{
			"finish_reason": "stop",
			"logprobs": {"content": [{"token": "Hello", "logprob": -0.5}]},
			"message": {"role": "assistant", "content": "Hello"}
		}]
	}`)
	out, err := chatCompletionToResponse(chatBody, "gpt-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	lp, ok := resp["logprobs"]
	if !ok {
		t.Fatal("missing logprobs")
	}
	lpMap, ok := lp.(map[string]any)
	if !ok {
		t.Fatalf("logprobs not map: %T", lp)
	}
	content, ok := lpMap["content"]
	if !ok {
		t.Fatal("logprobs.content missing")
	}
	contentArr, ok := content.([]any)
	if !ok {
		t.Fatalf("logprobs.content not array: %T", content)
	}
	if len(contentArr) != 1 {
		t.Fatalf("logprobs.content len = %d, want 1", len(contentArr))
	}
}

func TestResponsesToChat_EncryptedContentRejected(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"include": ["encrypted_content"]
	}`)
	_, _, _, err := responsesToChatCompletions(body, "test-tenant")
	if err == nil {
		t.Fatal("expected error for include=encrypted_content, got nil")
	}
}

func TestResponsesToChat_StoreTrueWarns(t *testing.T) {
	// store=true should not return error, just warn (no panic).
	body := []byte(`{
		"model": "gpt-4",
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"store": true
	}`)
	chat, _, _, err := responsesToChatCompletions(body, "test-tenant")
	if err != nil {
		t.Fatalf("unexpected error for store=true: %v", err)
	}
	if chat == nil {
		t.Fatal("expected non-nil chat body")
	}
}