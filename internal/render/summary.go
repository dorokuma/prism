package render

import "strings"

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
	// CacheHits is the number of tokens served from the cache.
	CacheHits int64
	// InputTokens is the total input token count; the cache-hit ratio is
	// computed against this, because caching only applies to the input side.
	InputTokens int64
}

// RenderSummary renders s as two indented lines:
//
//	{Period}  ·  {requests} 请求  ·  {tokens} token  ·  {cost}
//	失败 {n} ({pct})   流式 {n} ({pct})   缓存命中 {tokens} ({pct})
//
// The failure segment is omitted when Failures is zero. The failure and
// streaming percentages use Requests as denominator; the cache-hit
// percentage uses InputTokens. Percentages keep one decimal; a zero
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

	parts := make([]string, 0, 3)
	if s.Failures != 0 {
		parts = append(parts, "失败 "+FormatInt(s.Failures)+" ("+FormatPercent(float64(s.Failures), float64(s.Requests))+")")
	}
	parts = append(parts, "流式 "+FormatInt(s.Streaming)+" ("+FormatPercent(float64(s.Streaming), float64(s.Requests))+")")
	parts = append(parts, "缓存命中 "+FormatTokens(s.CacheHits)+" ("+FormatPercent(float64(s.CacheHits), float64(s.InputTokens))+")")

	b.WriteString("  ")
	b.WriteString(strings.Join(parts, "   "))
	b.WriteByte('\n')
	return b.String()
}
