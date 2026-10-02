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
// per (account, window) for every provider EXCEPT ClinePass (whose cards
// are merged: one card per profile + window, one row per account — see the
// merged-card section below): the window label rides in the title, the
// capsule bar takes row 2, and row 3 carries used/total + reset. Every
// line of every card — title, bar, detail, note, footer, borders — is
// exactly cardWidth (56) columns wide:
//
//	╭─ Gemini acct-1 · 5小时限额 ──────────────────────────╮
//	│ ▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱  34% │
//	│ 已用 34% / 总额 3.5M 词元              3h 12m 后重置 │
//	╰──────────────────────────────────────────────────────╯
//
// Bar row:    "│ " + capsule(47) + gap(1) + pct(4) + " │"
// Detail row: "│ " + detail(34) + reset(18) + " │"
// Both add up to 2 + 52 + 2 = 56 columns. The body gutter is SYMMETRIC:
// one space either side of the content and nothing else — the bar and the
// detail rows carry no indent of their own, so left and right breathing
// room stay 1:1 at every line. The capsule replaced the old 18-cell mini
// bar: taking the whole inner width minus the percentage label is what
// makes it read as a capsule, and it is also why the label column is
// reserved first (see barCells).
const (
	cardWidth   = 56
	cardInner   = cardWidth - 4                             // width between "│ " and " │"
	barIndent   = 0                                         // no indent: the gutter is the border's single space
	pctWidth    = 4                                         // right-aligned " 34%" / "100%"
	barGap      = 1                                         // one space between capsule and pct
	barCells    = cardInner - barIndent - pctWidth - barGap // 47 capsule cells
	resetWidth  = 18                                        // right-aligned "3h 12m 后重置"
	detailWidth = cardInner - barIndent - resetWidth        // 34
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

// RenderCards renders the quota cards; cards are sorted by provider
// display order (gemini < clinepass < xai; unlisted providers fall back
// to lexicographic) and then by account name, separated by one blank
// line. ANSI true-color escapes are emitted by default — pass
// CardOptions{NoColor: true} for pipes, redirects and --no-color, which
// strips the escapes and nothing else. (The pipe-friendly table is
// RenderTable's job.)
//
// Two layouts share this entry point:
//
//   - every provider but ClinePass keeps the one-card-per-(account,
//     window) capsule layout: title = service + account + window, row 2 =
//     the capsule, row 3 = detail text + used/total + reset countdown, and
//     a 已达限额 footer when the window is exhausted. Nothing below
//     touches it.
//   - ClinePass is merged (see clineBar/README in the merged-card section):
//     one card per (profile, window) with one ROW per account. ClinePass
//     accounts are polled one key each, so their snapshots are collected
//     into groups BEFORE any card is emitted — a single snapshot never
//     carries two ClinePass accounts.
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

	groups := clinePassGroups(sorted)
	emitted := make(map[clineKey]bool)

	var cards []string
	for _, s := range sorted {
		if isClinePass(s.Provider) {
			// The merged card is emitted at the position of the first
			// snapshot that contributes to it, so Gemini and SuperGrok keep
			// their relative order around the ClinePass block.
			for _, key := range clinePassKeys(s) {
				if emitted[key] {
					continue
				}
				emitted[key] = true
				cards = append(cards, renderClineCard(groups[key], pal))
			}
			continue
		}
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
		lines = append(lines, cardNote(cardErrorNote(s.Err), pal))
	}
	return joinCard(lines, pal)
}

// renderInfoCard renders an account with no windows at all: the title
// (without a window label), the fetch error when there is one, and the
// bottom border. Such accounts stay visible instead of vanishing.
func renderInfoCard(s Snapshot, accountTitle string, pal cardPalette) string {
	lines := []string{cardTitleLine(providerDisplayName(s.Provider), accountCell(s, accountTitle), "", pal)}
	if s.Err != "" {
		lines = append(lines, cardNote(cardErrorNote(s.Err), pal))
	}
	return joinCard(lines, pal)
}

