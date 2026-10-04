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
// first (see providerDisplayOrder: gemini < xai < clinepass, unlisted
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

// Card geometry, in display columns measured with ANSI stripped. EVERY
// provider renders the ONE unified shape: one card per (provider, window)
// group, carrying one ROW per account of that group (see the unified-card
// section below). A card is a title line, N data rows, the fetch-failure
// notes (if any) and the bottom border:
//
//	╭─ Gemini · 周限额 · 6天23小时 ──────────────────────────────╮
//	│ · gemini-acct ▰▰▰▰▰▰▱▱▱▱  34%       3.4亿/10亿        │
//	╰──────────────────────────────────────────────────────────╯
//
// Row: "│ " + dot(1) + " " + name(n) + " " + capsule(10) + " " +
// pct(4, right-aligned) + " " + metric(13, right-aligned) + " │" =
// clineRowFixed + n + 4 display columns, whatever the percentage and the
// token pair happen to be: every module boundary carries exactly ONE space
// and each segment sits in its OWN reserved width, so a row is exactly the
// card's width with no fill to absorb (the trailing fill stays as the guard
// for a value wider than its reservation, which no real window produces).
// The card is max(row width, the width the title needs) columns wide, so the
// LONGEST account display name and the title both fit and no line ever
// overflows its own border. There is no fixed card width any more: the old
// 56-column two-row layout (title account + 47-cell capsule + detail row +
// 已达限额 footer) is gone for good.
//
// ONE width is shared by every card of ONE render (see cardWidth): the
// widest card need of that render, floored by the name column's minimum
// (clineNameColMin). The three providers' cards therefore line up column
// for column instead of each being exactly as wide as its own longest
// account name. A name longer than the floor still widens every card of
// the render TOGETHER — the name column is never truncated.

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
// display order (gemini < xai < clinepass; unlisted providers fall back
// to lexicographic) and then by account name, separated by one blank
// line. ANSI true-color escapes are emitted by default — pass
// CardOptions{NoColor: true} for pipes, redirects and --no-color, which
// strips the escapes and nothing else. (The pipe-friendly table is
// RenderTable's job.)
//
// EVERY provider renders the ONE unified layout: one card per (provider,
// window) group holding one ROW per account of that group (see the
// unified-card section below). Accounts are polled one key each — a
// snapshot carries one account per key group — so the snapshots are
// collected into GROUPS before any card is emitted, and the accounts of
// one provider land as rows of the same card instead of one card each.
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

	groups := cardGroups(sorted)
	// ONE width for the whole render: every card (title line, rows, notes,
	// borders) is laid out at it, so the providers' cards are exactly as
	// wide as each other.
	width := cardWidth(groups, now)
	emitted := make(map[clineKey]bool)

	var cards []string
	for _, s := range sorted {
		// A card is emitted at the position of the FIRST snapshot that
		// contributes to it, so the provider order (and the window order
		// inside one provider) stays the one accountSortKey and the
		// snapshot's own window order gave.
		for _, key := range clinePassKeys(s) {
			if emitted[key] {
				continue
			}
			emitted[key] = true
			cards = append(cards, renderGroupCard(groups[key], width, now, pal))
		}
	}
	return strings.Join(cards, "\n\n") + "\n"
}

// cardTitles is the per-row account identifier of one snapshot: the
// snapshot's account names, or the provider when the upstream reported
// none. An account-less snapshot still needs a row, otherwise its window
// group would be an empty card.
func cardTitles(s Snapshot) []string {
	if len(s.Accounts) > 0 {
		return s.Accounts
	}
	if s.Provider == "" {
		return []string{"unknown"}
	}
	return []string{s.Provider}
}

// cardProfileName is a group's provider display name; a provider the
// display table does not know is shown by its own key ("unknown" when
// even that is empty), like the rows of an unknown provider.
func cardProfileName(provider string) string {
	if name := providerDisplayName(provider); name != "" {
		return name
	}
	return "unknown"
}

