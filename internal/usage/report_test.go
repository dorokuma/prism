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
		Requests:                  1783,
		PromptTokens:              2_000_000,
		CompletionTokens:          230_000,
		TotalTokens:               2_230_000,
		CachedTokens:              1_100_000,
		ReasoningTokens:           0,
		CacheWriteTokens:          0,
		TotalCost:                 ptr64(0.836),
		CostMissingRequests:       3,
		FailedRequests:            12,
		StreamingRequests:         1690,
		OpenAIRequests:            1783,
		OpenAIPromptTokens:        2_000_000,
		OpenAICachedTokens:        1_100_000,
		AnthropicRequests:         0,
		AnthropicPromptTokens:     0,
		AnthropicCachedTokens:     0,
		AnthropicCacheWriteTokens: 0,
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
	if !strings.Contains(got, "  今天  ·  1,783 请求  ·  2.23M 词元  ·  $0.836\n") {
		t.Errorf("summary line missing or wrong:\n%s", got)
	}
	// Failure + streaming counts from Overview; the cache-hit segment is the
	// OpenAI-family one (only openai requests in this dataset).
	if !strings.Contains(got, "失败 12 (0.7%)   流式 1,690 (94.8%)   缓存命中(openai) 1.1M (55.0%)") {
		t.Errorf("failure/streaming/cache line missing or wrong:\n%s", got)
	}
	// The anthropic family had zero requests: its segment must be omitted.
	if strings.Contains(got, "anthropic") {
		t.Errorf("zero-request anthropic segment must be omitted:\n%s", got)
	}
	// Missing-cost warning when CostMissingRequests > 0.
	if !strings.Contains(got, "⚠ 有 3 个请求未算出金额（模型未配置单价），总费用可能偏低") {
		t.Errorf("missing-cost warning missing:\n%s", got)
	}
	// Blank line between summary and table.
	if !strings.Contains(got, "\n\n") {
		t.Errorf("no blank line between summary and table:\n%s", got)
	}
	// Compact table headers: model view uses the 模型 title, the Total
	// column is gone, the 未计价 column appears because a row has unpriced
	// requests, and the request/cache headers are the short 请求/缓存.
	for _, h := range []string{"模型", "请求", "输入词元", "缓存", "命中率", "输出词元", "花费", "未计价"} {
		if !strings.Contains(got, h) {
			t.Errorf("table header %q missing:\n%s", h, got)
		}
	}
	if strings.Contains(got, "请求数") || strings.Contains(got, "Total") {
		t.Errorf("the long 请求数 header and the Total column must be gone:\n%s", got)
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
}
func TestRenderUsageReportAlignment(t *testing.T) {
	// Exact-output test: the table must be aligned (CJK headers, right-
	// aligned numbers, one-space column gaps, two-space left indent matching
	// the summary and warning lines, dashes for nil cost), and the hit-rate
	// column must show cached/prompt with one decimal (deepseek
	// 1,000,000/1,500,000 = 66.7%, glm 100,000/500,000 = 20.0%). Counts/tokens
	// use the compact k/M form, the cost cell the compact cost form. The
	// expected strings were verified visually for column alignment.
	ov := &Overview{Requests: 1783, PromptTokens: 2_000_000, CompletionTokens: 230_000, TotalTokens: 2_230_000, CachedTokens: 1_100_000, TotalCost: ptr64(0.836), CostMissingRequests: 3, FailedRequests: 12, StreamingRequests: 1690, OpenAIRequests: 1783, OpenAIPromptTokens: 2_000_000, OpenAICachedTokens: 1_100_000}
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "deepseek-v4-pro"}, Requests: 1500, PromptTokens: 1_500_000, CompletionTokens: 200_000, TotalTokens: 1_700_000, CachedTokens: 1_000_000, CostUSD: ptr64(0.65)},
		{Groups: map[string]any{"model": "glm-5.2"}, Requests: 283, PromptTokens: 500_000, CompletionTokens: 30_000, TotalTokens: 530_000, CachedTokens: 100_000, CostUSD: nil, CostMissingRequests: 3},
	}
	// widths: 模型 15 (deepseek-v4-pro) | 请求 4 | 输入词元 8 | 缓存 4 | 命中率 6 | 输出词元 8 | 花费 5 | 未计价 6
	want := "  今天  ·  1,783 请求  ·  2.23M 词元  ·  $0.836\n" +
		"  失败 12 (0.7%)   流式 1,690 (94.8%)   缓存命中(openai) 1.1M (55.0%)\n" +
		"  ⚠ 有 3 个请求未算出金额（模型未配置单价），总费用可能偏低\n" +
		"\n" +
		"  模型" + strings.Repeat(" ", 12) + "请求 输入词元 缓存 命中率 输出词元  花费 未计价\n" +
		"  deepseek-v4-pro" + strings.Repeat(" ", 3) + "1k" + strings.Repeat(" ", 5) + "1.5M" + strings.Repeat(" ", 3) + "1M" + strings.Repeat(" ", 2) + "66.7%" + strings.Repeat(" ", 5) + "200k $0.65" + strings.Repeat(" ", 6) + "0\n" +
		"  glm-5.2" + strings.Repeat(" ", 10) + "283" + strings.Repeat(" ", 5) + "500k 100k" + strings.Repeat(" ", 2) + "20.0%" + strings.Repeat(" ", 6) + "30k" + strings.Repeat(" ", 5) + "-" + strings.Repeat(" ", 6) + "3\n"
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