// ── ClinePass merged cards: one card per (profile, window) ──────────────
//
// ClinePass is the one provider whose cards are MERGED. Every ClinePass
// account belongs to the same plan, and the accounts are polled one key
// each, so the useful grouping is the plan (套餐/profile) plus the window —
// not the (account, window) pair the other providers keep. One card per
// (profile, window) holds one ROW per account:
//
//	╭─ ClinePass · 5小时限额 ─────────────────────────────────╮
//	│ · cline-user ▰▰▰▰▰▰▰▰▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱  34%    3.4M/10.0M │
//	│ · cline-user ▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰▰ 100%     4.2M/4.2M │
//	╰─────────────────────────────────────────────────────────╯
//
// Row format, left to right (every element separated by one space):
//
//	color dot(1) account name(n) capsule(23) pct(4) number(13)
//
// so a row is clineRowFixed+n display columns and the CARD is 4 columns
// wider than its widest row. n is the LONGEST account DISPLAY name in the
// card (see clineDisplayName): the displayed name is never truncated, a
// long name widens the whole card instead, and every row of one card
// shares that width, so the capsules, the percentages and the number
// fields stay in one column each. The account name and its dot carry the
// same accountColor(fp) (see accountNameText/accountDot), so rows of one
// card are told apart by color AND by name even when two accounts share a
// name.
//
// The name a row DISPLAYS drops its trailing pure-digit suffix (Cline1 →
// Cline, stripNumericSuffix via clineDisplayName). The row's IDENTITY is
// untouched by that: clineRowID keys on the FULL name plus the fingerprint,
// so Cline1 and Cline2 stay TWO rows (with or without a fingerprint) and
// are told apart by the dot and the name color instead.
//
// A merged row carries ONLY those elements. It has no detail text line
// (no 已用 x% / 总额 …, that wording belongs to the other providers), no
// reset countdown, no 已达限额 footer, no status suffix and nothing
// appended to the account name (its trailing digit suffix is DROPPED, see
// clineDisplayName): the ONE exhausted signal is the capsule
// going solid red plus the X/X number field (see clineNumberField).
const (
	clineCapCells    = 23 // capsule columns
	clinePctWidth    = 4  // right-aligned pct: "100%" / " 34%"
	clineNumberWidth = 13 // right-aligned used/total
	// clineRowFixed is a merged row's display columns WITHOUT the account
	// name: dot(1) + 4 one-column gaps + capsule + pct + number.
	clineRowFixed = 1 + 4 + clineCapCells + clinePctWidth + clineNumberWidth // 45
)

// clineKey identifies one merged card: the profile (normalized provider
// key) plus the window NAME. Snapshots of one profile describe the same
// plan, so their windows merge; different providers or windows never do.
// A windowless snapshot (a failed fetch) keys on the empty window, so the
// failing accounts of one plan still share one card.
type clineKey struct{ provider, window string }

// clineRow is one account's row inside a merged card. win is nil for a
// windowless snapshot: the row then shows the account (dot + name) and
// leaves the metric columns blank.
type clineRow struct {
	name string
	fp   string
	win  *Window
}

// clineGroup is one merged card under construction.
type clineGroup struct {
	profile string // provider display name, e.g. ClinePass
	window  string // window display title; "" for a windowless snapshot
	rows    []clineRow
	notes   []string
	seen    map[string]bool
}

// isClinePass reports whether a provider key is the ClinePass family. It
// mirrors ClinePassFetcher.Match's provider test (case-insensitive,
// whitespace-trimmed) so the cards and the fetcher agree on what
// "clinepass" is.
func isClinePass(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), clinepassProviderName)
}

