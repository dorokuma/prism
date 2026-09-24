package planusage

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/render"
)

const reportIndent = "  "

// ── legacy table (kept for ?format=table and internal callers) ──────────

// RenderTable is the ?format=table report. The CLI `prism quota` command
// renders the capsule cards via RenderCards, so this table is no longer
// the CLI's default output — it survives as the HTTP format=table
// representation and for internal callers. It is a compact single-line
// table with the same visual rules as the usage report: every
// output block is indented two spaces, columns are separated by one space,
// each window is one row, names render in full (never truncated) and the
// layout never depends on the terminal width.
func RenderTable(snaps []Snapshot) string {
	return RenderTableAt(snaps, time.Now())
}

// RenderTableAt is RenderTable with an injectable clock (tests). Each
// account is one MODULE: its windows are consecutive rows, the account
// name sits on the module's first window row, later rows leave the
// account cell empty. Modules are sorted by provider display order
// first (see providerDisplayOrder: gemini < clinepass < xai, unlisted
// providers after them), then by account name, so they never
// interleave (a bare row can not look like it belongs to the previous
// module). Load-balanced plans are never merged. Snapshots without
// windows keep the account line and the error line, if any. Error lines
// always carry the account attribution ("  account: fetch_failed").
func RenderTableAt(snaps []Snapshot, now time.Time) string {
	if len(snaps) == 0 {
		return "  没有套餐数据\n"
	}

	cols := []render.Column{
		{Title: "账号", Align: render.AlignLeft},
		{Title: "窗口", Align: render.AlignLeft},
		{Title: "状态", Align: render.AlignLeft},
		{Title: "占用", Align: render.AlignLeft},
		{Title: "重置", Align: render.AlignLeft},
		{Title: "限额估算", Align: render.AlignLeft},
	}

	// Stable sort by accountSortKey — provider display order first, then
	// the first account name — so modules stay contiguous and in a
	// readable order (Cache.List is map-ordered).
	sorted := append([]Snapshot(nil), snaps...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return accountSortKey(sorted[i]) < accountSortKey(sorted[j])
	})

	var rows [][]string
	var notes strings.Builder

	for _, s := range sorted {
		titles := s.Accounts
		if len(titles) == 0 {
			title := s.Provider
			if title == "" {
				title = "unknown"
			}
			titles = []string{title}
		}
		for _, title := range titles {
			account := accountCell(s, title)
			if len(s.Windows) == 0 {
				notes.WriteString(reportIndent + account + "\n")
				if s.Err != "" {
					notes.WriteString(reportIndent + account + ": " + s.Err + "\n")
				}
				continue
			}
			if s.Err != "" {
				notes.WriteString(reportIndent + account + ": " + s.Err + "\n")
			}
			for i, w := range s.Windows {
				cell := ""
				if i == 0 {
					cell = account
				}
				rows = append(rows, []string{
					cell,
					windowLabel(w.Name),
					statusLabel(w.Status),
					fmt.Sprintf("%d%%", w.Percent),
					formatRemain(now, w.ResetsAt),
					formatEstimate(w),
				})
			}
		}
	}

	var b strings.Builder
	if len(rows) > 0 {
		t := &render.Table{Columns: cols, Rows: rows, Indent: reportIndent, Gap: " "}
		b.WriteString(t.Render())
	}
	b.WriteString(notes.String())
	return b.String()
}

// ── card-style TUI dashboard ─────────────────────────────────────────────

// Card geometry, in display columns measured with ANSI stripped. One card
// per (account, window): the window label rides in the title, the capsule
// bar takes row 2, and row 3 carries used/total + reset. Every line of
// every card — title, bar, detail, note, footer, borders — is exactly
// cardWidth (60) columns wide:
//
//	╭─ Gemini acct-1 · 5小时限额 ───────────────────────╮
//	│ ▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱  34% │
//	│ 已用 34% / 总额 3.5M tok                resets in 3h 12m │
//	╰──────────────────────────────────────────────────╯
//
// Bar row:    "│ " + capsule(51) + gap(1) + pct(4) + " │"
// Detail row: "│ " + detail(38) + reset(18) + " │"
// Both add up to 2 + 56 + 2 = 60 columns. The body gutter is SYMMETRIC:
// one space either side of the content and nothing else — the bar and the
// detail rows carry no indent of their own, so left and right breathing
// room stay 1:1 at every line. The capsule replaced the old 18-cell mini
// bar: taking the whole inner width minus the percentage label is what
// makes it read as a capsule, and it is also why the label column is
// reserved first (see barCells).
const (
	cardWidth   = 60
	cardInner   = cardWidth - 4                             // width between "│ " and " │"
	barIndent   = 0                                         // no indent: the gutter is the border's single space
	pctWidth    = 4                                         // right-aligned " 34%" / "100%"
	barGap      = 1                                         // one space between capsule and pct
	barCells    = cardInner - barIndent - pctWidth - barGap // 51 capsule cells
	resetWidth  = 18                                        // right-aligned "resets in 3h 12m"
	detailWidth = cardInner - barIndent - resetWidth        // 38
)

