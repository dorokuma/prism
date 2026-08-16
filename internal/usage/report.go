package usage

import (
	"fmt"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/render"
)

// DescribePeriod renders a human-readable time-range description for the
// summary header, e.g. "今天", "近 7 天", "08-01 至 08-10" or — when the
// range crosses a year — "2025-12-01 至 2026-01-05". from/to/now are unix
// seconds; a zero bound means unbounded on that side ("全部时间").
func DescribePeriod(from, to, now int64) string {
	if from <= 0 && to <= 0 {
		return "全部时间"
	}
	loc := time.Local
	f := time.Unix(from, 0).In(loc)
	t := time.Unix(to, 0).In(loc)
	n := time.Unix(now, 0).In(loc)

	if from <= 0 {
		return fmt.Sprintf("启用以来 至 %02d-%02d", t.Month(), t.Day())
	}
	if to <= 0 {
		to = now
		t = time.Unix(to, 0).In(loc)
	}
	if to < from {
		return ""
	}

	startOfToday := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	if f.Equal(startOfToday) && !t.Before(startOfToday) && t.Before(startOfToday.Add(24*time.Hour)) {
		return "今天"
	}

	// "近 N 天": from is exactly to minus N calendar days (same clock
	// time) AND the range ends at the present moment — i.e. the user gave a
	// relative --since like "7d". A range like "08-01 至 08-10" (both
	// midnights) does not end at now, so it falls through to the date form.
	if to >= now-60 && to <= now+60 {
		fy, fm, fd := f.Date()
		ty, tm, td := t.Date()
		if f.Hour() == t.Hour() && f.Minute() == t.Minute() &&
			f.Second() == t.Second() && f.Nanosecond() == t.Nanosecond() {
			days := int(time.Date(ty, tm, td, 0, 0, 0, 0, loc).
				Sub(time.Date(fy, fm, fd, 0, 0, 0, 0, loc)).Hours() / 24)
			if days > 0 {
				return fmt.Sprintf("近 %d 天", days)
			}
		}
	}

	fy, fm, fd := f.Date()
	ty, tm, td := t.Date()
	if fy == ty {
		return fmt.Sprintf("%02d-%02d 至 %02d-%02d", fm, fd, tm, td)
	}
	return fmt.Sprintf("%d-%02d-%02d 至 %d-%02d-%02d", fy, fm, fd, ty, tm, td)
}

// ReportOptions controls the shared report renderer used by both the prism
// usage CLI and the HTTP format=table output.
type ReportOptions struct {
	// Period is the human-readable time-range description (DescribePeriod).
	Period string
	// Color enables ANSI coloring (bold header). Alignment is computed on
	// the de-colored text, so colored output stays aligned; with Color
	// false, ANSI sequences are stripped and piped output is plain text.
	Color bool
}

// RenderUsageReport renders the summary block (taken from Overview — never
// from summing the grouped rows, because a truncated LIMIT would make the
// totals look small), an optional missing-cost warning line, a blank line
// and the grouped detail table. The detail section is the compact
// single-line table by default — one row per group, short headers, a
// one-space column gap and compact numbers — and never depends on the
// terminal width or switches to another layout. It is the single
// implementation behind both the prism usage CLI and the HTTP format=table
// output, so the two outputs can never drift apart.
func RenderUsageReport(ov *Overview, rows []SummaryRow, groupBy []string, opts ReportOptions) string {
	var b strings.Builder
	b.WriteString(render.RenderSummary(render.Summary{
		Period:    opts.Period,
		Requests:  ov.Requests,
		Tokens:    ov.TotalTokens,
		Cost:      ov.TotalCost,
		Failures:  ov.FailedRequests,
		Streaming: ov.StreamingRequests,
		// Cache-hit segments are split by usage_source with per-family
		// denominators: OpenAI-form rows (usage_source = 'openai', legacy
		// NULL rows and empty-string rows — everything ComputeCost prices
		// with the OpenAI formula) count cached/prompt — cached is a subset
		// of prompt there. Anthropic-form rows count cache_read against the
		// assembled total input (input + cache_read + cache_creation),
		// because Anthropic input_tokens excludes the cache counters; that
		// keeps the ratio ≤ 100% and matches the upstream billing basis. A
		// source family with zero requests gets no segment at all.
		OpenAI: segment(ov.OpenAIRequests, ov.OpenAICachedTokens, ov.OpenAIPromptTokens),
		Anthropic: segment(ov.AnthropicRequests, ov.AnthropicCachedTokens,
			ov.AnthropicPromptTokens+ov.AnthropicCachedTokens+ov.AnthropicCacheWriteTokens),
	}))
	if ov.CostMissingRequests > 0 {
		fmt.Fprintf(&b, "  ⚠ 有 %s 个请求未算出金额（模型未配置单价），总费用可能偏低\n",
			render.FormatInt(ov.CostMissingRequests))
	}
	b.WriteByte('\n')
	cols, cells := usageTableData(rows, groupBy)
	b.WriteString(renderUsageTable(cols, cells, opts.Color))
	return b.String()
}