// clinePassKeys lists the merged-card keys one snapshot contributes to, in
// card order: one per window, or the single empty (windowless) key when
// the snapshot carries no window at all.
func clinePassKeys(s Snapshot) []clineKey {
	provider := strings.ToLower(strings.TrimSpace(s.Provider))
	if len(s.Windows) == 0 {
		return []clineKey{{provider: provider}}
	}
	keys := make([]clineKey, 0, len(s.Windows))
	for _, w := range s.Windows {
		keys = append(keys, clineKey{provider: provider, window: w.Name})
	}
	return keys
}

// clinePassGroups collects the merged cards of a snapshot list. The
// snapshots must already be sorted (accountSortKey); rows are appended in
// that order, so accounts keep the ordering the rest of the report uses.
//
// Row IDENTITY is (window, name, fingerprint): the fingerprint is what
// makes TWO DIFFERENT accounts that share a name two rows (real case: two
// metapi ClinePass rows with the same username but different api_tokens),
// and it is also what keeps one account from being listed twice when it
// shows up in more than one snapshot of the group (accounts that share a
// key are ONE snapshot with several names, and a failed/retried round can
// repeat a roster). Without a fingerprint there is no way to tell "the
// same account twice" from "two accounts, same name" apart, and the
// same-name case is the one that must stay visible: such a row is keyed by
// its position (snapshot ordinal, account index) instead.
func clinePassGroups(sorted []Snapshot) map[clineKey]*clineGroup {
	groups := map[clineKey]*clineGroup{}
	for si, s := range sorted {
		if !isClinePass(s.Provider) {
			continue
		}
		names := s.Accounts
		if len(names) == 0 {
			names = []string{providerDisplayName(s.Provider)}
		}
		fps := s.AccountFPs()
		for _, key := range clinePassKeys(s) {
			g := groups[key]
			if g == nil {
				g = &clineGroup{
					profile: providerDisplayName(s.Provider),
					window:  windowTitle(key.window),
					seen:    map[string]bool{},
				}
				groups[key] = g
			}
			var win *Window
			if key.window != "" {
				for i := range s.Windows {
					if s.Windows[i].Name == key.window {
						win = &s.Windows[i]
						break
					}
				}
			}
			for ai, name := range names {
				fp := ""
				if ai < len(fps) {
					fp = fps[ai]
				}
				id := clineRowID(name, fp, si, ai)
				if g.seen[id] {
					continue
				}
				g.seen[id] = true
				g.rows = append(g.rows, clineRow{name: name, fp: fp, win: win})
			}
			if s.Err != "" {
				if note := clineNote(names[0], s.Err); !containsString(g.notes, note) {
					g.notes = append(g.notes, note)
				}
			}
		}
	}
	return groups
}

// clineRowID is the dedupe key of one merged row (see clinePassGroups).
func clineRowID(name, fp string, snapshot, account int) string {
	if fp != "" {
		return "fp\x00" + name + "\x00" + fp
	}
	return "at\x00" + name + "\x00" + strconv.Itoa(snapshot) + ":" + strconv.Itoa(account)
}

// clineNote attributes a fetch failure on a merged card: the account the
// failure came from plus the localized code. The account name is what the
// title no longer carries, so the note is where it has to appear.
func clineNote(name, code string) string {
	if name == "" {
		return cardErrorNote(code)
	}
	return "⚠ " + name + ": " + localizedCode(code)
}

// containsString reports whether list already holds s. The merged card
// keeps its failure notes unique, so a repeated snapshot cannot duplicate
// a note line.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// clineCardWidth is the merged card's width in display columns: the row's
// fixed columns plus the LONGEST account DISPLAY name in the card (the name
// the row actually shows, clineDisplayName), so a long name widens the card
// instead of being truncated. The width comes from the DISPLAY name, not
// the identity's full name, so a stripped digit suffix never reserves an
// invisible blank column. The title's own needs are a floor, so a short
// account name can never squeeze the plan + window name into an ellipsis.
func clineCardWidth(g *clineGroup) int {
	name := 0
	for _, r := range g.rows {
		if w := render.DisplayWidth(clineDisplayName(r)); w > name {
			name = w
		}
	}
	width := 4 + clineRowFixed + name
	if need := titleWidth(g.profile, "", g.window, render.DisplayWidth(" · ")) + 6; need > width {
		width = need
	}
	return width
}

