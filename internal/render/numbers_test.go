package render

import "testing"

func TestFormatInt(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1783, "1,783"},
		{999999, "999,999"},
		{1000000, "1,000,000"},
		{1234567890, "1,234,567,890"},
		{-1783, "-1,783"},
		{-1234567, "-1,234,567"},
		{-9223372036854775808, "-9,223,372,036,854,775,808"},
	}
	for _, c := range cases {
		if got := FormatInt(c.in); got != c.want {
			t.Errorf("FormatInt(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatTokens(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		// plain and k segments: small values are unchanged.
		{0, "0"},
		{340, "340"},
		{999, "999"},
		{1000, "1k"},
		{1500, "1k"},
		{1999, "1k"},
		{340000, "340k"},
		{999999, "999k"},
		// M segment: the original two-decimal window and one-decimal style.
		{1000000, "1M"},
		{1500000, "1.5M"},
		{1540000, "1.54M"},
		{9999999, "10M"},
		{10000000, "10M"},
		{100000000, "100M"},
		{100000001, "100M"},
		{123456789, "123.5M"},
		// No carry: an M value that still scales below 1000 stays in M.
		{166500000, "166.5M"},
		{839200000, "839.2M"},
		{999900000, "999.9M"},
		// Carry: a render that round-trips to 1000 of its unit moves one
		// unit up, including the rounding boundary 999.96M -> 1B.
		{999960000, "1B"},
		{999999999, "1B"},
		{1000000000, "1B"},
		{2235900000, "2.2B"},
		{4557800000, "4.6B"},
		{999900000000, "999.9B"},
		// Carry across two unit steps: B fills up and overflows into T, and
		// the chain keeps carrying (T -> P -> E) so the scaled value stays
		// below 1000 across the whole int64 range.
		{999960000000, "1T"},
		{1234567000000, "1.2T"},
		{999960000000000, "1P"},
		{1999000000000000, "2P"},
		{9223372036854775807, "9.2E"},
		// negated values carry the same way.
		{-1500, "-1k"},
		{-1000000, "-1M"},
		{-1500000, "-1.5M"},
		{-999960000, "-1B"},
		{-4557800000, "-4.6B"},
		{-9223372036854775808, "-9.2E"},
	}
	for _, c := range cases {
		if got := FormatTokens(c.in); got != c.want {
			t.Errorf("FormatTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFormatTokensYi pins the Chinese-unit formatter the quota card's
// used/total pair uses: ONE unit (亿), one decimal, and a whole 亿 that
// drops its fraction. A non-zero count below 0.05 亿 — which one decimal
// would collapse to "0" — is rendered with TWO decimals instead, so a tiny
// pool reads as a number ("0.02亿") instead of colliding with the "no data"
// zero; only an exact zero, and anything below 0.005 亿, reads "0亿". There
// is no 万 / 千万 step and no "<0.1亿" fallback. "9999亿" and "0.04亿" are
// both the widest single side, so the quota card's 13-column pair budget
// ("0.02亿/9999亿" = 13) is never exceeded.
func TestFormatTokensYi(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0亿"},
		{499_999, "0亿"}, // below 0.005 亿: two decimals still round to zero
		{500_000, "0.01亿"},
		{1_000_000, "0.01亿"},
		{2_000_000, "0.02亿"},
		{4_000_000, "0.04亿"},
		{4_999_999, "0.05亿"},
		{5_000_000, "0.1亿"}, // one decimal takes over from here up
		{10_000_000, "0.1亿"},
		{30_000_000, "0.3亿"},
		{220_000_000, "2.2亿"},
		{340_000_000, "3.4亿"},
		{1_000_000_000, "10亿"},
		{1_300_000_000, "13亿"},
		{999_900_000_000, "9999亿"},
		{-1_300_000_000, "-13亿"},
		{-30_000_000, "-0.3亿"},
	}
	for _, c := range cases {
		if got := FormatTokensYi(c.in); got != c.want {
			t.Errorf("FormatTokensYi(%d) = %q, want %q", c.in, got, c.want)
		}
	}
	// The widest pair stays inside the quota card's clineNumberWidth = 13
	// display columns: a two-decimal side is exactly as wide as "9999亿".
	for _, pair := range [][2]int64{{999_900_000_000, 999_900_000_000}, {2_000_000, 999_900_000_000}, {999_900_000_000, 2_000_000}} {
		if w := DisplayWidth(FormatTokensYi(pair[0]) + "/" + FormatTokensYi(pair[1])); w != 13 {
			t.Errorf("pair %v = %d display columns, want 13", pair, w)
		}
	}
}

func TestFormatPercent(t *testing.T) {
	cases := []struct {
		part, total float64
		want        string
	}{
		{12, 1783, "0.7%"},
		{1690, 1783, "94.8%"},
		{0, 100, "0.0%"},
		{1, 2, "50.0%"},
		{1100000, 2200000, "50.0%"},
		{100, 100, "100.0%"},
		{1, 0, "-"},
		{0, 0, "-"},
		{1.1, 2.2, "50.0%"},
	}
	for _, c := range cases {
		if got := FormatPercent(c.part, c.total); got != c.want {
			t.Errorf("FormatPercent(%v, %v) = %q, want %q", c.part, c.total, got, c.want)
		}
	}
}