// Capsule glyphs, the per-cell ramp and the equal-color run merging live in
// internal/render (CapUsed / CapEmpty / CapsuleRamp / CapsuleUsedCells /
// CapsuleLevel / CapsuleBar), so the quota cards and the usage report's
// hit-rate capsule share ONE implementation. The constants below are the
// quota cards' local names for the shared glyphs.
const (
	capUsed  = render.CapUsed
	capEmpty = render.CapEmpty
)

// CardOptions carries the optional switches of RenderCards.
type CardOptions struct {
	// NoColor strips every ANSI escape sequence from the card. The
	// layout is untouched: cell counts, padding and glyphs are decided
	// before any color wrapper runs, so a no-color render is byte
	// identical to the colored one minus the escapes.
	NoColor bool
}

// RenderCards renders each (account, window) as one fixed-width
// (56-column) capsule card; cards are sorted by provider display order
// (gemini < clinepass < xai; unlisted providers fall back to
// lexicographic) and then by account name, separated by one blank
// line. ANSI true-color escapes are emitted by
// default — pass CardOptions{NoColor: true} for pipes, redirects and
// --no-color, which strips the escapes and nothing else. (The
// pipe-friendly table is RenderTable's job.)
func RenderCards(snaps []Snapshot, now time.Time, opts ...CardOptions) string {
	if len(snaps) == 0 {
		return "  没有套餐数据\n"
	}
	pal := cardPalette{color: true}
	for _, o := range opts {
		if o.NoColor {
			pal.color = false
		}
	}

	sorted := append([]Snapshot(nil), snaps...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return accountSortKey(sorted[i]) < accountSortKey(sorted[j])
	})

	var cards []string
	for _, s := range sorted {
		for _, acc := range cardTitles(s) {
			if len(s.Windows) == 0 {
				cards = append(cards, renderInfoCard(s, acc, pal))
				continue
			}
			for _, w := range s.Windows {
				cards = append(cards, renderWindowCard(s, acc, w, now, pal))
			}
		}
	}
	return strings.Join(cards, "\n\n") + "\n"
}

// cardTitles is the per-card account identifier: the snapshot's account
// titles, or the provider when the upstream reported none.
func cardTitles(s Snapshot) []string {
	if len(s.Accounts) > 0 {
		return s.Accounts
	}
	if s.Provider == "" {
		return []string{"unknown"}
	}
	return []string{s.Provider}
}

// renderWindowCard renders one (snapshot, account, window) triple as one
// capsule card. The account identifier lives in the title only — never in
// the bar or detail rows — so long account names can never collide with
// the metrics.
func renderWindowCard(s Snapshot, accountTitle string, w Window, now time.Time, pal cardPalette) string {
	lines := []string{
		cardTitleLine(providerDisplayName(s.Provider), accountCell(s, accountTitle), windowTitle(w.Name), pal),
		cardBarRow(w, pal),
		cardDetailRow(w, now, pal),
	}
	if windowExhausted(w) {
		lines = append(lines, cardFooter(pal))
	}
	if s.Err != "" {
		lines = append(lines, cardNote("⚠ "+s.Err, pal))
	}
	return joinCard(lines, pal)
}

// renderInfoCard renders an account with no windows at all: the title
// (without a window label), the fetch error when there is one, and the
// bottom border. Such accounts stay visible instead of vanishing.
func renderInfoCard(s Snapshot, accountTitle string, pal cardPalette) string {
	lines := []string{cardTitleLine(providerDisplayName(s.Provider), accountCell(s, accountTitle), "", pal)}
	if s.Err != "" {
		lines = append(lines, cardNote("⚠ "+s.Err, pal))
	}
	return joinCard(lines, pal)
}

