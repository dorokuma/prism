package main

import (
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
	s = reSkillsBlock.ReplaceAllString(s, "")
	s = rePermsBlock.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if s == "" {
		return "You are a helpful coding assistant."
	}
	const maxSystemRunes = 12000
	if utf8.RuneCountInString(s) > maxSystemRunes {
		// Truncate to maxSystemRunes runes, preserving complete UTF-8 characters
		runes := []rune(s)
		return string(runes[:maxSystemRunes]) + "\n\n[... truncated for upstream compatibility]"
	}
	return s
}
