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
	// OpenAI holds the cache-hit segment for OpenAI-form rows (including
	// legacy rows with no recorded source); nil when that family has no
	// requests in range. Caching only applies to the input side, so the
	// ratio uses the family's input tokens as denominator.
	OpenAI *CacheStats
	// Anthropic holds the cache-hit segment for Anthropic-form and pi-session
	// rows; nil when that family has no requests in range. Input must already
	// be the assembled total input (input_tokens + cache_read + cache_creation).
	Anthropic *CacheStats
}

// RenderSummary renders s as two indented lines:
//
//	{Period}  ·  {requests} 请求  ·  {tokens} 词元  ·  {cost}
//	命中(OpenAI) {tokens} ({pct})   命中(Anthropic) {tokens} ({pct})
//
// The failure and streaming segments are not rendered. Each cache-hit
// segment uses its own Input denominator. A segment whose source family had
// no requests (nil) is omitted entirely — when neither family has requests
// only the first line is emitted. Percentages keep one decimal; a zero
// denominator renders as "-". The result ends with a newline.
func RenderSummary(s Summary) string {
	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(s.Period)
	b.WriteString("  ·  ")
	b.WriteString(FormatInt(s.Requests))
	b.WriteString(" 请求  ·  ")
	b.WriteString(FormatTokens(s.Tokens))
	b.WriteString(" 词元  ·  ")
	b.WriteString(FormatCost(s.Cost))
	b.WriteByte('\n')

	parts := make([]string, 0, 2)
	if s.OpenAI != nil {
		parts = append(parts, "命中(OpenAI) "+FormatTokens(s.OpenAI.Hits)+" ("+FormatPercent(float64(s.OpenAI.Hits), float64(s.OpenAI.Input))+")")
	}
	if s.Anthropic != nil {
		parts = append(parts, "命中(Anthropic) "+FormatTokens(s.Anthropic.Hits)+" ("+FormatPercent(float64(s.Anthropic.Hits), float64(s.Anthropic.Input))+")")
	}

	if len(parts) > 0 {
		b.WriteString("  ")
		b.WriteString(strings.Join(parts, "   "))
		b.WriteByte('\n')
	}
	return b.String()
}
