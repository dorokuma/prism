package main

import (
	"encoding/json"
	"errors"
	"log"
	"fmt"
	"strings"
)

func responsesToChatCompletions(body []byte, tenantID string) (chatBody []byte, stream bool, reqTools json.RawMessage, err error) {
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(body, &raw); err != nil {
		return nil, false, nil, fmt.Errorf("responses body: %w", err)
	}
	model, _ := rawStringField(raw, "model")
	if model == "" {
		return nil, false, nil, fmt.Errorf("responses body: missing model")
	}
	stream = rawBoolField(raw, "stream")
	messages, err := inputToMessages(raw["input"])
	if err != nil {
		return nil, false, nil, err
	}
	if instr, ok := rawStringField(raw, "instructions"); ok && strings.TrimSpace(instr) != "" {
		messages = append([]map[string]any{{"role": "system", "content": instr}}, messages...)
	}
	if len(messages) == 0 {
		return nil, false, nil, fmt.Errorf("responses body: empty input")
	}
	for _, m := range messages {
		if hasImagePart(m["content"]) {
			return nil, false, nil, errors.New("image input not supported by prism proxy on /v1/responses; use /v1/chat/completions")
		}
	}
	messages = normalizeMessagesForChatAPI(messages)
	out := map[string]any{"model": model, "messages": messages, "stream": stream}
	if v, ok := raw["tools"]; ok && len(v) > 0 && string(v) != "null" {
		if tools := sanitizeToolsForChatCompletions(v, tenantID); tools != nil {
			out["tools"] = tools
			copyOptionalRaw(raw, out, "tool_choice")
		}
	}
	copyOptionalRaw(raw, out, "temperature", "top_p", "stream_options", "thinking", "parallel_tool_calls")
	if v, ok := raw["max_output_tokens"]; ok {
		out["max_tokens"] = jsonRawToAny(v)
	}
	if effort := reasoningEffortFromRaw(raw["reasoning"]); effort != "" {
		out["reasoning_effort"] = effort
	} else if v, ok := raw["reasoning_effort"]; ok {
		out["reasoning_effort"] = jsonRawToAny(v)
	}
	chatBody, err = json.Marshal(out)
	return chatBody, stream, raw["tools"], err
}

func copyOptionalRaw(raw map[string]json.RawMessage, out map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := raw[k]; ok && len(v) > 0 && string(v) != "null" {
			out[k] = jsonRawToAny(v)
		}
	}
}

