package render

import (
	"strings"
	"testing"
)

func TestSummaryLine(t *testing.T) {
	cost := 0.836
	s := Summary{
		Requests: 1783,
		Tokens:   2_230_000,
		Cost:     &cost,
	}
	want := "请求 1,783 · 词元 2.23M · 开销 $0.836"
	if got := SummaryLine(s); got != want {
		t.Fatalf("SummaryLine mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	if strings.HasSuffix(want, "\n") {
		t.Error("the summary row carries no trailing newline (the caller pads it)")
	}
}

func TestSummaryLineNilCost(t *testing.T) {
	s := Summary{
		Requests: 100,
		Tokens:   500_000,
		Cost:     nil,
	}
	want := "请求 100 · 词元 500k · 开销 -"
	got := SummaryLine(s)
	if got != want {
		t.Fatalf("SummaryLine mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	if strings.Contains(got, "$0.000") {
		t.Error("nil cost must not render as $0.000")
	}
	if strings.Contains(got, "命中") {
		t.Error("cache hit lines must not appear")
	}
}

func TestSummaryLineEmpty(t *testing.T) {
	want := "请求 0 · 词元 0 · 开销 -"
	if got := SummaryLine(Summary{}); got != want {
		t.Fatalf("SummaryLine mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}
