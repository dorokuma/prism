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

func f(v float64) *float64 { return &v }

func TestFormatCost(t *testing.T) {
	cases := []struct {
		in   *float64
		want string
	}{
		{nil, "-"},
		{f(0.836), "$0.836"},
		{f(0), "$0.000"},
		{f(1234.5), "$1,234.500"},
		{f(999.9999), "$1,000.000"},
		{f(1000000.25), "$1,000,000.250"},
		{f(-0.5), "-$0.500"},
	}
	for _, c := range cases {
		if got := FormatCost(c.in); got != c.want {
			t.Errorf("FormatCost(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFormatCostCompact pins the compact cost precision: nil stays "-",
// amounts of $0.01 and above use at most two decimals ($21.0267 ->
// "$21.03"), amounts below $0.01 keep three decimals ($0.005 stays
// "$0.005") so a small fee never collapses to "$0.00" or "$0", and the
// thousands separators stay. The value itself is never modified.
func TestFormatCostCompact(t *testing.T) {
	cases := []struct {
		in   *float64
		want string
	}{
		{nil, "-"},
		{f(21.0267), "$21.03"},
		{f(0.836), "$0.84"},
		{f(0.6), "$0.60"},
		{f(21), "$21.00"},
		{f(0), "$0.000"},
		{f(0.005), "$0.005"},
		{f(0.004), "$0.004"},
		{f(0.0099), "$0.010"},
		{f(1234.5678), "$1,234.57"},
		{f(999.9999), "$1,000.00"},
		{f(1000000.25), "$1,000,000.25"},
		{f(-0.5), "-$0.50"},
	}
	for _, c := range cases {
		if got := FormatCostCompact(c.in); got != c.want {
			t.Errorf("FormatCostCompact(%v) = %q, want %q", c.in, got, c.want)
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
