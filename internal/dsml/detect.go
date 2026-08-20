package dsml

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// fullwidthBar is U+FF5C FULLWIDTH VERTICAL LINE, UTF-8 e3 bd 9c.
	fullwidthBar = "\uFF5C"
	dsmlToken    = fullwidthBar + "DSML" + fullwidthBar
)

// maxHoldBytes is the longest suffix we keep back as a possible marker prefix.
// It covers the longest prefix variant (fullwidth double-bar token plus a short
// ASCII tag start) including a split 3-byte fullwidth bar.
const maxHoldBytes = 48

// QuickCheck reports whether s might contain a DSML marker. It is a cheap
// ASCII scan; callers skip further work on the normal (no-DSML) path.
func QuickCheck(s string) bool {
	return containsDSMLFold(s)
}

// HasMarker reports whether s contains a DSML prefix variant outside a
// markdown code fence. Matching follows pi-tool-repair's prefix set:
// ｜DSML｜, ｜｜DSML｜｜, DSML｜, and halfwidth | DSML | with optional spaces.
func HasMarker(s string) bool {
	if !containsDSMLFold(s) {
		return false
	}
	return findMarker(s) >= 0
}

func containsDSMLFold(s string) bool {
	n := len(s)
	for i := 0; i+4 <= n; i++ {
		if foldDSML(s[i], s[i+1], s[i+2], s[i+3]) {
			return true
		}
	}
	return false
}

func foldDSML(a, b, c, d byte) bool {
	return (a|0x20) == 'd' && (b|0x20) == 's' && (c|0x20) == 'm' && (d|0x20) == 'l'
}

func findMarker(s string) int {
	n := len(s)
	for i := 0; i+4 <= n; i++ {
		if !foldDSML(s[i], s[i+1], s[i+2], s[i+3]) {
			continue
		}
		if isInsideCodeFence(s, i) {
			continue
		}
		if markerAround(s, i) {
			return i
		}
	}
	return -1
}

// markerAround reports whether the DSML at index i is wrapped in a known
// prefix variant. i points at the 'D'/'d'.
func markerAround(s string, i int) bool {
	after := s[i+4:]
	before := s[:i]

	if hasFullwidthAfter(after) {
		// ｜DSML｜ or ｜｜DSML｜｜ (leading bars optional for DSML｜).
		if strings.HasPrefix(after, fullwidthBar+fullwidthBar) || strings.HasPrefix(after, fullwidthBar) {
			return true
		}
		// DSML｜
		return true
	}
	if hasFullwidthBefore(before) && hasFullwidthAfter(after) {
		return true
	}
	// Halfwidth | DSML | with optional whitespace on either side of each bar.
	return halfwidthBars(before, after)
}

func hasFullwidthAfter(after string) bool {
	return strings.HasPrefix(after, fullwidthBar)
}

func hasFullwidthBefore(before string) bool {
	return strings.HasSuffix(before, fullwidthBar)
}

func halfwidthBars(before, after string) bool {
	if !pipeAfter(after) {
		return false
	}
	return pipeBefore(before)
}

func pipeAfter(after string) bool {
	i := 0
	for i < len(after) && isWS(after[i]) {
		i++
	}
	return i < len(after) && after[i] == '|'
}

func pipeBefore(before string) bool {
	i := len(before) - 1
	for i >= 0 && isWS(before[i]) {
		i--
	}
	return i >= 0 && before[i] == '|'
}

func isWS(b byte) bool {
	return b == ' ' || b == '\t'
}

// isInsideCodeFence reports whether index sits inside an odd number of ```
// fences (pi-tool-repair isInsideCodeFence).
func isInsideCodeFence(s string, index int) bool {
	if index < 0 {
		index = 0
	}
	if index > len(s) {
		index = len(s)
	}
	n := 0
	run := 0
	for i := 0; i < index; i++ {
		if s[i] == '`' {
			run++
			if run == 3 {
				n++
				run = 0
			}
		} else {
			run = 0
		}
	}
	return n%2 == 1
}