// cardTitleLine builds the top border with the embedded title:
//
//	╭─ ␣service␣account␣·␣window␣Dim(───…╮)
//
// One space separates each title element and the fill; the fill dashes
// sit on the RIGHT of the title only; the total line is exactly cardWidth
// display columns. An over-long title is shrunk IN THE VARIABLE SEGMENTS —
// the account first (a long account is the common case), then the window
// label (only an unknown upstream name is ever long: the known labels are
// short and fixed), and the service name only when there is nothing left
// to borrow room from. The whole line is never truncated: that old safety
// net ate the right border ╮ and could leave the card at 59 columns, so
// the fill — derived from the width the segments ACTUALLY occupy, since a
// double-width rune that cannot fit the column the ellipsis needs stops a
// truncation one column short — is what absorbs the difference.
//
// The title TEXT (service name, account and window label) is rendered as
// plain text — no color, no bold — so every string in the card looks the
// same, exactly like the usage report's card title; only the non-text
// elements (the ╭─ border, the dash fill) stay dim. The separators are
// ordinary spaces, so the segments read as one plain phrase.
func cardTitleLine(service, account, window string, pal cardPalette) string {
	const prefixW, suffixW, sep = 3, 1, " · "
	sepW := render.DisplayWidth(sep)
	// The line is "╭─ " + body + " " + fill + "╮" = 3 + body + 1 + fill
	// + 1, so the body may take at most 54 columns and still leave one
	// fill dash.
	bodyMax := cardWidth - prefixW - suffixW - 2 // 54
	svc, acc, win := service, account, window
	// De-duplicate the account segment: when the account name is exactly the
	// provider display name ("Gemini Gemini"), it adds no information, so drop
	// it and show the service name only once. Account names that differ from
	// the service keep the usual service + account pair.
	if acc != "" && acc == svc {
		acc = ""
	}
	// Shrink the variable segments until the body fits bodyMax. Every step
	// re-measures with DisplayWidth, so a segment whose truncation stopped
	// one column short is picked up by the next pass instead of pushing the
	// border out — and no step ever truncates the line itself.
shrink:
	for titleWidth(svc, acc, win, sepW) > bodyMax {
		switch {
		case acc != "":
			// The account gives first: leave the room the service and the
			// window label need, drop it when there is none left.
			if budget := bodyMax - render.DisplayWidth(svc) - 1 - titleWidth("", "", win, sepW); budget >= 1 {
				acc = render.Truncate(acc, budget)
			} else {
				acc = ""
			}
		case win != "" && bodyMax-titleWidth(svc, acc, "", sepW)-sepW >= 1:
			// Account gone and the window label itself is over-long: give it
			// the room the service and the account leave over.
			win = render.Truncate(win, bodyMax-titleWidth(svc, acc, "", sepW)-sepW)
		case svc != "":
			// Nothing left to borrow from (a long unknown provider key):
			// shrink the service name.
			if budget := bodyMax - titleWidth("", acc, win, sepW); budget >= 1 {
				svc = render.Truncate(svc, budget)
			} else {
				svc = ""
			}
		default:
			break shrink // unreachable: an all-empty body always fits
		}
	}

	var b strings.Builder
	b.WriteString(pal.dim("╭─ "))
	b.WriteString(svc)
	if acc != "" {
		b.WriteString(" ")
		b.WriteString(acc)
	}
	if win != "" {
		b.WriteString(sep)
		b.WriteString(win)
	}
	// Fill from the MEASURED body width: 3 + body + 1 + fill + 1 = 60.
	used := titleWidth(svc, acc, win, sepW)
	fill := cardWidth - prefixW - suffixW - used - 1
	if fill < 1 {
		fill = 1
	}
	b.WriteString(pal.dim(" " + strings.Repeat("─", fill) + "╮"))
	return b.String()
}

// titleWidth is the display width of the title body (service, account,
// window and their separators), the fill excluded.
func titleWidth(svc, acc, win string, sepW int) int {
	n := render.DisplayWidth(svc)
	if acc != "" {
		n += 1 + render.DisplayWidth(acc)
	}
	if win != "" {
		n += sepW + render.DisplayWidth(win)
	}
	return n
}