// TestRenderUsageTableHitRateAndGroupTitles pins the per-view table
// contract: the model view titles its first column 模型, every other
// group_by view keeps the dynamic first-column name, the hit-rate column
// shows cached over the source-aware input with one decimal, and a zero
// input total renders a stable 0.0% — never NaN/Inf.
func TestRenderUsageTableHitRateAndGroupTitles(t *testing.T) {
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "m1"}, Requests: 1, PromptTokens: 1000, CachedTokens: 968, CompletionTokens: 100, CostUSD: ptr64(0.1)},
	}
	// Model view: 模型 title, hit rate 968/1000 = 96.8%, no Total column.
	got := RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{Period: "x"})
	for _, want := range []string{"模型", "请求", "输入词元", "缓存", "96.8%", "输出词元", "花费"} {
		if !strings.Contains(got, want) {
			t.Errorf("model view missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Total") || strings.Contains(got, "请求数") {
		t.Errorf("Total column and the long 请求数 header must be gone from the model view:\n%s", got)
	}
	// No cache segments in the summary here (zero requests in Overview), so
	// 缓存命中 may only appear as the old long header — its absence pins
	// the short 缓存 header.
	if strings.Contains(got, "缓存命中") {
		t.Errorf("the header must be 缓存, not 缓存命中:\n%s", got)
	}

	// Other group views keep the dynamic first-column name.
	got = RenderUsageReport(&Overview{}, rows, []string{"provider"}, ReportOptions{Period: "x"})
	if !strings.Contains(got, "provider") {
		t.Errorf("provider view must keep the dynamic first column:\n%s", got)
	}
	if strings.Contains(got, "模型") {
		t.Errorf("provider view must not use the 模型 title:\n%s", got)
	}

	// Zero prompt tokens: stable 0.0% regardless of cached tokens, never
	// NaN/Inf (cached=5 with prompt=0 would otherwise divide by zero).
	rows = []SummaryRow{
		{Groups: map[string]any{"model": "m0"}, Requests: 1, PromptTokens: 0, CachedTokens: 5, CompletionTokens: 0},
	}
	got = RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{Period: "x"})
	for _, bad := range []string{"NaN", "Inf"} {
		if strings.Contains(got, bad) {
			t.Errorf("zero-prompt hit rate must not render %q:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "0.0%") {
		t.Errorf("zero-prompt hit rate must render 0.0%%:\n%s", got)
	}

	// Zero cached with a non-zero prompt also renders one-decimal 0.0%.
	rows = []SummaryRow{
		{Groups: map[string]any{"model": "m1"}, Requests: 1, PromptTokens: 100, CachedTokens: 0, CompletionTokens: 10},
	}
	got = RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{Period: "x"})
	if !strings.Contains(got, "0.0%") {
		t.Errorf("zero-cached hit rate must render 0.0%%:\n%s", got)
	}
}

// TestRenderUsageTableAnthropicHitRate pins the table hit-rate column to the
// same source-aware denominator as the overview segments. Anthropic-form
// cache_read sits outside input_tokens; cached/prompt would explode past
// 100% (500/1 = 50000%). Mixed groups sum OpenAI prompt with Anthropic
// assembled input.
func TestRenderUsageTableAnthropicHitRate(t *testing.T) {
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "claude-opus-5"}, Requests: 1,
			PromptTokens: 1, CachedTokens: 500, CacheWriteTokens: 0,
			HitRateInputTokens: 501, CompletionTokens: 50},
	}
	got := RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{Period: "x"})
	if !strings.Contains(got, "99.8%") {
		t.Errorf("anthropic table hit rate must be 500/501 = 99.8%%:\n%s", got)
	}
	if strings.Contains(got, "50000") {
		t.Errorf("anthropic table must not use cached/prompt:\n%s", got)
	}

	rows = []SummaryRow{
		{Groups: map[string]any{"model": "mixed"}, Requests: 2,
			PromptTokens: 1001, CachedTokens: 1400, CacheWriteTokens: 0,
			HitRateInputTokens: 1501, CompletionTokens: 150},
	}
	got = RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{Period: "x"})
	if !strings.Contains(got, "93.3%") {
		t.Errorf("mixed-group table hit rate must be 1400/1501 = 93.3%%:\n%s", got)
	}
	if strings.Contains(got, "139.9%") {
		t.Errorf("mixed-group table must not use cached/prompt:\n%s", got)
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

// TestRenderUsageReportSplitCacheSegments is the acceptance test for the
// two-segment summary: pure OpenAI data shows only the openai segment with
// cached/prompt, pure Anthropic data shows only the anthropic segment with
// cache_read over the ASSEMBLED total input (input + cache_read +
// cache_creation), and mixed data shows both side by side with independent,
// never-above-100% ratios.
func TestRenderUsageReportSplitCacheSegments(t *testing.T) {
	// Mixed: openai prompt 1000 / cached 900 (90.0%); anthropic input 1 /
	// cache_read 500 / cache_creation 0 → 500/(1+500+0) = 99.8%.
	ov := &Overview{
		Requests:       2,
		PromptTokens:   1001,
		CachedTokens:   1400,
		OpenAIRequests: 1, OpenAIPromptTokens: 1000, OpenAICachedTokens: 900,
		AnthropicRequests: 1, AnthropicPromptTokens: 1, AnthropicCachedTokens: 500, AnthropicCacheWriteTokens: 0,
	}
	got := RenderUsageReport(ov, nil, []string{"model"}, ReportOptions{Period: "x"})
	if !strings.Contains(got, "缓存命中(openai) 900 (90.0%)   缓存命中(anthropic) 500 (99.8%)") {
		t.Errorf("mixed segments must render side by side with independent ratios:\n%s", got)
	}
	if strings.Contains(got, "缓存命中 900 (") || strings.Contains(got, "缓存命中 500 (") {
		t.Errorf("old single-segment format must be gone:\n%s", got)
	}

	// Pure openai: only the openai segment (anthropic has zero requests).
	ov = &Overview{Requests: 1, OpenAIRequests: 1, OpenAIPromptTokens: 1000, OpenAICachedTokens: 900}
	got = RenderUsageReport(ov, nil, []string{"model"}, ReportOptions{Period: "x"})
	if !strings.Contains(got, "缓存命中(openai) 900 (90.0%)") {
		t.Errorf("pure openai: openai segment missing:\n%s", got)
	}
	if strings.Contains(got, "anthropic") {
		t.Errorf("pure openai: anthropic segment must be omitted:\n%s", got)
	}

	// Pure anthropic: only the anthropic segment; the denominator is the
	// assembled total input including cache_creation: 500/(100+500+400)=50%.
	ov = &Overview{Requests: 1, AnthropicRequests: 1, AnthropicPromptTokens: 100, AnthropicCachedTokens: 500, AnthropicCacheWriteTokens: 400}
	got = RenderUsageReport(ov, nil, []string{"model"}, ReportOptions{Period: "x"})
	if !strings.Contains(got, "缓存命中(anthropic) 500 (50.0%)") {
		t.Errorf("pure anthropic: segment missing or denominator wrong:\n%s", got)
	}
	if strings.Contains(got, "openai") {
		t.Errorf("pure anthropic: openai segment must be omitted:\n%s", got)
	}

	// Neither family has requests: no cache segment at all, no "0 (0.0%)".
	got = RenderUsageReport(&Overview{}, nil, []string{"model"}, ReportOptions{Period: "x"})
	if strings.Contains(got, "缓存命中") {
		t.Errorf("no requests: cache segments must be omitted:\n%s", got)
	}
}

// TestRenderUsageReportCompactNumbers pins the compact number contract of
// the detail table: request/token cells use the k/M notation and the cost
// cell the compact cost format, so the acceptance values 938,553,722 /
// 908,356,736 / 50,913,334 / 12,322 / $21.0267 render as 938.6M / 908.4M /
// 50.91M / 12k / $21.03 — and the long forms never appear.
func TestRenderUsageReportCompactNumbers(t *testing.T) {
	rows := []SummaryRow{
		{
			Groups:           map[string]any{"model": "big"},
			Requests:         938_553_722,
			PromptTokens:     908_356_736,
			CachedTokens:     50_913_334,
			CompletionTokens: 12_322,
			CostUSD:          ptr64(21.0267),
		},
	}
	got := RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{Period: "x"})
	for _, want := range []string{"938.6M", "908.4M", "50.91M", "12k", "$21.03"} {
		if !strings.Contains(got, want) {
			t.Errorf("compact number %q missing:\n%s", want, got)
		}
	}
	for _, gone := range []string{"938,553,722", "908,356,736", "50,913,334", "12,322", "$21.0267", "…"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q must not appear in compact output:\n%s", gone, got)
		}
	}
}

