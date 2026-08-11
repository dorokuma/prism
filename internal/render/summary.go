package render

import "strings"

// CacheStats carries one source family's cache-hit numbers for the summary
// line. Hits is the token count served from the upstream cache and Input is
// the input-token denominator the hit ratio is computed against. The two
// families are NOT comparable: for OpenAI-form rows cached is a subset of
// prompt, while for Anthropic-form rows the cache counters are separate
// top-level counters excluded from input_tokens, so the renderer receives an
// already-assembled Input (input + cache_read + cache_creation) for that
// family. A nil *CacheStats means the source family had no requests in range;
// the segment is then omitted entirely — never rendered as "0 (0.0%)" noise.
type CacheStats struct {
	Hits  int64
	Input int64
}

// Summary carries the aggregate numbers rendered by RenderSummary.
type Summary struct {
	// Period is a human-readable time-range description, e.g. "今天 08-10".
	Period string
	// Requests is the total request count.
	Requests int64
	// Tokens is the total token count (input + output).
	Tokens int64
	// Cost is the total cost in USD. nil means no unit price is configured
	// for the models, which renders as "-" and is distinct from $0.000.
	Cost *float64
	// Failures is the number of failed requests.
	Failures int64
	// Streaming is the number of streaming requests.
	Streaming int64
	// OpenAI holds the cache-hit segment for OpenAI-form rows (including
	// legacy rows with no recorded source); nil when that family has no
	// requests in range. Caching only applies to the input side, so the
	// ratio uses the family's input tokens as denominator.
	OpenAI *CacheStats
	// Anthropic holds the cache-hit segment for Anthropic-form rows; nil
	// when that family has no requests in range. Input must already be the
	// assembled total input (input_tokens + cache_read + cache_creation).
	Anthropic *CacheStats
}

// RenderSummary renders s as two indented lines:
//
//	{Period}  ·  {requests} 请求  ·  {tokens} token  ·  {cost}
//	失败 {n} ({pct})   流式 {n} ({pct})   缓存命中(openai) {tokens} ({pct})   缓存命中(anthropic) {tokens} ({pct})
//
// The failure segment is omitted when Failures is zero. The failure and
// streaming percentages use Requests as denominator; each cache-hit segment
// uses its own Input denominator. A segment whose source family had no
// requests (nil) is omitted entirely. Percentages keep one decimal; a zero
// denominator renders as "-". The result ends with a newline.
func RenderSummary(s Summary) string {
	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(s.Period)
	b.WriteString("  ·  ")
	b.WriteString(FormatInt(s.Requests))
	b.WriteString(" 请求  ·  ")
	b.WriteString(FormatTokens(s.Tokens))
	b.WriteString(" token  ·  ")
	b.WriteString(FormatCost(s.Cost))
	b.WriteByte('\n')

	parts := make([]string, 0, 4)
	if s.Failures != 0 {
		parts = append(parts, "失败 "+FormatInt(s.Failures)+" ("+FormatPercent(float64(s.Failures), float64(s.Requests))+")")
	}
	parts = append(parts, "流式 "+FormatInt(s.Streaming)+" ("+FormatPercent(float64(s.Streaming), float64(s.Requests))+")")
	if s.OpenAI != nil {
		parts = append(parts, "缓存命中(openai) "+FormatTokens(s.OpenAI.Hits)+" ("+FormatPercent(float64(s.OpenAI.Hits), float64(s.OpenAI.Input))+")")
	}
	if s.Anthropic != nil {
		parts = append(parts, "缓存命中(anthropic) "+FormatTokens(s.Anthropic.Hits)+" ("+FormatPercent(float64(s.Anthropic.Hits), float64(s.Anthropic.Input))+")")
	}

	b.WriteString("  ")
	b.WriteString(strings.Join(parts, "   "))
	b.WriteByte('\n')
	return b.String()
}
