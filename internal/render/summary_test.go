package render

import (
	"strings"
	"testing"
)

func TestRenderSummaryExample(t *testing.T) {
	cost := 0.836
	s := Summary{
		Period:      "今天 08-10",
		Requests:    1783,
		Tokens:      2_230_000,
		Cost:        &cost,
		Failures:    12,
		Streaming:   1690,
		CacheHits:   1_100_000,
		InputTokens: 2_200_000,
	}
	want := "  今天 08-10  ·  1,783 请求  ·  2.23M token  ·  $0.836\n" +
		"  失败 12 (0.7%)   流式 1,690 (94.8%)   缓存命中 1.1M (50.0%)\n"
	if got := RenderSummary(s); got != want {
		t.Fatalf("RenderSummary mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

func TestRenderSummarySkipsZeroFailures(t *testing.T) {
	s := Summary{Period: "x", Requests: 100, Streaming: 10}
	got := RenderSummary(s)
	if strings.Contains(got, "失败") {
		t.Errorf("zero failures must be omitted, got %q", got)
	}
	want := "  x  ·  100 请求  ·  0 token  ·  -\n  流式 10 (10.0%)   缓存命中 0 (-)\n"
	if got != want {
		t.Fatalf("RenderSummary mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	if strings.Contains(got, "$0.000") {
		t.Error("nil cost must not render as $0.000")
	}
}

func TestRenderSummaryPercentDenominators(t *testing.T) {
	s := Summary{
		Period:      "p",
		Requests:    1000,
		Tokens:      10_000_000, // must NOT be the cache-hit denominator
		Failures:    5,
		Streaming:   100,
		CacheHits:   1_100_000,
		InputTokens: 2_200_000,
	}
	got := RenderSummary(s)
	if !strings.Contains(got, "失败 5 (0.5%)") {
		t.Errorf("failure percent must use requests as denominator, got %q", got)
	}
	if !strings.Contains(got, "流式 100 (10.0%)") {
		t.Errorf("streaming percent must use requests as denominator, got %q", got)
	}
	if !strings.Contains(got, "缓存命中 1.1M (50.0%)") {
		t.Errorf("cache-hit percent must use input tokens as denominator, got %q", got)
	}
}

func TestRenderSummaryEmpty(t *testing.T) {
	got := RenderSummary(Summary{})
	if strings.Contains(got, "$0.000") {
		t.Errorf("nil cost must not render as $0.000, got %q", got)
	}
	if !strings.Contains(got, "0 请求") {
		t.Errorf("expected zero requests, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("output must end with a newline, got %q", got)
	}
}