// renderClineCard renders one merged (profile, window) card: the title
// (plan display name + window name, never an account), one row per
// account, the failure notes, and the bottom border.
func renderClineCard(g *clineGroup, pal cardPalette) string {
	width := clineCardWidth(g)
	lines := []string{cardTitleLineAt(width, g.profile, "", g.window, pal)}
	for _, r := range g.rows {
		lines = append(lines, clineRowLine(r, width, pal))
	}
	for _, note := range g.notes {
		lines = append(lines, cardBodyAt(width, note, pal))
	}
	return joinCardAt(lines, width, pal)
}

// clineDisplayName is the account name a merged row SHOWS: the trailing
// pure-digit suffix is dropped (Cline1 → Cline, Cline2 → Cline), so two
// accounts whose names differ only by that suffix read as one name. It is a
// DISPLAY-ONLY transform — the row's identity stays clineRowID(name, fp,
// …), which keys on the FULL name plus the fingerprint, so Cline1 and
// Cline2 remain TWO rows (also when no fingerprint is available, where the
// key falls back to the row's position). Same-name accounts are told apart
// by the dot and the name color (accountColor(fp)), not by the text.
func clineDisplayName(r clineRow) string {
	return stripNumericSuffix(r.name)
}

// clineRowLine renders one merged row. The account name is padded to the
// card's name column, so the capsule, the percentage and the number field
// of every row start at the same column. The dot and the name share one
// color (accountColor of the account's fingerprint); with no usable
// fingerprint the dot degrades to the plain · and the name stays plain.
// The row shows the account's DISPLAY name (clineDisplayName, trailing
// digits dropped); only the row identity (clineRowID) keeps the full name.
func clineRowLine(r clineRow, width int, pal cardPalette) string {
	name := clineDisplayName(r)
	pad := width - 4 - clineRowFixed - render.DisplayWidth(name)
	if pad < 0 {
		pad = 0
	}
	capsule := strings.Repeat(" ", clineCapCells)
	pct := strings.Repeat(" ", clinePctWidth)
	number := strings.Repeat(" ", clineNumberWidth)
	if r.win != nil {
		capsule = clineBar(*r.win, pal)
		pct = render.PadLeft(pctLabel(*r.win), clinePctWidth)
		number = render.PadLeft(clineNumberField(*r.win), clineNumberWidth)
	}
	return pal.dim("│ ") +
		accountDot(r.fp, pal) + " " +
		accountNameText(name, r.fp, pal) + strings.Repeat(" ", pad+1) +
		capsule + " " + pct + " " + number +
		pal.dim(" │")
}

// clineBar is a merged row's capsule: the used share solid ▰, the rest
// hollow ▱, colored per cell along the shared severity ramp — and the
// WHOLE capsule solid red once the window is exhausted. "Went red" is the
// only exhausted signal a merged row carries (there is no footer and no
// countdown any more), so the drained pill is drawn solid, exactly like
// the legacy exhausted capsule.
func clineBar(w Window, pal cardPalette) string {
	if windowExhausted(w) {
		return pal.red(strings.Repeat(capUsed, clineCapCells))
	}
	pct := displayPercent(w)
	return render.CapsuleBar(clineCapCells, render.CapsuleUsedCells(pct, clineCapCells), nil, pal.color)
}

// clineNumberField is a merged row's number field: used/total, where the
// total is the window's MEASURED total (LimitTokensEstimate, or
// MeasuredTokens when the pool is unknown) and used is the window's share
// of it — the same 已用 percentage the pct column shows.
//
// A 100 % (exhausted) row is forced to the FULL pair, so a drained row
// always reads X/X — never "-" and never a fraction of a measured total.
// "-" survives for the one case the data cannot answer: no estimate AND
// no measured consumption at all (metapi unreadable).
func clineNumberField(w Window) string {
	total := w.LimitTokensEstimate
	if total <= 0 {
		total = w.MeasuredTokens
	}
	if total <= 0 {
		return "-"
	}
	if windowExhausted(w) {
		return formatTokenPair(total, total)
	}
	used := int64(float64(total) * float64(displayPercent(w)) / 100)
	return formatTokenPair(used, total)
}

