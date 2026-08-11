// Package usagemeta is the single place that maps upstream token usage
// payloads (OpenAI form and Anthropic form) into one field set. Every
// audit capture point in the proxy and stream packages goes through this
// package so the mapping rules (and the "record as-is, never add or
// subtract" semantics below) cannot drift between paths.
package usagemeta

import (
	"bytes"
	"encoding/json"
)

// Source markers identifying which upstream wire format a Usage was parsed
// from. They select the cost formula in the usage package: OpenAI
// prompt_tokens includes the cached tokens (so the cost layer reprices the
// cached portion at the CacheRead rate), while Anthropic input_tokens
// EXCLUDES the cache counters (cache_read_input_tokens and
// cache_creation_input_tokens are separate top-level counters, billed at
// their own rates).
const (
	SourceOpenAI    = "openai"
	SourceAnthropic = "anthropic"
)

// Usage holds the token counts of one upstream response or stream in a
// wire-format-independent shape.
//
// Semantic invariants (enforced by the parsers, documented here because
// they matter for cost accounting):
//
//   - Reasoning is a subset of Completion: upstreams report reasoning
//     tokens already included in completion_tokens/output_tokens. It is
//     recorded as-is, never added to or subtracted from Completion.
//   - OpenAI-form: Cached is a subset of Prompt (prompt_tokens includes
//     the cached tokens); CacheWrite is likewise already included in
//     Prompt.
//   - Anthropic-form: Cached and CacheWrite are NOT subsets of Prompt:
//     input_tokens excludes cache_read_input_tokens and
//     cache_creation_input_tokens — all three are separate counters.
//     The parsers record each as-is; the cost layer (usage.ComputeCost)
//     applies the matching formula via Source.
//
// Total defaults to Prompt+Completion only when the upstream did not
// provide one (Anthropic never does).
type Usage struct {
	Prompt     int // input / prompt tokens
	Completion int // output / completion tokens
	Total      int // upstream total, or Prompt+Completion when absent
	Cached     int // cached prompt tokens (included in Prompt for OpenAI form)
	Reasoning  int // reasoning tokens (included in Completion)
	CacheWrite int // prompt tokens written to upstream cache (included in Prompt for OpenAI form)
	Source     string
}

// usageMember extracts the usage object from data. data may be a bare
// usage object (stream chunk usage, extracted event usage) or a full
// response body that nests the usage object under the top-level "usage"
// member (non-streaming OpenAI chat completion and Anthropic Messages
// bodies both nest it). A bare usage object never has a "usage" member
// itself, so the key lookup is unambiguous. Returns the object to parse,
// or nil when data is empty, not a JSON object, or the usage object itself
// is empty (null or {}) — an empty object carries no token counts, so the
// parsers treat it as absent and return a fully zero Usage.
func usageMember(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return nil
	}
	obj := data
	if usage, ok := top["usage"]; ok {
		obj = usage
	}
	s := bytes.TrimSpace(obj)
	if len(s) == 0 || string(s) == "null" || string(s) == "{}" {
		return nil
	}
	return obj
}

