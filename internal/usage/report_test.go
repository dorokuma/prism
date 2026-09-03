package usage

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
			Groups:           map[string]any{"model": "deepseek-v4-pro"},
			Requests:         1500,
			PromptTokens:     1_500_000,
			CompletionTokens: 200_000,
			TotalTokens:      1_700_000,
			CachedTokens:     1_000_000,
			ReasoningTokens:  0,
			CacheWriteTokens: 0,
			CostUSD:          ptr64(0.65),
		},
		{
			Groups:           map[string]any{"model": "glm-5.2"},
			Requests:         283,
			PromptTokens:     500_000,
			CompletionTokens: 30_000,
			TotalTokens:      530_000,
			CachedTokens:     100_000,
			ReasoningTokens:  0,
			CacheWriteTokens: 0,
			CostUSD:          nil,
		},
	}
	got := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{})

	// Summary header comes from Overview (2.23M total), not from summing the
	// rows (which would be fine here, but the point is the source is Overview).
	if !strings.Contains(got, "  总请求   1,783\n  总词元   2.23M\n  总开销   $0.836\n") {
		t.Errorf("summary line missing or wrong:\n%s", got)
	}
	// Cache hit lines must not appear in the overview.
	if strings.Contains(got, "命中(OpenAI)") || strings.Contains(got, "命中(Anthropic)") {
		t.Errorf("cache line must not appear in overview:\n%s", got)
	}
	// Missing-cost warning must not appear.
	if strings.Contains(got, "未算出金额") {
		t.Errorf("missing-cost warning must not appear:\n%s", got)
	}
	// Blank line between summary and table.
	if !strings.Contains(got, "\n\n") {
		t.Errorf("no blank line between summary and table:\n%s", got)
	}
	// Compact table headers: model view uses the 模型 title, the Total
	// column is gone, and the request/cache headers are the short 请求/缓存.
	for _, h := range []string{"模型", "请求", "缓存", "命中率"} {
		if !strings.Contains(got, h) {
			t.Errorf("table header %q missing:\n%s", h, got)
		}
	}
	for _, gone := range []string{"输入词元", "输出词元", "未计价", "请求数", "Total", "花费"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q column/header must not appear:\n%s", gone, got)
		}
	}
}
func TestRenderUsageReportAlignment(t *testing.T) {
	// Exact-output test: the table must be aligned (CJK headers, right-
	// aligned numbers, one-space column gaps, two-space left indent matching
	// the summary lines), and the hit-rate
	// column must show cached/prompt with one decimal (deepseek
	// 1,000,000/1,500,000 = 66.7%, glm 100,000/500,000 = 20.0%). Counts/tokens
	// use the compact k/M form. The
	// expected strings were verified visually for column alignment.
	ov := &Overview{Requests: 1783, PromptTokens: 2_000_000, CompletionTokens: 230_000, TotalTokens: 2_230_000, CachedTokens: 1_100_000, TotalCost: ptr64(0.836), FailedRequests: 12, StreamingRequests: 1690, OpenAIRequests: 1783, OpenAIPromptTokens: 2_000_000, OpenAICachedTokens: 1_100_000}
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "deepseek-v4-pro"}, Requests: 1500, PromptTokens: 1_500_000, CompletionTokens: 200_000, TotalTokens: 1_700_000, CachedTokens: 1_000_000, CostUSD: ptr64(0.65)},
		{Groups: map[string]any{"model": "glm-5.2"}, Requests: 283, PromptTokens: 500_000, CompletionTokens: 30_000, TotalTokens: 530_000, CachedTokens: 100_000, CostUSD: nil},
	}
	// widths: 模型 15 (deepseek-v4-pro) | 请求 4 | 缓存 4 | 命中率 6
	want := "  总请求   1,783\n" +
		"  总词元   2.23M\n" +
		"  总开销   $0.836\n" +
		"\n" +
		"  模型" + strings.Repeat(" ", 12) + "请求 缓存 命中率\n" +
		"  deepseek-v4-pro   1k   1M  66.7%\n" +
		"  glm-5.2          283 100k  20.0%\n"
	if got := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{}); got != want {
		t.Fatalf("RenderUsageReport mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

func TestRenderUsageReportNoData(t *testing.T) {
	ov := &Overview{}
	got := RenderUsageReport(ov, nil, []string{"model"}, ReportOptions{})
	if !strings.Contains(got, "(no data)") {
		t.Errorf("empty table must render the no-data hint:\n%s", got)
	}
	if !strings.Contains(got, "总请求   0") {
		t.Errorf("summary must still render from Overview on an empty range:\n%s", got)
	}
}

func TestRenderUsageReportColor(t *testing.T) {
	ov := &Overview{Requests: 1}
	rows := []SummaryRow{{Groups: map[string]any{"model": "m"}, Requests: 1}}
	plain := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{})
	colored := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{Color: true})
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
	got := RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{})
	for _, want := range []string{"模型", "请求", "缓存", "96.8%"} {
		if !strings.Contains(got, want) {
			t.Errorf("model view missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Total") || strings.Contains(got, "请求数") || strings.Contains(got, "输入词元") || strings.Contains(got, "输出词元") || strings.Contains(got, "花费") {
		t.Errorf("Total column, long 请求数, token columns, and 花费 column must be gone from the model view:\n%s", got)
	}
	// No cache segments in the summary here (zero requests in Overview), so
	// 缓存命中 may only appear as the old long header — its absence pins
	// the short 缓存 header.
	if strings.Contains(got, "缓存命中") {
		t.Errorf("the header must be 缓存, not 缓存命中:\n%s", got)
	}

	// Other group views keep the dynamic first-column name.
	got = RenderUsageReport(&Overview{}, rows, []string{"provider"}, ReportOptions{})
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
	got = RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{})
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
	got = RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{})
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
	got := RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{})
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
	got = RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{})
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
	if got := formatGroupValue("model", "openai/gpt-5.5"); got != "gpt-5.5" {
		t.Errorf("model with prefix = %q, want gpt-5.5", got)
	}
	if got := formatGroupValue("model", nil); got != "" {
		t.Errorf("nil group value = %q, want empty", got)
	}
}

