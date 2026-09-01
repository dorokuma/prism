package render

import "strings"

// Summary carries the aggregate numbers rendered by RenderSummary.
type Summary struct {
	// Requests is the total request count.
	Requests int64
	// Tokens is the total token count (input + output).
	Tokens int64
	// Cost is the total cost in USD. nil means no unit price is configured
	// for the models, which renders as "-" and is distinct from $0.000.
	Cost *float64
}

// RenderSummary renders s as three indented lines:
//
//	请求     {requests}
//	总词元   {tokens}
//	总开销   {cost}
//
// The result ends with a newline.
func RenderSummary(s Summary) string {
	var b strings.Builder
	b.WriteString("  请求     ")
	b.WriteString(FormatInt(s.Requests))
	b.WriteByte('\n')
	b.WriteString("  总词元   ")
	b.WriteString(FormatTokens(s.Tokens))
	b.WriteByte('\n')
	b.WriteString("  总开销   ")
	b.WriteString(FormatCost(s.Cost))
	b.WriteByte('\n')
	return b.String()
}
