package main

import (
	"encoding/json"
	"strings"
)

// normalizeMessagesForChatAPI flattens multimodal/text parts and merges system turns
// for OpenAI-compatible upstreams (OpenCode Go, DeepSeek, etc.).
func normalizeMessagesForChatAPI(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	var systemBuf strings.Builder

	flushSystem := func() {
		if systemBuf.Len() == 0 {
			return
		}
		out = append(out, map[string]any{
			"role":    "system",
			"content": stripCodexUpstreamBloat(systemBuf.String()),
		})
		systemBuf.Reset()
	}

	for _, m := range messages {
		role, _ := m["role"].(string)
		if role == "developer" {
			role = "system"
		}
		content := flattenMessageContent(m["content"])
		if role == "system" {
			if systemBuf.Len() > 0 {
				systemBuf.WriteString("\n\n")
			}
			systemBuf.WriteString(content)
			continue
		}
		flushSystem()
		msg := map[string]any{"role": role, "content": content}
		if rc, ok := m["reasoning_content"]; ok {
			msg["reasoning_content"] = rc
		}
		if tc, ok := m["tool_calls"]; ok {
			msg["tool_calls"] = tc
		}
		if tid, ok := m["tool_call_id"]; ok {
			msg["tool_call_id"] = tid
		}
		out = append(out, msg)
	}
	flushSystem()
	return mergeConsecutiveAssistantMessages(out)
}

func copyStringMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// mergeConsecutiveAssistantMessages collapses consecutive assistant turns into
// a single message, preserving the longest non-empty reasoning_content and
// concatenating tool_calls. Other roles/system/user/tool are left untouched.
func mergeConsecutiveAssistantMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	var pending *map[string]any

	flushPending := func() {
		if pending == nil {
			return
		}
		out = append(out, *pending)
		pending = nil
	}

	for _, m := range messages {
		role, _ := m["role"].(string)
		if role != "assistant" {
			flushPending()
			out = append(out, copyStringMap(m))
			continue
		}

		if pending == nil {
			p := copyStringMap(m)
			pending = &p
			continue
		}

		// merge: keep the first non-empty content value
		if c, ok := asString((*pending)["content"]); !ok || c == "" {
			if c2, ok2 := asString(m["content"]); ok2 && c2 != "" {
				(*pending)["content"] = c2
			}
		}

		// merge reasoning_content: keep the longest non-empty string
		if rc, ok := asString(m["reasoning_content"]); ok && rc != "" {
			if existing, exOk := asString((*pending)["reasoning_content"]); !exOk || len(rc) > len(existing) {
				(*pending)["reasoning_content"] = rc
			}
		}

		// merge tool_calls arrays
		if tcs, ok := m["tool_calls"].([]map[string]any); ok && len(tcs) > 0 {
			existing, _ := (*pending)["tool_calls"].([]map[string]any)
			merged := make([]map[string]any, 0, len(existing)+len(tcs))
			merged = append(merged, existing...)
			merged = append(merged, tcs...)
			(*pending)["tool_calls"] = merged
		}
	}

	flushPending()
	return out
}

func flattenMessageContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case nil:
		return ""
	case []any:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text", "input_text", "output_text", "summary_text", "reasoning_text":
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			default:
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	case []map[string]any:
		parts := make([]any, len(v))
		for i := range v {
			parts[i] = v[i]
		}
		return flattenMessageContent(parts)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
