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

// ── capsule card tests (A1·命中率胶囊) ───────────────────────────────────
//
// The invariants these tests guard: every report line is EXACTLY 60 display
// columns (ANSI counted as 0) at every shape the data can take — long names,
// missing hit rates, an empty result, several group keys — in both color
// modes; the card mirrors the quota capsule card (borders, palette,
// capsule primitive) while the hit-rate gradient runs the OTHER way (a
// higher hit rate is greener); a missing denominator renders "-" and draws
// no capsule instead of fabricating one; and the no-color render is the
// colored render minus its escapes.

// reportLine is one card line with its display width, split by the shared
// helper.
type reportLine struct {
	plain string
	width int
}

// reportLines splits a report into lines and returns the ANSI-stripped
// text plus the display width of each non-empty line. The width is what
// the terminal shows, so escapes cost nothing.
func reportLines(t *testing.T, got string) []reportLine {
	t.Helper()
	raw := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	out := make([]reportLine, 0, len(raw))
	for _, l := range raw {
		if l == "" {
			continue
		}
		plain := render.StripANSI(l)
		out = append(out, reportLine{plain: plain, width: render.DisplayWidth(plain)})
	}
	return out
}

// assertCardWidth fails unless every line of got is exactly reportWidth
// display columns. It is the headline invariant of the card layout.
func assertCardWidth(t *testing.T, got string) {
	t.Helper()
	for i, l := range reportLines(t, got) {
		if l.width != reportWidth {
			t.Fatalf("line %d: width %d, want %d:\n%q", i, l.width, reportWidth, l.plain)
		}
	}
}

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

	// Summary row comes from Overview (2.23M total), not from summing the
	// rows (which would be fine here, but the point is the source is Overview).
	if !strings.Contains(got, "请求 1,783 · 词元 2.23M · 开销 $0.836") {
		t.Errorf("summary row missing or wrong:\n%s", got)
	}
	// Cache hit lines must not appear in the overview.
	if strings.Contains(got, "命中(OpenAI)") || strings.Contains(got, "命中(Anthropic)") {
		t.Errorf("cache line must not appear in overview:\n%s", got)
	}
	// Missing-cost warning must not appear.
	if strings.Contains(got, "未算出金额") {
		t.Errorf("missing-cost warning must not appear:\n%s", got)
	}
	// Card structure: ╭─ title with the grouping description, ├─ rule,
	// ╰─ bottom border.
	if !strings.HasPrefix(got, "╭─ 按模型分组 ") {
		t.Errorf("title border/description wrong:\n%s", got)
	}
	if !strings.Contains(got, "├──────────────────────────────────────────────────────────┤\n") {
		t.Errorf("├─ separator missing:\n%s", got)
	}
	if !strings.HasSuffix(got, "╰──────────────────────────────────────────────────────────╯\n") {
		t.Errorf("bottom border missing:\n%s", got)
	}
	// Compact detail headers: the model view uses the 模型 title, the Total
	// column is gone, and the request/cache headers are the short 请求/缓存.
	for _, h := range []string{"模型", "请求", "缓存", "命中率"} {
		if !strings.Contains(got, h) {
			t.Errorf("table header %q missing:\n%s", h, got)
		}
	}
	for _, gone := range []string{"输入词元", "输出词元", "未计价", "请求数", "Total", "花费", "总请求", "总词元", "总开销"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q column/header must not appear:\n%s", gone, got)
		}
	}
	// The hit-rate column is a mini capsule, not a bare percentage.
	if !strings.Contains(got, "▰▰▰▰▰▰▰▱▱▱") {
		t.Errorf("hit-rate capsule missing (deepseek 66.7%% = 7 cells):\n%s", got)
	}
	if !strings.Contains(got, "▰▰▱▱▱▱▱▱▱▱") {
		t.Errorf("hit-rate capsule missing (glm 20.0%% = 2 cells):\n%s", got)
	}
	// Every line is the card width.
	assertCardWidth(t, got)
}

