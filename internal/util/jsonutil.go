package util

import (
	"encoding/json"
	"log/slog"
	"strings"
)

// RawStringField extracts a string value from a JSON object.
func RawStringField(m map[string]json.RawMessage, key string) (string, bool) {
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

// RawBoolField extracts a bool value from a JSON object.
func RawBoolField(m map[string]json.RawMessage, key string) bool {
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

// JSONRawToAny unmarshals a raw message into an any value.
func JSONRawToAny(raw json.RawMessage) any {
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

// IsDeepSeekModel reports whether the upstream model name indicates a DeepSeek model.
func IsDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "deepseek")
}

// MapThoughtLevel maps Codex thinking level to DeepSeek-compatible level.
// low/medium/high → high, xhigh → max. Other values pass through unchanged.
func MapThoughtLevel(level string) string {
	switch strings.ToLower(level) {
	case "low", "medium", "high":
		return "high"
	case "xhigh":
		return "max"
	default:
		return level
	}
}

// ReasoningEffortFromRaw extracts the reasoning effort from a raw message.
func ReasoningEffortFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	if _, ok := obj["summary"]; ok {
		slog.Warn("responses_convert: reasoning.summary not supported by prism proxy, ignoring")
	}
	if e, ok := obj["effort"].(string); ok {
		return e
	}
	return ""
}

// FinishReasonToStatus maps an OpenAI finish_reason to a Responses API status.
func FinishReasonToStatus(reason string) string {
	switch reason {
	case "length", "content_filter":
		return "incomplete"
	default:
		return "completed"
	}
}