// ── unified cards: one card per (provider, window), one row per account ──
//
// This is the ONE card layout every provider renders. It was introduced for
// ClinePass (whose accounts are polled one key each, so a card has to merge
// the snapshots of one plan) and is now the only layout, which is why the
// cline* identifier names below are historical. One card per (provider,
// window) holds one ROW per account of that group:
//
//	╭─ ClinePass · 月限额 · 6天23小时 ──────────────────────────╮
//	│ · cline-user ▰▰▰▰▰▰▱▱▱▱  34%       3.4亿/10亿        │
//	│ · cline-user ▰▰▰▰▰▰▰▰▰▰ 100%       10亿/10亿          │
//	╰─────────────────────────────────────────────────────────╯
//
// The title NEVER carries an account name — it is the provider, the window
// label and the window's RESET COUNTDOWN (" · " between all three) — so a
// long account name can never collide with the metrics. The countdown is a
// WINDOW-level property (every account of one card shares the same
// ResetsAt), which is exactly why it moved off the row: it used to be
// repeated once per account, saying the same thing N times. A window with
// no reset instant carries no countdown segment at all.
//
// Row format, left to right, with exactly ONE space between the elements:
//
//	color dot(1) account name(n) capsule(10) pct metric
//
// The percentage is right-aligned in its reserved clinePctWidth columns and
// the token pair right-aligned in clineNumberWidth columns, so both always
// END on the same column across every card of the render and a short value
// keeps its spare columns on its LEFT instead of opening a variable gap
// between two modules. Every row is therefore exactly clineRowFixed+n display
// columns
// — the same for every row of every card — and the CARD is 4 columns wider
// than that row. n is the render's name column (see cardWidth): the
// LONGEST account DISPLAY name CELL of EVERY card of the render, floored by
// clineNameColMin. The displayed name is never truncated — a name longer
// than the floor widens every card of the render together (see
// clineCardWidth/cardWidth) — and every row of every card shares that one
// column, so the capsules, the percentages and the metric fields stay in one
// column each across all of them. The account name and its dot carry the same
// accountColor(fp) (see accountNameText/accountDot), so rows of one card are
// told apart by
// color AND by name even when two accounts share a name. A stale snapshot's
// account cell carries the 旧 marker (the title used to hold it): the marker
// rides in the name column, which is sized from the MARKED cell, so it can
// never push that row's capsule out of the shared columns.
//
// The name a row DISPLAYS drops its trailing pure-digit suffix (Cline1 and
// Cline2 both read "Cline", stripNumericSuffix via clineDisplayName). The
// row's IDENTITY is untouched by that: clineRowID keys on the FULL name plus
// the fingerprint, so Cline1 and Cline2 stay TWO rows (with or without a
// fingerprint) and are told apart by the dot and the name color instead.
//
// A row carries ONLY those elements. It has no detail text line (no
// 已用 x% / 总额 …, no 额度 …), no 已达限额 footer, no status suffix and
// nothing appended to the account name: the ONE exhausted signal is the
// capsule going solid red plus the percentage and the X/X metric (see
// clineMetricField). The metric field is:
//
//   - weekly / monthly / 5h: the used/total TOKEN pair in Chinese 亿 units,
//     no "~" and no 估算池 marker (the total is the pool
//     ApplyClinePassEstimates / ApplyWeekEstimate DERIVE from the live
//     percent — the upstream never reports one — but an inferred total is
//     still the only total that window has, so it is shown plainly);
//   - a window that cannot answer — no pool AND no measured consumption
//     (usage db / metapi unreadable, or a fresh window with no traffic) —
//     reads "-". Its countdown is already on the title, so the row does not
//     repeat it.
const (
	clineCapCells = 10 // capsule columns: 10 cells = 10 % each, a coarse gauge

	// clinePctWidth and clineNumberWidth are what a row's percentage and its
	// token-pair segment can occupy at their LONGEST: "100%" and a used/total
	// pair ("9999亿/9999亿"). Both segments are written RIGHT-aligned inside
	// their own reserved width (see clineRowLine), so a value always ends on
	// the same column no matter how many digits it has. The two widths decide
	// what a row RESERVES — through clineRowFixed, so the widest possible row
	// still fits inside the card's border — and are the blank placeholders of
	// a windowless row.
	clinePctWidth    = 4  // widest percentage: "100%"
	clineNumberWidth = 13 // widest metric: "9999亿/9999亿"
	// clineRowFixed is a row's display columns WITHOUT the account name
	// column: the space that separates it from the capsule (1), the capsule
	// (clineCapCells), the space after the capsule (1), the widest percentage
	// (clinePctWidth), the space before the metric (1), the widest metric
	// (clineNumberWidth) and the closing " │" (2). Every module boundary
	// carries exactly ONE space; because both segments are right-aligned
	// inside their reservation, the row lands on the card's width with NOTHING
	// left over (the trailing fill in clineRowLine stays as the guard for a
	// value wider than its reservation, which no real window produces).
	clineRowFixed = 1 + clineCapCells + 1 + clinePctWidth + 1 + clineNumberWidth + 2 // 32
	// clineNameColMin is the FLOOR of the name column in display columns: the
	// longest account name in the current production roster — the accounts the
	// user's config and the metapi account table feed in — i.e. "SuperGrok",
	// 9 columns. That is a deployment fact, not a property of this package's
	// display table. One render lays every card out at ONE width (cardWidth),
	// so without a floor the same command would come out narrower when only
	// short-named providers answer than when SuperGrok is in the roster — the
	// width would jump between runs. The floor pins the common case at
	// max(...) >= 4 + clineRowFixed + 9 = 45 columns; a name LONGER than the
	// floor still widens every card of the render together (it is never
	// truncated).
	clineNameColMin = 9
)

