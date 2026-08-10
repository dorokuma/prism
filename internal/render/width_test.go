package render

import "testing"

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
