package main

import (
	"github.com/dorokuma/prism/internal/config"
)

// WireAPIMode controls which client-facing OpenAI-compatible surfaces are exposed.
//
//   - legacy:    POST /v1/chat/completions only (Prism, old clients)
//   - responses: POST /v1/responses only (Codex wire_api=responses), translated to upstream chat
//   - both:      expose both paths (default)
type WireAPIMode config.WireAPIMode

const (
	WireAPILegacy    WireAPIMode = "legacy"
	WireAPIResponses WireAPIMode = "responses"
	WireAPIBoth      WireAPIMode = "both"
)

// ParseWireAPIMode parses a WireAPI mode string.
func ParseWireAPIMode(s string) (WireAPIMode, error) {
	m, err := config.ParseWireAPIMode(s)
	return WireAPIMode(m), err
}

func (m WireAPIMode) allowsLegacy() bool {
	return config.WireAPIMode(m).AllowsLegacy()
}

func (m WireAPIMode) allowsResponses() bool {
	return config.WireAPIMode(m).AllowsResponses()
}
