// Package render renders usage statistics as terminal tables and summary
// lines. It is a leaf package: it depends only on the standard library and
// must not import any other package of this module.
package render

import (
	"strings"
	"unicode/utf8"
)

// wideRanges lists Unicode code point ranges whose characters occupy two
// terminal columns (East Asian Wide / Fullwidth). The list errs on the side
// of over-coverage so that CJK, fullwidth and emoji text never misaligns a
// table.
var wideRanges = []struct{ lo, hi rune }{
	{0x1100, 0x11FF},   // Hangul Jamo
	{0x231A, 0x231B},   // watch, hourglass (emoji)
	{0x23E9, 0x23F3},   // transport, clocks (emoji)
	{0x25FD, 0x25FE},   // medium squares (emoji)
	{0x2600, 0x27BF},   // Misc Symbols, Dingbats (emoji)
	{0x2934, 0x2935},   // curved arrows (emoji)
	{0x2B00, 0x2B55},   // arrows, squares, star (emoji)
	{0x2E80, 0x2EFF},   // CJK Radicals Supplement
	{0x2F00, 0x2FDF},   // Kangxi Radicals
	{0x2FF0, 0x2FFB},   // Ideographic Description Characters
	{0x3000, 0x303F},   // CJK Symbols and Punctuation
	{0x3040, 0x309F},   // Hiragana
	{0x30A0, 0x30FF},   // Katakana
	{0x3105, 0x312F},   // Bopomofo
	{0x3131, 0x318E},   // Hangul Compatibility Jamo
	{0x31A0, 0x31E3},   // Bopomofo Extended, CJK Strokes
	{0x31F0, 0x321E},   // Katakana Phonetic Extensions, Enclosed CJK
	{0x3220, 0x3247},   // Enclosed CJK Letters and Months
	{0x3250, 0x4DBF},   // Enclosed CJK, CJK Unified Ideographs Extension A
	{0x4E00, 0x9FFF},   // CJK Unified Ideographs
	{0xA000, 0xA4CF},   // Yi Syllables, Yi Radicals
	{0xA960, 0xA97C},   // Hangul Jamo Extended-A
	{0xAC00, 0xD7A3},   // Hangul Syllables
	{0xD7B0, 0xD7FF},   // Hangul Jamo Extended-B
	{0xF900, 0xFAFF},   // CJK Compatibility Ideographs
	{0xFE10, 0xFE19},   // Vertical Forms
	{0xFE30, 0xFE52},   // CJK Compatibility Forms
	{0xFE54, 0xFE66},   // CJK Compatibility Forms
	{0xFE68, 0xFE6B},   // CJK Compatibility Forms
	{0xFF00, 0xFF60},   // Fullwidth Forms
	{0xFFE0, 0xFFE6},   // Fullwidth Signs
	{0x16FE0, 0x16FE4}, // Ideographic Symbols and Punctuation
	{0x17000, 0x187F7}, // Tangut
	{0x18800, 0x18CD5}, // Tangut Components
	{0x1AFF0, 0x1AFFF}, // Kana Extended-B
	{0x1B000, 0x1B0FF}, // Kana Supplement
	{0x1B120, 0x1B122}, // Kana Extended-A
	{0x1B170, 0x1B2FB}, // Nushu
	{0x1F000, 0x1F0FF}, // Mahjong, Dominoes, Playing Cards
	{0x1F200, 0x1F2FF}, // Enclosed Ideographic Supplement
	{0x1F300, 0x1F9FF}, // Emoji (Misc Symbols and Pictographs et al.)
	{0x1FA70, 0x1FAFF}, // Symbols and Pictographs Extended-A
	{0x20000, 0x2A6DF}, // CJK Unified Ideographs Extension B
	{0x2A700, 0x2EBEF}, // CJK Unified Ideographs Extensions C-F
	{0x2EBF0, 0x2EE5F}, // CJK Unified Ideographs Extension I
	{0x2F800, 0x2FA1F}, // CJK Compatibility Ideographs Supplement
	{0x30000, 0x323AF}, // CJK Unified Ideographs Extensions G-H
}

