package dsml

import "encoding/json"

// RewriteCompletion rewrites a non-streaming OpenAI chat completion body.
// Events without a DSML marker in message.content are returned unchanged
// (same slice when no work is needed).
func RewriteCompletion(body []byte) []byte {
	if !QuickCheck(string(body)) {
		return body
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	root, wrapKey := completionRoot(raw)
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(root["choices"], &choices); err != nil || len(choices) == 0 {
		return body
	}
	changed := false
	for i, ch := range choices {
		msgRaw, ok := ch["message"]
		if !ok {
			continue
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(msgRaw, &msg); err != nil {
			continue
		}
		craw, ok := msg["content"]
		if !ok || len(craw) == 0 || string(craw) == "null" {
			continue
		}
		content, ok := decodeJSONString(craw)
		if !ok {
			continue
		}
		if !HasMarker(string(content)) {
			continue
		}
		changed = true
		if calls, recovered := RecoverInvokes(string(content)); recovered {
			msg["content"] = json.RawMessage("null")
			msg["tool_calls"] = mustJSON(openaiToolCalls(calls))
			ch["message"] = mustJSON(msg)
			ch["finish_reason"] = mustJSON("tool_calls")
		} else {
			msg["content"] = mustJSON(removedNotice(len(content)))
			ch["message"] = mustJSON(msg)
		}
		choices[i] = ch
	}
	if !changed {
		return body
	}
	root["choices"] = mustJSON(choices)
	if wrapKey != "" {
		raw[wrapKey] = mustJSON(root)
	} else {
		raw = root
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// completionRoot returns the object that holds choices. Clinepass non-stream
// bodies are sometimes wrapped as {"data":{...completion...},"success":true}.
func completionRoot(raw map[string]json.RawMessage) (map[string]json.RawMessage, string) {
	if _, ok := raw["choices"]; ok {
		return raw, ""
	}
	innerRaw, ok := raw["data"]
	if !ok || len(innerRaw) == 0 {
		return raw, ""
	}
	var inner map[string]json.RawMessage
	if json.Unmarshal(innerRaw, &inner) != nil {
		return raw, ""
	}
	if _, ok := inner["choices"]; !ok {
		return raw, ""
	}
	return inner, "data"
}

func openaiToolCalls(calls []ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]any{
			"id":   newCallID(),
			"type": "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": c.Arguments,
			},
		})
	}
	return out
}