// accountNameText renders an account name in the same palette color as its
// dot. It shares accountColor with accountDot, so the two can never
// disagree; without a usable fingerprint (empty or shorter than 8 hex
// characters) both degrade to the plain · and plain text.
func accountNameText(name, fp string, pal cardPalette) string {
	if !pal.color {
		return name
	}
	color := accountColor(fp)
	if color == "" {
		return name
	}
	return render.FgFromHex(color, name)
}

// cardTitleLine builds the top border with the embedded title at the
// legacy card width. The merged ClinePass cards call cardTitleLineAt with
// their own (name-driven) width; everything else goes through here.
func cardTitleLine(service, account, window string, pal cardPalette) string {
	return cardTitleLineAt(cardWidth, service, account, window, pal)
}

// cardTitleLineAt is cardTitleLine for an explicit card width:
//
//	╭─ ␣service␣account␣·␣window␣Dim(───…╮)
//
// One space separates each title element and the fill; the fill dashes
// sit on the RIGHT of the title only; the total line is exactly width
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
func cardTitleLineAt(width int, service, account, window string, pal cardPalette) string {
	const prefixW, suffixW, sep = 3, 1, " · "
	sepW := render.DisplayWidth(sep)
	// The line is "╭─ " + body + " " + fill + "╮" = 3 + body + 1 + fill
	// + 1, so the body may take at most width-6 columns and still leave
	// one fill dash (50 at the legacy width).
	bodyMax := width - prefixW - suffixW - 2
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
	// Fill from the MEASURED body width: 3 + body + 1 + fill + 1 = width.
	used := titleWidth(svc, acc, win, sepW)
	fill := width - prefixW - suffixW - used - 1
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
	return cardBodyAt(cardWidth, text, pal)
}

// cardBodyAt is cardBody for an explicit card width.
func cardBodyAt(width int, text string, pal cardPalette) string {
	return pal.dim("│ ") + render.PadRight(text, width-4) + pal.dim(" │")
}

// cardFooter renders the exhausted-window warning row: the yellow
// (#F4A261) 已达限额. Only the text, the color and the position on the
// card belong to this row — its structure is cardBody's, like every
// other middle row.
func cardFooter(pal cardPalette) string {
	return cardBody(pal.yellow("已达限额"), pal)
}

// cardNote renders a note row (fetch errors etc).
func cardNote(text string, pal cardPalette) string {
	return cardBody(text, pal)
}

// cardErrorNote is the card's note for a fetch failure: the ⚠ prefix plus
// the LOCALIZED error code. Snapshot.Err itself keeps the English code —
// ErrorCode sets it, logs and the HTTP JSON carry it, and
// Cache.StoreFailed decides window retention on it — so the translation
// lives in this render layer only. The mapped set is exactly what a card
// can carry (unauthorized, no_subscription, unexpected_status,
// fetch_failed); a code the table does not know passes through unchanged,
// still behind the ⚠ prefix.
func cardErrorNote(code string) string {
	return "⚠ " + localizedCode(code)
}

// localizedCode is the Chinese wording of one fetch-failure code; an
// unmapped code passes through unchanged.
func localizedCode(code string) string {
	switch code {
	case "unauthorized":
		return "未授权"
	case "no_subscription":
		return "无订阅"
	case "unexpected_status":
		return "上游状态异常"
	case "fetch_failed":
		return "拉取失败"
	}
	return code
}

// joinCard appends the bottom border to the card lines.
func joinCard(lines []string, pal cardPalette) string {
	return joinCardAt(lines, cardWidth, pal)
}