// TestRenderUsageReportCostCompact pins the compact cost cell: nil stays
// "-", amounts under $0.01 keep three decimals (a small fee never shows as
// "$0.00" or "$0"), larger amounts use at most two decimals.
func TestRenderUsageReportCostCompact(t *testing.T) {
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "nil"}, Requests: 1, PromptTokens: 10, CompletionTokens: 5, CostUSD: nil},
		{Groups: map[string]any{"model": "tiny"}, Requests: 1, PromptTokens: 10, CompletionTokens: 5, CostUSD: ptr64(0.005)},
		{Groups: map[string]any{"model": "sub"}, Requests: 1, PromptTokens: 10, CompletionTokens: 5, CostUSD: ptr64(0.004)},
		{Groups: map[string]any{"model": "mid"}, Requests: 1, PromptTokens: 10, CompletionTokens: 5, CostUSD: ptr64(0.6)},
		{Groups: map[string]any{"model": "big"}, Requests: 1, PromptTokens: 10, CompletionTokens: 5, CostUSD: ptr64(21.0267)},
	}
	got := RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{Period: "x"})
	for _, want := range []string{"-", "$0.005", "$0.004", "$0.60", "$21.03"} {
		if !strings.Contains(got, want) {
			t.Errorf("cost cell %q missing:\n%s", want, got)
		}
	}
	// The cost cells are exactly the compact forms; no cell may collapse a
	// small fee to "$0.00" or "$0".
	for _, line := range strings.Split(got, "\n") {
		for _, tok := range strings.Fields(line) {
			if strings.HasPrefix(tok, "$") && (tok == "$0.00" || tok == "$0") {
				t.Errorf("small fees must never render as %q:\n%s", tok, got)
			}
		}
	}
}