func TestFormatModelName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// Scout & prompt examples
		{"commandcode go/laguna-s-2.1", "laguna-s-2.1"},
		{"cline-pass/deepseek-v4-flash", "deepseek-v4-flash"},
		{"z-ai/glm-5.2:free", "glm-5.2:free"},
		{"~deepseek/deepseek-v4-flash-latest", "deepseek-v4-flash"},
		{"nvidia/step-3.7-flash", "step-3.7-flash"},
		{"anthropic/claude-sonnet-4-20250514", "claude-sonnet-4"},
		{"openai/gpt-4o-2024-05-13", "gpt-4o"},
		// Date suffixes: -YYYYMMDD and -YYYY-MM-DD
		{"claude-3-5-sonnet-20241022", "claude-3-5-sonnet"},
		{"openai/chatgpt-4o-latest", "chatgpt-4o"},
		{"provider/model-20250929-latest", "model"},
		{"provider/model-latest-20250929", "model"},
		{"provider/model-2024-05-13-latest", "model"},
		// Other suffixes that must be preserved
		{"gpt-4.5", "gpt-4.5"},
		{"claude-3-5-sonnet-preview", "claude-3-5-sonnet-preview"},
		{"deepseek-v4-flash", "deepseek-v4-flash"},
		{"glm-5.2:free", "glm-5.2:free"},
		{"custom-model-4.5-preview", "custom-model-4.5-preview"},
		// Multi-level slash prefix
		{"org/team/subteam/model-v1", "model-v1"},
		// Space in prefix
		{"provider with spaces /model-name", "model-name"},
		// Edge cases: no slash
		{"deepseek-v4-pro", "deepseek-v4-pro"},
		{"gpt-4o-20240513", "gpt-4o"},
		// Edge cases: stripped empty fallback to original
		{"provider/", "provider/"},
		{"/", "/"},
		{"///", "///"},
		{"", ""},
		// Edge cases: already shortest
		{"o3", "o3"},
		{"gpt-4", "gpt-4"},
		{"claude", "claude"},
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			if got := FormatModelName(c.input); got != c.want {
				t.Errorf("FormatModelName(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// TestRenderUsageReportNoCacheSegmentsInOverview verifies that the top-level
// overview does not render source-family cache hit lines, while still rendering
// the 4-line overview.
func TestRenderUsageReportNoCacheSegmentsInOverview(t *testing.T) {
	ov := &Overview{
		Requests:       2,
		PromptTokens:   1001,
		CachedTokens:   1400,
		OpenAIRequests: 1, OpenAIPromptTokens: 1000, OpenAICachedTokens: 900,
		AnthropicRequests: 1, AnthropicPromptTokens: 1, AnthropicCachedTokens: 500, AnthropicCacheWriteTokens: 0,
	}
	got := RenderUsageReport(ov, nil, []string{"model"}, ReportOptions{})
	if strings.Contains(got, "命中(OpenAI)") || strings.Contains(got, "命中(Anthropic)") || strings.Contains(got, "缓存命中") {
		t.Errorf("cache segments must not appear in overview:\n%s", got)
	}
	if !strings.Contains(got, "  总请求   2\n") {
		t.Errorf("expected 3-line overview:\n%s", got)
	}
}

// TestRenderUsageReportCompactNumbers pins the compact number contract of
// the detail table: request/token cells use the k/M notation, so the
// acceptance values 938,553,722 / 50,913,334 render as 938.6M / 50.91M —
// and the long forms never appear.
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
	got := RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{})
	for _, want := range []string{"938.6M", "50.91M"} {
		if !strings.Contains(got, want) {
			t.Errorf("compact number %q missing:\n%s", want, got)
		}
	}
	for _, gone := range []string{"938,553,722", "908,356,736", "50,913,334", "12,322", "$21.0267", "$21.03", "…"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q must not appear in compact output:\n%s", gone, got)
		}
	}
}

