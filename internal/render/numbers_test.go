package render

import (
	"math"
	"testing"
)

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

// TestFormatTokensYi pins the Chinese-unit formatter for the 亿 unit: ONE unit
// (亿), one decimal, and a whole 亿 that drops its fraction. A non-zero count
// below 0.05 亿 — which one decimal would collapse to "0" — is rendered with
// TWO decimals instead, so a tiny pool reads as a number instead of colliding
// with the "no data" zero; only an exact zero, and anything below
// 0.005 亿, reads "0亿". This function itself has no 万 / 千万 step and no
// "<0.1亿" fallback. The width loop below feeds the shapes the quota card's
// field is sized around (a two-decimal side against a whole 亿 side) and pins
// that such a pair is exactly the field wide (clineNumberWidth). It is not a
// statement about every side this function can be handed — a count in the high
// 亿 magnitudes writes more integer digits and a wider string, which is the
// pair ladder's own residual (see planusage.formatTokenPair).
//
// The 亿 unit is no longer the ONLY unit a quota card may use (a card picks 亿 /
// 万 / 千 from the window's total, see planusage.formatTokenPair), but this is
// still the function that defines the 亿 shape — and FormatTokensUnit, which the
// other units go through, carries exactly the precision rule asserted here.
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
	// The pairs fed below stay inside the quota card's clineNumberWidth display
	// columns: a two-decimal side and a four-digit whole 亿 side are the same width.
	for _, pair := range [][2]int64{{999_900_000_000, 999_900_000_000}, {2_000_000, 999_900_000_000}, {999_900_000_000, 2_000_000}} {
		if w := DisplayWidth(FormatTokensYi(pair[0]) + "/" + FormatTokensYi(pair[1])); w != 13 {
			t.Errorf("pair %v = %d display columns, want 13", pair, w)
		}
	}
}

// TestFormatTokensUnit pins the formatter the quota card's used/total pair goes
// through, for every unit on its ladder: div is the divisor that defines the unit
// (1e8 亿, 1e4 万, 1e3 千) and suffix is what follows the number, while the
// PRECISION rule is the one 亿 defines — one decimal, trailing zeros then the
// point trimmed, two decimals for a non-zero count one decimal would collapse to
// "0", and "0<unit>" only for an exact zero or a value below 0.005 of the unit.
//
// A non-positive div renders digits (div = 1) instead of an "Inf": the divisor is
// a caller's constant, so a mistake must stay readable rather than poison the row.
func TestFormatTokensUnit(t *testing.T) {
	cases := []struct {
		in     int64
		div    int64
		suffix string
		want   string
	}{
		// 亿 is FormatTokensYi's contract, reached through the same code path.
		{1_300_000_000, 1e8, "亿", "13亿"},
		{2_000_000, 1e8, "亿", "0.02亿"},
		// int64's top: the float64 scale holds the count one token high, and one
		// decimal then rounds the sub-unit remainder away, so the string lands below
		// the exact count (92233720368.54775807… 亿).
		{math.MaxInt64, 1e8, "亿", "92233720368.5亿"},
		// 万: a whole unit, one decimal, two decimals for a tiny share.
		{10_000_000, 1e4, "万", "1000万"},
		{1_000_000, 1e4, "万", "100万"},
		{20_000, 1e4, "万", "2万"},
		{200, 1e4, "万", "0.02万"},
		{5_000, 1e4, "万", "0.5万"},
		// 千: the smallest unit, so a small pool is a number and not a row of zeros.
		{1_299, 1e3, "千", "1.3千"},
		{1_000, 1e3, "千", "1千"},
		{20, 1e3, "千", "0.02千"},
		{0, 1e3, "千", "0千"},
		{4, 1e3, "千", "0千"}, // below 0.005 千: even two decimals round away
		// A non-positive divisor is treated as 1.
		{7, 0, "x", "7x"},
		{7, -1e8, "x", "7x"},
		// The sign is carried like FormatTokensYi's.
		{-30_000, 1e4, "万", "-3万"},
	}
	for _, c := range cases {
		if got := FormatTokensUnit(c.in, c.div, c.suffix); got != c.want {
			t.Errorf("FormatTokensUnit(%d, %d, %q) = %q, want %q", c.in, c.div, c.suffix, got, c.want)
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
