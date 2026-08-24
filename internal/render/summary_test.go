package render

import (
	"strings"
	"testing"
)

func TestRenderSummaryExample(t *testing.T) {
	cost := 0.836
	s := Summary{
		Period:   "今天 08-10",
		Requests: 1783,
		Tokens:   2_230_000,
		Cost:     &cost,
		OpenAI:   &CacheStats{Hits: 1_100_000, Input: 2_200_000},
	}
	want := "  今天 08-10  ·  1,783 请求  ·  2.23M 词元  ·  $0.836\n" +
		"  命中(OpenAI) 1.1M (50.0%)\n"
	if got := RenderSummary(s); got != want {
		t.Fatalf("RenderSummary mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

func TestRenderSummaryOmitsSecondLineWhenNoCache(t *testing.T) {
	// No cache segments set: neither family has requests, so no second
	// line may appear — not even "命中 0 (-)" noise.
	s := Summary{Period: "x", Requests: 100}
	got := RenderSummary(s)
	if strings.Contains(got, "命中") {
		t.Errorf("families with no requests must render no cache segment, got %q", got)
	}
	want := "  x  ·  100 请求  ·  0 词元  ·  -\n"
	if got != want {
		t.Fatalf("RenderSummary mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	if strings.Contains(got, "$0.000") {
		t.Error("nil cost must not render as $0.000")
	}
}

func TestRenderSummaryCachePercentDenominator(t *testing.T) {
	s := Summary{
		Period:   "p",
		Requests: 1000,
		Tokens:   10_000_000, // must NOT be the cache-hit denominator
		OpenAI:   &CacheStats{Hits: 1_100_000, Input: 2_200_000},
	}
	got := RenderSummary(s)
	if !strings.Contains(got, "命中(OpenAI) 1.1M (50.0%)") {
		t.Errorf("cache-hit percent must use the segment's input tokens as denominator, got %q", got)
	}
}

func TestRenderSummaryCacheSegments(t *testing.T) {
	// Both segments side by side, OpenAI first, each with its own
	// denominator: the anthropic input is the already-assembled total
	// (input + cache_read + cache_creation), which is why 500/501 renders
	// 99.8% — never a value above 100%.
	s := Summary{
		Period:    "p",
		Requests:  100,
		OpenAI:    &CacheStats{Hits: 900_000, Input: 1_000_000},
		Anthropic: &CacheStats{Hits: 500, Input: 501},
	}
	got := RenderSummary(s)
	if !strings.Contains(got, "命中(OpenAI) 900k (90.0%)   命中(Anthropic) 500 (99.8%)") {
		t.Errorf("both segments must render side by side, got %q", got)
	}

	// A nil anthropic segment (no anthropic requests) must be omitted.
	got = RenderSummary(Summary{Period: "p", Requests: 5, OpenAI: &CacheStats{Hits: 1, Input: 10}})
	if strings.Contains(got, "Anthropic") {
		t.Errorf("nil anthropic segment must be omitted, got %q", got)
	}
	if !strings.Contains(got, "命中(OpenAI) 1 (10.0%)") {
		t.Errorf("openai segment missing, got %q", got)
	}

	// A nil openai segment (no openai requests) must be omitted.
	got = RenderSummary(Summary{Period: "p", Requests: 5, Anthropic: &CacheStats{Hits: 1, Input: 10}})
	if strings.Contains(got, "OpenAI") {
		t.Errorf("nil openai segment must be omitted, got %q", got)
	}
	if !strings.Contains(got, "命中(Anthropic) 1 (10.0%)") {
		t.Errorf("anthropic segment missing, got %q", got)
	}

	// A zero-input denominator renders "-" (never a division by zero).
	got = RenderSummary(Summary{Period: "p", Requests: 1, OpenAI: &CacheStats{Hits: 0, Input: 0}})
	if !strings.Contains(got, "命中(OpenAI) 0 (-") {
		t.Errorf("zero denominator must render as -, got %q", got)
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