// TestRenderUsageReportExact pins the whole card byte for byte: borders,
// summary row, plain-text header (no color, no bold), the dim
// sub-separator, the compact k/M numbers and the 10-cell capsule with its
// right-aligned percentage.
func TestRenderUsageReportExact(t *testing.T) {
	ov := &Overview{Requests: 1783, PromptTokens: 2_000_000, CompletionTokens: 230_000, TotalTokens: 2_230_000, CachedTokens: 1_100_000, TotalCost: ptr64(0.836), OpenAIRequests: 1783, OpenAIPromptTokens: 2_000_000, OpenAICachedTokens: 1_100_000}
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "deepseek-v4-pro"}, Requests: 1500, PromptTokens: 1_500_000, CompletionTokens: 200_000, TotalTokens: 1_700_000, CachedTokens: 1_000_000, CostUSD: ptr64(0.65)},
		{Groups: map[string]any{"model": "glm-5.2"}, Requests: 283, PromptTokens: 500_000, CompletionTokens: 30_000, TotalTokens: 530_000, CachedTokens: 100_000, CostUSD: nil},
	}
	// The group column takes the layout budget (56 − 29 − 3 = 24 columns),
	// 请求/缓存 are 6 wide each and 命中率 is 10 cells + 1 gap + 6 pct.
	want := "╭─ 按模型分组 ─────────────────────────────────────────────╮\n" +
		"│ 请求 1,783 · 词元 2.23M · 开销 $0.836                    │\n" +
		"├──────────────────────────────────────────────────────────┤\n" +
		"│ 模型                       请求   缓存            命中率 │\n" +
		"│ ──────────────────────────────────────────────────────── │\n" +
		"│ deepseek-v4-pro              1k     1M ▰▰▰▰▰▰▰▱▱▱  66.7% │\n" +
		"│ glm-5.2                     283   100k ▰▰▱▱▱▱▱▱▱▱  20.0% │\n" +
		"╰──────────────────────────────────────────────────────────╯\n"
	if got := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{}); got != want {
		t.Fatalf("RenderUsageReport mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderUsageReportNoData keeps the empty result inside the card: the
// title, the summary row (from Overview) and the friendly hint, all at the
// card width.
func TestRenderUsageReportNoData(t *testing.T) {
	ov := &Overview{}
	got := RenderUsageReport(ov, nil, []string{"model"}, ReportOptions{})
	if !strings.Contains(got, "（暂无数据）") {
		t.Errorf("empty table must render the no-data hint:\n%s", got)
	}
	if !strings.Contains(got, "请求 0 · 词元 0 · 开销 -") {
		t.Errorf("summary must still render from Overview on an empty range:\n%s", got)
	}
	if !strings.HasPrefix(got, "╭─ ") || !strings.HasSuffix(got, "╯\n") {
		t.Errorf("the empty card must keep its borders:\n%s", got)
	}
	assertCardWidth(t, got)
}

// TestRenderUsageReportColor pins the palette contract: the piped render
// carries no escape at all, neither render carries color or bold on the card
// TEXT (title and header included) — only the dim borders and the hit-rate
// capsule keep their escapes — and the two are byte identical once the
// escapes are stripped.
func TestRenderUsageReportColor(t *testing.T) {
	ov := &Overview{Requests: 1, TotalCost: ptr64(0.5)}
	rows := []SummaryRow{{Groups: map[string]any{"model": "m"}, Requests: 1, PromptTokens: 10, CachedTokens: 5}}
	plain := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{})
	colored := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{Color: true})
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("plain output must not contain ANSI escapes:\n%q", plain)
	}
	// Card text is plain in BOTH modes: no bold escape and no brand color
	// around the title or the column titles.
	for _, bad := range []string{"\x1b[1m", "\x1b[38;2;0;180;216m模型", "\x1b[38;2;0;180;216m按模型分组"} {
		if strings.Contains(colored, bad) {
			t.Errorf("colored output must keep %q unstyled:\n%q", bad, colored)
		}
	}
	// Colored output still colors what is not text: the dim borders.
	if !strings.Contains(colored, "\x1b[38;2;102;102;102m│ ") {
		t.Errorf("colored output must still dim the body border:\n%q", colored)
	}
	// Both render the same visible text once ANSI is stripped.
	if render.StripANSI(colored) != plain {
		t.Errorf("color must not change the visible text\nplain: %q\ncolored: %q", plain, colored)
	}
	// The width invariant holds in both modes.
	assertCardWidth(t, plain)
	assertCardWidth(t, colored)
}

