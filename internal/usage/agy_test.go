package usage

import (
	"testing"
)

func TestMergeSummaryRowsByModel(t *testing.T) {
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "grok-4"}, Requests: 3, PromptTokens: 10, TotalTokens: 30, HitRateInputTokens: 10},
		{Groups: map[string]any{"model": "gemini-3.7-flash"}, Requests: 1, PromptTokens: 5, CachedTokens: 5, TotalTokens: 15, HitRateInputTokens: 10},
	}
	extra := []SummaryRow{
		{Groups: map[string]any{"model": "gemini-3.7-flash"}, Requests: 2, PromptTokens: 100, CachedTokens: 400, CompletionTokens: 20, ReasoningTokens: 50, TotalTokens: 570, HitRateInputTokens: 500},
		{Groups: map[string]any{"model": "gemini-3.8-flash"}, Requests: 1, PromptTokens: 10, CachedTokens: 90, TotalTokens: 100, HitRateInputTokens: 100},
	}
	got := MergeSummaryRows(rows, extra, []string{"model"})
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[1].Requests != 3 || got[1].PromptTokens != 105 || got[1].CachedTokens != 405 ||
		got[1].HitRateInputTokens != 510 || got[1].TotalTokens != 585 {
		t.Fatalf("merged gemini-3.7-flash = %+v", got[1])
	}
	if got[2].Groups["model"] != "gemini-3.8-flash" || got[2].Requests != 1 {
		t.Fatalf("appended row = %+v", got[2])
	}
}

func TestAddOverviewAnthropicDenom(t *testing.T) {
	ov := &Overview{Requests: 5, PromptTokens: 50, TotalTokens: 80, CachedTokens: 10}
	extra := []SummaryRow{{
		Requests: 2, PromptTokens: 100, CachedTokens: 400, CompletionTokens: 20,
		ReasoningTokens: 50, TotalTokens: 570, HitRateInputTokens: 500,
	}}
	AddOverview(ov, extra)
	if ov.Requests != 7 || ov.PromptTokens != 150 || ov.CachedTokens != 410 ||
		ov.CompletionTokens != 20 || ov.ReasoningTokens != 50 || ov.TotalTokens != 650 {
		t.Fatalf("overview = %+v", ov)
	}
	if ov.AnthropicRequests != 2 || ov.AnthropicPromptTokens != 100 || ov.AnthropicCachedTokens != 400 {
		t.Fatalf("anthropic bucket = %+v", ov)
	}
}

func TestCacheHitRateAgyDenom(t *testing.T) {
	r := SummaryRow{PromptTokens: 100, CachedTokens: 400, HitRateInputTokens: 500}
	if got := cacheHitRate(r.CachedTokens, r.cacheHitInput()); got != "80.0%" {
		t.Fatalf("hit rate = %s, want 80.0%% (400/500, not 400/100)", got)
	}
}
