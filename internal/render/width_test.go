package render

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"ascii", "hello", 5},
		{"model-name", "deepseek-v4-flash", 17},
		{"cjk", "中文", 4},
		{"cjk-mixed", "a中b", 4},
		{"cjk-ext-a", "\u3400\u4dbf", 4},
		{"cjk-ext-b", "\U00020000", 2},
		{"cjk-ext-g", "\U00030000", 2},
		{"cjk-ext-h", "\U00031350", 2},
		{"cjk-ext-i", "\U0002ebf0", 2},
		{"cjk-compat", "\uf900", 2},
		{"cjk-compat-supp", "\U0002f800", 2},
		{"kangxi", "\u2f00", 2},
		{"cjk-forms", "\ufe30", 2},
		{"vertical-forms", "\ufe10", 2},
		{"hiragana", "あいう", 6},
		{"katakana", "アイウ", 6},
		{"halfwidth-katakana", "ｱｲｳ", 3},
		{"hangul", "한글", 4},
		{"hangul-jamo", "\u1100\u1161", 4},
		{"hangul-jamo-jongseong", "\u11a8", 2},
		{"hangul-ext-a", "\ua960", 2},
		{"hangul-ext-b", "\ud7b0", 2},
		{"bopomofo", "ㄅㄆ", 4},
		{"yi", "\ua000", 2},
		{"fullwidth-forms", "ＡＢＣ", 6},
		{"fullwidth-signs", "￥", 2},
		{"halfwidth-punct", "\uff61", 1},
		{"cjk-punct", "，。！", 6},
		{"ideographic-space", "\u3000", 2},
		{"middle-dot", "·", 1},
		{"emoji-misc-symbols", "☀", 2},
		{"emoji-dingbats", "➡", 2},
		{"emoji-main", "🔥", 2},
		{"emoji-plus-cjk", "🔥中", 4},
		{"emoji-skin-tone-zero", "👍🏻", 2},
		{"emoji-zwj-family", "👨\u200d👩\u200d👧", 6},
		{"emoji-variation-selector", "\u2764\ufe0f", 2},
		{"combining-accent", "e\u0301", 1},
		{"zero-width-space", "a\u200bb", 2},
		{"control-chars", "a\x00b\x07c", 3},
		{"tab", "\ta", 1},
		{"ansi-stripped", "\x1b[31m中\x1b[0m", 2},
		{"ansi-multiple", "\x1b[1;31mabc\x1b[0m", 3},
		{"ansi-osclink", "\x1b]8;;https://x\x1b\\中\x1b]8;;\x1b\\", 2},
		{"tangut", "\U00017000", 2},
		{"kana-supplement", "\U0001b000", 2},
		{"nushu", "\U0001b170", 2},
		{"enclosed-cjk", "㊙", 2},
		{"ideographic-marks", "\u16fe0", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DisplayWidth(c.in); got != c.want {
				t.Fatalf("DisplayWidth(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestDisplayWidthMixedASCIIChineseEmoji(t *testing.T) {
	s := "a中b🔥c" // a=1 中=2 b=1 🔥=2 c=1
	if got := DisplayWidth(s); got != 7 {
		t.Fatalf("DisplayWidth(%q) = %d, want 7", s, got)
	}
}

func TestDisplayWidthCapsuleGlyphs(t *testing.T) {
	// The quota capsule bar is measured in cells: ▰ (used) and ▱
	// (remaining) must each cost exactly one column, or every card row
	// drifts off cardWidth.
	if got := DisplayWidth("▰"); got != 1 {
		t.Errorf("DisplayWidth(▰) = %d, want 1", got)
	}
	if got := DisplayWidth("▱"); got != 1 {
		t.Errorf("DisplayWidth(▱) = %d, want 1", got)
	}
	if got := DisplayWidth("▰▰▰▱▱▱"); got != 6 {
		t.Errorf("DisplayWidth(capsule) = %d, want 6", got)
	}
}

func TestPadLeftPadRight(t *testing.T) {
	cases := []struct {
		name  string
		s     string
		w     int
		right string
		left  string
	}{
		{"ascii pad", "ab", 5, "ab   ", "   ab"},
		{"exact", "abc", 3, "abc", "abc"},
		{"cjk pad", "中", 3, "中 ", " 中"},
		{"truncate right", "abcdef", 4, "abc…", "abc…"},
		// Wide-character truncation: Truncate stops one column short when
		// the double-width rune that follows does not fit the column the
		// ellipsis needs, so the pad buys the slot its last column back.
		{"cjk truncate odd budget", "中中中中中", 4, "中… ", " 中…"},
		{"emoji truncate odd budget", "🔥🔥🔥", 4, "🔥… ", " 🔥…"},
		{"zero width", "abc", 0, "", ""},
		{"negative width", "abc", -2, "", ""},
		{"ansi not counted", Brand("x"), 3, Brand("x") + "  ", "  " + Brand("x")},
		{"empty string", "", 3, "   ", "   "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PadRight(c.s, c.w); got != c.right {
				t.Errorf("PadRight(%q, %d) = %q, want %q", c.s, c.w, got, c.right)
			}
			if got := PadLeft(c.s, c.w); got != c.left {
				t.Errorf("PadLeft(%q, %d) = %q, want %q", c.s, c.w, got, c.left)
			}
			if dw := DisplayWidth(PadRight(c.s, c.w)); dw != max(c.w, 0) {
				t.Errorf("PadRight width = %d, want %d", dw, max(c.w, 0))
			}
			if dw := DisplayWidth(PadLeft(c.s, c.w)); dw != max(c.w, 0) {
				t.Errorf("PadLeft width = %d, want %d", dw, max(c.w, 0))
			}
		})
	}
}

func TestPadWideTruncationFillsToWidth(t *testing.T) {
	// The "59-column pit": Truncate reserves one column for the ellipsis,
	// so a truncation budget that ends on an odd column cannot take the
	// next double-width rune and the result lands at w-1 columns. Every
	// padded slot must still measure exactly w columns, whatever the
	// content — that is the invariant the 60-column cards rely on.
	// Truncation semantics are NOT changed by the pad: an over-wide string
	// is still "kept prefix + …" and never longer than w.
	inputs := map[string]string{
		"cjk":        strings.Repeat("中", 40),
		"cjk-odd":    strings.Repeat("度", 33) + "a",
		"cjk-mixed":  "深" + strings.Repeat("度", 30) + "abc",
		"emoji":      strings.Repeat("\U0001f525", 40),
		"fullwidth":  strings.Repeat("Ａ", 40),
		"cjk-suffix": strings.Repeat("模型", 20) + "-0123456789",
	}
	for name, in := range inputs {
		if DisplayWidth(in) <= 60 {
			t.Fatalf("%s: fixture must be wider than 60 columns", name)
		}
		for w := 1; w <= 60; w++ {
			right, left := PadRight(in, w), PadLeft(in, w)
			if dw := DisplayWidth(right); dw != w {
				t.Fatalf("%s: PadRight(in, %d) is %d columns: %q", name, w, dw, right)
			}
			if dw := DisplayWidth(left); dw != w {
				t.Fatalf("%s: PadLeft(in, %d) is %d columns: %q", name, w, dw, left)
			}
			for _, got := range []string{right, left} {
				if !utf8.ValidString(got) {
					t.Fatalf("%s: w=%d split a rune: %q", name, w, got)
				}
				if !strings.Contains(got, "…") {
					t.Fatalf("%s: w=%d lost the ellipsis: %q", name, w, got)
				}
				if trimmed := strings.TrimRight(got, " "); DisplayWidth(trimmed) > w {
					t.Fatalf("%s: w=%d truncated content overflows: %q", name, w, got)
				}
			}
		}
	}
}

// TestPadRightCJKTruncatePadsToWidth is the headline case: a 60-column budget
// filled with CJK. Truncate keeps 29 中 and stops (the 30th needs the
// column the ellipsis occupies), so the raw truncation is 59 columns and
// the pad — NOT a change of the truncation rule — buys the 60th column
// back. Without it every card row that outgrows its slot lands one column
// narrow and the right border drifts.
func TestPadRightCJKTruncatePadsToWidth(t *testing.T) {
	in := strings.Repeat("中", 40) // 80 columns
	want := strings.Repeat("中", 29) + "… "
	if got := PadRight(in, 60); got != want {
		t.Fatalf("PadRight(cjk, 60) = %q, want %q", got, want)
	}
	if got := PadLeft(in, 60); got != " "+strings.Repeat("中", 29)+"…" {
		t.Fatalf("PadLeft(cjk, 60) = %q", got)
	}
	// A string that already fits is untouched: no ellipsis, no pad column.
	exact := strings.Repeat("中", 30) // exactly 60 columns
	if got := PadRight(exact, 60); got != exact {
		t.Fatalf("an exact-fit string must not be touched: %q", got)
	}
}

func TestStripANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[38;5;196mhi\x1b[39m", "hi"},
		{"a\x1b[1mb\x1b[0mc", "abc"},
		{"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"\x1b]0;title\x07x", "x"},
		{"\x1bM", ""},
		{"\x1b", ""},
		{"tail\x1b", "tail"},
	}
	for _, c := range cases {
		if got := StripANSI(c.in); got != c.want {
			t.Errorf("StripANSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"abcdefgh", 5, "abcd…"},
		{"abcdefgh", 3, "ab…"},
		{"abcdefgh", 1, "…"},
		{"abcdefgh", 8, "abcdefgh"},
		{"abcdefgh", 0, "abcdefgh"},
		{"", 5, ""},
		{"中中中中中", 7, "中中中…"},
		{"中中中中中", 9, "中中中中…"},
		{"中a中", 4, "中a…"},
		{"deepseek-v4-flash中文", 10, "deepseek-…"},
		{"🔥🔥🔥", 5, "🔥🔥…"},
		{"e\u0301e\u0301e\u0301", 2, "e\u0301…"},
		{"\x1b[31mred\x1b[0m", 2, "\x1b[31mr…"},
		{"\x1b[31mred\x1b[0m", 3, "\x1b[31mred\x1b[0m"},
	}
	for _, c := range cases {
		if got := Truncate(c.in, c.max); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}
