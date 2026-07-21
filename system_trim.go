package main

import (
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	reSkillsBlock = regexp.MustCompile(`(?is)<skills_instructions>.*?</skills_instructions>`)
	rePermsBlock  = regexp.MustCompile(`(?is)<permissions instructions>.*?</permissions instructions>`)
)

// stripCodexUpstreamBloat removes Codex-only context that OpenCode Go chat endpoints
// often reject or that blows request size without helping the upstream model.
func stripCodexUpstreamBloat(system string) string {
	s := strings.TrimSpace(system)
	origLen := len(s)
	s = reSkillsBlock.ReplaceAllString(s, "")
	s = rePermsBlock.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if debugMode {
		if len(s) != origLen {
			slog.Debug("strip bloat removed", "before", origLen, "after", len(s))
		}
	}
	if s == "" {
		if debugMode {
			slog.Debug("strip empty system prompt after bloat removal, replacing with default")
		}
		return "You are a helpful coding assistant."
	}
	if utf8.RuneCountInString(s) > systemPromptMaxRunes {
		// Truncate to systemPromptMaxRunes runes, preserving complete UTF-8 characters
		if debugMode {
			slog.Debug("strip truncating system prompt", "from_runes", utf8.RuneCountInString(s), "to_runes", systemPromptMaxRunes)
		}
		runes := []rune(s)
		return string(runes[:systemPromptMaxRunes]) + truncationSuffix
	}
	return s
}
