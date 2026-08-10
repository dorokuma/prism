package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/render"
)

func TestDescribePeriod(t *testing.T) {
	loc := time.Local
	mid := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	now := func(y int, m time.Month, d, h, mi int) time.Time {
		return time.Date(y, m, d, h, mi, 0, 0, loc)
	}
	cases := []struct {
		name string
		from time.Time
		to   time.Time
		now  time.Time
		want string
	}{
		{
			"today default range",
			mid(2026, 3, 10), now(2026, 3, 10, 15, 4), now(2026, 3, 10, 15, 4),
			"今天",
		},
		{
			"近 7 天 (--since 7d)",
			now(2026, 3, 3, 15, 4), now(2026, 3, 10, 15, 4), now(2026, 3, 10, 15, 4),
			"近 7 天",
		},
		{
			"近 1 天 (--since 24h)",
			now(2026, 3, 9, 15, 4), now(2026, 3, 10, 15, 4), now(2026, 3, 10, 15, 4),
			"近 1 天",
		},
		{
			"近 30 天 crosses month",
			now(2026, 2, 8, 15, 4), now(2026, 3, 10, 15, 4), now(2026, 3, 10, 15, 4),
			"近 30 天",
		},
		{
			"month-day range same year",
			mid(2026, 8, 1), mid(2026, 8, 10), now(2026, 8, 10, 15, 4),
			"08-01 至 08-10",
		},
		{
			"cross-year range shows full dates",
			mid(2025, 12, 1), mid(2026, 1, 5), now(2026, 1, 5, 9, 0),
			"2025-12-01 至 2026-01-05",
		},
		{
			"from today midnight to tomorrow midnight",
			mid(2026, 3, 10), mid(2026, 3, 11), now(2026, 3, 10, 15, 4),
			"03-10 至 03-11",
		},
		{
			"unbounded both sides",
			time.Time{}, time.Time{}, now(2026, 3, 10, 15, 4),
			"全部时间",
		},
		{
			"from before epoch bound (0 = unbounded)",
			mid(1970, 1, 1), mid(2026, 3, 10), now(2026, 3, 10, 15, 4),
			"启用以来 至 03-10",
		},
	}
	for _, c := range cases {
		got := DescribePeriod(c.from.Unix(), c.to.Unix(), c.now.Unix())
		if got != c.want {
			t.Errorf("%s: DescribePeriod = %q, want %q", c.name, got, c.want)
		}
	}
}

func ptr64(v float64) *float64 { return &v }

func TestRenderUsageReportStructure(t *testing.T) {
	ov := &Overview{
		Requests:            1783,
		PromptTokens:        2_000_000,
		CompletionTokens:    230_000,
		TotalTokens:         2_230_000,
		CachedTokens:        1_100_000,
		ReasoningTokens:     0,
		CacheWriteTokens:    0,
		TotalCost:           ptr64(0.836),
		CostMissingRequests: 3,
		FailedRequests:      12,
		StreamingRequests:   1690,
	}
	rows := []SummaryRow{
		{
			Groups:              map[string]any{"model": "deepseek-v4-pro"},
			Requests:            1500,
			PromptTokens:        1_500_000,
			CompletionTokens:    200_000,
			TotalTokens:         1_700_000,
			CachedTokens:        1_000_000,
			ReasoningTokens:     0,
			CacheWriteTokens:    0,
			CostUSD:             ptr64(0.65),
			CostMissingRequests: 0,
		},
		{
			Groups:              map[string]any{"model": "glm-5.2"},
			Requests:            283,
			PromptTokens:        500_000,
			CompletionTokens:    30_000,
			TotalTokens:         530_000,
			CachedTokens:        100_000,
			ReasoningTokens:     0,
			CacheWriteTokens:    0,
			CostUSD:             nil,
			CostMissingRequests: 3,
		},
	}
	got := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{Period: "今天"})

	// Summary header comes from Overview (2.23M total), not from summing the
	// rows (which would be fine here, but the point is the source is Overview).
	if !strings.Contains(got, "  今天  ·  1,783 请求  ·  2.23M token  ·  $0.836\n") {
		t.Errorf("summary line missing or wrong:\n%s", got)
	}
	// Failure + streaming counts from Overview.
	if !strings.Contains(got, "失败 12 (0.7%)   流式 1,690 (94.8%)   缓存命中 1.1M (55.0%)") {
		t.Errorf("failure/streaming/cache line missing or wrong:\n%s", got)
	}
	// Missing-cost warning when CostMissingRequests > 0.
	if !strings.Contains(got, "⚠ 有 3 个请求未算出金额（模型未配置单价），总费用可能偏低") {
		t.Errorf("missing-cost warning missing:\n%s", got)
	}
	// Blank line between summary and table.
	if !strings.Contains(got, "\n\n") {
		t.Errorf("no blank line between summary and table:\n%s", got)
	}
	// Table headers.
	for _, h := range []string{"model", "请求", "Prompt", "Completion", "Total", "缓存", "费用", "未计价"} {
		if !strings.Contains(got, h) {
			t.Errorf("table header %q missing:\n%s", h, got)
		}
	}
	// nil cost renders as "-", not $0.000.
	var glmLine string
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "glm-5.2") {
			glmLine = line
			break
		}
	}
	if !strings.Contains(glmLine, "-") {
		t.Errorf("nil-cost row must render a dash placeholder, got line %q:\n%s", glmLine, got)
	}
	if strings.Contains(got, "$0.000") {
		t.Errorf("nil cost must not render as $0.000:\n%s", got)
	}
	// FormatTokens trailing-zero behavior: the summary line already shows
	// "2.23M"; token cells in the table use FormatInt (thousands
	// separators), the FormatTokens rule itself is covered by the render
	// package tests.
}