// openAIUsage is the OpenAI usage object shape (chat completions
// non-streaming body and streaming chunk both use it).
type openAIUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails   *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// ParseOpenAI parses an OpenAI-form usage payload (prompt_tokens /
// completion_tokens / total_tokens / prompt_cache_hit_tokens /
// prompt_tokens_details.cached_tokens / completion_tokens_details.
// reasoning_tokens). data may be a full chat completion response body or a
// bare usage object; unknown fields are ignored. It returns a zero Usage
// when the payload cannot be parsed.
//
// Cached is taken from prompt_cache_hit_tokens, falling back to
// prompt_tokens_details.cached_tokens when the former is 0 (mirrors the
// pre-existing stream translation logic). Reasoning is taken from
// completion_tokens_details.reasoning_tokens. Neither value is ever added
// to or subtracted from Prompt/Completion: both are already included in
// them upstream.
func ParseOpenAI(data []byte) Usage {
	obj := usageMember(data)
	if obj == nil {
		return Usage{}
	}
	var u openAIUsage
	if json.Unmarshal(obj, &u) != nil {
		return Usage{}
	}
	// Entry clamps (defense in depth on top of the ComputeCost clamp): the
	// four cost-relevant counts are non-negative by definition, so a broken
	// upstream report with negative values is normalized to 0 before it can
	// reach cost accounting. Well-formed payloads are recorded as-is.
	prompt := u.PromptTokens
	if prompt < 0 {
		prompt = 0
	}
	completion := u.CompletionTokens
	if completion < 0 {
		completion = 0
	}
	// OpenAI chat completions usage has no cache-write counter (that is an
	// Anthropic field); CacheWrite stays 0. prompt_cache_miss_tokens is the
	// non-cached portion of prompt_tokens and must NOT be treated as a
	// cache write — it is already priced inside prompt.
	total := u.TotalTokens
	if total < 0 {
		total = 0
	}
	cached := u.PromptCacheHitTokens
	if cached == 0 && u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	// Defensive normalization of a broken upstream report: in the OpenAI
	// wire format Cached is a subset of Prompt, so clamp it into [0, prompt]
	// before it can reach cost accounting (a cached > prompt would make the
	// OpenAI cost formula's prompt-cached term negative). The "record as-is"
	// rule still holds for every well-formed payload — this only repairs
	// impossible values.
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	var reasoning int
	if u.CompletionTokensDetails != nil {
		reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	out := Usage{
		Prompt:     prompt,
		Completion: completion,
		Total:      total,
		Cached:     cached,
		Reasoning:  reasoning,
	}
	// No token information at all (e.g. an Anthropic body mis-selected into
	// this parser): return a fully zero Usage, marker included, so a
	// mis-selected parse can never masquerade as a real OpenAI record.
	if out.Prompt == 0 && out.Completion == 0 && out.Cached == 0 && out.CacheWrite == 0 {
		return Usage{}
	}
	out.Source = SourceOpenAI
	return out
}

// anthropicUsage is the Anthropic Messages API usage object shape. Total
// is not sent by Anthropic; the field is parsed for completeness and the
// Prompt+Completion fallback in ParseAnthropic covers the real payloads.
type anthropicUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	TotalTokens         int `json:"total_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
}

// ParseAnthropic parses an Anthropic-form usage payload (input_tokens /
// output_tokens / cache_read_input_tokens / cache_creation_input_tokens).
// data may be a full Messages API response body, the message.usage object
// of a message_start event, or the usage object of a message_delta event;
// unknown fields are ignored. It returns a zero Usage when the payload
// cannot be parsed.
//
// Mapping: input_tokens→Prompt, output_tokens→Completion,
// cache_read_input_tokens→Cached, cache_creation_input_tokens→CacheWrite.
// Total is taken from total_tokens when present (Anthropic never sends it)
// and otherwise computed as Prompt+Completion. Source is set to
// SourceAnthropic: unlike the OpenAI form, input_tokens does NOT include the
// cache counters, which is why the cost layer must not subtract them.
func ParseAnthropic(data []byte) Usage {
	obj := usageMember(data)
	if obj == nil {
		return Usage{}
	}
	var u anthropicUsage
	if json.Unmarshal(obj, &u) != nil {
		return Usage{}
	}
	// Entry clamps (defense in depth on top of the ComputeCost clamp): the
	// four cost-relevant counts are non-negative by definition, so a broken
	// upstream report with negative values is normalized to 0. Anthropic
	// semantics are preserved: input_tokens is billed in full and the cache
	// counters are separate — nothing is ever subtracted, only negatives
	// clamped. Well-formed payloads are recorded as-is.
	prompt := u.InputTokens
	if prompt < 0 {
		prompt = 0
	}
	completion := u.OutputTokens
	if completion < 0 {
		completion = 0
	}
	cached := u.CacheReadTokens
	if cached < 0 {
		cached = 0
	}
	cacheWrite := u.CacheCreationTokens
	if cacheWrite < 0 {
		cacheWrite = 0
	}
	total := u.TotalTokens
	if total < 0 {
		total = 0
	}
	if total == 0 {
		total = prompt + completion
	}
	out := Usage{
		Prompt:     prompt,
		Completion: completion,
		Total:      total,
		Cached:     cached,
		CacheWrite: cacheWrite,
	}
	if out.Prompt == 0 && out.Completion == 0 && out.Cached == 0 && out.CacheWrite == 0 {
		return Usage{}
	}
	out.Source = SourceAnthropic
	return out
}
