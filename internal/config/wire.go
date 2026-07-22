package config

import (
	"fmt"
	"strings"
)

// WireAPIMode controls which client-facing OpenAI-compatible surfaces are exposed.
//
//   - legacy:    POST /v1/chat/completions only (Prism, old clients)
//   - responses: POST /v1/responses only (Codex wire_api=responses), translated to upstream chat
//   - both:      expose both paths (default)
type WireAPIMode string

const (
	WireAPILegacy    WireAPIMode = "legacy"
	WireAPIResponses WireAPIMode = "responses"
	WireAPIBoth      WireAPIMode = "both"
)

// ParseWireAPIMode parses a WireAPI mode string.
func ParseWireAPIMode(s string) (WireAPIMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "both":
		return WireAPIBoth, nil
	case "legacy", "chat", "chat_completions", "chat-completions":
		return WireAPILegacy, nil
	case "responses", "response":
		return WireAPIResponses, nil
	default:
		return "", fmt.Errorf("wire_api: unknown value %q (want legacy, responses, or both)", s)
	}
}

// AllowsLegacy returns true if the mode supports legacy chat completions.
func (m WireAPIMode) AllowsLegacy() bool {
	return m == WireAPILegacy || m == WireAPIBoth
}

// AllowsResponses returns true if the mode supports the responses API.
func (m WireAPIMode) AllowsResponses() bool {
	return m == WireAPIResponses || m == WireAPIBoth
}