// cardBarRow renders the capsule row: the capsule, one space and the
// right-aligned percentage. The percentage column is padded to
// pctWidth (4) so " 34%" and "100%" share one label column, and the
// capsule is barCells long whatever the value is, so the row width never
// depends on the usage.
func cardBarRow(w Window, pal cardPalette) string {
	return pal.dim("│ ") +
		capsuleBar(w, pal) +
		strings.Repeat(" ", barGap) +
		render.PadLeft(pctLabel(w), pctWidth) +
		pal.dim(" │")
}

// cardDetailRow renders the detail row: used/total left-aligned (flush
// with the capsule) and the reset countdown right-aligned. Both columns
// have fixed widths and render.PadRight/PadLeft truncate with an ellipsis
// when a value outgrows its column, so the row can never push the right
// border out.
func cardDetailRow(w Window, now time.Time, pal cardPalette) string {
	return pal.dim("│ ") +
		render.PadRight(windowDetail(w), detailWidth) +
		render.PadLeft(resetText(w, now), resetWidth) +
		pal.dim(" │")
}

// cardBody renders one mostly-empty middle line: the content
// left-aligned at the card body and padded to cardInner.
func cardBody(text string, pal cardPalette) string {
	return pal.dim("│ ") + render.PadRight(text, cardInner) + pal.dim(" │")
}

// cardFooter renders the exhausted-window warning row: yellow (#F4A261)
// "! limit reached".
func cardFooter(pal cardPalette) string {
	return cardBody(pal.yellow("! limit reached"), pal)
}

// cardNote renders a note row (fetch errors etc).
func cardNote(text string, pal cardPalette) string {
	return cardBody(text, pal)
}

// joinCard appends the bottom border to the card lines.
func joinCard(lines []string, pal cardPalette) string {
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	sb.WriteString(pal.dim("╰" + strings.Repeat("─", cardWidth-2) + "╯"))
	return sb.String()
}

// ── window label & detail helpers ─────────────────────────────────────

// providerDisplayName maps known provider keys to human names; unknown
// providers are returned unchanged (spec: "unknown provider 原样透传").
func providerDisplayName(p string) string {
	switch p {
	case "chatgpt", "openai":
		return "ChatGPT"
	case "claude", "anthropic":
		return "Claude"
	case "cursor":
		return "Cursor"
	case "opencode-go":
		return "Opus"
	case "gemini", "google":
		return "Gemini"
	case "clinepass":
		return "ClinePass"
	case "xai", "grok":
		return "SuperGrok"
	default:
		return p
	}
}

// col2Label names what the detail row counts. Percent is the CONSUMED
// (占用) share in prism — not the remainder — so the default label is 已用
// (never "remains/剩余": that was a semantic error). The distinct states
// keep their own Chinese labels.
func col2Label(w Window) string {
	switch {
	case w.Status == "used up":
		return "已耗尽"
	case w.Status == "rate-limited":
		return "限流"
	case w.LimitUSDEstimate > 0 && w.USDStatus == "estimated":
		return "额度"
	default:
		return "已用"
	}
}

// windowDetail is the detail row's left-aligned text: what the window
// counts (已用/已耗尽/限流/额度) plus the consumed share, and — when the
// upstream reported one — the total it is a share OF.
//
// The model carries no absolute used amount (Window has a share and a
// total, never a consumed token or dollar count), so the numerator stays
// the percentage rather than an invented "1.2M":
//
//	已用 34% / 总额 3.5M tok     (weekly token pool inferred)
//	额度 12% / $60.00           (dollar estimate)
//	已用 7%                      (no estimate concept at all)
func windowDetail(w Window) string {
	s := col2Label(w) + " " + pctLabel(w)
	if w.LimitUSDEstimate > 0 && w.USDStatus == "estimated" {
		// The dollar limit IS the 额度, so it needs no extra label.
		return s + " / " + fmt.Sprintf("$%d.00", w.LimitUSDEstimate)
	}
	return s + totalPart(w)
}

// totalPart is the " / 总额 …" denominator: the inferred weekly token
// pool when there is one, otherwise nothing (the window has no estimate
// concept, e.g. the Gemini 5h window).
func totalPart(w Window) string {
	if w.LimitTokensEstimate > 0 {
		return " / 总额 " + render.FormatTokens(w.LimitTokensEstimate) + " tok"
	}
	return ""
}