// clineKey identifies one card: the provider (normalized provider key)
// plus the window NAME. Snapshots of one provider describe the same
// accounts, so their windows merge; different providers or windows never
// do. A windowless snapshot (a failed fetch) keys on the empty window, so
// the failing accounts of one provider still share one card.
type clineKey struct{ provider, window string }

// clineRow is one account's row inside a card. win is nil for a windowless
// snapshot: the row then shows the account (dot + name + the 旧 marker when
// the snapshot is stale) and leaves the metric columns blank. stale is the
// snapshot's staleness flag, kept per ROW because two accounts of one group
// can come from a fresh and from a stale snapshot at the same time.
type clineRow struct {
	name  string
	fp    string
	stale bool
	win   *Window
}

// clineGroup is one card under construction. resetsAt is the reset instant
// of the group's window — a WINDOW-level property every account of the card
// shares — and is what the title's countdown segment reads. The FIRST non-nil
// one wins and is never overwritten inside one render, so a retried round or a
// repeated roster cannot make the countdown flip-flop, and a snapshot that
// reports no reset cannot blank a title another snapshot already informed. nil
// for a windowless card (a failed fetch) and for a window whose upstream
// reported no reset instant: the title then carries no countdown segment at
// all.
type clineGroup struct {
	profile  string // provider display name, e.g. ClinePass
	window   string // window display title; "" for a windowless snapshot
	resetsAt *time.Time
	rows     []clineRow
	notes    []string
	seen     map[string]bool
}

// clinePassKeys lists the card keys one snapshot contributes to, in card
// order: one per window, or the single empty (windowless) key when the
// snapshot carries no window at all. The provider is normalized
// (lower-cased, trimmed) so two spellings of one provider still share their
// cards; the window key is the RAW window name ("weekly" / "5h"), so the
// window label is looked up once, when the card is built.
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

