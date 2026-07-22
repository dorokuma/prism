package convert

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dorokuma/prism/internal/mcp"
	"github.com/dorokuma/prism/internal/sanitize"
	"github.com/dorokuma/prism/internal/util"
)

// ResponsesToChatCompletions converts a Responses API request body to a Chat Completions API request body.
func ResponsesToChatCompletions(body []byte, tenantID string) (chatBody []byte, stream bool, reqTools json.RawMessage, err error) {
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(body, &raw); err != nil {
		return nil, false, nil, fmt.Errorf("responses body: %w", err)
	}
	model, _ := util.RawStringField(raw, "model")
	if model == "" {
		return nil, false, nil, fmt.Errorf("responses body: missing model")
	}
	stream = util.RawBoolField(raw, "stream")
	messages, err := inputToMessages(raw["input"])
	if err != nil {
		return nil, false, nil, err
	}
	if instr, ok := util.RawStringField(raw, "instructions"); ok && strings.TrimSpace(instr) != "" {
		messages = append([]map[string]any{{"role": "system", "content": instr}}, messages...)
	}
	if len(messages) == 0 {
		return nil, false, nil, fmt.Errorf("responses body: empty input")
	}
	if v, ok := raw["previous_response_id"]; ok && len(v) > 0 && string(v) != "null" {
		return nil, false, nil, errors.New("previous_response_id not supported by stateless prism proxy")
	}
	if storeRaw, ok := raw["store"]; ok && len(storeRaw) > 0 && string(storeRaw) != "null" {
		var store bool
		if err := json.Unmarshal(storeRaw, &store); err == nil && store {
			slog.Warn("responses_convert: store=true not supported by stateless prism proxy, ignoring")
		}
	}
	if includeRaw, ok := raw["include"]; ok && len(includeRaw) > 0 && string(includeRaw) != "null" {
		var includes []string
		if err := json.Unmarshal(includeRaw, &includes); err == nil {
			for _, inc := range includes {
				if inc == "encrypted_content" {
					return nil, false, nil, errors.New("include=encrypted_content not supported by prism proxy")
				}
				if inc == "annotations" {
					slog.Warn("responses_convert: include=annotations not supported by prism proxy, ignoring")
				}
			}
		}
	}
	for _, m := range messages {
		if hasImagePart(m["content"]) {
			return nil, false, nil, errors.New("image input not supported by prism proxy on /v1/responses; use /v1/chat/completions")
		}
	}
	messages = sanitize.NormalizeMessagesForChatAPI(messages)
	out := map[string]any{"model": model, "messages": messages, "stream": stream}
	if v, ok := raw["tools"]; ok && len(v) > 0 && string(v) != "null" {
		tools, toolsErr := mcp.SanitizeToolsForChatCompletions(v, tenantID)
		if toolsErr != nil {
			return nil, false, nil, toolsErr
		}
		if tools != nil {
			out["tools"] = tools
			copyOptionalRaw(raw, out, "tool_choice")
		}
	}
	copyOptionalRaw(raw, out, "temperature", "top_p", "stream_options", "thinking", "parallel_tool_calls", "user", "seed")
	if textRaw, ok := raw["text"]; ok && len(textRaw) > 0 && string(textRaw) != "null" {
		var textMap map[string]any
		if err := json.Unmarshal(textRaw, &textMap); err == nil {
			if fmtRaw, ok := textMap["format"]; ok {
				if fmtMap, ok := fmtRaw.(map[string]any); ok {
					if ft, ok := fmtMap["type"].(string); ok && (ft == "json_schema" || ft == "json_object") {
						out["response_format"] = fmtMap
					}
				}
			}
		}
	}
	if v, ok := raw["max_output_tokens"]; ok {
		out["max_tokens"] = util.JSONRawToAny(v)
	}
	if effort := util.ReasoningEffortFromRaw(raw["reasoning"]); effort != "" {
		out["reasoning_effort"] = effort
	} else if v, ok := raw["reasoning_effort"]; ok {
		out["reasoning_effort"] = util.JSONRawToAny(v)
	}
	chatBody, err = json.Marshal(out)
	return chatBody, stream, raw["tools"], err
}

func copyOptionalRaw(raw map[string]json.RawMessage, out map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := raw[k]; ok && len(v) > 0 && string(v) != "null" {
			out[k] = util.JSONRawToAny(v)
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
	messages = sanitize.MergeConsecutiveAssistantMessages(messages)

	return messages, nil
}

func responseItemToMessage(item json.RawMessage) (map[string]any, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(item, &obj); err != nil {
		return nil, err
	}
	typ, _ := util.RawStringField(obj, "type")
	if typ == "" {
		if role, _ := util.RawStringField(obj, "role"); role != "" {
			return map[string]any{"role": role, "content": contentFromRaw(obj["content"])}, nil
		}
		return nil, fmt.Errorf("missing type")
	}
	switch typ {
	case "message":
		role, _ := util.RawStringField(obj, "role")
		if role == "" {
			role = "user"
		}
		if role == "developer" {
			role = "system"
		}
		return map[string]any{"role": role, "content": contentFromRaw(obj["content"])}, nil
	case "function_call_output":
		callID, _ := util.RawStringField(obj, "call_id")
		if callID == "" {
			callID, _ = util.RawStringField(obj, "tool_call_id")
		}
		out, _ := util.RawStringField(obj, "output")
		return map[string]any{"role": "tool", "tool_call_id": callID, "content": out}, nil
	case "function_call":
		name, _ := util.RawStringField(obj, "name")
		ns, _ := util.RawStringField(obj, "namespace")
		if ns != "" {
			name = ns + "__" + name
		}
		args, _ := util.RawStringField(obj, "arguments")
		if args == "" {
			args = "{}"
		}
		callID, _ := util.RawStringField(obj, "call_id")
		if callID == "" {
			callID, _ = util.RawStringField(obj, "id")
		}
		msg := map[string]any{
			"role": "assistant",
			"tool_calls": []map[string]any{{
				"id": callID, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}
		if rc, ok := obj["reasoning_content"]; ok && len(rc) > 0 && string(rc) != "null" {
			msg["reasoning_content"] = util.JSONRawToAny(rc)
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
		if role, _ := util.RawStringField(obj, "role"); role != "" {
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
	return util.JSONRawToAny(raw)
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
			if util.DebugMode {
				slog.Debug("unknown content part type", "type", p["type"])
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
	if s, ok := util.RawStringField(obj, "summary"); ok && s != "" {
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