// TestRenderUsageReportTitleTextIsPlainText locks the plain-text title
// contract, the usage card's half of the planusage
// TestRenderCardsTitleTextIsPlainText pin. In a COLORED render the title line
// is EXACTLY Dim("╭─ ") + desc + Dim(" " + fill + "╮"): the 「用量」 head and
// the 「 · 」 separator are gone, so the grouping description sits bare
// between the two dim runs, and there is not a single escape between them.
// The colored render is the no-color render plus those escapes, byte for
// byte, and no bold (ESC [ 1 m) survives anywhere in the card.
func TestRenderUsageReportTitleTextIsPlainText(t *testing.T) {
	ov := &Overview{Requests: 2, TotalCost: ptr64(0.15)}
	rows := []SummaryRow{{Groups: map[string]any{"model": "gpt-5"}, Requests: 2, PromptTokens: 300}}
	colored := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{Color: true})
	plain := RenderUsageReport(ov, rows, []string{"model"}, ReportOptions{})

	const ansiDim, ansiReset = "\x1b[38;2;102;102;102m", "\x1b[0m"
	// The description is the whole title: one space, then the dash fill that
	// absorbs whatever width the description does not take (3 + 10 + 1 + 45
	// + 1 = 60 for 按模型分组).
	const desc = "按模型分组"
	fill := reportWidth - 3 - 1 - render.DisplayWidth(desc) - 1
	wantTitle := ansiDim + "╭─ " + ansiReset +
		desc +
		ansiDim + " " + strings.Repeat("─", fill) + "╮" + ansiReset
	if rawTitle := strings.SplitN(colored, "\n", 2)[0]; rawTitle != wantTitle {
		t.Fatalf("title line mismatch\n got: %q\nwant: %q", rawTitle, wantTitle)
	}
	// The plain render shows the same line with nothing in between: a
	// scraper that used to key on the 「用量 ·」 head finds no such prefix.
	if plainTitle := strings.SplitN(plain, "\n", 2)[0]; plainTitle != "╭─ "+desc+" "+strings.Repeat("─", fill)+"╮" {
		t.Fatalf("plain title line mismatch: %q", plainTitle)
	}
	// Card text carries no color and no weight in EITHER mode: no bold, and
	// no brand cyan anywhere (the ramp green/yellow/red and the dim borders
	// are the only colors the card is allowed to paint).
	for _, bad := range []string{"\x1b[1m", "\x1b[38;2;0;180;216m"} {
		if strings.Contains(colored, bad) {
			t.Errorf("colored output must keep the card text unstyled, found %q:\n%q", bad, colored)
		}
	}
	// Color changes nothing but the escapes.
	if render.StripANSI(colored) != plain {
		t.Errorf("color must not change the visible text\nplain: %q\ncolored: %q",
			plain, render.StripANSI(colored))
	}
	if strings.Contains(plain, "\x1b") {
		t.Errorf("no-color render still carries escapes:\n%q", plain)
	}
	assertCardWidth(t, plain)
	assertCardWidth(t, colored)
}

// TestRenderUsageReportHitRateCell pins the 命中率 column: the denominator
// is the source-aware cacheHitInput, a zero or missing denominator renders
// "-" with NO capsule (nothing to measure, nothing fabricated), and a zero
// numerator renders 0.0% over an all-hollow capsule.
func TestRenderUsageReportHitRateCell(t *testing.T) {
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "m1"}, Requests: 1, PromptTokens: 1000, CachedTokens: 968, CompletionTokens: 100, CostUSD: ptr64(0.1)},
	}
	// Model view: 命中率 968/1000 = 96.8% → 10 filled cells.
	got := RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{})
	if !strings.Contains(got, "96.8%") {
		t.Errorf("model view hit rate must be 96.8%%:\n%s", got)
	}
	if !strings.Contains(got, strings.Repeat("▰", 10)) {
		t.Errorf("a 96.8%% hit rate must fill the whole 10-cell capsule:\n%s", got)
	}
	for _, gone := range []string{"Total", "请求数", "输入词元", "输出词元", "花费", "缓存命中"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q must be gone from the model view:\n%s", gone, got)
		}
	}

	// Other group views label the first column with the key's Chinese name.
	got = RenderUsageReport(&Overview{}, rows, []string{"provider"}, ReportOptions{})
	if !strings.Contains(got, "供应商") {
		t.Errorf("provider view must label its first column 供应商:\n%s", got)
	}
	if strings.Contains(got, "模型") {
		t.Errorf("provider view must not use the 模型 title:\n%s", got)
	}

	// Denominator missing (prompt 0): "-" and no capsule — not "0.0%" and
	// not a full bar, both of which would be invented data.
	rows = []SummaryRow{
		{Groups: map[string]any{"model": "m0"}, Requests: 1, PromptTokens: 0, CachedTokens: 5, CompletionTokens: 0},
	}
	got = RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{})
	for _, bad := range []string{"NaN", "Inf", "0.0%", "▰", "▱"} {
		if strings.Contains(got, bad) {
			t.Errorf("missing denominator must render a bare %q placeholder (no %q):\n%s", "-", bad, got)
		}
	}
	if !strings.Contains(got, "                - │") {
		t.Errorf("missing denominator must render the '-' placeholder:\n%s", got)
	}

	// Zero cached with a non-zero prompt: 0.0% over an all-hollow capsule.
	rows = []SummaryRow{
		{Groups: map[string]any{"model": "m1"}, Requests: 1, PromptTokens: 100, CachedTokens: 0, CompletionTokens: 10},
	}
	got = RenderUsageReport(&Overview{}, rows, []string{"model"}, ReportOptions{})
	if !strings.Contains(got, "0.0%") {
		t.Errorf("zero-cached hit rate must render 0.0%%:\n%s", got)
	}
	if !strings.Contains(got, "▱▱▱▱▱▱▱▱▱▱") {
		t.Errorf("a 0%% hit rate must draw an all-hollow capsule:\n%s", got)
	}
	if strings.Contains(got, "▰") {
		t.Errorf("a 0%% hit rate must draw no filled cell:\n%s", got)
	}
}