func inputToMessages(input json.RawMessage) ([]map[string]any, error) {
	if len(input) == 0 || string(input) == "null" {
		return nil, nil
	}
	var asString string
	if err := json.Unmarshal(input, &asString); err == nil {
		return []map[string]any{{"role": "user", "content": asString}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	messages := make([]map[string]any, 0, len(items))
	for i, item := range items {
		m, err := responseItemToMessage(item)
		if err != nil {
			return nil, fmt.Errorf("input[%d]: %w", i, err)
		}
		if m != nil {
			messages = append(messages, m)
		}
	}

	// Merge consecutive assistant + tool_calls turns into one assistant message.
	messages = mergeConsecutiveAssistantMessages(messages)

	return messages, nil
}

func responseItemToMessage(item json.RawMessage) (map[string]any, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(item, &obj); err != nil {
		return nil, err
	}
	typ, _ := rawStringField(obj, "type")
	if typ == "" {
		if role, _ := rawStringField(obj, "role"); role != "" {
			return map[string]any{"role": role, "content": contentFromRaw(obj["content"])}, nil
		}
		return nil, fmt.Errorf("missing type")
	}
	switch typ {
	case "message":
		role, _ := rawStringField(obj, "role")
		if role == "" {
			role = "user"
		}
		if role == "developer" {
			role = "system"
		}
		return map[string]any{"role": role, "content": contentFromRaw(obj["content"])}, nil
	case "function_call_output":
		callID, _ := rawStringField(obj, "call_id")
		if callID == "" {
			callID, _ = rawStringField(obj, "tool_call_id")
		}
		out, _ := rawStringField(obj, "output")
		return map[string]any{"role": "tool", "tool_call_id": callID, "content": out}, nil
	case "function_call":
		name, _ := rawStringField(obj, "name")
		ns, _ := rawStringField(obj, "namespace")
		if ns != "" {
			name = ns + "__" + name
		}
		args, _ := rawStringField(obj, "arguments")
		if args == "" { args = "{}" }
		callID, _ := rawStringField(obj, "call_id")
		if callID == "" {
			callID, _ = rawStringField(obj, "id")
		}
		msg := map[string]any{
			"role": "assistant",
			"tool_calls": []map[string]any{{
				"id": callID, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}
		if rc, ok := obj["reasoning_content"]; ok && len(rc) > 0 && string(rc) != "null" {
			msg["reasoning_content"] = jsonRawToAny(rc)
		}
		return msg, nil
	case "reasoning":
		text := extractReasoningText(obj)
		if text == "" {
			return nil, nil
		}
		return map[string]any{"role": "assistant", "content": "", "reasoning_content": text}, nil
	case "item_reference":
		return nil, nil
	default:
		if role, _ := rawStringField(obj, "role"); role != "" {
			return map[string]any{"role": role, "content": contentFromRaw(obj["content"])}, nil
		}
		return nil, fmt.Errorf("unsupported item type %q", typ)
	}
}

func contentFromRaw(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err == nil {
		return flattenResponseContentParts(parts)
	}
	return jsonRawToAny(raw)
}

func flattenResponseContentParts(parts []map[string]any) any {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		if t, _ := parts[0]["type"].(string); t == "input_text" || t == "output_text" {
			if text, ok := parts[0]["text"].(string); ok {
				return text
			}
		}
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		typ, _ := p["type"].(string)
		switch typ {
		case "input_text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": p["text"]})
		default:
			if debugMode {
				log.Printf("[debug] unknown content part type: %s", p["type"])
			}
			out = append(out, p)
		}
	}
	return out
}

// hasImagePart checks whether a message content contains image parts
// (image_url, input_image, or input_file) that would be silently dropped
// by flattenMessageContent in the normalize path.
func hasImagePart(content any) bool {
	switch v := content.(type) {
	case string, nil:
		return false
	case []any:
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			if typ == "image_url" || typ == "input_image" || typ == "input_file" {
				return true
			}
		}
	case []map[string]any:
		for _, m := range v {
			typ, _ := m["type"].(string)
			if typ == "image_url" || typ == "input_image" || typ == "input_file" {
				return true
			}
		}
	}
	return false
}

func extractReasoningText(obj map[string]json.RawMessage) string {
	if s, ok := rawStringField(obj, "summary"); ok && s != "" {
		return s
	}
	if raw, ok := obj["content"]; ok {
		var parts []map[string]any
		if err := json.Unmarshal(raw, &parts); err == nil {
			var b strings.Builder
			for _, p := range parts {
				if t, _ := p["type"].(string); t == "reasoning_text" || t == "summary_text" {
					if text, ok := p["text"].(string); ok {
						b.WriteString(text)
					}
				}
			}
			return b.String()
		}
	}
	return ""
}

func reasoningEffortFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	if e, ok := obj["effort"].(string); ok {
		return e
	}
	return ""
}

