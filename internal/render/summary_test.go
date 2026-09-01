package render

import (
	"strings"
	"testing"
)

func TestRenderSummaryExample(t *testing.T) {
	cost := 0.836
	s := Summary{
		Requests: 1783,
		Tokens:   2_230_000,
		Cost:     &cost,
	}
	want := "  请求     1,783\n" +
		"  总词元   2.23M\n" +
		"  总开销   $0.836\n"
	if got := RenderSummary(s); got != want {
		t.Fatalf("RenderSummary mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

func TestRenderSummaryNilCost(t *testing.T) {
	s := Summary{
		Requests: 100,
		Tokens:   500_000,
		Cost:     nil,
	}
	want := "  请求     100\n" +
		"  总词元   500k\n" +
		"  总开销   -\n"
	got := RenderSummary(s)
	if got != want {
		t.Fatalf("RenderSummary mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	if strings.Contains(got, "$0.000") {
		t.Error("nil cost must not render as $0.000")
	}
	if strings.Contains(got, "命中") {
		t.Error("cache hit lines must not appear")
	}
}

func TestRenderSummaryEmpty(t *testing.T) {
	got := RenderSummary(Summary{})
	if strings.Contains(got, "$0.000") {
		t.Errorf("nil cost must not render as $0.000, got %q", got)
	}
	if !strings.Contains(got, "请求     0") {
		t.Errorf("expected zero requests, got %q", got)
	}
	if strings.Contains(got, "命中") {
		t.Errorf("cache hit line must not appear, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("output must end with a newline, got %q", got)
	}
	want := "  请求     0\n" +
		"  总词元   0\n" +
		"  总开销   -\n"
	if got != want {
		t.Fatalf("RenderSummary mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}
