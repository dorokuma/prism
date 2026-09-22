package render

// Summary carries the aggregate numbers rendered by SummaryLine.
type Summary struct {
	// Requests is the total request count.
	Requests int64
	// Tokens is the total token count (input + output).
	Tokens int64
	// Cost is the total cost in USD. nil means no unit price is configured
	// for the models, which renders as "-" and is distinct from $0.000.
	Cost *float64
}

// SummaryLine renders s as the single summary row of the usage report:
//
//	请求 {requests} · 词元 {tokens} · 开销 {cost}
//
// Requests use thousands separators, tokens the compact k/M notation of
// FormatTokens and cost FormatCost (nil renders as "-"). No newline and no
// padding: the caller places the row inside its own fixed-width layout.
func SummaryLine(s Summary) string {
	return "请求 " + FormatInt(s.Requests) +
		" · 词元 " + FormatTokens(s.Tokens) +
		" · 开销 " + FormatCost(s.Cost)
}