// TestRenderUsageReportModelColumnTruncation pins the model column behavior:
// 1. Strips provider prefix and date/latest suffix.
// 2. Caps model column at MaxWidth 20 with ellipsis truncation.
// 3. CJK characters are truncated safely without splitting multi-byte runes.
func TestRenderUsageReportModelColumnTruncation(t *testing.T) {
	rows := []SummaryRow{
		{
			Groups:           map[string]any{"model": "anthropic/claude-sonnet-4-20250514"},
			Requests:         938_553_722,
			PromptTokens:     908_356_736,
			CachedTokens:     50_913_334,
			CompletionTokens: 12_322,
			CostUSD:          ptr64(21.0267),
		},
		{
			Groups:           map[string]any{"model": "provider/very-long-model-name-exceeding-twenty-characters"},
			Requests:         50,
			PromptTokens:     50_000,
			CompletionTokens: 5_000,
			CostUSD:          ptr64(0.50),
		},
		{
			Groups:           map[string]any{"model": "custom/自定义超长中文模型名称测试专用"},
			Requests:         10,
			PromptTokens:     10_000,
			CompletionTokens: 1_000,
			CostUSD:          ptr64(0.10),
		},
	}
	got := RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{})

	// claude-sonnet-4 (15 display width) fits in MaxWidth 20: formatted and not truncated
	if !strings.Contains(got, "claude-sonnet-4") {
		t.Errorf("expected formatted model name 'claude-sonnet-4' in report:\n%s", got)
	}
	if strings.Contains(got, "anthropic/claude-sonnet-4-20250514") {
		t.Errorf("original unformatted model name must not appear:\n%s", got)
	}

	// Long ASCII model name is truncated to MaxWidth 20 with ellipsis
	// "very-long-model-nam…" has display width 20 (19 ASCII chars + 1 ellipsis)
	if !strings.Contains(got, "very-long-model-nam…") {
		t.Errorf("expected truncated model name 'very-long-model-nam…' in report:\n%s", got)
	}

	// CJK model name is truncated safely with ellipsis without corrupting UTF-8
	// 9 Chinese runes (18 width) + "…" (1 width) = 19 display width <= 20
	if !strings.Contains(got, "自定义超长中文模型…") {
		t.Errorf("expected truncated CJK model name '自定义超长中文模型…' in report:\n%s", got)
	}

	if !utf8.ValidString(got) {
		t.Error("output is not valid UTF-8")
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Error("output contains replacement character U+FFFD (rune split)")
	}

	// Summary and table header/rows all start with exactly two leading spaces
	for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			t.Errorf("line %d must start with exactly two spaces: %q", i, line)
		}
	}

	// The first row still carries every numeric value
	var firstLine string
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "claude-sonnet-4") {
			firstLine = line
			break
		}
	}
	for _, v := range []string{"938.6M", "50.91M"} {
		if !strings.Contains(firstLine, v) {
			t.Errorf("formatted row must keep value %q: %q", v, firstLine)
		}
	}
}

// TestRenderUsageReportOtherGroupByNotTruncated verifies that non-model group
// columns are not capped at MaxWidth 20.
func TestRenderUsageReportOtherGroupByNotTruncated(t *testing.T) {
	rows := []SummaryRow{
		{
			Groups:           map[string]any{"provider": "very-long-provider-name-that-exceeds-twenty-characters"},
			Requests:         10,
			PromptTokens:     100,
			CompletionTokens: 50,
		},
	}
	got := RenderUsageReport(&Overview{}, rows, []string{"provider"}, ReportOptions{})
	if !strings.Contains(got, "very-long-provider-name-that-exceeds-twenty-characters") {
		t.Errorf("non-model group column must not be truncated:\n%s", got)
	}
	if strings.Contains(got, "…") {
		t.Errorf("non-model group column must not contain ellipsis:\n%s", got)
	}
}

// TestRenderUsageReportFitsPiPanel pins the width budget for realistic
// model names: with representative data (realistic-length model names, big
// compact counts, a priced and an unpriced row) every detail-table line
// fits 72 display columns — the π panel budget — with the one-space gap
// and the two-space indent. Names longer than ~22 display columns render
// in full (see TestRenderUsageReportLongGroupFull) and may exceed the
// budget by design.
func TestRenderUsageReportFitsPiPanel(t *testing.T) {
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "deepseek-v4-pro"}, Requests: 938_553_722, PromptTokens: 908_356_736, CachedTokens: 50_913_334, CompletionTokens: 12_322, CostUSD: ptr64(21.0267)},
		{Groups: map[string]any{"model": "glm-5.2"}, Requests: 283, PromptTokens: 500_000, CompletionTokens: 30_000, CachedTokens: 100_000, CostUSD: nil},
	}
	got := RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{})
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