// TestRenderUsageReportLongGroupFull pins the full-width group rule: the
// model/group column is never capped, so an over-long model name renders
// in full with no ellipsis anywhere, the long row still carries every
// numeric value, the normal model name also renders in full, and the
// summary line, the missing-cost warning and every table line all start
// at the same third column (a two-space indent).
func TestRenderUsageReportLongGroupFull(t *testing.T) {
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "anthropic/claude-sonnet-4-20250514"}, Requests: 938_553_722, PromptTokens: 908_356_736, CachedTokens: 50_913_334, CompletionTokens: 12_322, CostUSD: ptr64(21.0267)},
		{Groups: map[string]any{"model": "glm-5.2"}, Requests: 283, PromptTokens: 500_000, CompletionTokens: 30_000, CachedTokens: 100_000},
	}
	got := RenderUsageReport(&Overview{CostMissingRequests: 1}, rows, []string{"model"}, ReportOptions{Period: "x"})
	// The over-long model name renders in full — no ellipsis anywhere.
	if !strings.Contains(got, "anthropic/claude-sonnet-4-20250514") {
		t.Errorf("over-long model name must render in full:\n%s", got)
	}
	if strings.Contains(got, "…") {
		t.Errorf("no group value may be truncated with an ellipsis:\n%s", got)
	}
	// Summary, warning and table header/rows all start at the same third
	// column: exactly two leading spaces (the blank separator is skipped).
	for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			t.Errorf("line %d must start with exactly two spaces: %q", i, line)
		}
	}
	// The long row still carries every numeric value.
	var longLine string
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "anthropic/claude-sonnet-4-20250514") {
			longLine = line
			break
		}
	}
	for _, v := range []string{"938.6M", "908.4M", "50.91M", "12k", "$21.03"} {
		if !strings.Contains(longLine, v) {
			t.Errorf("long row must keep value %q: %q", v, longLine)
		}
	}
	// The normal model name also renders in full.
	if !strings.Contains(got, "glm-5.2") {
		t.Errorf("normal model name must render in full:\n%s", got)
	}
}

