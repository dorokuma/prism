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

// TestComputeCost_CachedClampedToPrompt guards the negative-cost hole: a
// broken upstream report with cached > prompt must be clamped to [0, prompt]
// so the OpenAI prompt-cached term can never go negative. Negative cached is
// clamped to 0.
func TestComputeCost_CachedClampedToPrompt(t *testing.T) {
	price := &Price{Input: 1.0, Output: 1.0, CacheRead: 0.5, CacheWrite: 0.5}

	// cached (100) > prompt (10): clamp to 10 → all prompt priced at
	// CacheRead: 10/1e6*0.5 = 0.000005, never negative.
	cost, status := ComputeCost(10, 0, 100, 0, SourceOpenAI, price)
	if status != CostStatusOK {
		t.Fatalf("status = %q, want ok", status)
	}
	want := 10.0 / 1e6 * 0.5
	if math.Abs(*cost-want) > 1e-15 {
		t.Errorf("cost = %v, want %v (cached must be clamped to prompt)", *cost, want)
	}
	if *cost < 0 {
		t.Fatal("cost must never be negative")
	}

	// Negative cached: clamp to 0 → full prompt priced at Input.
	cost, status = ComputeCost(10, 0, -5, 0, SourceOpenAI, price)
	if status != CostStatusOK {
		t.Fatalf("status = %q, want ok", status)
	}
	want = 10.0 / 1e6 * 1.0
	if math.Abs(*cost-want) > 1e-15 {
		t.Errorf("cost with negative cached = %v, want %v (clamped to 0)", *cost, want)
	}

	// Anthropic form never subtracts cached from prompt: unchanged.
	cost, status = ComputeCost(10, 0, 100, 0, SourceAnthropic, price)
	if status != CostStatusOK {
		t.Fatalf("status = %q, want ok", status)
	}
	want = 10.0/1e6*1.0 + 100.0/1e6*0.5
	if math.Abs(*cost-want) > 1e-15 {
		t.Errorf("anthropic cost = %v, want %v (no subtraction)", *cost, want)
	}
}

// TestComputeCost_NegativeInputsClamped guards the entry clamp: every token
// count is clamped to >= 0 before any arithmetic, so a broken upstream
// report (or a programmatically built event) with negative values can never
// produce a negative cost — in either formula. OpenAI keeps the cached ≤
// prompt cap; Anthropic only clamps non-negative and keeps its formula
// semantics.
func TestComputeCost_NegativeInputsClamped(t *testing.T) {
	price := &Price{Input: 1.0, Output: 2.0, CacheRead: 0.5, CacheWrite: 0.5}

	// All four negative → clamped to zero → no_usage, cost 0.
	cost, status := ComputeCost(-10, -5, -3, -2, SourceOpenAI, price)
	if status != CostStatusNoUsage {
		t.Errorf("all-negative OpenAI: status = %q, want no_usage", status)
	}
	if cost == nil || *cost != 0 {
		t.Errorf("all-negative OpenAI: cost = %v, want 0", cost)
	}

	// Mixed: negative prompt/cached/cache_write with positive completion →
	// only the positive completion is priced (2M at Output 2.0 = 4.0); the
	// negative values must not leak into any term.
	cost, status = ComputeCost(-100, 2_000_000, -50, -20, SourceOpenAI, price)
	if status != CostStatusOK {
		t.Errorf("mixed OpenAI: status = %q, want ok", status)
	}
	if cost == nil || math.Abs(*cost-4.0) > 1e-12 {
		t.Errorf("mixed OpenAI: cost = %v, want 4.0 (only completion priced)", cost)
	}

	// OpenAI: negative cached with positive prompt → cached clamped to 0,
	// full prompt priced at Input (cached ≤ prompt still enforced).
	cost, status = ComputeCost(10, 0, -5, 0, SourceOpenAI, price)
	if status != CostStatusOK {
		t.Errorf("negative cached OpenAI: status = %q, want ok", status)
	}
	if cost == nil || math.Abs(*cost-10.0/1e6*1.0) > 1e-15 {
		t.Errorf("negative cached OpenAI: cost = %v, want %v", cost, 10.0/1e6*1.0)
	}

	// Anthropic: same negative-input shape — clamped to zero, never
	// negative; formula semantics unchanged for valid inputs.
	cost, status = ComputeCost(-10, -5, -3, -2, SourceAnthropic, price)
	if status != CostStatusNoUsage {
		t.Errorf("all-negative Anthropic: status = %q, want no_usage", status)
	}
	if cost == nil || *cost != 0 {
		t.Errorf("all-negative Anthropic: cost = %v, want 0", cost)
	}
	cost, status = ComputeCost(10, 0, -3, -2, SourceAnthropic, price)
	if status != CostStatusOK {
		t.Errorf("mixed Anthropic: status = %q, want ok", status)
	}
	if cost == nil || math.Abs(*cost-10.0/1e6*1.0) > 1e-15 {
		t.Errorf("mixed Anthropic: cost = %v, want %v (prompt billed in full, negatives dropped)", cost, 10.0/1e6*1.0)
	}

	// Every result is non-negative across the whole negative-input space.
	for _, src := range []string{SourceOpenAI, SourceAnthropic} {
		for _, tc := range [][4]int64{
			{-1, 0, 0, 0}, {0, -1, 0, 0}, {0, 0, -1, 0}, {0, 0, 0, -1},
			{-5, -5, 0, 0}, {-5, 0, 100, 0}, {0, -5, 0, 100}, {-1, -1, -1, -1},
		} {
			c, _ := ComputeCost(tc[0], tc[1], tc[2], tc[3], src, price)
			if c != nil && *c < 0 {
				t.Errorf("ComputeCost(%v, %q) = %v, must never be negative", tc, src, *c)
			}
		}
	}
}