// TestRenderUsageReportAnthropicHitRate pins the table hit-rate column to
// the same source-aware denominator as the overview segments. Anthropic-form
// cache_read sits outside input_tokens; cached/prompt would explode past
// 100% (500/1 = 50000%).
func TestRenderUsageReportAnthropicHitRate(t *testing.T) {
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
	// 99.8 % still fits inside the 10-cell capsule (ceil(9.98) = 10) and the
	// percentage column is 6 wide.
	if !strings.Contains(got, "99.8% │") {
		t.Errorf("the percentage must stay inside the card:\n%s", got)
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

// TestHitCellGradientDirection locks the ONE knob that differs from the
// quota capsule card: the level fed to the shared ramp is inverted, so a
// high hit rate fills GREEN cells and a cold cache reads red. The glyph
// run is identical to the quota direction.
func TestHitCellGradientDirection(t *testing.T) {
	pal := reportPalette{color: true}
	ansiGreen := "\x1b[38;2;82;183;136m"
	ansiDim := "\x1b[38;2;102;102;102m"
	reset := "\x1b[0m"

	// 95 % cached: the capsule is nearly full and reaches the flat green
	// band (its last filled cell stands for a ≥60 % hit rate).
	high := hitCell(950, 1000, pal)
	if !strings.Contains(high, ansiGreen) {
		t.Errorf("a 95%% hit rate must read green:\n%q", high)
	}
	if strings.Count(high, "▰") != 10 || strings.Count(high, "▱") != 0 {
		t.Errorf("a 95%% hit rate must fill all 10 cells: %q", high)
	}
	if !strings.HasSuffix(high, "95.0%") {
		t.Errorf("the percentage must stay right-aligned: %q", high)
	}

	// 5 % cached: one short, warm (red-side) cell and a dim remainder.
	low := hitCell(50, 1000, pal)
	if strings.Contains(low, ansiGreen) {
		t.Errorf("a 5%% hit rate must not reach green:\n%q", low)
	}
	// The leading cell of a 10-cell bar stands for a 90 % severity (100 -
	// CapsuleLevel(0, 10)), which is far warmer than the healthy green band.
	coldest := render.CapsuleRamp(100 - render.CapsuleLevel(0, hitCells))
	if coldest == [3]uint8{82, 183, 136} {
		t.Errorf("the cold end of the hit-rate ramp must not be the flat green: %v", coldest)
	}
	if coldest[0] <= 82 || coldest[1] >= 183 || coldest[2] >= 136 {
		t.Errorf("the cold end of the hit-rate ramp must be warm (red side): %v", coldest)
	}
	if !strings.Contains(low, ansiDim+strings.Repeat("▱", 9)+reset) {
		t.Errorf("the remaining 9 cells must be dim hollow:\n%q", low)
	}

	// The filled LENGTH is the hit rate itself: 50 % is exactly half a bar.
	half := hitCell(100, 200, pal)
	if strings.Count(half, "▰") != 5 || strings.Count(half, "▱") != 5 {
		t.Errorf("a 50%% hit rate must fill 5 of 10 cells: %q", half)
	}

	// No denominator: a dash, no glyph at all.
	if got := hitCell(5, 0, pal); strings.ContainsAny(got, "▰▱") || !strings.HasSuffix(got, "-") {
		t.Errorf("no denominator must render a bare dash: %q", got)
	}
}

// TestRenderUsageReportCardWidth walks every shape the card must survive —
// long and CJK model names, a missing hit rate, an empty result, several
// group keys and no group_by at all — and asserts every line is exactly
// reportWidth columns, colored AND no-color.
func TestRenderUsageReportCardWidth(t *testing.T) {
	loc := time.Local
	dayStart := time.Date(2026, 3, 10, 0, 0, 0, 0, loc)
	ov := &Overview{Requests: 4, PromptTokens: 10_000, CompletionTokens: 500, TotalTokens: 10_500, CachedTokens: 4_000, TotalCost: ptr64(0.5)}
	cases := []struct {
		name    string
		rows    []SummaryRow
		groupBy []string
	}{
		{
			name: "model view",
			rows: []SummaryRow{
				{Groups: map[string]any{"model": "deepseek-v4-pro"}, Requests: 1500, PromptTokens: 1_500_000, CachedTokens: 1_000_000, CostUSD: ptr64(0.65)},
				{Groups: map[string]any{"model": "glm-5.2"}, Requests: 283, PromptTokens: 500_000, CachedTokens: 100_000},
			},
			groupBy: []string{"model"},
		},
		{
			name: "long ascii model name",
			rows: []SummaryRow{
				{Groups: map[string]any{"model": "provider/very-long-model-name-exceeding-twenty-characters"}, Requests: 50, PromptTokens: 50_000, CachedTokens: 10_000},
			},
			groupBy: []string{"model"},
		},
		{
			name: "cjk model name",
			rows: []SummaryRow{
				{Groups: map[string]any{"model": "custom/自定义超长中文模型名称测试专用"}, Requests: 10, PromptTokens: 10_000, CachedTokens: 9_500},
			},
			groupBy: []string{"model"},
		},
		{
			name:    "missing hit rate",
			rows:    []SummaryRow{{Groups: map[string]any{"model": "m"}, Requests: 1, PromptTokens: 0, CachedTokens: 5}},
			groupBy: []string{"model"},
		},
		{
			name:    "zero hit rate",
			rows:    []SummaryRow{{Groups: map[string]any{"model": "m"}, Requests: 1, PromptTokens: 100, CachedTokens: 0}},
			groupBy: []string{"model"},
		},
		{
			name:    "empty result",
			rows:    nil,
			groupBy: []string{"model"},
		},
		{
			name: "two group keys",
			rows: []SummaryRow{
				{Groups: map[string]any{"model": "gpt-5.5", "provider": "openai"}, Requests: 12, PromptTokens: 1000, CachedTokens: 900},
				{Groups: map[string]any{"model": "deepseek-v4-pro", "provider": "very-long-provider-name"}, Requests: 40, PromptTokens: 900, CachedTokens: 30},
			},
			groupBy: []string{"model", "provider"},
		},
		{
			name: "three group keys",
			rows: []SummaryRow{
				{Groups: map[string]any{"model": "gpt-5.5", "provider": "openai", "stream": int64(1)}, Requests: 12, PromptTokens: 1000, CachedTokens: 900},
			},
			groupBy: []string{"model", "provider", "stream"},
		},
		{
			name: "time bucket view",
			rows: []SummaryRow{
				{Groups: map[string]any{"day": dayStart.Unix()}, Requests: 12, PromptTokens: 1000, CachedTokens: 900},
			},
			groupBy: []string{"day"},
		},
		{
			name:    "no group_by",
			rows:    []SummaryRow{{Requests: 5, PromptTokens: 1000, CachedTokens: 250}},
			groupBy: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, color := range []bool{false, true} {
				got := RenderUsageReport(ov, tc.rows, tc.groupBy, ReportOptions{Color: color})
				assertCardWidth(t, got)
				if !color && strings.Contains(got, "\x1b[") {
					t.Fatalf("no-color render carries escapes:\n%q", got)
				}
			}
			plain := RenderUsageReport(ov, tc.rows, tc.groupBy, ReportOptions{})
			colored := RenderUsageReport(ov, tc.rows, tc.groupBy, ReportOptions{Color: true})
			if render.StripANSI(colored) != plain {
				t.Fatalf("no-color render is not the colored render minus escapes:\ngot  %q\nwant %q", plain, colored)
			}
		})
	}
}

// TestRenderUsageReportNoGroupBy covers the ungrouped single-row shape:
// no group column at all, the fixed columns only, and the 未分组 title.
func TestRenderUsageReportNoGroupBy(t *testing.T) {
	rows := []SummaryRow{{Requests: 5, PromptTokens: 1000, CachedTokens: 250}}
	got := RenderUsageReport(&Overview{Requests: 5, TotalTokens: 1000}, rows, nil, ReportOptions{})
	if !strings.Contains(got, "未分组") {
		t.Errorf("the ungrouped title description is 未分组:\n%s", got)
	}
	if !strings.Contains(got, "25.0%") {
		t.Errorf("ungrouped hit rate must render 250/1000 = 25.0%%:\n%s", got)
	}
	assertCardWidth(t, got)
}

// TestRenderUsageReportMultiGroupKeys covers a multi-key view: every group
// key gets a column (each capped by the layout budget), the title lists
// every key, and the columns still add up to the card width.
func TestRenderUsageReportMultiGroupKeys(t *testing.T) {
	rows := []SummaryRow{
		{Groups: map[string]any{"model": "gpt-5.5", "provider": "openai"}, Requests: 12, PromptTokens: 1000, CachedTokens: 900},
		{Groups: map[string]any{"model": "glm-5.2", "provider": "z-ai"}, Requests: 40, PromptTokens: 900, CachedTokens: 30},
	}
	got := RenderUsageReport(&Overview{}, rows, []string{"model", "provider"}, ReportOptions{})
	if !strings.Contains(got, "按模型/供应商分组") {
		t.Errorf("the title must list every group key:\n%s", got)
	}
	for _, want := range []string{"gpt-5.5", "openai", "glm-5.2", "z-ai", "模型", "供应商"} {
		if !strings.Contains(got, want) {
			t.Errorf("multi-key view missing %q:\n%s", want, got)
		}
	}
	assertCardWidth(t, got)

	// Column budget: 2 group keys → 24 columns split 12/12, so a group value
	// longer than its share is ellipsis-truncated instead of pushing the
	// border out.
	cols := reportColumns([]string{"model", "provider"})
	if len(cols) != 5 {
		t.Fatalf("want 5 columns (2 group + 3 fixed), got %d", len(cols))
	}
	if cols[0].width+cols[1].width+cols[2].width+cols[3].width+cols[4].width+
		len(colGap)*(len(cols)-1) != tableWidth {
		t.Fatalf("columns do not fill the table area: %+v", cols)
	}
	long := RenderUsageReport(&Overview{}, []SummaryRow{
		{Groups: map[string]any{"model": "provider-with-a-very-long-name", "provider": "another-very-long-provider"}, Requests: 1, PromptTokens: 10, CachedTokens: 5},
	}, []string{"model", "provider"}, ReportOptions{})
	if !strings.Contains(long, "…") {
		t.Errorf("over-long group values must be ellipsis-truncated:\n%s", long)
	}
	assertCardWidth(t, long)
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
	for _, gone := range []string{"938,553,722", "908,356,736", "50,913,334", "12,322", "$21.0267", "$21.03"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q must not appear in compact output:\n%s", gone, got)
		}
	}
}