// resetText is the detail row's right-aligned text: "resets in 3h 12m"
// when the upstream reported ResetsAt, "resets now" once the window has
// rolled over, and "-" when there is no reset time at all.
func resetText(w Window, now time.Time) string {
	if w.ResetsAt == nil || w.ResetsAt.IsZero() {
		return "-"
	}
	if !w.ResetsAt.After(now) {
		return "resets now"
	}
	return "resets in " + cardCountdown(now, *w.ResetsAt)
}

// cardCountdown formats the remaining time for the detail row's reset
// column. Minutes are zero-padded and the components are separated by one
// space (2h 01m, 4h 51m, 3d 4h) so the column reads "resets in 3h 12m";
// the day/hour cases mirror formatRemain. formatRemain itself is
// untouched: the legacy table's output must not change.
func cardCountdown(now, at time.Time) string {
	d := at.Sub(now)
	if d <= 0 {
		return "已到"
	}
	if d < time.Minute {
		return "<1m"
	}
	mins := int(d.Minutes())
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	hours := mins / 60
	mins %= 60
	if hours < 24 {
		if mins == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %02dm", hours, mins)
	}
	days := hours / 24
	hours %= 24
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, hours)
}

// windowTitle is the capsule card's window label. The spec wording
// ("5小时限额") replaces the legacy table's 短期/中期/长期 — the window
// name is now part of the card title, so it says what the window IS.
// Unknown names pass through unchanged, an empty name renders "--".
func windowTitle(name string) string {
	switch name {
	case "rolling", "5h":
		return "5小时限额"
	case "weekly":
		return "周限额"
	case "monthly":
		return "月限额"
	case "":
		return "--"
	default:
		return name
	}
}

// pctLabel is the bar row's percentage: the consumed share, right-aligned
// into pctWidth (" 34%" / "100%").
func pctLabel(w Window) string {
	return strconv.Itoa(displayPercent(w)) + "%"
}

// displayPercent is the consumed share (0..100) shared by the capsule and
// the percentage label. UsedFraction refines Percent's precision (it
// exists so sub-percent weeks are not floored away).
func displayPercent(w Window) int {
	pct := w.Percent
	if w.UsedFraction > 0 {
		pct = int(math.Ceil(w.UsedFraction * 100))
	}
	if pct > 100 {
		pct = 100
	}
	if pct < 0 {
		pct = 0
	}
	return pct
}

// capsuleUsedCells is the number of solid ▰ cells for a consumed share
// pct (0..100): ceil(pct/100 * barCells), clamped into [0, barCells]. The
// FILLED LENGTH is the used share (占用), so the value is readable from
// the capsule's length alone — 0 % draws no ▰ at all, 99 % nearly all of
// them. (Drawing the remaining quota as a full-width track was rejected
// in review: 0 % and 98 % then came out the same length.) The arithmetic
// itself is render.CapsuleUsedCells, shared with the usage report.
func capsuleUsedCells(pct int) int {
	return render.CapsuleUsedCells(pct, barCells)
}

// capsuleLevel is the consumed share (0..100) that capsule cell i of a
// full bar stands for: the leading cell is ~2 % (100/51), the last is
// 100 %. The capsule is colored per cell by that level, so the fill
// warms up as it grows instead of switching tiers in one step. The
// arithmetic is render.CapsuleLevel, shared with the usage report (which
// passes the inverted level — see render.CapsuleRamp).
func capsuleLevel(i int) int {
	return render.CapsuleLevel(i, barCells)
}

// capsuleBar renders the barCells-long capsule for one window.
//   - exhausted window (used up / rate-limited / ≥100 %): the whole
//     capsule is a solid red ▰ row. A hollow ▱ row was rejected: hollow
//     reads as "nothing used", which is the opposite of an exhausted
//     window — a drained pill is shown solid red, and the
//     "! limit reached" footer spells the state out.
//   - otherwise the used share is solid ▰, colored per cell along the
//     green→yellow→red ramp, and the remaining share is hollow ▱ in dim
//     gray (#666666).
//
// The glyphs, the per-cell ramp and the equal-color run merging are
// render.CapsuleBar — the same primitive the usage report reuses with the
// level inverted (a higher cache hit rate is greener there).
func capsuleBar(w Window, pal cardPalette) string {
	if windowExhausted(w) {
		return pal.red(strings.Repeat(capUsed, barCells))
	}
	return render.CapsuleBar(barCells, capsuleUsedCells(displayPercent(w)), func(i int) int {
		return capsuleLevel(i)
	}, pal.color)
}

