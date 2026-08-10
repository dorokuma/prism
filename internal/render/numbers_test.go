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
		{0, "0"},
		{340, "340"},
		{999, "999"},
		{1000, "1k"},
		{1500, "1k"},
		{1999, "1k"},
		{340000, "340k"},
		{999999, "999k"},
		{1000000, "1M"},
		{1500000, "1.5M"},
		{1540000, "1.54M"},
		{9999999, "10M"},
		{10000000, "10M"},
		{100000000, "100M"},
		{100000001, "100M"},
		{123456789, "123.5M"},
		{-1500, "-1k"},
		{-1000000, "-1M"},
		{-1500000, "-1.5M"},
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
