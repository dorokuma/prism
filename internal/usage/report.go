package usage

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/dorokuma/prism/internal/render"
)

// blankModel reports whether s is NULL-equivalent for the model field:
// empty string or pure whitespace (including invisible whitespace runes).
func blankModel(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// FilterBlankModelRows drops rows whose model group value is blank.
// It is used when the summary view is grouped by model; other group-by
// modes keep these rows because their non-model grouping values are
// still meaningful. Rows with Groups==nil are kept: they carry no model
// key (they come from an ungrouped aggregate), and the caller can decide
// whether they should appear in a grouped view.
func FilterBlankModelRows(rows []SummaryRow) []SummaryRow {
	out := make([]SummaryRow, 0, len(rows))
	for _, r := range rows {
		if r.Groups == nil {
			out = append(out, r)
			continue
		}
		s, _ := r.Groups["model"].(string)
		if blankModel(s) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// PeriodWeek is the header label when usage defaults to the SuperGrok week.
const PeriodWeek = "本周"

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
	// time) AND the range ends at the present moment — i.e. the user gave
	// a relative --since like "7d". A range like "08-01 至 08-10" (both
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
	// Color enables ANSI coloring (dim borders and the hit-rate capsule
	// ramp). Alignment is computed on the de-colored text, so colored
	// output stays aligned; with Color false, ANSI sequences are never
	// emitted, so piped output is plain text.
	Color bool
}

// ── capsule card layout (A1·命中率胶囊) ──────────────────────────────────
//
// The report is one fixed-width card that mirrors the quota capsule card
// (internal/planusage.RenderCards): the same 56 display columns, the same
// ╭─ / ├─ / ╰─ border vocabulary, the same palette and the same capsule
// primitive (internal/render.CapsuleBar). Every line — title, summary,
// separators, header, detail rows, bottom border — is EXACTLY reportWidth
// display columns wide, whatever the data looks like:
//
//	╭─ 按模型分组 ─────────────────────────────────────────╮
//	│ 请求 1,783 · 词元 2.23M · 开销 $0.836                │
//	├──────────────────────────────────────────────────────┤
//	│ 模型                   请求   缓存            命中率 │
//	│ ──────────────────────────────────────────────────── │
//	│ deepseek-v4-pro          1k     1M ▰▰▰▰▰▰▰▱▱▱  66.7% │
//	│ glm-5.2                 283   100k ▰▰▱▱▱▱▱▱▱▱  20.0% │
//	╰──────────────────────────────────────────────────────╯
//
// Geometry (display columns, ANSI counted as 0):
//
//	title     Dim("╭─ ") + desc + Dim(" " + fill + "╮")
//	body      Dim("│ ") + content(52) + Dim(" │") = 56
//	rules     "├" + "─"×54 + "┤" / "╰" + "─"×54 + "╯" = 56
//	table     group columns + 请求 + 缓存 + 命中率 = 52
//
// The body gutter is SYMMETRIC: one space either side of the content
// ("│ " / " │") and nothing else — no extra indent inside the card — so
// the left and right breathing room is exactly 1:1 at every line.
//
// The 命中率 column reserves its label FIRST (10 capsule cells + 1 gap +
// 6 percentage columns) and the group columns share whatever is left, so
// the capsule and the percentage can never be squeezed out — the same
// "reserve the label, then size the bar" rule the quota card uses. Cell
// padding, value truncation and the capsule all go through
// render.PadRight / render.PadLeft / render.Truncate / render.CapsuleBar,
// which are ANSI-aware; the row builders additionally pad (and, as a
// safety net, truncate) the whole body to reportInner.
const (
	// reportWidth is the total display width of every report line.
	reportWidth = 56
	// reportInner is the width between "│ " and " │" (56 - 4).
	reportInner = reportWidth - 4
	// colGap separates two table columns.
	colGap = " "

	// reqWidth / cacheWidth are the fixed request and cache columns.
	// FormatTokens keeps every render at 6 columns or fewer across its
	// whole carry chain, M -> B -> T -> P -> E: two-decimal M below 100M
	// ("50.91M"), one decimal from 100M up ("938.6M"), and the same
	// single-decimal shape after each carry ("4.6B", "999.9T"), so 6
	// keeps realistic counts intact.
	reqWidth   = 6
	cacheWidth = 6

	// hitCells is the mini capsule's cell count (▰▱), hitGap the space
	// between capsule and percentage, pctWidth the percentage column
	// ("100.0%" is exactly 6).
	hitCells = 10
	hitGap   = 1
	pctWidth = 6
	// hitWidth is the whole 命中率 column: capsule + gap + percentage.
	hitWidth = hitCells + hitGap + pctWidth

	// tableWidth is the table area inside the card body: the whole
	// reportInner, with no indent of its own — the only padding left is
	// the symmetric one-space body gutter.
	tableWidth = reportInner

	// modelMaxWidth keeps the established model column cap: a formatted
	// model name longer than 20 columns is truncated with an ellipsis.
	modelMaxWidth = 20

	// noDataLine is the friendly hint rendered instead of detail rows.
	noDataLine = "（暂无数据）"
)

// columnKind tells what a report column holds: a group value, or one of
// the three fixed metrics.
type columnKind int

const (
	kindGroup columnKind = iota
	kindRequests
	kindCache
	kindHit
)

// reportColumn is one detail column of the usage card. width is the fixed
// display width the column occupies inside the 52-column table area, so a
// row's columns always add up to tableWidth exactly; maxWidth caps the
// VALUE before padding (0 = uncapped, the model column is capped at
// modelMaxWidth).
type reportColumn struct {
	key      string
	title    string
	align    render.Align
	width    int
	maxWidth int
	kind     columnKind
}

// reportColumns lays out the detail columns: one column per group_by key
// (the model group keeps the 模型 title and the 20-column cap, every other
// known group key carries its Chinese label from groupKeyLabels, and an
// unknown key keeps its raw name), then 请求 / 缓存 / 命中率. The group
// columns share the horizontal budget that the fixed columns and the
// column gaps leave over — split as evenly as possible, with the remainder
// to the leading columns — so a multi-key view still adds up to tableWidth.
// The Total column is deliberately not rendered.
func reportColumns(groupBy []string) []reportColumn {
	cols := make([]reportColumn, 0, len(groupBy)+3)
	if len(groupBy) > 0 {
		budget := tableWidth - reqWidth - cacheWidth - hitWidth -
			len(colGap)*(len(groupBy)+2)
		base, rem := budget/len(groupBy), budget%len(groupBy)
		for i, g := range groupBy {
			w := base
			if i < rem {
				w++
			}
			title, capWidth := groupKeyLabel(g), w
			if g == "model" && i == 0 && capWidth > modelMaxWidth {
				capWidth = modelMaxWidth
			}
			cols = append(cols, reportColumn{
				key: g, title: title, align: render.AlignLeft,
				width: w, maxWidth: capWidth, kind: kindGroup,
			})
		}
	}
	cols = append(cols,
		reportColumn{title: "请求", align: render.AlignRight, width: reqWidth, kind: kindRequests},
		reportColumn{title: "缓存", align: render.AlignRight, width: cacheWidth, kind: kindCache},
		reportColumn{title: "命中率", align: render.AlignRight, width: hitWidth, kind: kindHit},
	)
	return cols
}

// RenderUsageReport renders the usage summary as the capsule card: the
// title (grouping description only — no 「用量 ·」 prefix), the summary row
// (taken from Overview — never from summing the grouped rows, because a
// truncated LIMIT would make the totals look small), a ├─ rule, the detail
// table with its plain-text header and dim sub-separator, and the ╰─ bottom
// border. It is the single implementation behind both the prism usage CLI
// and the HTTP format=table output, so the two outputs can never drift
// apart. The layout never depends on the terminal width, so --watch
// redraws are stable and non-TTY output (e.g. a π panel capture) is
// identical.
func RenderUsageReport(ov *Overview, rows []SummaryRow, groupBy []string, opts ReportOptions) string {
	pal := reportPalette{color: opts.Color}
	cols := reportColumns(groupBy)

	var b strings.Builder
	b.WriteString(pal.titleLine(groupDesc(groupBy)))
	b.WriteByte('\n')
	b.WriteString(pal.body(render.SummaryLine(render.Summary{
		Requests: ov.Requests,
		Tokens:   ov.TotalTokens,
		Cost:     ov.TotalCost,
	})))
	b.WriteByte('\n')
	b.WriteString(pal.rule("├", "┤"))
	b.WriteByte('\n')
	b.WriteString(pal.body(pal.headerContent(cols)))
	b.WriteByte('\n')
	b.WriteString(pal.body(pal.dim(strings.Repeat("─", tableWidth))))
	b.WriteByte('\n')
	if len(rows) == 0 {
		b.WriteString(pal.body(noDataLine))
		b.WriteByte('\n')
	}
	for _, r := range rows {
		b.WriteString(pal.body(pal.rowContent(r, cols)))
		b.WriteByte('\n')
	}
	b.WriteString(pal.rule("╰", "╯"))
	b.WriteByte('\n')
	return b.String()
}

// groupKeyLabels maps the query language's group_by keys to their Chinese
// display names. The table is a LABEL table, not a whitelist: an unknown
// key passes through unchanged, so a new grouping key is never hidden from
// the user (and the SQL side never has to be taught about the UI). The
// same table feeds both the card title and the column headers, so the two
// can never disagree.
var groupKeyLabels = map[string]string{
	"model":    "模型",
	"provider": "供应商",
	"account":  "账号",
	"hour":     "小时",
	"day":      "日期",
	"stream":   "流式",
	"success":  "状态",
}

// groupKeyLabel is the display name of one group_by key: its Chinese label
// when the mapping knows it, the raw key otherwise.
func groupKeyLabel(g string) string {
	if label, ok := groupKeyLabels[g]; ok {
		return label
	}
	return g
}

// groupDesc names the grouping keys for the card title: "按模型分组"
// for a single key, every key (Chinese-labelled) enumerated with "/" for a
// multi-key view — "按模型/供应商分组" — and "未分组" when the query carries
// no group_by at all. The keys carry no half-width spaces, so the title
// reads as one Chinese phrase; the multi-key separator is "/" per the
// product copy decision (see .agents/notes/20260922-usage-quota-cn-text.md).
// The table's column gaps are a separate alignment concern and are
// deliberately left alone.
func groupDesc(groupBy []string) string {
	if len(groupBy) == 0 {
		return "未分组"
	}
	labels := make([]string, len(groupBy))
	for i, g := range groupBy {
		labels[i] = groupKeyLabel(g)
	}
	return "按" + strings.Join(labels, "/") + "分组"
}

// formatRequests renders a request count with the same compact k/M
// notation render.FormatTokens uses. The shared formatter is token-named,
// but its algorithm is a generic count formatter; this wrapper gives the
// request column a semantically honest name without duplicating the
// algorithm.
func formatRequests(n int64) string {
	return render.FormatTokens(n)
}

// cell renders one column value: capped (ellipsis-truncated) at the
// column's maxWidth when it has one, then padded to the column's fixed
// width according to its alignment. PadRight/PadLeft are ANSI-aware and
// truncate defensively, so a value can never push the row out of the card.
func (c reportColumn) cell(content string) string {
	if c.maxWidth > 0 {
		content = render.Truncate(content, c.maxWidth)
	}
	if c.align == render.AlignRight {
		return render.PadLeft(content, c.width)
	}
	return render.PadRight(content, c.width)
}

// value is the plain text of one non-capsule cell: the formatted group
// value, the compact request count or the compact cached-token count.
func (c reportColumn) value(r SummaryRow) string {
	switch c.kind {
	case kindRequests:
		return formatRequests(r.Requests)
	case kindCache:
		return render.FormatTokens(r.CachedTokens)
	default:
		return formatGroupValue(c.key, r.Groups[c.key])
	}
}

// rowContent builds one detail row's table content: every column at its
// fixed width, separated by colGap. The 命中率 column renders the mini
// capsule itself, so it bypasses the plain-text cell path.
func (pal reportPalette) rowContent(r SummaryRow, cols []reportColumn) string {
	cells := make([]string, len(cols))
	for i, c := range cols {
		if c.kind == kindHit {
			cells[i] = hitCell(r.CachedTokens, r.cacheHitInput(), pal)
			continue
		}
		cells[i] = c.cell(c.value(r))
	}
	return joinCells(cells)
}

// headerContent builds the header row's table content: plain-text column
// titles (no color, no bold) at the same fixed widths as the data rows.
// Every Chinese string in the card therefore renders as ordinary text, so
// the header reads at the same size and weight as the rows below it.
func (pal reportPalette) headerContent(cols []reportColumn) string {
	cells := make([]string, len(cols))
	for i, c := range cols {
		cells[i] = c.cell(c.title)
	}
	return joinCells(cells)
}

// joinCells joins already-padded column cells with the column gap.
func joinCells(cells []string) string {
	return strings.Join(cells, colGap)
}

// hitCell renders the 命中率 column: the hitCells-long mini capsule plus
// the right-aligned one-decimal percentage. The denominator is the group's
// source-aware cacheHitInput (OpenAI-form prompt plus Anthropic assembled
// input), so the ratio cannot exceed 100 %.
//
// The gradient direction is the INVERSE of the quota card's: the level fed
// to the shared ramp is 100 - CapsuleLevel(i), i.e. a cell's MISSING hit
// share. A high hit rate therefore fills green cells and a cold cache
// reads red — the same ramp, the same glyphs, only the mapping is flipped
// (see render.CapsuleRamp).
//
// A zero or missing denominator renders "-" and draws NO capsule: 0 filled
// cells would claim "0 % cached" and a full bar would claim "100 %", and
// both would be fabricated data.
func hitCell(cached, input int64, pal reportPalette) string {
	if input <= 0 {
		return strings.Repeat(" ", hitCells+hitGap) +
			render.PadLeft("-", pctWidth)
	}
	frac := float64(cached) / float64(input)
	if frac > 1 {
		frac = 1
	}
	if frac < 0 {
		frac = 0
	}
	used := render.CapsuleUsedCells(int(math.Round(frac*100)), hitCells)
	bar := render.CapsuleBar(hitCells, used, func(i int) int {
		return 100 - render.CapsuleLevel(i, hitCells)
	}, pal.color)
	return bar + strings.Repeat(" ", hitGap) +
		render.PadLeft(cacheHitRate(cached, input), pctWidth)
}

// cacheHitRate renders cached/input with one decimal ("66.7%"). input is
// the source-aware denominator from SummaryRow.cacheHitInput — OpenAI-form
// prompt_tokens (cached already included) or Anthropic assembled input
// (input + cache_read + cache_creation) — so the ratio cannot exceed 100%
// when the upstream reports cache_read outside input_tokens. A zero input
// total renders a stable "0.0%" — the guard avoids NaN/Inf from a
// division by zero (FormatPercent would render "-"). The CELL decides
// whether that case is worth a bar at all: with no denominator there is
// nothing to measure, so hitCell shows "-" instead.
func cacheHitRate(cached, input int64) string {
	if input == 0 {
		return "0.0%"
	}
	return render.FormatPercent(float64(cached), float64(input))
}

var modelDateSuffixRe = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)

// FormatModelName formats a model name for table display:
// 1. Strips the provider prefix (the substring after the last '/'; if empty, keeps original).
// 2. Strips trailing date suffixes (-YYYYMMDD, -YYYY-MM-DD) and trailing -latest.
// Other suffixes (such as -4.5, -flash, -preview, :free) are preserved.
func FormatModelName(name string) string {
	orig := name
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		short := name[idx+1:]
		if short != "" {
			name = short
		}
	}
	for {
		trimmed := modelDateSuffixRe.ReplaceAllString(name, "")
		trimmed = strings.TrimSuffix(trimmed, "-latest")
		if trimmed == name || trimmed == "" {
			break
		}
		name = trimmed
	}
	if name == "" {
		return orig
	}
	return name
}