// joinCardAt is joinCard for an explicit card width.
func joinCardAt(lines []string, width int, pal cardPalette) string {
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	sb.WriteString(pal.dim("╰" + strings.Repeat("─", width-2) + "╯"))
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
//	已用 34% / 总额 3.5M 词元     (weekly token pool inferred)
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
		return " / 总额 " + render.FormatTokens(w.LimitTokensEstimate) + " 词元"
	}
	return ""
}

// resetText is the detail row's right-aligned text: "3h 12m 后重置"
// when the upstream reported ResetsAt, "已重置" once the window has
// rolled over, and "-" when there is no reset time at all. The countdown
// itself comes from cardCountdown, so the resetWidth column keeps its
// width at every value.
func resetText(w Window, now time.Time) string {
	if w.ResetsAt == nil || w.ResetsAt.IsZero() {
		return "-"
	}
	if !w.ResetsAt.After(now) {
		return "已重置"
	}
	return cardCountdown(now, *w.ResetsAt) + " 后重置"
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
// full bar stands for: the leading cell is ~2 % (100/47), the last is
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
//     已达限额 footer spells the state out.
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
// the solid red capsule and the 已达限额 footer.
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

// ── multi-account helpers (new) ────────────────────────────────────────

// stripNumericSuffix removes the trailing run of digits from an account name,
// leaving everything before it. "Cline1" → "Cline", "account123" → "account",
// "Cline-1" → "Cline-" (a hyphen is not a digit and stays).
//
// A name that is ENTIRELY digits ("12345") keeps its name: stripping down to
// the empty string would leave the row with nothing but the colour dot, which
// is worse than showing the account's actual name — the row identity
// (clineRowID) and the colour already tell such accounts apart.
//
// It is wired into the merged card through clineDisplayName: a merged row
// SHOWS the stripped name (Cline1/Cline2 both read "Cline"), while the row
// IDENTITY (clineRowID) keeps the full name plus the fingerprint — so a
// suffixed pair is still exactly two rows, and two same-name accounts are
// told apart by their dot and name color (accountColor(fp)). No other
// renderer calls it.
func stripNumericSuffix(s string) string {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == 0 {
		return s
	}
	return s[:i]
}

// accountColor picks a high-contrast bright color for the account from
// a fixed palette, indexed by the key fingerprint modulo palette length.
// The color is stable across renders for the same key.
func accountColor(fp string) string {
	if fp == "" || len(fp) < 8 {
		return ""
	}
	palette := []string{
		"00FFFF", // 青
		"FF00FF", // 洋红
		"FFFF00", // 黄
		"00FF00", // 绿
		"FF8800", // 橙
		"8800FF", // 紫
	}
	var h uint64
	for i := 0; i < 8; i++ {
		h = h*16 + uint64(hexValue(string(fp[i])))
	}
	return palette[h%uint64(len(palette))]
}

// accountDot returns a 1-display-column color dot for the account. The
// color is derived from a fixed bright palette indexed by the key
// fingerprint. In no-color mode the bare middle dot is returned.
func accountDot(fp string, pal cardPalette) string {
	if fp == "" || !pal.color {
		return "·"
	}
	color := accountColor(fp)
	if color == "" {
		return "·"
	}
	return render.FgFromHex(color, "·")
}

func hexValue(s string) int {
	v, _ := strconv.ParseUint(s, 16, 8)
	return int(v)
}

// formatTokenPair formats a used/total token pair with one decimal per
// side: "3.4M/10.0M". Joined without spaces, because the pair shares ONE
// right-aligned column (the merged ClinePass card's number field).
func formatTokenPair(used, total int64) string {
	u := render.FormatTokensOneDecimal(used)
	t := render.FormatTokensOneDecimal(total)
	return u + "/" + t
}

// ── card title & body helpers ─────────────────────────────────────────

// cardCountdown formats a duration as a human-friendly countdown.
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