// segment builds a render.CacheStats for one source family, or nil when that
// family had no requests in range (the renderer then omits the segment
// entirely instead of showing "0 (0.0%)"). Hits and input are already the
// family-specific numbers; the Anthropic caller passes the assembled total
// input as input.
func segment(requests, hits, input int64) *render.CacheStats {
	if requests == 0 {
		return nil
	}
	return &render.CacheStats{Hits: hits, Input: input}
}

// usageTableData builds the detail section's column definitions and cell
// rows from the summary rows: one column per group_by key (the model group
// uses the "模型" title, other group keys keep their short name), then
// 请求 / 输入词元 / 缓存 / 命中率 / 输出词元 / 花费, plus an 未计价 column
// when at least one group contains rows without a price. The Total column
// is deliberately not rendered. Request/token cells use the compact k/M
// notation (display precision only — the stored aggregates are unchanged),
// the cost cell uses the compact cost format. Group values are never
// truncated: the group column is sized to its widest cell, so model names
// and other group values render in full.
func usageTableData(rows []SummaryRow, groupBy []string) ([]render.Column, [][]string) {
	hasMissing := false
	for _, r := range rows {
		if r.CostMissingRequests > 0 {
			hasMissing = true
			break
		}
	}
	cols := make([]render.Column, 0, len(groupBy)+7)
	for _, g := range groupBy {
		title := g
		if g == "model" {
			title = "模型"
		}
		cols = append(cols, render.Column{Title: title, Align: render.AlignLeft})
	}
	cols = append(cols,
		render.Column{Title: "请求", Align: render.AlignRight},
		render.Column{Title: "输入词元", Align: render.AlignRight},
		render.Column{Title: "缓存", Align: render.AlignRight},
		render.Column{Title: "命中率", Align: render.AlignRight},
		render.Column{Title: "输出词元", Align: render.AlignRight},
		render.Column{Title: "花费", Align: render.AlignRight},
	)
	if hasMissing {
		cols = append(cols, render.Column{Title: "未计价", Align: render.AlignRight})
	}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		row := make([]string, 0, len(cols))
		for _, g := range groupBy {
			row = append(row, formatGroupValue(g, r.Groups[g]))
		}
		row = append(row,
			formatRequests(r.Requests),
			render.FormatTokens(r.PromptTokens),
			render.FormatTokens(r.CachedTokens),
			cacheHitRate(r.CachedTokens, r.PromptTokens),
			render.FormatTokens(r.CompletionTokens),
			render.FormatCostCompact(r.CostUSD),
		)
		if hasMissing {
			row = append(row, render.FormatInt(r.CostMissingRequests))
		}
		cells = append(cells, row)
	}
	return cols, cells
}

// formatRequests renders a request count with the same compact k/M
// notation render.FormatTokens uses. The shared formatter is token-named,
// but its algorithm is a generic count formatter; this wrapper gives the
// request column a semantically honest name without duplicating the
// algorithm.
func formatRequests(n int64) string {
	return render.FormatTokens(n)
}

// renderUsageTable renders the compact single-line detail table from
// pre-built columns and cells: a two-space left indent (aligned with the
// summary and warning lines above) and a one-space column gap.
func renderUsageTable(cols []render.Column, cells [][]string, color bool) string {
	if color {
		for i := range cols {
			cols[i].Title = "\x1b[1m" + cols[i].Title + "\x1b[0m"
		}
	}
	t := &render.Table{Columns: cols, Color: color, Rows: cells, Indent: "  ", Gap: " "}
	return t.Render()
}

// cacheHitRate renders the CachedTokens/PromptTokens hit ratio with one
// decimal ("66.7%"). A zero prompt total must show a stable "0.0%" — the
// guard avoids NaN/Inf from a division by zero.
func cacheHitRate(cached, prompt int64) string {
	if prompt == 0 {
		return "0.0%"
	}
	return render.FormatPercent(float64(cached), float64(prompt))
}

// formatGroupValue renders one group key value for the table. Time buckets
// (hour/day) are unix seconds and are shown as local-time dates; stream and
// success are 0/1 integers and are shown as words; everything else is the
// stored string.
func formatGroupValue(g string, v any) string {
	if v == nil {
		return ""
	}
	switch g {
	case "hour", "day":
		if n, ok := v.(int64); ok {
			t := time.Unix(n, 0)
			if g == "hour" {
				return t.Format("01-02 15:00")
			}
			return t.Format("01-02")
		}
	case "stream":
		if n, ok := v.(int64); ok {
			if n == 1 {
				return "yes"
			}
			return "no"
		}
	case "success":
		if n, ok := v.(int64); ok {
			if n == 1 {
				return "ok"
			}
			return "fail"
		}
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