// TestRenderUsageReportOverLongTitle is the usage card's half of the
// title width pit. The grouping description is the title's only variable
// segment, and an unknown group_by key passes through its raw name — which
// can be arbitrarily long. The description must be truncated FIRST and the
// dash fill recomputed from the width it ACTUALLY occupies; the line itself
// is never truncated, so the right border ╮ always survives and the title
// stays at exactly reportWidth columns (a whole-line truncation safety net
// used to eat the ╮ and could leave the card at 59 columns).
func TestRenderUsageReportOverLongTitle(t *testing.T) {
	ov := &Overview{Requests: 1, PromptTokens: 10, CachedTokens: 5}
	rows := []SummaryRow{{Requests: 1, PromptTokens: 10, CachedTokens: 5}}
	cases := [][]string{
		{strings.Repeat("k", 80)}, // ASCII key: 80 columns
		{strings.Repeat("键", 30)},
		{"model", "provider", "account", "key_id", "stream", "success", "hour", "day", strings.Repeat("x", 40)},
		{"模型", strings.Repeat("键", 26)}, // CJK key: 59 columns, truncated to 53 — the double-width rune the ellipsis needs stops one column short
	}
	for _, groupBy := range cases {
		got := RenderUsageReport(ov, rows, groupBy, ReportOptions{})
		assertCardWidth(t, got)
		title := reportLines(t, got)[0].plain
		if !strings.HasPrefix(title, "╭─ ") {
			t.Errorf("group_by %q: title head wrong: %q", groupBy, title)
		}
		if !strings.HasSuffix(title, "╮") {
			t.Errorf("group_by %q: the ╮ was cut off: %q", groupBy, title)
		}
		if !strings.Contains(title, "…") {
			t.Errorf("group_by %q: over-long description not truncated: %q", groupBy, title)
		}
		// After the ellipsis: one space, dash fill only, then ╮.
		fill := title[strings.LastIndex(title, "…")+len("…"):]
		if !strings.HasPrefix(fill, " ") || !strings.Contains(fill, "─") ||
			strings.Trim(fill, " ─╮") != "" {
			t.Errorf("group_by %q: dash fill malformed: %q", groupBy, title)
		}
	}
}