func chatCompletionToResponse(body []byte, model string, reqTools json.RawMessage) ([]byte, error) {
	var comp chatCompletionResponse
	if err := json.Unmarshal(body, &comp); err != nil {
		return nil, err
	}
	if len(comp.Choices) == 0 {
		return nil, fmt.Errorf("chat completion: no choices")
	}
	ch := comp.Choices[0]
	respID := "resp_" + randomID()
	output := make([]map[string]any, 0)
	if ch.Message.ReasoningContent != "" {
		output = append(output, map[string]any{
			"type": "reasoning", "id": "rs_" + randomID(), "status": "completed",
			"summary": []map[string]any{{"type": "summary_text", "text": ch.Message.ReasoningContent}},
		})
	}
	if ch.Message.Refusal != "" {
		output = append(output, map[string]any{
			"type": "message", "id": "msg_" + randomID(), "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": ch.Message.Refusal}},
		})
	} else if ch.Message.Content != nil && contentString(ch.Message.Content) != "" {
		output = append(output, map[string]any{
			"type": "message", "id": "msg_" + randomID(), "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": contentString(ch.Message.Content)}},
		})
	}
	for _, tc := range ch.Message.ToolCalls {
		name := ResolveNamespaceTool(tc.Function.Name)
		ns := NamespaceForTool(tc.Function.Name)
		item := map[string]any{
			"type": "function_call", "id": "fc_" + randomID(), "call_id": tc.ID,
			"name": name, "arguments": tc.Function.Arguments, "status": "completed",
		}
		if ns != "" {
			item["namespace"] = ns
		}
		output = append(output, item)
	}
	usage := map[string]any{}
	if comp.Usage != nil {
		hit := comp.Usage.PromptCacheHitTokens
		miss := comp.Usage.PromptCacheMissTokens
		if hit == 0 && comp.Usage.PromptTokensDetails != nil {
			hit = comp.Usage.PromptTokensDetails.CachedTokens
		}
		if miss == 0 && hit > 0 && comp.Usage.PromptTokens > hit {
			miss = comp.Usage.PromptTokens - hit
		}
		usage = map[string]any{
			"input_tokens": comp.Usage.PromptTokens, "output_tokens": comp.Usage.CompletionTokens,
			"total_tokens": comp.Usage.TotalTokens,
			"prompt_tokens": comp.Usage.PromptTokens,
			"completion_tokens": comp.Usage.CompletionTokens,
			"prompt_cache_hit_tokens": hit,
			"prompt_cache_miss_tokens": miss,
		}
		if comp.Usage.CompletionTokensDetails != nil {
			usage["completion_tokens_details"] = map[string]any{
				"reasoning_tokens": comp.Usage.CompletionTokensDetails.ReasoningTokens,
			}
		}
	}
	resp := map[string]any{
		"id": respID, "object": "response", "status": finishReasonToStatus(ch.FinishReason),
		"model": model, "output": output, "usage": usage,
	}
	if len(reqTools) > 0 && string(reqTools) != "null" {
		resp["tools"] = jsonRawToAny(reqTools)
	}
	return json.Marshal(resp)
}

// finishReasonToStatus maps an OpenAI finish_reason to a Responses API status.
func finishReasonToStatus(reason string) string {
	switch reason {
	case "length":
		return "incomplete"
	default:
		return "completed"
	}
}

type chatCompletionResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role             string `json:"role"`
			Content          any    `json:"content"`
			Refusal          string `json:"refusal"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens            int `json:"prompt_tokens"`
		CompletionTokens        int `json:"completion_tokens"`
		TotalTokens             int `json:"total_tokens"`
		PromptCacheHitTokens    int `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens   int `json:"prompt_cache_miss_tokens"`
		PromptTokensDetails     *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

func contentString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case []any:
		// Multimodal content parts: extract text portions
		var b strings.Builder
		for _, part := range t {
			if m, ok := part.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func rawStringField(m map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := m[key]
	if !ok || len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func rawBoolField(m map[string]json.RawMessage, key string) bool {
	raw, ok := m[key]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false
	}
	return b
}

func jsonRawToAny(raw json.RawMessage) any {
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}


// isDeepSeekModel reports whether the upstream model name indicates a DeepSeek model.
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "deepseek")
}

// mapThoughtLevel maps Codex thinking level to DeepSeek-compatible level.
// low/medium/high → high, xhigh → max. Other values pass through unchanged.
func mapThoughtLevel(level string) string {
	switch strings.ToLower(level) {
	case "low", "medium", "high":
		return "high"
	case "xhigh":
		return "max"
	default:
		return level
	}
}


