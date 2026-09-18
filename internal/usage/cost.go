package usage

import "log/slog"

// Price holds per-million-token USD prices for one model. The unit is USD per
// 1,000,000 tokens, matching the production pricing table (values in the
// 0.14–4.4 range would be absurd per-token). A nil *Price means the model has
// no known price.
type Price struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Cost status values persisted in usage_events.cost_status.
const (
	CostStatusOK           = "ok"
	CostStatusNoUsage      = "no_usage"
	CostStatusMissingPrice = "missing_price"
)

// Source markers for the usage wire format an Event's token counts were
// parsed from. They select the cost formula (see ComputeCost) and are
// persisted so the pricing basis of any row can be audited. Empty Source
// (legacy events built without a parser) falls back to the OpenAI formula.
const (
	SourceOpenAI    = "openai"
	SourceAnthropic = "anthropic"
	// SourcePi is usage_source for historical Pi-session import rows.
	// Overview and hit-rate SQL treat it like Anthropic (cache counters
	// excluded from input_tokens). New rows are no longer imported.
	SourcePi = "pi"
)

// ComputeCost computes the USD cost of a usage record. It is the single
// pricing point: called once, on the synchronous request finalization path
// (middleware.EmitAudit → the wiring-stage pricer), and the result is
// carried on the event and persisted as-is — the async write path never
// re-prices, so the audit log amount and the stored amount are identical.
// The result is therefore fixed at request finalization time; later price
// changes never rewrite history.
//
// The divisor is 1e6 because prices are per million tokens:
//
//	OpenAI (source == SourceOpenAI, or empty): prompt_tokens INCLUDES the
//	cached tokens, so the cached portion is repriced at CacheRead and the
//	rest at Input:
//	    cost = (prompt - cached)/1e6*Input + cached/1e6*CacheRead
//	         + cache_write/1e6*CacheWrite + max(0, completion - reasoning)/1e6*Output + reasoning/1e6*ReasoningPrice
//
//	Anthropic (source == SourceAnthropic): input_tokens EXCLUDES the cache
//	counters (cache_read_input_tokens and cache_creation_input_tokens are
//	separate top-level counters), so the prompt is billed in full and the
//	cache counters at their own rates — no subtraction:
//	    cost = prompt/1e6*Input + cached/1e6*CacheRead
//	         + cache_write/1e6*CacheWrite + max(0, completion - reasoning)/1e6*Output + reasoning/1e6*ReasoningPrice
//
//	A CacheRead of 0 (YAML cache_read omitted, or an all-zero Price) is
//	treated as "not configured" and falls back to Input. Cache hits are
//	never silently free; set an explicit positive cache_read to discount.
//
// reasoning_tokens are billed at the model's Output rate (ReasoningPrice=Output)
// without double billing: under subset semantics (reasoning included in completion),
// the output portion is max(0, completion - reasoning)*Output + reasoning*ReasoningPrice.
// If reasoning > completion (abnormal/parallel reporting), a warning is logged.
// Returns:
//
//   - nil, CostStatusMissingPrice when price is nil (tokens are still
//     persisted; cost_usd is NULL because the request was not priced);
//   - 0, CostStatusNoUsage when every token count is zero (e.g. streaming
//     ended without a usage payload);
//   - the computed cost, CostStatusOK otherwise.
func ComputeCost(promptTokens, completionTokens, cachedTokens, cacheWriteTokens, reasoningTokens int64, source string, price *Price) (*float64, string) {
	if price == nil {
		return nil, CostStatusMissingPrice
	}
	// Entry clamp: every token count is non-negative by definition, so a
	// broken upstream report (or a programmatically built event) with
	// negative tokens is normalized to 0 before any arithmetic — a negative
	// count can never generate a negative cost term. The OpenAI branch
	// additionally caps cached into [0, prompt] below; the Anthropic branch
	// only clamps non-negative and keeps its formula semantics (input billed
	// in full, cache counters priced separately).
	if promptTokens < 0 {
		promptTokens = 0
	}
	if completionTokens < 0 {
		completionTokens = 0
	}
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cacheWriteTokens < 0 {
		cacheWriteTokens = 0
	}
	if reasoningTokens < 0 {
		reasoningTokens = 0
	}
	if promptTokens == 0 && completionTokens == 0 && cachedTokens == 0 && cacheWriteTokens == 0 && reasoningTokens == 0 {
		zero := 0.0
		return &zero, CostStatusNoUsage
	}
	// cache_read omitted in YAML unmarshals to 0. That is "not configured",
	// not "cache is free": fall back to Input so a missed field cannot
	// silently undercharge. An explicit positive CacheRead still wins.
	cacheRead := price.CacheRead
	if cacheRead == 0 {
		cacheRead = price.Input
	}
	var promptTerm float64
	if source == SourceAnthropic {
		// Anthropic input_tokens excludes cache tokens: the prompt is billed
		// in full, and the cache counters are priced separately below. The
		// old shared formula (prompt - cached) undercounted Anthropic by
		// repricing part of the input at the CacheRead rate.
		promptTerm = float64(promptTokens)/1e6*price.Input +
			float64(cachedTokens)/1e6*cacheRead
	} else {
		// OpenAI prompt_tokens includes cached tokens: price the cached
		// portion at CacheRead and the rest at Input. Cached is clamped into
		// [0, prompt] (defense in depth: the parsers already normalize, but
		// callers may build token counts programmatically) so a broken
		// report can never produce a negative prompt term or a negative
		// cost.
		cached := cachedTokens
		if cached < 0 {
			cached = 0
		}
		if cached > promptTokens {
			cached = promptTokens
		}
		promptTerm = float64(promptTokens-cached)/1e6*price.Input +
			float64(cached)/1e6*cacheRead
	}
	reasoningPrice := price.Output
	if reasoningTokens > completionTokens {
		slog.Warn("reasoning_tokens exceeds completion_tokens", "reasoning", reasoningTokens, "completion", completionTokens)
	}
	completionTerm := float64(max(0, completionTokens-reasoningTokens))/1e6*price.Output +
		float64(reasoningTokens)/1e6*reasoningPrice

	cost := promptTerm +
		float64(cacheWriteTokens)/1e6*price.CacheWrite +
		completionTerm
	return &cost, CostStatusOK
}