// TestRenderUsageReportModelColumnTruncation pins the model column
// behavior: strips the provider prefix and date/latest suffix, caps the
// column at 20 columns with ellipsis truncation, truncates CJK safely, and
// keeps every card line at the card width.
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

	// claude-sonnet-4 (15 display width) fits the 20-column cap: formatted
	// and not truncated.
	if !strings.Contains(got, "claude-sonnet-4") {
		t.Errorf("expected formatted model name 'claude-sonnet-4' in report:\n%s", got)
	}
	if strings.Contains(got, "anthropic/claude-sonnet-4-20250514") {
		t.Errorf("original unformatted model name must not appear:\n%s", got)
	}

	// Long ASCII model name is truncated to the 20-column cap with ellipsis
	// ("very-long-model-nam…" = 19 ASCII chars + 1 ellipsis = 20).
	if !strings.Contains(got, "very-long-model-nam…") {
		t.Errorf("expected truncated model name 'very-long-model-nam…' in report:\n%s", got)
	}

	// CJK model name is truncated safely with ellipsis without corrupting
	// UTF-8 (9 Chinese runes = 18 width + 1 ellipsis = 19 ≤ 20).
	if !strings.Contains(got, "自定义超长中文模型…") {
		t.Errorf("expected truncated CJK model name '自定义超长中文模型…' in report:\n%s", got)
	}

	if !utf8.ValidString(got) {
		t.Error("output is not valid UTF-8")
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Error("output contains replacement character U+FFFD (rune split)")
	}
	assertCardWidth(t, got)
}