// windowExhausted reports whether one window counts as exhausted: at or
// above 100 %, used up, or rate-limited. The same determination drives
// the solid red capsule and the "! limit reached" footer.
func windowExhausted(w Window) bool {
	return w.Percent >= 100 || w.Status == "used up" || w.Status == "rate-limited"
}

// ── card palette (one color decision per render) ────────────────────────

// cardPalette carries the color decision for one card render. Every
// colored element goes through it, so NoColor strips ALL escapes while the
// layout — cell counts, padding, glyphs — is decided before any wrapper
// runs. The palette therefore guarantees "colors off, layout unchanged":
// the no-color render is the colored render minus its escape sequences,
// byte for byte. Text is deliberately NOT part of this: the title (service
// name, account, window label) and every Chinese string render as plain
// text, so no card text carries color or bold.
type cardPalette struct{ color bool }

func (pal cardPalette) dim(s string) string {
	if pal.color {
		return render.Dim(s)
	}
	return s
}

func (pal cardPalette) red(s string) string {
	if pal.color {
		return render.Red(s)
	}
	return s
}

func (pal cardPalette) yellow(s string) string {
	if pal.color {
		return render.Yellow(s)
	}
	return s
}

// run is gone: render.CapsuleBar now owns the per-cell ramp, the
// equal-color run merging and the color decision, shared with the usage
// report. The palette has no capsule entry point left.

// ── legacy helpers (kept) ─────────────────────────────────────────────────

// accountSortKey orders snapshots by provider display order first (see
// providerDisplayOrder: gemini < clinepass < xai), then by their first
// account name (provider as fallback), so modules never interleave in
// the table or the cards.
func accountSortKey(s Snapshot) string {
	name := s.Provider
	if len(s.Accounts) > 0 {
		name = s.Accounts[0]
	}
	rank, _ := providerDisplayRank(s.Provider)
	return fmt.Sprintf("%03d%s", rank, name)
}

// accountCell is the first-column value for one account. The provider
// prefix is deliberately NOT shown: account names are globally unique in
// prism, and each module's first window row carries its account so a
// module's other rows (account cell empty) can not be mistaken for a
// neighbour's windows. A stale snapshot keeps the 旧 marker.
func accountCell(s Snapshot, title string) string {
	account := title
	if s.Stale {
		account += " 旧"
	}
	return account
}

func windowLabel(name string) string {
	switch name {
	case "rolling", "5h":
		return "短期"
	case "weekly":
		return "中期"
	case "monthly":
		return "长期"
	default:
		if name == "" {
			return "--"
		}
		return name
	}
}

// statusLabel keeps the established 限流 marker for rate-limited windows
// and passes every other upstream status through unchanged.
func statusLabel(status string) string {
	switch status {
	case "rate-limited":
		return "限流"
	case "":
		return "-"
	default:
		return status
	}
}

// formatEstimate prefers the token-pool inference, then the old OpenCode
// dollar window estimate. A weekly window with a wired estimate mechanism
// but no data yet renders -- (placeholder); plain "-" means the window
// has no estimate concept at all (e.g. the 5h Gemini window).
func formatEstimate(w Window) string {
	if w.LimitTokensEstimate > 0 {
		return render.FormatTokens(w.LimitTokensEstimate)
	}
	if w.LimitUSDEstimate > 0 && w.USDStatus == "estimated" {
		return fmt.Sprintf("$%d.00", w.LimitUSDEstimate)
	}
	if w.Name == "weekly" {
		return "--"
	}
	return "-"
}

func formatRemain(now time.Time, at *time.Time) string {
	if at == nil || at.IsZero() {
		return "-"
	}
	d := at.Sub(now)
	if d <= 0 {
		return "已到"
	}
	if d < time.Minute {
		return "<1m"
	}
	mins := int(d.Minutes())
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	hours := mins / 60
	mins %= 60
	if hours < 24 {
		if mins == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	days := hours / 24
	hours %= 24
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}