// cardGroups collects the cards of a snapshot list: one group per
// (provider, window), holding one row per account. The snapshots must
// already be sorted (accountSortKey); rows are appended in that order, so
// accounts keep the ordering the rest of the report uses.
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
//
// A snapshot listing the SAME window name twice contributes its first such
// window only (the group is keyed by the name); no fetcher in this package
// can produce one — each parses its windows into a per-name map or appends
// fixed, distinct names — so the drop is unreachable rather than lossy.
func cardGroups(sorted []Snapshot) map[clineKey]*clineGroup {
	groups := map[clineKey]*clineGroup{}
	for si, s := range sorted {
		names := cardTitles(s)
		fps := s.AccountFPs()
		for _, key := range clinePassKeys(s) {
			g := groups[key]
			if g == nil {
				g = &clineGroup{
					profile: cardProfileName(s.Provider),
					seen:    map[string]bool{},
				}
				// A windowless group (a 401/403 cleared the windows) keeps the
				// window segment EMPTY: there is no window to label, and a "--"
				// placeholder would claim one.
				if key.window != "" {
					g.window = windowTitle(key.window)
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
				// The window's reset instant is the card's title countdown. The
				// FIRST non-nil one wins and is never overwritten inside one
				// render, so a snapshot that reports no reset cannot blank a
				// title another snapshot already informed, and the countdown
				// does not flip-flop between renders of the same group.
				if g.resetsAt == nil && win != nil {
					g.resetsAt = win.ResetsAt
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
				g.rows = append(g.rows, clineRow{name: name, fp: fp, stale: s.Stale, win: win})
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

// clineRowID is the dedupe key of one card row (see cardGroups).
func clineRowID(name, fp string, snapshot, account int) string {
	if fp != "" {
		return "fp\x00" + name + "\x00" + fp
	}
	return "at\x00" + name + "\x00" + strconv.Itoa(snapshot) + ":" + strconv.Itoa(account)
}

// clineNote attributes a fetch failure on a card: the account the failure
// came from plus the localized code. The account name is what the title no
// longer carries, so the note is where it has to appear.
func clineNote(name, code string) string {
	if name == "" {
		return cardErrorNote(code)
	}
	return "⚠ " + name + ": " + localizedCode(code)
}

// containsString reports whether list already holds s. A card keeps its
// failure notes unique, so a repeated snapshot cannot duplicate a note line.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// clineCardWidth is the width ONE card NEEDS in display columns: the row's
// fixed columns plus the LONGEST account name cell in the card (the name the
// row actually shows — display name plus the 旧 marker, clineRowNameCell), so
// a long name widens the card instead of being truncated. The title's own
// needs are a floor, so a short account name can never squeeze the provider
// + window + countdown title into an ellipsis.
//
// This is the card's own NEED, NOT the width it is rendered at: one render
// gives every card the same width (cardWidth, the widest need of the whole
// render), so the three providers' cards cannot come out at three different
// widths. now decides the countdown segment's width — "9天23小时" is wider
// than "2小时01分" — so the same group can need a different width at a
// different moment.
func clineCardWidth(g *clineGroup, now time.Time) int {
	name := 0
	for _, r := range g.rows {
		if w := render.DisplayWidth(clineRowNameCell(r)); w > name {
			name = w
		}
	}
	width := 4 + clineRowFixed + name
	if need := titleWidth(g.profile, g.window, titleCountdown(g.resetsAt, now), render.DisplayWidth(" · ")) + 6; need > width {
		width = need
	}
	return width
}

// cardWidth is the ONE width, in display columns, that every card of ONE
// render is laid out at: the widest card need the render contains
// (clineCardWidth — the card's longest name cell plus the fixed row columns,
// the title's own need included), floored by the name column's minimum
// (clineNameColMin) so a render whose providers all answer with short
// account names is exactly as wide as one that carries the longest known
// name. The floor is a MINIMUM, never a cap: a name longer than it widens
// every card of the render together, and no name column is ever truncated
// (see clineRowLine).
//
// Iteration order is irrelevant: the result is a maximum over all groups,
// and the groups of one render are exactly the cards it emits.
func cardWidth(groups map[clineKey]*clineGroup, now time.Time) int {
	width := 4 + clineRowFixed + clineNameColMin
	for _, g := range groups {
		if w := clineCardWidth(g, now); w > width {
			width = w
		}
	}
	return width
}

// renderGroupCard renders one (provider, window) card at the render's shared
// width: the title (provider display name + window label + the window's
// reset countdown, never an account), one row per account, the failure
// notes, and the bottom border. now is what the title's countdown segment is
// measured against.
//
// width is cardWidth's render-wide value, not this card's own need, so all
// cards of the render — their borders, titles, rows and note lines alike —
// start and end in the same columns.
func renderGroupCard(g *clineGroup, width int, now time.Time, pal cardPalette) string {
	lines := []string{cardTitleLineAt(width, g.profile, g.window, titleCountdown(g.resetsAt, now), pal)}
	for _, r := range g.rows {
		lines = append(lines, clineRowLine(r, width, pal))
	}
	for _, note := range g.notes {
		lines = append(lines, cardBodyAt(width, note, pal))
	}
	return joinCardAt(lines, width, pal)
}

// clineDisplayName is the account name a row SHOWS: the account's full
// name with its trailing pure-digit suffix dropped (Cline1 and Cline2 both
// read "Cline"; see stripNumericSuffix). Stripping is DISPLAY-ONLY: the row
// identity (clineRowID) keeps the full name plus the fingerprint, so a
// suffixed pair is still exactly two rows — and with the digits gone, the
// dot and the name color (accountColor(fp)) are what tell the two
// same-reading rows apart.
//
// Every provider's rows go through it: the display contract is ONE contract,
// so a Gemini account named "acct-1" reads "acct-" exactly like a ClinePass
// "Cline-1" does (the hyphen is not a digit and stays). A name that is only
// digits keeps its name — see stripNumericSuffix.
func clineDisplayName(r clineRow) string {
	return stripNumericSuffix(r.name)
}

// clineRowNameCell is the text a row's name column holds: the DISPLAY name
// plus the 旧 marker of a stale snapshot. The card's name column is sized
// from it (clineCardWidth), so a stale row cannot push its own capsule out
// of the shared columns. The title used to carry the marker; it no longer
// carries the account at all, so the marker moved down with the name.
func clineRowNameCell(r clineRow) string {
	name := clineDisplayName(r)
	if r.stale {
		return name + " 旧"
	}
	return name
}

// clineRowLine renders one row at the render's shared width. The account name
// is padded to that width's name column — the render-wide one (cardWidth),
// NOT this card's own longest name — so the capsule, the percentage and the
// token pair of every row start at the same column, on this card and on every
// other card of the render. The percentage and the token pair are written
// RIGHT-aligned inside their own reserved width (clinePctWidth /
// clineNumberWidth) and exactly ONE space away from the capsule and from each
// other, so a value always lands on the same column whatever its length and
// the row is exactly clineRowFixed + name columns wide with nothing to fill.
// A windowless snapshot (a failed fetch) shows neither a percentage nor a
// pair: it keeps the two segments as blanks of the width they RESERVE
// (clinePctWidth / clineNumberWidth), so its blank row carries the same
// structure — and the same total width — as a row with data.
// The dot and the name share one color (accountColor of the account's
// fingerprint); with no usable fingerprint the dot degrades to the plain ·
// and the name stays plain. The row shows the account's DISPLAY name
// (clineDisplayName) plus the 旧 marker of a stale snapshot; the row identity
// (clineRowID) keeps the full name plus the fingerprint.
func clineRowLine(r clineRow, width int, pal cardPalette) string {
	display := clineDisplayName(r)
	cell := clineRowNameCell(r)
	// The pad is what makes the name cell occupy the FULL render-wide name
	// column (cardWidth >= this card's own need, so the cell always fits):
	// the cell plus its padding are width-4-clineRowFixed columns wide, and
	// the trailing space of the name segment is the pad's +1. The clamp is a
	// guard only — a cell wider than the render width is what cardWidth
	// exists to rule out — and it never truncates: the cell is written in
	// full either way.
	pad := width - 4 - clineRowFixed - render.DisplayWidth(cell)
	if pad < 0 {
		pad = 0
	}
	capsule := strings.Repeat(" ", clineCapCells)
	pct := strings.Repeat(" ", clinePctWidth)
	metric := strings.Repeat(" ", clineNumberWidth)
	if r.win != nil {
		capsule = clineBar(*r.win, pal)
		pct = render.PadLeft(pctLabel(*r.win), clinePctWidth)
		// Truncate keeps the one-row-width invariant a hard guarantee: a token
		// pair wider than the segment reserved for it (no real window has one —
		// "9999亿/9999亿" is 13 columns) is cut off at the border instead of
		// pushing it out. PadLeft right-aligns what fits inside the reservation.
		metric = render.PadLeft(render.Truncate(clineMetricField(*r.win), clineNumberWidth), clineNumberWidth)
	}
	row := pal.dim("│ ") +
		accountDot(r.fp, pal) + " " +
		accountNameText(display, r.fp, pal) + strings.TrimPrefix(cell, display) +
		strings.Repeat(" ", pad+1) +
		capsule + " " + pct + " " + metric
	// The row is completed at its END. Because both segments are right-aligned
	// inside their reservation, a well-formed row already sits exactly on the
	// card's width and this fill is zero; it stays as the guard that keeps the
	// right border from drifting if a value ever came out narrower than its
	// reservation.
	fill := width - render.DisplayWidth(row) - 2
	if fill < 0 {
		fill = 0
	}
	return row + strings.Repeat(" ", fill) + pal.dim(" │")
}

// clineBar is a row's capsule: the used share solid ▰, the rest hollow ▱,
// colored per cell along the shared severity ramp — and the WHOLE capsule
// solid red once the window is exhausted. "Went red" (with the percentage
// and the X/X pair next to it) is the only exhausted signal a row
// carries; there is no footer any more.
//
// 10 cells at 10 % each is a COARSE gauge on purpose: it reads the order of
// magnitude at a glance, and the exact value is the percentage column's job.
// A low-usage segment (say 3 %) therefore still draws one cell — no special
// compensation, no "at least one cell" fudging, and no per-segment rounding
// rule that only some percentages get.
func clineBar(w Window, pal cardPalette) string {
	if windowExhausted(w) {
		return pal.red(strings.Repeat(capUsed, clineCapCells))
	}
	pct := displayPercent(w)
	return render.CapsuleBar(clineCapCells, render.CapsuleUsedCells(pct, clineCapCells), nil, pal.color)
}

// clineMetricField is a row's metric field — the text the row writes there,
// RIGHT-aligned inside the card's clineNumberWidth = 13 reservation — and the
// single rule of the two-metric-column layout:
//
//   - a TOKEN window (weekly / monthly / 5h, clineTokenPairWindow) shows the
//     used/total pair in Chinese 亿 units with NO "~": the total is the
//     window's pool — LimitTokensEstimate, or the MEASURED consumption
//     (MeasuredTokens) when there is no pool — and used is the window's share
//     of it, the very percentage the pct column shows. The upstream never
//     REPORTS a pool (ApplyClinePassEstimates / ApplyWeekEstimate derive it
//     from the live percent, see estimate.go), but the derived pool is the
//     only total that window has, so it is shown plainly instead of being
//     flagged as an inference.
//   - a window that cannot answer — no pool AND no measured consumption
//     (usage db / metapi unreadable, or a fresh window that has seen no
//     traffic yet) — reads "-". It does NOT fall back to the countdown: the
//     countdown is a window-level property the TITLE already carries, and a
//     row repeating it per account is exactly what this layout removed.
//
// A 100 % (exhausted) token window is forced to the FULL pair, so a drained
// row always reads X/X — never "-" and never a fraction of a measured
// total.
func clineMetricField(w Window) string {
	if clineTokenPairWindow(w.Name) {
		total := w.LimitTokensEstimate
		if total <= 0 {
			total = w.MeasuredTokens
		}
		if total > 0 {
			if windowExhausted(w) {
				return formatTokenPair(total, total)
			}
			used := int64(float64(total) * float64(displayPercent(w)) / 100)
			return formatTokenPair(used, total)
		}
	}
	return "-"
}

// clineTokenPairWindow reports whether a window's metric is the used/total
// TOKEN pair, i.e. whether it carries a reversible pool: the weekly, the
// monthly and the 5-hour windows do (each has its own PeriodStart and its
// own used fraction, so each gets its own reversal — see
// ApplyClinePassEstimates / ApplyWeekEstimate). The opencode-go "rolling"
// window is NOT one of them: it is a USD credit window with no token
// denominator at all, so its row reads "-" and its title carries the
// countdown.
func clineTokenPairWindow(name string) bool {
	return name == "weekly" || name == "monthly" || name == "5h"
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

// cardTitleLineAt builds the top border with the embedded title:
//
//	╭─ ␣service␣·␣window␣·␣countdown␣Dim(───…╮)
//
// One space separates each title element and the fill; the fill dashes sit
// on the RIGHT of the title only; the total line is exactly width display
// columns. An over-long title is shrunk IN THE VARIABLE SEGMENTS — the
// window label first (only an unknown upstream name is ever long: the known
// labels are short and fixed), then the countdown (a window reset far in the
// future can be "29天23小时"), then the service name — and the whole line is
// never truncated: the fill — derived from the width the segments ACTUALLY
// occupy, since a double-width rune that cannot fit the column the ellipsis
// needs stops a truncation one column short — is what absorbs the
// difference. The cards size themselves from this width (clineCardWidth),
// so in practice nothing here has to shrink; the loop stays as the guard for
// a title wider than the caller's width.
//
// The title TEXT (service name, window label and countdown) is rendered as
// plain text — no color, no bold — so every string in the card looks the same,
// exactly like the usage report's card title; only the non-text elements (the
// ╭─ border, the dash fill) stay dim. The separator is an ordinary space, so
// the segments read as one plain phrase. The ACCOUNT is deliberately absent:
// it is the row's first element (see clineRowLine), so a long account name
// can never collide with the title.
func cardTitleLineAt(width int, service, window, countdown string, pal cardPalette) string {
	const prefixW, suffixW, sep = 3, 1, " · "
	sepW := render.DisplayWidth(sep)
	// The line is "╭─ " + body + " " + fill + "╮" = 3 + body + 1 + fill
	// + 1, so the body may take at most width-6 columns and still leave
	// one fill dash.
	bodyMax := width - prefixW - suffixW - 2
	svc, win, cd := service, window, countdown
	// Shrink the variable segments until the body fits bodyMax. Every step
	// re-measures with DisplayWidth, so a segment whose truncation stopped
	// one column short is picked up by the next pass instead of pushing the
	// border out — and no step ever truncates the line itself.
shrink:
	for titleWidth(svc, win, cd, sepW) > bodyMax {
		switch {
		case win != "" && bodyMax-titleWidth(svc, "", cd, sepW)-sepW >= 1:
			// The window label itself is over-long: give it the room the
			// service name and the countdown leave over.
			win = render.Truncate(win, bodyMax-titleWidth(svc, "", cd, sepW)-sepW)
		case cd != "" && bodyMax-titleWidth(svc, win, "", sepW)-sepW >= 1:
			// Then the countdown: it is the segment that can legally grow the
			// longest ("29天23小时"), so it gives its room back first.
			cd = render.Truncate(cd, bodyMax-titleWidth(svc, win, "", sepW)-sepW)
		case svc != "":
			// Nothing left to borrow from (a long unknown provider key):
			// shrink the service name.
			if budget := bodyMax - titleWidth("", win, cd, sepW); budget >= 1 {
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
	if win != "" {
		b.WriteString(sep)
		b.WriteString(win)
	}
	if cd != "" {
		b.WriteString(sep)
		b.WriteString(cd)
	}
	// Fill from the MEASURED body width: 3 + body + 1 + fill + 1 = width.
	used := titleWidth(svc, win, cd, sepW)
	fill := width - prefixW - suffixW - used - 1
	if fill < 1 {
		fill = 1
	}
	b.WriteString(pal.dim(" " + strings.Repeat("─", fill) + "╮"))
	return b.String()
}

// titleWidth is the display width of the title body (service, window and
// countdown, each joined by the separator), the fill excluded. Empty
// segments contribute nothing but their separator is never emitted.
func titleWidth(svc, win, cd string, sepW int) int {
	n := render.DisplayWidth(svc)
	if win != "" {
		n += sepW + render.DisplayWidth(win)
	}
	if cd != "" {
		n += sepW + render.DisplayWidth(cd)
	}
	return n
}

// titleCountdown is the card title's countdown segment: the window-level
// reset countdown in Chinese wording (see cardCountdown), "已重置" once the
// window has rolled over, and "" when the window reported no reset instant at
// all — a card then simply has no countdown segment. resetsAt is the group's
// window reset instant (clineGroup.resetsAt), the ONE instant every account
// row of that card shares.
func titleCountdown(resetsAt *time.Time, now time.Time) string {
	if resetsAt == nil || resetsAt.IsZero() {
		return ""
	}
	if !resetsAt.After(now) {
		return "已重置"
	}
	return cardCountdown(now, *resetsAt)
}

// cardBodyAt renders one mostly-empty middle line: the content
// left-aligned at the card body and padded to the card's inner width. The
// failure notes ride here (see renderGroupCard).
func cardBodyAt(width int, text string, pal cardPalette) string {
	return pal.dim("│ ") + render.PadRight(text, width-4) + pal.dim(" │")
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

// joinCardAt appends the bottom border to the card lines.
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

// pctLabel is a row's percentage: the consumed share as "34%" / "100%"
// (3 to 4 columns). The caller right-aligns it inside clinePctWidth, so the
// value always ends on the same column whatever its digit count.
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

// windowExhausted reports whether one window counts as exhausted: at or
// above 100 %, used up, or rate-limited. The same determination drives the
// solid red capsule and the forced X/X metric (see clineBar /
// clineMetricField); there is no footer any more.
func windowExhausted(w Window) bool {
	return w.Percent >= 100 || w.Status == "used up" || w.Status == "rate-limited"
}

// ── card palette (one color decision per render) ──────────────────────

// cardPalette carries the color decision for one card render. Every
// colored element goes through it, so NoColor strips ALL escapes while the
// layout — cell counts, padding, glyphs — is decided before any wrapper
// runs. The palette therefore guarantees "colors off, layout unchanged":
// the no-color render is the colored render minus its escape sequences,
// byte for byte. Text is deliberately NOT part of this: the title (service
// name, window label) and every Chinese string render as plain text, so no
// card text carries color or bold.
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

// run is gone: render.CapsuleBar now owns the per-cell ramp, the
// equal-color run merging and the color decision, shared with the usage
// report. The palette has no capsule entry point left, and the yellow the
// deleted 已达限额 footer used went with it. The account dot/name color is
// accountColor's job (see accountNameText/accountDot), not the palette's.

// ── legacy helpers (kept) ─────────────────────────────────────────────────

// accountSortKey orders snapshots by provider display order first (see
// providerDisplayOrder: gemini < xai < clinepass), then by their first
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
// "Cline-1" → "Cline-" (a hyphen is not a digit and stays). Digits in the
// MIDDLE of a name are never touched.
//
// A name that is ENTIRELY digits ("12345") keeps its name: stripping down to
// the empty string would leave the row with nothing but the colour dot, which
// is worse than showing the account's actual name — the row identity
// (clineRowID) and the colour already tell such accounts apart.
//
// It is wired into the merged card through clineDisplayName: a merged row
// SHOWS the stripped name (Cline/Cline2 both read "Cline"), while the row
// IDENTITY (clineRowID) keeps the full name plus the fingerprint — so a
// suffixed pair is still exactly two rows, and two same-reading accounts are
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

// formatTokenPair formats a used/total token pair in the Chinese 亿 unit:
// "3.4亿/10亿". Joined without spaces, because the pair IS one metric segment
// (a row's right-aligned metric field, 13 columns at its widest,
// "9999亿/9999亿"). Neither side is marked: the package writes a derived pool
// the same way it writes a measured one (see clineMetricField). Both sides go
// through render.FormatTokensYi, so a whole 亿 drops its fraction, a value one
// decimal would collapse to zero is shown with TWO decimals instead ("0.02亿",
// see FormatTokensYi), and only a true zero — or a value below 0.005 亿, where
// even two decimals round away — reads "0亿".
func formatTokenPair(used, total int64) string {
	return render.FormatTokensYi(used) + "/" + render.FormatTokensYi(total)
}

// ── card title & body helpers ─────────────────────────────────────────

// cardCountdown formats a duration as the Chinese reset countdown the card
// title (and no row any more) uses:
//
//	2小时01分   inside one day, minutes zero-padded so the column stays even
//	6天23小时   across days; a whole number of days reads "3天"
//	不足1分     below a minute — a countdown too short to divide
//	已重置      the reset instant has passed
//
// The 5-hour window never leaves the hour form (its countdown cannot exceed
// five hours), so the day form is what the weekly and monthly titles show.
// The legacy ?format=table keeps its own English formatRemain wording.
func cardCountdown(now, at time.Time) string {
	d := at.Sub(now)
	if d <= 0 {
		return "已重置"
	}
	if d < time.Minute {
		return "不足1分"
	}
	mins := int(d.Minutes())
	if mins < 60 {
		return fmt.Sprintf("%d分", mins)
	}
	hours := mins / 60
	mins %= 60
	if hours < 24 {
		return fmt.Sprintf("%d小时%02d分", hours, mins)
	}
	days := hours / 24
	hours %= 24
	if hours == 0 {
		return fmt.Sprintf("%d天", days)
	}
	return fmt.Sprintf("%d天%d小时", days, hours)
}
