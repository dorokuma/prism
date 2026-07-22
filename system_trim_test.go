package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dorokuma/prism/internal/util"
)

func TestStripCodexUpstreamBloat(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty string returns default prompt",
			input: "",
			want:  "You are a helpful coding assistant.",
		},
		{
			name:  "whitespace only returns default prompt",
			input: "   \n  \t  ",
			want:  "You are a helpful coding assistant.",
		},
		{
			name:  "normal text passes through unchanged",
			input: "You are a helpful assistant.",
			want:  "You are a helpful assistant.",
		},
		{
			name:  "text with leading/trailing whitespace is trimmed",
			input: "  \n  helpful  \n  ",
			want:  "helpful",
		},
		{
			name:  "removes skills_instructions block",
			input: "System prompt before\n<skills_instructions>\nUse codegraph for search.\n</skills_instructions>\nSystem prompt after",
			want:  "System prompt before\n\nSystem prompt after",
		},
		{
			name:  "removes permissions instructions block",
			input: "Header\n<permissions instructions>\nallow all\n</permissions instructions>\nFooter",
			want:  "Header\n\nFooter",
		},
		{
			name:  "removes both skills and permissions blocks",
			input: "A\n<skills_instructions>\nskill stuff\n</skills_instructions>\nB\n<permissions instructions>\nperm stuff\n</permissions instructions>\nC",
			want:  "A\n\nB\n\nC",
		},
		{
			name:  "case insensitive matching for skills block",
			input: "A\n<SKILLS_INSTRUCTIONS>\nstuff\n</SKILLS_INSTRUCTIONS>\nB",
			want:  "A\n\nB",
		},
		{
			name:  "case insensitive matching for permissions block",
			input: "A\n<PERMISSIONS INSTRUCTIONS>\nstuff\n</PERMISSIONS INSTRUCTIONS>\nB",
			want:  "A\n\nB",
		},
		{
			name:  "multiline regex matches across newlines inside tags",
			input: "Before\n<skills_instructions>\nline1\nline2\nline3\n</skills_instructions>\nAfter",
			want:  "Before\n\nAfter",
		},
		{
			name:  "no bloat blocks returns original",
			input: "Just a normal system prompt.",
			want:  "Just a normal system prompt.",
		},
		{
			name:  "nested tags handled by lazy regex",
			input: "Outer\n<skills_instructions>\n<skills_instructions>inner</skills_instructions>\n</skills_instructions>\nEnd",
			// The lazy regex <skills_instructions>.*?</skills_instructions> stops at the first </skills_instructions>,
			// removing the inner pair and leaving the orphaned outer closing tag.
			want: "Outer\n\n</skills_instructions>\nEnd",
		},
		{
			name:  "malformed tag (no closing) leaves text untouched by that regex",
			input: "<skills_instructions>\nno closing tag here\nFinal text",
			want:  "<skills_instructions>\nno closing tag here\nFinal text",
		},
		{
			name:  "only bloat blocks result in default prompt",
			input: "<skills_instructions>\nbloat\n</skills_instructions>",
			want:  "You are a helpful coding assistant.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripCodexUpstreamBloat(tc.input)
			if got != tc.want {
				t.Errorf("stripCodexUpstreamBloat() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStripCodexUpstreamBloat_Truncation(t *testing.T) {
	// Build a string with systemPromptMaxRunes + 100 runes
	over := systemPromptMaxRunes + 100
	var sb strings.Builder
	for i := 0; i < over; i++ {
		sb.WriteRune('a')
	}
	longInput := sb.String()

	got := stripCodexUpstreamBloat(longInput)

	// Should be truncated to systemPromptMaxRunes + truncation suffix
	if !strings.HasSuffix(got, truncationSuffix) {
		t.Fatalf("expected truncation suffix, got (last 100 chars): %q", got[len(got)-min(100, len(got)):])
	}

	gotRunes := []rune(got)
	suffixRunes := utf8.RuneCountInString(truncationSuffix)
	expectedTotal := systemPromptMaxRunes + suffixRunes
	if len(gotRunes) != expectedTotal {
		t.Errorf("truncated length = %d runes, want %d runes (%d content + %d suffix)",
			len(gotRunes), expectedTotal, systemPromptMaxRunes, suffixRunes)
	}

	// First systemPromptMaxRunes should be 'a' characters
	for i := 0; i < systemPromptMaxRunes; i++ {
		if gotRunes[i] != 'a' {
			t.Fatalf("content rune at position %d is %q, want 'a'", i, string(gotRunes[i]))
		}
	}
}

func TestStripCodexUpstreamBloat_ExactlyAtLimit(t *testing.T) {
	// Build a string exactly at systemPromptMaxRunes
	var sb strings.Builder
	for i := 0; i < systemPromptMaxRunes; i++ {
		sb.WriteRune('x')
	}
	exactInput := sb.String()

	got := stripCodexUpstreamBloat(exactInput)

	if got != exactInput {
		t.Errorf("string at limit should not be truncated, got length %d, want %d", len([]rune(got)), systemPromptMaxRunes)
	}
}

func TestStripCodexUpstreamBloat_MultiByteUTF8(t *testing.T) {
	// Build a string with multi-byte characters exceeding the limit
	// Each '世' is 3 bytes but 1 rune
	var sb strings.Builder
	for i := 0; i < systemPromptMaxRunes+10; i++ {
		sb.WriteString("世")
	}
	input := sb.String()

	got := stripCodexUpstreamBloat(input)

	// Should have systemPromptMaxRunes content runes + suffix
	gotRunes := []rune(got)
	suffixRunes := utf8.RuneCountInString(truncationSuffix)
	if len(gotRunes) != systemPromptMaxRunes+suffixRunes {
		t.Errorf("multi-byte truncated length = %d runes, want %d", len(gotRunes), systemPromptMaxRunes+suffixRunes)
	}

	// The content should be valid UTF-8 - no split multi-byte chars
	if !strings.HasSuffix(got, truncationSuffix) {
		t.Fatal("multi-byte truncation should end with suffix")
	}
}

func TestStripCodexUpstreamBloat_DebugLogging(t *testing.T) {
	// Capture slog output when debugMode is enabled.
	var buf bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(oldDefault)

	oldDebug := util.DebugMode
	util.DebugMode = true
	defer func() { util.DebugMode = oldDebug }()

	_ = stripCodexUpstreamBloat("Some system prompt.")

	output := buf.String()
	// No bloat blocks, no empty, no truncation → no debug logs expected.
	if output != "" {
		t.Logf("debug output for normal prompt: %s", output)
	}

	buf.Reset()
	_ = stripCodexUpstreamBloat("")
	output = buf.String()
	if !strings.Contains(output, "replacing with default") {
		t.Errorf("expected debug log about empty prompt replacement, got: %s", output)
	}

	buf.Reset()
	_ = stripCodexUpstreamBloat("<skills_instructions>\nstuff\n</skills_instructions>")
	output = buf.String()
	// After bloat removal, string becomes empty → should trigger the empty replacement log.
	if !strings.Contains(output, "replacing with default") {
		t.Errorf("expected empty-replacement debug log after bloat removal, got: %s", output)
	}

	buf.Reset()
	// Build a string that triggers truncation.
	var sb strings.Builder
	for i := 0; i < systemPromptMaxRunes+100; i++ {
		sb.WriteRune('a')
	}
	_ = stripCodexUpstreamBloat(sb.String())
	output = buf.String()
	if !strings.Contains(output, "truncating system prompt") {
		t.Errorf("expected debug log about truncation, got: %s", output)
	}
}

func TestStripCodexUpstreamBloat_RemovesAndTruncates(t *testing.T) {
	// Input has a bloat block in the middle, plus enough content BEFORE
	// and AFTER the block so that after bloat removal the remaining text
	// still exceeds systemPromptMaxRunes → removal first, then truncation.
	var sb strings.Builder
	// Fill prefix so it alone exceeds the limit
	for i := 0; i < systemPromptMaxRunes+500; i++ {
		sb.WriteRune('a')
	}
	sb.WriteString("\n<skills_instructions>\n")
	sb.WriteString("bloat inside skills block")
	sb.WriteString("\n</skills_instructions>\n")
	// Fill suffix so after removal the total is still > limit
	for i := 0; i < 500; i++ {
		sb.WriteRune('b')
	}

	got := stripCodexUpstreamBloat(sb.String())

	// Block must be removed
	if strings.Contains(got, "skills_instructions") {
		t.Error("skills_instructions block should have been removed")
	}

	// Truncation must have happened (removal first, then truncation)
	if !strings.HasSuffix(got, truncationSuffix) {
		t.Fatal("expected truncation suffix after removal+truncation")
	}

	gotRunes := []rune(got)
	suffixLen := utf8.RuneCountInString(truncationSuffix)
	if len(gotRunes) != systemPromptMaxRunes+suffixLen {
		t.Errorf("after removal+truncation got %d runes, want %d (content) + %d (suffix) = %d",
			len(gotRunes), systemPromptMaxRunes, suffixLen, systemPromptMaxRunes+suffixLen)
	}
}