func TestRenderUsageReportAlignment(t *testing.T) {
	// Exact-output test: the table must be aligned (CJK headers, right-
	// aligned numbers, dashes for nil cost) and the FormatTokens trailing-
	// zero rule must show in cells. The expected strings were verified
	// visually for column alignment.
	ov := &Overview{Requests: 1783, PromptTokens: 2_000_000, CompletionTokens: 230_000, TotalTokens: 2_230_000, CachedTokens: 1_100_000, TotalCost: ptr64(0.836), CostMissingRequests: 3, FailedRequests: 12, StreamingRequests: 1690}
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "deepseek-v4-pro"}, Requests: 1500, PromptTokens: 1_500_000, CompletionTokens: 200_000, TotalTokens: 1_700_000, CachedTokens: 1_000_000, CostUSD: ptr64(0.65)},
		{Groups: map[string]any{"model": "glm-5.2"}, Requests: 283, PromptTokens: 500_000, CompletionTokens: 30_000, TotalTokens: 530_000, CachedTokens: 100_000, CostUSD: nil, CostMissingRequests: 3},
	}
	want := "  今天  ·  1,783 请求  ·  2.23M token  ·  $0.836\n" +
		"  失败 12 (0.7%)   流式 1,690 (94.8%)   缓存命中 1.1M (55.0%)\n" +
		"  ⚠ 有 3 个请求未算出金额（模型未配置单价），总费用可能偏低\n" +
		"\n" +
		"  model              请求      Prompt   Completion       Total        缓存     费用   未计价\n" +
		"  deepseek-v4-pro   1,500   1,500,000      200,000   1,700,000   1,000,000   $0.650        0\n" +
		"  glm-5.2             283     500,000       30,000     530,000     100,000        -        3\n"
	if got := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{Period: "今天"}); got != want {
		t.Fatalf("RenderUsageReport mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

func TestRenderUsageReportNoData(t *testing.T) {
	ov := &Overview{}
	got := RenderUsageReport(ov, nil, []string{"model"}, ReportOptions{Period: "全部时间"})
	if !strings.Contains(got, "(no data)") {
		t.Errorf("empty table must render the no-data hint:\n%s", got)
	}
	if !strings.Contains(got, "0 请求") {
		t.Errorf("summary must still render from Overview on an empty range:\n%s", got)
	}
}

func TestRenderUsageReportColor(t *testing.T) {
	ov := &Overview{Requests: 1}
	rows := []SummaryRow{{Groups: map[string]any{"model": "m"}, Requests: 1}}
	plain := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{Period: "x"})
	colored := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{Period: "x", Color: true})
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("plain output must not contain ANSI escapes:\n%q", plain)
	}
	if !strings.Contains(colored, "\x1b[1m") {
		t.Errorf("colored output must contain a bold header:\n%q", colored)
	}
	// Both render the same visible text once ANSI is stripped.
	if render.StripANSI(colored) != plain {
		t.Errorf("color must not change the visible text\nplain: %q\ncolored: %q", plain, colored)
	}
}

func TestFormatGroupValue(t *testing.T) {
	loc := time.Local
	dayStart := time.Date(2026, 3, 10, 0, 0, 0, 0, loc)
	if got := formatGroupValue("day", dayStart.Unix()); got != "03-10" {
		t.Errorf("day bucket = %q, want 03-10", got)
	}
	hourStart := time.Date(2026, 3, 10, 15, 0, 0, 0, loc)
	if got := formatGroupValue("hour", hourStart.Unix()); got != "03-10 15:00" {
		t.Errorf("hour bucket = %q, want 03-10 15:00", got)
	}
	if got := formatGroupValue("stream", int64(1)); got != "yes" {
		t.Errorf("stream 1 = %q, want yes", got)
	}
	if got := formatGroupValue("stream", int64(0)); got != "no" {
		t.Errorf("stream 0 = %q, want no", got)
	}
	if got := formatGroupValue("success", int64(1)); got != "ok" {
		t.Errorf("success 1 = %q, want ok", got)
	}
	if got := formatGroupValue("success", int64(0)); got != "fail" {
		t.Errorf("success 0 = %q, want fail", got)
	}
	if got := formatGroupValue("model", "gpt-5.5"); got != "gpt-5.5" {
		t.Errorf("model = %q, want gpt-5.5", got)
	}
	if got := formatGroupValue("model", nil); got != "" {
		t.Errorf("nil group value = %q, want empty", got)
	}
}