// TestRenderUsageReportGroupColumnBudget pins the layout budget for group
// columns: the model column keeps its 20-column cap while every other
// group column shares the same budget (so a long provider name is
// ellipsis-truncated to the card width instead of overflowing the border,
// and a short one is left intact).
func TestRenderUsageReportGroupColumnBudget(t *testing.T) {
	rows := []SummaryRow{
		{
			Groups:           map[string]any{"provider": "very-long-provider-name-that-exceeds-twenty-characters"},
			Requests:         10,
			PromptTokens:     100,
			CompletionTokens: 50,
		},
		{
			Groups:           map[string]any{"provider": "short"},
			Requests:         10,
			PromptTokens:     100,
			CompletionTokens: 50,
		},
	}
	got := RenderUsageReport(&Overview{}, rows, []string{"provider"}, ReportOptions{})
	// The provider column gets the whole single-group budget (24 columns),
	// so a 53-character value is ellipsis-truncated rather than printed in
	// full: a fixed card cannot grow to fit it.
	if !strings.Contains(got, "…") {
		t.Errorf("over-long group value must be ellipsis-truncated:\n%s", got)
	}
	if strings.Contains(got, "very-long-provider-name-that-exceeds-twenty-characters") {
		t.Errorf("an over-long group value must not be printed in full:\n%s", got)
	}
	if !strings.Contains(got, "short") {
		t.Errorf("a short group value must render intact:\n%s", got)
	}
	// A value that fits the budget is not touched (no ellipsis on it).
	if strings.Count(got, "…") != 1 {
		t.Errorf("only the over-long value may carry an ellipsis:\n%s", got)
	}
	assertCardWidth(t, got)

	// The model column keeps its historical 20-column cap, which is
	// narrower than the 24-column single-group budget.
	cols := reportColumns([]string{"model"})
	if len(cols) != 4 || cols[0].maxWidth != modelMaxWidth {
		t.Fatalf("model column cap = %d, want %d (cols %+v)", cols[0].maxWidth, modelMaxWidth, cols)
	}
}