// zeroWidthRanges lists code points that occupy no terminal column. They are
// checked before wideRanges so that, for example, emoji skin-tone modifiers
// inside U+1F300..U+1F9FF still count as zero.
var zeroWidthRanges = []struct{ lo, hi rune }{
	{0x0300, 0x036F},   // Combining Diacritical Marks
	{0x1AB0, 0x1AFF},   // Combining Diacritical Marks Extended
	{0x1DC0, 0x1DFF},   // Combining Diacritical Marks Supplement
	{0x200B, 0x200D},   // zero-width space / non-joiner / joiner
	{0xFE00, 0xFE0F},   // Variation Selectors
	{0xFE20, 0xFE2F},   // Combining Half Marks
	{0x1F3FB, 0x1F3FF}, // Emoji Modifiers (skin tones)
	{0xE0100, 0xE01EF}, // Variation Selectors Supplement
}

// DisplayWidth returns the number of terminal columns s occupies. ANSI escape
// sequences are ignored (they cost no columns), CJK / fullwidth / emoji
// characters count as 2, combining and zero-width characters as 0, and
// control characters as 0.
func DisplayWidth(s string) int {
	s = StripANSI(s)
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func runeWidth(r rune) int {
	if r <= 0x1F || (r >= 0x7F && r <= 0x9F) {
		return 0 // C0/C1 control characters
	}
	if inRanges(r, zeroWidthRanges) {
		return 0
	}
	if inRanges(r, wideRanges) {
		return 2
	}
	return 1
}

// inRanges reports whether r falls into any of the ranges. Ranges must be
// disjoint; the scan is linear, so ordering does not matter.
func inRanges(r rune, ranges []struct{ lo, hi rune }) bool {
	for _, rg := range ranges {
		if r >= rg.lo && r <= rg.hi {
			return true
		}
	}
	return false
}

// StripANSI removes ANSI escape sequences from s: CSI (ESC [ ... final byte
// in 0x40-0x7E), OSC (ESC ] ... terminated by BEL or ESC \), and two-
// character ESC X sequences. It returns s unchanged when no ESC is present.
func StripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1B) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != 0x1B {
			b.WriteByte(s[i])
			i++
			continue
		}
		i += ansiSeqLen(s[i:])
	}
	return b.String()
}

// ansiSeqLen returns the byte length of the escape sequence starting at the
// beginning of s (s[0] must be ESC). Unknown sequences are consumed
// conservatively so that no partial escape sequence is ever emitted.
func ansiSeqLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[': // CSI: ESC [ ... final byte in 0x40-0x7E
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7E {
				return i + 1
			}
		}
		return len(s)
	case ']': // OSC: ESC ] ... terminated by BEL or ST (ESC \)
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1B && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	default: // two-character sequence (ESC X)
		return 2
	}
}

// Truncate shortens s so that its display width is at most maxWidth,
// appending the ellipsis "…". Truncation is measured in display columns
// (see DisplayWidth) and never splits a multi-byte rune, so Chinese text or
// emoji are never cut in half. ANSI escape sequences are preserved whole:
// they cost no width and are never truncated mid-sequence. A maxWidth <= 0
// disables truncation and returns s unchanged.
func Truncate(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return s
	}
	if DisplayWidth(s) <= maxWidth {
		return s
	}
	budget := maxWidth - 1 // reserve one column for the ellipsis
	var b strings.Builder
	b.Grow(len(s) + 3)
	for i := 0; i < len(s); {
		if s[i] == 0x1B {
			n := ansiSeqLen(s[i:])
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := runeWidth(r)
		if rw > budget {
			break
		}
		budget -= rw
		b.WriteString(s[i : i+size])
		i += size
	}
	b.WriteRune('…')
	return b.String()
}