// TestRenderUsageReportFitsPiPanel pins the width budget for realistic
// model names: with representative data (realistic-length model names, big
// compact counts, a priced and an unpriced row → the widest usual column
// set including 未计价) every detail-table line fits 72 display columns —
// the π panel budget — with the one-space gap and the two-space indent.
// Names longer than ~22 display columns render in full (see
// TestRenderUsageReportLongGroupFull) and may exceed the budget by design.
func TestRenderUsageReportFitsPiPanel(t *testing.T) {
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "deepseek-v4-pro"}, Requests: 938_553_722, PromptTokens: 908_356_736, CachedTokens: 50_913_334, CompletionTokens: 12_322, CostUSD: ptr64(21.0267)},
		{Groups: map[string]any{"model": "glm-5.2"}, Requests: 283, PromptTokens: 500_000, CompletionTokens: 30_000, CachedTokens: 100_000, CostUSD: nil, CostMissingRequests: 3},
	}
	got := RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{Period: "x"})
	table := got[strings.Index(got, "\n\n")+2:]
	for _, line := range strings.Split(strings.TrimRight(table, "\n"), "\n") {
		if line == "" {
			continue
		}
		if dw := render.DisplayWidth(line); dw > 72 {
			t.Errorf("table line exceeds 72 columns (%d): %q", dw, line)
		}
	}
}
