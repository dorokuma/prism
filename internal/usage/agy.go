package usage

import (
	"fmt"
	"strings"
)

// MergeSummaryRows folds extra rows (agy local generations) into the
// usage_events Summary result by the same group_by key. Matching keys add
// counters; new keys are appended. extra must already be filtered/grouped.
func MergeSummaryRows(rows, extra []SummaryRow, groupBy []string) []SummaryRow {
	if len(extra) == 0 {
		return rows
	}
	if rows == nil {
		rows = []SummaryRow{}
	}
	idx := make(map[string]int, len(rows))
	for i, r := range rows {
		idx[groupKey(r, groupBy)] = i
	}
	for _, e := range extra {
		k := groupKey(e, groupBy)
		if i, ok := idx[k]; ok {
			rows[i] = addSummaryRow(rows[i], e)
			continue
		}
		idx[k] = len(rows)
		rows = append(rows, e)
	}
	return rows
}

// AddOverview adds extra (agy) token/request counters to the header
// aggregate so table gemini rows match the summary totals. Cache semantics
// follow the Anthropic bucket: prompt excludes cache, so the renderer
// assembles prompt+cached as the hit-rate denominator.
func AddOverview(ov *Overview, extra []SummaryRow) {
	if ov == nil {
		return
	}
	for _, e := range extra {
		ov.Requests += e.Requests
		ov.PromptTokens += e.PromptTokens
		ov.CompletionTokens += e.CompletionTokens
		ov.TotalTokens += e.TotalTokens
		ov.CachedTokens += e.CachedTokens
		ov.ReasoningTokens += e.ReasoningTokens
		ov.AnthropicRequests += e.Requests
		ov.AnthropicPromptTokens += e.PromptTokens
		ov.AnthropicCachedTokens += e.CachedTokens
	}
}

func groupKey(r SummaryRow, groupBy []string) string {
	if len(groupBy) == 0 {
		return ""
	}
	var b strings.Builder
	for i, g := range groupBy {
		if i > 0 {
			b.WriteByte(0)
		}
		if r.Groups == nil {
			continue
		}
		fmt.Fprintf(&b, "%v", r.Groups[g])
	}
	return b.String()
}

func addSummaryRow(a, b SummaryRow) SummaryRow {
	a.Requests += b.Requests
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.TotalTokens += b.TotalTokens
	a.CachedTokens += b.CachedTokens
	a.ReasoningTokens += b.ReasoningTokens
	a.CacheWriteTokens += b.CacheWriteTokens
	a.HitRateInputTokens += b.HitRateInputTokens
	if a.CostUSD != nil && b.CostUSD != nil {
		v := *a.CostUSD + *b.CostUSD
		a.CostUSD = &v
	} else if a.CostUSD == nil && b.CostUSD != nil {
		v := *b.CostUSD
		a.CostUSD = &v
	}
	if a.Groups == nil && b.Groups != nil {
		a.Groups = b.Groups
	}
	return a
}