// formatGroupValue renders one group key value for the table. Time buckets
// (hour/day) are unix seconds and are shown as local-time dates — the
// "01-02 15:00" / "01-02" forms are the data format, so they stay numeric;
// stream and success are 0/1 integers and are shown as 是/否 and 正常/失败;
// model names are formatted via FormatModelName; everything else is the
// stored string.
func formatGroupValue(g string, v any) string {
	if v == nil {
		return ""
	}
	switch g {
	case "model":
		if s, ok := v.(string); ok {
			return FormatModelName(s)
		}
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
				return "是"
			}
			return "否"
		}
	case "success":
		if n, ok := v.(int64); ok {
			if n == 1 {
				return "正常"
			}
			return "失败"
		}
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// ── card palette (one color decision per render) ────────────────────────

// reportPalette carries the color decision for one report render. Colored
// elements go through it (the dim borders and, via render.CapsuleBar, the
// hit-rate ramp), so Color=false strips ALL escapes while the layout —
// cell widths, padding, glyphs, capsule length — is decided before any
// wrapper runs. The palette therefore guarantees "colors off, layout
// unchanged": the no-color render is the colored render minus its escape
// sequences, byte for byte. Text is deliberately NOT part of this: the
// title, the header and every Chinese string render as plain text, so no
// card text carries color or bold.
type reportPalette struct{ color bool }