// TestRenderUsageReportNoCacheSegmentsInOverview verifies that the top-level
// overview does not render source-family cache hit lines, while still
// rendering the card.
func TestRenderUsageReportNoCacheSegmentsInOverview(t *testing.T) {
	ov := &Overview{
		Requests:       2,
		TotalTokens:    1001,
		PromptTokens:   1001,
		CachedTokens:   1400,
		OpenAIRequests: 1, OpenAIPromptTokens: 1000, OpenAICachedTokens: 900,
		AnthropicRequests: 1, AnthropicPromptTokens: 1, AnthropicCachedTokens: 500, AnthropicCacheWriteTokens: 0,
	}
	got := RenderUsageReport(ov, nil, []string{"model"}, ReportOptions{})
	if strings.Contains(got, "命中(OpenAI)") || strings.Contains(got, "命中(Anthropic)") || strings.Contains(got, "缓存命中") {
		t.Errorf("cache segments must not appear in overview:\n%s", got)
	}
	if !strings.Contains(got, "请求 2 · 词元 1k") {
		t.Errorf("expected the summary row from Overview:\n%s", got)
	}
}

// TestReportColumnsFillTheTableArea is the arithmetic guard behind the card
// width: the fixed columns plus the group columns plus the gaps must fill
// the 56-column table area exactly, for any number of group keys.
func TestReportColumnsFillTheTableArea(t *testing.T) {
	for n := 0; n <= 5; n++ {
		groupBy := []string{"model", "provider", "account", "key_id", "stream", "success", "hour", "day"}[:n]
		cols := reportColumns(groupBy)
		total := len(colGap) * (len(cols) - 1)
		for _, c := range cols {
			total += c.width
		}
		if len(cols) != n+3 {
			t.Fatalf("%d group keys: want %d columns, got %d", n, n+3, len(cols))
		}
		if n == 0 {
			// An ungrouped view simply has no group column: the three fixed
			// columns are left-aligned at the card body and the row builder
			// pads the rest of the card body out.
			if total != reqWidth+cacheWidth+hitWidth+2*len(colGap) {
				t.Fatalf("%d group keys: fixed columns = %d, want %d", n, total, reqWidth+cacheWidth+hitWidth+2*len(colGap))
			}
			continue
		}
		if total != tableWidth {
			t.Fatalf("%d group keys: columns fill %d of %d columns: %+v", n, total, tableWidth, cols)
		}
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
	if got := formatGroupValue("stream", int64(1)); got != "是" {
		t.Errorf("stream 1 = %q, want 是", got)
	}
	if got := formatGroupValue("stream", int64(0)); got != "否" {
		t.Errorf("stream 0 = %q, want 否", got)
	}
	if got := formatGroupValue("success", int64(1)); got != "正常" {
		t.Errorf("success 1 = %q, want 正常", got)
	}
	if got := formatGroupValue("success", int64(0)); got != "失败" {
		t.Errorf("success 0 = %q, want 失败", got)
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

// TestCacheHitRateZeroDenominator keeps the one-decimal formatter stable
// for a zero denominator: "0.0%", never NaN/Inf. The CELL decides what to
// do with it (a bare "-", see hitCell) — a shared formatter must not be
// the place that invents a "no data" answer.
func TestCacheHitRateZeroDenominator(t *testing.T) {
	if got := cacheHitRate(5, 0); got != "0.0%" {
		t.Errorf("cacheHitRate(5, 0) = %q, want 0.0%%", got)
	}
	if got := cacheHitRate(0, 0); got != "0.0%" {
		t.Errorf("cacheHitRate(0, 0) = %q, want 0.0%%", got)
	}
	if got := cacheHitRate(968, 1000); got != "96.8%" {
		t.Errorf("cacheHitRate(968, 1000) = %q, want 96.8%%", got)
	}
	if got := cacheHitRate(0, 100); got != "0.0%" {
		t.Errorf("cacheHitRate(0, 100) = %q, want 0.0%%", got)
	}
}