// holdLen returns how many trailing bytes of s must be withheld because they
// are a prefix of a DSML marker (including a split UTF-8 fullwidth bar).
func holdLen(s string) int {
	if s == "" {
		return 0
	}
	max := len(s)
	if max > maxHoldBytes {
		max = maxHoldBytes
	}
	for n := max; n > 0; n-- {
		// Hold at a UTF-8 boundary when possible, but also hold a 1- or 2-byte
		// tail that is a truncated U+FF5C.
		suf := s[len(s)-n:]
		if isMarkerPrefix(suf) {
			return n
		}
	}
	return 0
}

func isMarkerPrefix(suf string) bool {
	if suf == "" {
		return false
	}
	for _, n := range markerNeedles {
		if prefixOfNeedle(suf, n) {
			return true
		}
	}
	return truncatedFullwidthBar(suf)
}

func truncatedFullwidthBar(suf string) bool {
	bar := fullwidthBar // 3 bytes e3 bd 9c
	if len(suf) >= 3 {
		return false
	}
	return strings.HasPrefix(bar, suf)
}

func prefixOfNeedle(suf, needle string) bool {
	if len(suf) > len(needle) {
		return false
	}
	return asciiFoldPrefix(needle, suf)
}

// asciiFoldPrefix reports whether suf is a byte prefix of needle, folding only
// ASCII letters. Fullwidth bars and other non-ASCII must match exactly.
func asciiFoldPrefix(needle, suf string) bool {
	if len(suf) > len(needle) {
		return false
	}
	for i := 0; i < len(suf); {
		sr, ssize := utf8.DecodeRuneInString(suf[i:])
		nr, nsize := utf8.DecodeRuneInString(needle[i:])
		if ssize != nsize {
			return false
		}
		if sr == utf8.RuneError && ssize == 1 {
			if suf[i] != needle[i] {
				return false
			}
			i++
			continue
		}
		if sr <= unicode.MaxASCII && nr <= unicode.MaxASCII {
			if unicode.ToLower(sr) != unicode.ToLower(nr) {
				return false
			}
		} else if sr != nr {
			return false
		}
		i += ssize
	}
	return true
}

// markerNeedles are complete marker prefixes whose any prefix must be held
// back. Case variants of DSML are generated in init.
var markerNeedles []string

func init() {
	bases := []string{
		"<" + dsmlToken,
		"</" + dsmlToken,
		"<" + fullwidthBar + fullwidthBar + "DSML" + fullwidthBar + fullwidthBar,
		"</" + fullwidthBar + fullwidthBar + "DSML" + fullwidthBar + fullwidthBar,
		"<DSML" + fullwidthBar,
		"</DSML" + fullwidthBar,
		"<|DSML|",
		"</|DSML|",
		"<| DSML |",
		"</| DSML |",
		"< | DSML |",
		dsmlToken,
		fullwidthBar + fullwidthBar + "DSML" + fullwidthBar + fullwidthBar,
		"DSML" + fullwidthBar,
		"|DSML|",
		"| DSML |",
		"|  DSML  |",
		" | DSML | ",
		"|\tDSML\t|",
		"<|  DSML  |",
	}
	seen := make(map[string]struct{}, len(bases)*16)
	for _, b := range bases {
		for _, v := range dsmlCaseVariants(b) {
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			markerNeedles = append(markerNeedles, v)
		}
	}
}

func dsmlCaseVariants(s string) []string {
	idx := -1
	for i := 0; i+4 <= len(s); i++ {
		if foldDSML(s[i], s[i+1], s[i+2], s[i+3]) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return []string{s}
	}
	out := make([]string, 0, 16)
	for _, a := range []byte{'D', 'd'} {
		for _, b := range []byte{'S', 's'} {
			for _, c := range []byte{'M', 'm'} {
				for _, d := range []byte{'L', 'l'} {
					buf := []byte(s)
					buf[idx], buf[idx+1], buf[idx+2], buf[idx+3] = a, b, c, d
					out = append(out, string(buf))
				}
			}
		}
	}
	return out
}