func (pal reportPalette) dim(s string) string {
	if pal.color {
		return render.Dim(s)
	}
	return s
}

// body wraps one card line: Dim("│ ") + content padded to reportInner +
// Dim(" │") = exactly reportWidth display columns. PadRight measures the
// de-colored text, so a colored capsule row still aligns; a pathological
// content is truncated with an ellipsis rather than pushing the border
// out.
func (pal reportPalette) body(content string) string {
	return pal.dim("│ ") + render.PadRight(content, reportInner) + pal.dim(" │")
}

// rule renders a full-width horizontal rule: the given corner glyphs plus
// the dash fill, exactly reportWidth columns.
func (pal reportPalette) rule(left, right string) string {
	return pal.dim(left + strings.Repeat("─", reportWidth-2) + right)
}

// titleLine builds the top border with the embedded title:
//
//	╭─ ␣desc␣Dim(───…╮)
//
// The title is the grouping description alone: there is no 「用量」 head and
// no 「·」 separator, because the surrounding card (the usage command that
// renders it) already says what the numbers are, and the prefix only added
// visual noise to the one line that already has a job. The description is
// rendered as plain text — no color, no bold — so every Chinese string in
// the card looks the same.
//
// The description (the grouping keys) is the only variable-length part: it
// is capped at the width that leaves at least one dash fill, and the fill
// is then derived from the width the capped text ACTUALLY occupies
// (DisplayWidth, never a column count) — a double-width rune that cannot
// fit the column the ellipsis needs stops the truncation one column short,
// and the extra dash is what keeps the line at reportWidth. The line
// itself is never truncated: that safety net used to eat the right border
// ╮ and could leave the card at 59 columns, so the fill — never the
// line — absorbs the difference.
func (pal reportPalette) titleLine(desc string) string {
	const prefixW, suffixW = 3, 1 // "╭─ " and "╮"
	// The line is "╭─ " + desc + " " + fill + "╮", so the description may
	// take at most reportWidth - prefixW - suffixW - 2 columns (50 here) and
	// still leave one space and one fill dash: total = 3 + descW + 1 + fill
	// + 1 = 56.
	descMax := reportWidth - prefixW - suffixW - 2
	desc = render.Truncate(desc, descMax)
	descW := render.DisplayWidth(desc)
	fill := reportWidth - prefixW - suffixW - descW - 1
	if fill < 1 {
		fill = 1
	}
	return pal.dim("╭─ ") + desc + pal.dim(" "+strings.Repeat("─", fill)+"╮")
}
