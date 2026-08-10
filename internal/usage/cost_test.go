package usage

import (
	"math"
	"testing"
)

// TestCostFormulaDivisorOneMillion is the acceptance test for the pricing
// formula: the divisor is exactly 1e6 (prices are per million tokens). If the
// divisor were 1e3 the result would be 1000x, if 1e9 it would be 1000x less.
func TestCostFormulaDivisorOneMillion(t *testing.T) {
	price := &Price{Input: 1.5, Output: 3.0, CacheRead: 0.3, CacheWrite: 0.6}
	cost, status := ComputeCost(1_000_000, 2_000_000, 250_000, 100_000, SourceOpenAI, price)
	if status != CostStatusOK {
		t.Fatalf("status = %q, want ok", status)
	}
	if cost == nil {
		t.Fatal("cost is nil")
	}
	// (1e6-250e3)/1e6*1.5 + 250e3/1e6*0.3 + 100e3/1e6*0.6 + 2e6/1e6*3.0
	// = 1.125 + 0.075 + 0.06 + 6.0 = 7.26
	want := 7.26
	if math.Abs(*cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v (divisor must be 1e6)", *cost, want)
	}
}

func TestCostMissingPrice(t *testing.T) {
	cost, status := ComputeCost(100, 50, 0, 0, SourceOpenAI, nil)
	if cost != nil {
		t.Fatalf("cost = %v, want nil for missing price", *cost)
	}
	if status != CostStatusMissingPrice {
		t.Fatalf("status = %q, want missing_price", status)
	}
	// missing price takes precedence even when tokens are zero
	cost, status = ComputeCost(0, 0, 0, 0, SourceOpenAI, nil)
	if cost != nil || status != CostStatusMissingPrice {
		t.Fatalf("zero tokens + nil price: cost=%v status=%q, want nil/missing_price", cost, status)
	}
}

func TestCostNoUsage(t *testing.T) {
	cost, status := ComputeCost(0, 0, 0, 0, SourceOpenAI, &Price{Input: 1, Output: 1})
	if status != CostStatusNoUsage {
		t.Fatalf("status = %q, want no_usage", status)
	}
	if cost == nil || *cost != 0 {
		t.Fatalf("cost = %v, want 0", cost)
	}
}

func TestCostZeroPriceStructIsPriced(t *testing.T) {
	// a non-nil all-zero Price means "priced at 0", not "missing"
	cost, status := ComputeCost(100, 0, 0, 0, SourceOpenAI, &Price{})
	if status != CostStatusOK {
		t.Fatalf("status = %q, want ok", status)
	}
	if cost == nil || *cost != 0 {
		t.Fatalf("cost = %v, want 0", cost)
	}
}

func TestCostCachedTokensUseCacheReadPrice(t *testing.T) {
	// cached tokens must be priced at CacheRead, not Input
	price := &Price{Input: 1.0, CacheRead: 0.1, CacheWrite: 0.2}
	cost, status := ComputeCost(1000, 0, 1000, 0, SourceOpenAI, price)
	if status != CostStatusOK {
		t.Fatalf("status = %q", status)
	}
	want := 1000.0 / 1e6 * 0.1 // 0.0001
	if math.Abs(*cost-want) > 1e-12 {
		t.Fatalf("cached cost = %v, want %v", *cost, want)
	}
}

func TestCostCacheWriteTokensPricedSeparately(t *testing.T) {
	price := &Price{Input: 1.0, Output: 2.0, CacheRead: 0.1, CacheWrite: 0.2}
	cost, status := ComputeCost(0, 0, 0, 1_000_000, SourceOpenAI, price)
	if status != CostStatusOK {
		t.Fatalf("status = %q", status)
	}
	if math.Abs(*cost-0.2) > 1e-12 {
		t.Fatalf("cache_write cost = %v, want 0.2", *cost)
	}
}

// TestCostReasoningIncludedInCompletion documents that reasoning_tokens are
// not an input to ComputeCost: they are already part of completion_tokens and
// must never be priced a second time. The formula test above pins the
// completion term to Output exactly.
func TestCostReasoningIncludedInCompletion(t *testing.T) {
	// completion 1M tokens at Output 2.0 → 2.0 regardless of reasoning split
	cost, _ := ComputeCost(0, 1_000_000, 0, 0, SourceOpenAI, &Price{Input: 1, Output: 2})
	if math.Abs(*cost-2.0) > 1e-12 {
		t.Fatalf("completion cost = %v, want 2.0", *cost)
	}
}

// TestCostAnthropicCacheNotSubtracted is the acceptance test for the
// Anthropic pricing 口径: Anthropic input_tokens EXCLUDES the cache counters
// (cache_read_input_tokens / cache_creation_input_tokens are separate
// top-level counters), so the prompt must be billed in full at the Input
// rate. Applying the OpenAI formula (prompt - cached) would reprice part of
// the input at the cheaper CacheRead rate and undercount the cost.
func TestCostAnthropicCacheNotSubtracted(t *testing.T) {
	price := &Price{Input: 1.5, Output: 3.0, CacheRead: 0.3, CacheWrite: 0.6}
	// Same token counts as the OpenAI formula test: prompt 1M, cached 250K,
	// cache_write 100K, completion 2M.
	anth, status := ComputeCost(1_000_000, 2_000_000, 250_000, 100_000, SourceAnthropic, price)
	if status != CostStatusOK {
		t.Fatalf("status = %q, want ok", status)
	}
	// 1M/1e6*1.5 + 250K/1e6*0.3 + 100K/1e6*0.6 + 2M/1e6*3.0
	// = 1.5 + 0.075 + 0.06 + 6.0 = 7.635
	// The prompt is billed in FULL (1.5): the cached 250K must NOT be
	// subtracted from the input before pricing.
	wantAnthropic := 7.635
	if math.Abs(*anth-wantAnthropic) > 1e-9 {
		t.Fatalf("anthropic cost = %v, want %v (input must be billed in full)", *anth, wantAnthropic)
	}

	// The same numbers through the OpenAI formula: 7.26 (cached repriced at
	// CacheRead). The gap (0.375 = 250K/1e6*(1.5-0.3)) is exactly the
	// undercount the old shared formula produced for Anthropic.
	openai, _ := ComputeCost(1_000_000, 2_000_000, 250_000, 100_000, SourceOpenAI, price)
	if math.Abs(*openai-7.26) > 1e-9 {
		t.Fatalf("openai cost = %v, want 7.26", *openai)
	}
	if !(*anth > *openai) {
		t.Fatalf("anthropic cost %v must exceed openai cost %v (cache must not reduce the input bill)", *anth, *openai)
	}
}

// TestCostUnknownSourceDefaultsToOpenAIFormula: events without a Source
// marker (legacy paths, hand-built test events) keep the historical OpenAI
// formula — the fix must not change their numbers.
func TestCostUnknownSourceDefaultsToOpenAIFormula(t *testing.T) {
	price := &Price{Input: 1.5, Output: 3.0, CacheRead: 0.3, CacheWrite: 0.6}
	cost, status := ComputeCost(1_000_000, 2_000_000, 250_000, 100_000, "", price)
	if status != CostStatusOK {
		t.Fatalf("status = %q, want ok", status)
	}
	if math.Abs(*cost-7.26) > 1e-9 {
		t.Fatalf("cost with empty source = %v, want 7.26 (OpenAI formula preserved)", *cost)
	}
}
