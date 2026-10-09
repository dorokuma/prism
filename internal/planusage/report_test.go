package planusage

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/render"
)

// ── legacy table tests (unchanged: RenderTableAt is preserved) ────────────

func TestRenderTableTable(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2*time.Hour + 13*time.Minute)
	d3 := now.Add(3*24*time.Hour + 4*time.Hour)
	d12 := now.Add(12 * 24 * time.Hour)
	got := RenderTableAt([]Snapshot{
		{
			Provider: "opencode-go",
			Accounts: []string{"go-1"},
			Windows: []Window{
				{Name: "rolling", Status: "ok", Percent: 12, ResetsAt: &h2},
				{Name: "weekly", Status: "ok", Percent: 8, ResetsAt: &d3},
				{Name: "monthly", Status: "ok", Percent: 40, ResetsAt: &d12},
			},
		},
		{
			Provider: "opencode-go",
			Accounts: []string{"go-2"},
			Windows: []Window{
				{Name: "rolling", Status: "rate-limited", Percent: 100, ResetsAt: &h2},
				{Name: "weekly", Status: "ok", Percent: 30, ResetsAt: &d3},
				{Name: "monthly", Status: "ok", Percent: 55, ResetsAt: &d12},
			},
		},
	}, now)
	want := "" +
		"  账号 窗口 状态 占用 重置  限额估算\n" +
		"  go-1 短期 ok   12%  2h13m -       \n" +
		"       中期 ok   8%   3d4h  --      \n" +
		"       长期 ok   40%  12d   -       \n" +
		"  go-2 短期 限流 100% 2h13m -       \n" +
		"       中期 ok   30%  3d4h  --      \n" +
		"       长期 ok   55%  12d   -       \n"
	if got != want {
		t.Fatalf("layout\ngot:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "T10:00:00Z") || strings.Contains(got, "---") || strings.Contains(got, ", ") {
		t.Fatalf("old merged/ISO format leaked: %q", got)
	}
	var winLines []string
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if strings.Contains(line, "%") {
			winLines = append(winLines, line)
		}
	}
	if len(winLines) != 6 {
		t.Fatalf("window lines: %q", winLines)
	}
	if !strings.Contains(winLines[0], "go-1") || strings.Contains(winLines[1], "go-1") || strings.Contains(winLines[2], "go-1") {
		t.Fatalf("go-1 module layout wrong: %q", winLines[:3])
	}
	if !strings.Contains(winLines[3], "go-2") || strings.Contains(winLines[4], "go-2") || strings.Contains(winLines[5], "go-2") {
		t.Fatalf("go-2 module layout wrong: %q", winLines[3:])
	}
}

func TestRenderTableSplitsAccounts(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderTableAt([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1", "a2"},
		Windows:  []Window{{Name: "rolling", Status: "ok", Percent: 1}},
	}}, now)
	want := "" +
		"  账号 窗口 状态 占用 重置 限额估算\n" +
		"  a1   短期 ok   1%   -    -       \n" +
		"  a2   短期 ok   1%   -    -       \n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderTableModuleFirstRow(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2*time.Hour + 13*time.Minute)
	d3 := now.Add(3*24*time.Hour + 4*time.Hour)
	d12 := now.Add(12 * 24 * time.Hour)
	header := "  账号 窗口 状态 占用 重置  限额估算\n"
	cases := []struct {
		name    string
		windows []Window
		want    string
	}{
		{
			name: "three windows",
			windows: []Window{
				{Name: "rolling", Status: "ok", Percent: 12, ResetsAt: &h2},
				{Name: "weekly", Status: "ok", Percent: 8, ResetsAt: &d3},
				{Name: "monthly", Status: "ok", Percent: 40, ResetsAt: &d12},
			},
			want: header +
				"  a1   短期 ok   12%  2h13m -       \n" +
				"       中期 ok   8%   3d4h  --      \n" +
				"       长期 ok   40%  12d   -       \n",
		},
		{
			name:    "single window",
			windows: []Window{{Name: "rolling", Status: "ok", Percent: 12, ResetsAt: &h2}},
			want: header +
				"  a1   短期 ok   12%  2h13m -       \n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderTableAt([]Snapshot{{
				Provider: "opencode-go",
				Accounts: []string{"a1"},
				Windows:  tc.windows,
			}}, now)
			if got != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestRenderTableModuleSortAndEmptyCells(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderTableAt([]Snapshot{
		{Provider: "opencode-go", Accounts: []string{"z9"}, Windows: []Window{
			{Name: "rolling", Status: "ok", Percent: 12},
			{Name: "weekly", Status: "ok", Percent: 8},
		}},
		{Provider: "opencode-go", Accounts: []string{"a1"}, Windows: []Window{
			{Name: "rolling", Status: "ok", Percent: 1},
		}},
	}, now)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if !strings.Contains(lines[1], "a1") || !strings.Contains(lines[2], "z9") {
		t.Fatalf("modules not sorted:\n%s", got)
	}
	if !strings.Contains(lines[2], "z9") || strings.Contains(lines[3], "z9") {
		t.Fatalf("z9 module layout wrong:\n%s", got)
	}
}

func TestRenderTableStaleModuleFirstRow(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(45 * time.Minute)
	got := RenderTableAt([]Snapshot{
		{
			Provider: "opencode-go",
			Accounts: []string{"a1"},
			Stale:    true,
			Err:      "fetch_failed",
			Windows:  []Window{{Name: "rolling", Status: "ok", Percent: 3, ResetsAt: &h}},
		},
		{
			Provider: "acme",
			Accounts: []string{"b2"},
			Stale:    true,
			Err:      "timeout",
			Windows: []Window{
				{Name: "rolling", Status: "ok", Percent: 7, ResetsAt: &h},
				{Name: "weekly", Status: "ok", Percent: 8, ResetsAt: &h},
				{Name: "monthly", Status: "ok", Percent: 9, ResetsAt: &h},
			},
		},
	}, now)
	for _, s := range []string{"a1 旧: fetch_failed", "b2 旧: timeout"} {
		if !strings.Contains(got, s) {
			t.Fatalf("error attribution %q missing:\n%s", s, got)
		}
	}
	if strings.Contains(got, "opencode-go/a1") || strings.Contains(got, "acme/b2") {
		t.Fatalf("provider prefix leaked:\n%s", got)
	}
	if n := strings.Count(got, "a1 旧"); n != 2 {
		t.Fatalf("a1 旧 occurs %d times, want 2:\n%s", n, got)
	}
	if n := strings.Count(got, "b2 旧"); n != 2 {
		t.Fatalf("b2 旧 occurs %d times, want 2:\n%s", n, got)
	}
	if !strings.Contains(got, "3%") || !strings.Contains(got, "7%") {
		t.Fatalf("window rows missing:\n%s", got)
	}
}

func TestRenderTableAtErrorAndStale(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderTableAt([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1"},
		Err:      "unauthorized",
	}}, now)
	if got != "  a1\n  a1: unauthorized\n" {
		t.Fatalf("error card: %q", got)
	}
	got = RenderTableAt([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1"},
	}}, now)
	if got != "  a1\n" {
		t.Fatalf("empty-window snapshot: %q", got)
	}
	h := now.Add(45 * time.Minute)
	got = RenderTableAt([]Snapshot{{
		Provider: "opencode-go",
		Stale:    true,
		Windows:  []Window{{Name: "rolling", Status: "ok", Percent: 3, ResetsAt: &h}},
	}}, now)
	if !strings.Contains(got, "旧") || !strings.Contains(got, "45m") {
		t.Fatalf("stale card: %q", got)
	}
	got = RenderTableAt([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1"},
		Err:      "fetch_failed",
		Stale:    true,
		Windows:  []Window{{Name: "rolling", Status: "ok", Percent: 3, ResetsAt: &h}},
	}}, now)
	if !strings.Contains(got, "a1 旧: fetch_failed") || !strings.Contains(got, "3%") {
		t.Fatalf("stale+error attribution: %q", got)
	}
}

func TestRenderTableErrorAttribution(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(45 * time.Minute)
	got := RenderTableAt([]Snapshot{
		{
			Provider: "opencode-go",
			Accounts: []string{"a1"},
			Stale:    true,
			Err:      "fetch_failed",
			Windows:  []Window{{Name: "rolling", Status: "ok", Percent: 3, ResetsAt: &h}},
		},
		{
			Provider: "acme",
			Accounts: []string{"b2"},
			Stale:    true,
			Err:      "timeout",
			Windows:  []Window{{Name: "rolling", Status: "ok", Percent: 7, ResetsAt: &h}},
		},
	}, now)
	for _, s := range []string{"a1 旧: fetch_failed", "b2 旧: timeout"} {
		if !strings.Contains(got, s) {
			t.Fatalf("error attribution %q missing:\n%s", s, got)
		}
	}
	if strings.Contains(got, "opencode-go/a1") || strings.Contains(got, "acme/b2") {
		t.Fatalf("provider prefix leaked:\n%s", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if strings.Contains(line, "fetch_failed") && !strings.Contains(line, "a1 旧: ") {
			t.Fatalf("bare error line: %q", line)
		}
		if strings.Contains(line, "timeout") && !strings.Contains(line, "b2 旧: ") {
			t.Fatalf("bare error line: %q", line)
		}
	}
	if !strings.Contains(got, "a1 旧") || !strings.Contains(got, "b2 旧") {
		t.Fatalf("stale markers missing:\n%s", got)
	}
	if !strings.Contains(got, "3%") || !strings.Contains(got, "7%") {
		t.Fatalf("window rows missing:\n%s", got)
	}
}

func TestFormatRemain(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "<1m"},
		{45 * time.Minute, "45m"},
		{2 * time.Hour, "2h"},
		{2*time.Hour + 13*time.Minute, "2h13m"},
		{3 * 24 * time.Hour, "3d"},
		{3*24*time.Hour + 4*time.Hour, "3d4h"},
		{-time.Minute, "已到"},
	}
	for _, tc := range cases {
		at := now.Add(tc.d)
		if got := formatRemain(now, &at); got != tc.want {
			t.Errorf("%v: got %q want %q", tc.d, got, tc.want)
		}
	}
	if formatRemain(now, nil) != "-" {
		t.Fatal("nil resets")
	}
}

func TestRenderTableGeminiFiveHourAndWeekly(t *testing.T) {
	now := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	h5 := now.Add(5 * time.Hour)
	wk := now.Add(49 * time.Minute)
	got := RenderTableAt([]Snapshot{{
		Provider: "gemini",
		Accounts: []string{"Gemini"},
		Windows: []Window{
			{Name: "5h", Status: "ok", Percent: 0, ResetsAt: &h5},
			{Name: "weekly", Status: "ok", Percent: 94, ResetsAt: &wk},
		},
	}}, now)
	if !strings.Contains(got, "短期") {
		t.Fatalf("5h label missing:\n%s", got)
	}
	if !strings.Contains(got, "中期") {
		t.Fatalf("weekly label missing:\n%s", got)
	}
	if strings.Contains(got, "Claude") || strings.Contains(got, "3p-") || strings.Contains(got, "5小时") || strings.Contains(got, "周限") {
		t.Fatalf("old labels/claude leaked:\n%s", got)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	var first, second string
	for _, line := range lines {
		if strings.Contains(line, "%") && first == "" {
			first = line
		}
	}
	for _, line := range lines {
		if strings.Contains(line, "%") && line != first {
			second = line
		}
	}
	if !strings.Contains(first, "Gemini") {
		t.Fatalf("account missing on first window row:\n%s", got)
	}
	if strings.Contains(second, "Gemini") {
		t.Fatalf("weekly row should leave the account cell empty:\n%s", got)
	}
}

func TestRenderTableTokenEstimateColumn(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderTableAt([]Snapshot{{
		Provider: "xai",
		Accounts: []string{"SuperGrok"},
		Windows: []Window{
			{Name: "weekly", Status: "ok", Percent: 57, LimitTokensEstimate: 1_540_000},
		},
	}}, now)
	if !strings.Contains(got, "1.54M") {
		t.Fatalf("token estimate missing:\n%s", got)
	}
}

func TestRenderTableEstimateColumn(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderTableAt([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1"},
		Windows: []Window{
			{Name: "rolling", Status: "ok", Percent: 12, LimitUSDEstimate: 12, USDStatus: "estimated"},
			{Name: "weekly", Status: "ok", Percent: 8, LimitUSDEstimate: 30, USDStatus: "estimated"},
			{Name: "monthly", Status: "ok", Percent: 40, LimitUSDEstimate: 60, USDStatus: "confirmed"},
			{Name: "custom", Status: "ok", Percent: 5},
		},
	}}, now)
	if !strings.Contains(got, "$12.00") || !strings.Contains(got, "$30.00") {
		t.Fatalf("estimates missing:\n%s", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if strings.Contains(line, "长期") || strings.Contains(line, "custom") {
			if strings.Contains(line, "$") {
				t.Fatalf("non-estimated window showed dollars: %q", line)
			}
		}
	}
}

func TestRenderTableNoProviderPrefix(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	win := []Window{{Name: "rolling", Status: "ok", Percent: 1}}
	got := RenderTableAt([]Snapshot{
		{Provider: "opencode-go", Accounts: []string{"a1"}, Windows: win},
		{Provider: "acme", Accounts: []string{"b2"}, Windows: win},
	}, now)
	if strings.Contains(got, "opencode-go/") || strings.Contains(got, "acme/") {
		t.Fatalf("provider prefix still rendered:\n%s", got)
	}
	if !strings.Contains(got, "a1") || !strings.Contains(got, "b2") {
		t.Fatalf("accounts missing:\n%s", got)
	}
}

func TestRenderTableLongNamesInFull(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderTableAt([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"账户名称很长很长很长"},
		Windows: []Window{
			{Name: "super-long-window-name-for-testing", Status: "ok", Percent: 1},
			{Name: "", Status: "ok", Percent: 2},
		},
	}}, now)
	for _, s := range []string{"账户名称很长很长很长", "super-long-window-name-for-testing", "--"} {
		if !strings.Contains(got, s) {
			t.Fatalf("%q not rendered in full:\n%s", s, got)
		}
	}
	if strings.Contains(got, "…") {
		t.Fatalf("truncated: %s", got)
	}
}

func TestRenderTableEmpty(t *testing.T) {
	if RenderTable(nil) != "  没有套餐数据\n" {
		t.Fatal(RenderTable(nil))
	}
}

// ── unified quota card tests ─────────────────────────────────────────────
//
// ONE layout for every provider: one card per (provider, window) group, one
// ROW per account of that group. These tests guard it: the title is provider
// + window label + the window's reset countdown and NEVER an account, the row
// is
//
//	dot + name + capsule(10) + pct + token pair
//
// with exactly ONE space between the modules (the percentage is right-aligned
// in its clinePctWidth reservation and the token pair right-aligned in its
// clineNumberWidth one, so a row needs no fill), every line of a card is exactly
// THAT card's width (the card is sized from its longest account name cell and
// its title, so no name is truncated and no border is ever pushed out), the
// capsule is ▰ (used) + ▱ (remaining) on a per-cell green→yellow→red ramp
// with a solid red drained pill, the token pair is the used/total pair in 亿
// units (or "-" when the window has no pool and no measured tokens), the
// title text carries no color and no bold, and the account display name, the
// row identity and the 旧 marker all survive the merge.
//
// The old two-row card (title account + 47-cell capsule + 已用/总额 detail row
// + 已达限额 footer) is gone; the tests that pinned it were rewritten, so no
// assertion of that layout survives here.

const (
	ansiBrandCyan = "\x1b[38;2;0;180;216m"
	ansiGreen     = "\x1b[38;2;82;183;136m"
	ansiYellow    = "\x1b[38;2;244;162;97m" // #F4A261: a ramp stop
	ansiRed       = "\x1b[38;2;230;57;70m"
	ansiDim       = "\x1b[38;2;102;102;102m"
	ansiReset     = "\x1b[0m"
	// cardCapCells is the capsule width the assertions talk about. It is
	// clineCapCells: the card width itself is no longer a constant (see
	// clineCardWidth), but the capsule still is.
	cardCapCells = clineCapCells
)

// cardLines splits a card render into ANSI-stripped lines and asserts that
// EVERY line of the render is exactly as wide as the first one. The width is
// ONE per render — every card of it is laid out at the render-wide width
// (cardWidth: the widest card need, floored by the name column's minimum) —
// so a blank line only separates two cards, it does not reset the
// expectation: two cards of one render may never differ in width.
func cardLines(t *testing.T, got string) []string {
	t.Helper()
	raw := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	out := make([]string, 0, len(raw))
	width := -1
	for i, l := range raw {
		if l == "" {
			out = append(out, "")
			continue
		}
		plain := render.StripANSI(l)
		w := render.DisplayWidth(plain)
		if width == -1 {
			width = w
		} else if w != width {
			t.Fatalf("line %d: width %d, want the render-wide %d:\n%q", i, w, width, plain)
		}
		out = append(out, plain)
	}
	return out
}

// wantCardWidth is the width a card render must come out at for a given set of
// name cells: the widest cell's row width, floored by the name column's
// minimum (4 + clineRowFixed + clineNameColMin, 45 while the longest account
// name in the current production roster is 9 columns wide — that is a
// deployment fact, not a property of this package — and a longer name widens
// every card of the render instead of being truncated). Spelled out here
// rather than calling the production cardWidth, so the assertion states the
// contract instead of restating the code.
func wantCardWidth(nameCells ...string) int {
	width := 4 + clineRowFixed + clineNameColMin
	for _, c := range nameCells {
		if w := 4 + clineRowFixed + render.DisplayWidth(c); w > width {
			width = w
		}
	}
	return width
}

// cardTitleLines returns the title line of every card of a render, in order.
func cardTitleLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, "╭─ ") {
			out = append(out, l)
		}
	}
	return out
}

// rowTail is the right-hand end of a row: one space, the percentage written
// RIGHT-aligned inside its clinePctWidth-column reservation, one space, the
// token pair right-aligned inside clineNumberWidth columns, and the right
// border. The reservations are what every row of every card of the render
// shares, so EVERY row ends with this same tail whatever the account name and
// whatever the value lengths are — which is what keeps the borders and the
// module boundaries lined up down the card. The one space before the
// percentage and the one before the pair are the fixed module gaps: they do
// not depend on how many digits a value has, and a short value keeps its
// spare columns on its LEFT (inside its reservation) instead of opening a
// variable gap between two modules.
func rowTail(pct, metric string) string {
	return " " + render.PadLeft(pct, clinePctWidth) + " " + render.PadLeft(metric, clineNumberWidth) + " │"
}

// rowCapsule extracts one row's capsule: the run of ▰/▱ cells that follows
// the account name.
func rowCapsule(t *testing.T, row string) string {
	t.Helper()
	var b strings.Builder
	for _, r := range row {
		if r == '▰' || r == '▱' {
			b.WriteRune(r)
			continue
		}
		if b.Len() > 0 {
			break
		}
	}
	if b.Len() == 0 {
		t.Fatalf("no capsule in row: %q", row)
	}
	return b.String()
}

// assertCardTitle checks a stripped title line:
// "╭─ <provider> · <window> · <countdown>" followed by one space, ONLY dash
// fill, then "╮". No left-side fill, no ╶, and no account name (the row carries
// the account). An empty window means a windowless card: no separator and no
// window label at all. An empty countdown means the window reported no reset
// instant, so the title has no countdown segment either — the countdown is a
// window-level property, not a per-row one.
func assertCardTitle(t *testing.T, line, provider, window, countdown string) {
	t.Helper()
	head := "╭─ " + provider
	if window != "" {
		head += " · " + window
	}
	if countdown != "" {
		head += " · " + countdown
	}
	if !strings.HasPrefix(line, head) {
		t.Fatalf("title head %q not in %q", head, line)
	}
	rest := line[len(head):]
	if !strings.HasSuffix(rest, "╮") {
		t.Fatalf("title must end with ╮: %q", line)
	}
	body := strings.TrimSuffix(rest, "╮")
	if !strings.HasPrefix(body, " ") {
		t.Fatalf("title needs one space before the dash fill: %q", line)
	}
	fill := body[1:]
	if fill == "" || strings.Trim(fill, "─") != "" {
		t.Fatalf("title fill must be dashes only, got %q", fill)
	}
}

func TestRenderCardsEmpty(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	if got := RenderCards(nil, now); got != "  没有套餐数据\n" {
		t.Fatalf("empty: %q", got)
	}
}

func TestRenderCardsBasic(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2*time.Hour + 1*time.Minute)
	got := RenderCards([]Snapshot{{
		Provider: "claude",
		Accounts: []string{"claude-main"},
		Windows:  []Window{{Name: "5h", Status: "ok", Percent: 59, ResetsAt: &h2}},
	}}, now)
	lines := cardLines(t, got)

	// Title: plain-text provider + plain-text window label + plain-text
	// countdown + right-only dash fill (no color and no bold on any of the
	// title text, exactly like the usage report's card title). The ACCOUNT is
	// not on it.
	assertCardTitle(t, lines[0], "Claude", "5小时限额", "2小时01分")
	if strings.Contains(lines[0], "claude-main") {
		t.Fatalf("the title must not carry an account name: %q", lines[0])
	}
	for _, bad := range []string{
		"\x1b[1m",                            // bold
		ansiBrandCyan + "Claude" + ansiReset, // colored provider name
		ansiBrandCyan + "5小时限额" + ansiReset,  // colored window label
		ansiBrandCyan + "2小时01分" + ansiReset, // colored countdown
	} {
		if strings.Contains(got, bad) {
			t.Fatalf("title text must stay unstyled, found %q:\n%q", bad, got)
		}
	}
	// The non-text parts of the title line keep their dim border.
	if !strings.Contains(got, ansiDim+"╭─ ") {
		t.Fatalf("dim title border missing:\n%q", got)
	}
	if strings.Contains(got, "╶") {
		t.Fatalf("left-side title fill artifact (╶) present:\n%q", got)
	}

	// One account, one window: title + one data row + bottom border.
	if len(lines) != 3 {
		t.Fatalf("want 3 lines (title + row + border), got %d:\n%q", len(lines), lines)
	}
	row := lines[1]
	if !strings.HasPrefix(row, "│ · claude-main ") {
		t.Fatalf("the row must be dot + account name: %q", row)
	}
	// The capsule's FILLED LENGTH is the used share: 59 % → 6 ▰ + 4 ▱ of the
	// 10-cell capsule (10 % per cell, a coarse gauge).
	bar := rowCapsule(t, row)
	if n := strings.Count(bar, capUsed); n != 6 {
		t.Fatalf("used cells = %d, want 6: %q", n, bar)
	}
	if n := strings.Count(bar, capEmpty); n != cardCapCells-6 {
		t.Fatalf("remaining cells = %d, want %d: %q", n, cardCapCells-6, bar)
	}
	if n := len([]rune(bar)); n != cardCapCells {
		t.Fatalf("capsule width = %d, want %d: %q", n, cardCapCells, bar)
	}
	// The healthy band is flat green up to the 40 % ramp stop (cells 0..3 for
	// a 10-cell capsule); the rest is dim gray.
	if !strings.Contains(got, ansiGreen+strings.Repeat(capUsed, 4)+ansiReset) {
		t.Fatalf("flat green used run missing:\n%q", got)
	}
	if !strings.Contains(got, ansiDim+strings.Repeat(capEmpty, 4)+ansiReset) {
		t.Fatalf("dim remaining run missing:\n%q", got)
	}
	if strings.Contains(got, ansiRed) {
		t.Fatalf("59 %% is not exhausted, red must not appear:\n%q", got)
	}
	// Percentage and token-pair columns: 59 % and the honest "-" — a 5-hour
	// window with no pool and no measured consumption has no pair to show, and
	// its countdown is already on the title.
	if !strings.HasSuffix(row, rowTail("59%", "-")) {
		t.Fatalf("row tail wrong:\n%q", row)
	}

	// No invented cycle dots, no detail text, no 估算池 marker, no footer.
	if strings.ContainsAny(got, "○●◆") {
		t.Fatalf("cycle dots must not be fabricated:\n%q", got)
	}
	for _, bad := range []string{"已达限额", "已用", "总额", "词元", "估算池", "~"} {
		if strings.Contains(got, bad) {
			t.Fatalf("the unified card must not carry %q:\n%q", bad, got)
		}
	}
	// Bottom border: ╰ + width-2 dashes + ╯, the width being the render-wide
	// one.
	width := render.DisplayWidth(lines[0])
	bottom := lines[len(lines)-1]
	if !strings.HasPrefix(bottom, "╰") || !strings.HasSuffix(bottom, "╯") ||
		strings.Count(bottom, "─") != width-2 {
		t.Fatalf("bottom border malformed:\n%q", bottom)
	}
	if want := wantCardWidth("claude-main"); width != want {
		t.Fatalf("card width = %d, want %d (row width): %q", width, want, lines[0])
	}
}

// TestRenderCardsTitleTextIsPlainText locks the plain-text title contract: in
// a COLORED render no title segment (provider name, " · " separator, window
// label) carries an escape, the only colored part of the title line is the
// dim ╭─/─…╮ border, and the plain and colored renders are byte identical
// once the escapes are stripped — so the palette can never re-introduce color
// or bold into the card text.
func TestRenderCardsTitleTextIsPlainText(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2 * time.Hour)
	snaps := []Snapshot{{
		Provider: "claude",
		Accounts: []string{"claude-main"},
		Windows:  []Window{{Name: "5h", Status: "ok", Percent: 59, ResetsAt: &h2}},
	}}
	colored := RenderCards(snaps, now)
	plain := RenderCards(snaps, now, CardOptions{NoColor: true})

	title := strings.SplitN(render.StripANSI(colored), "\n", 2)[0]
	for _, seg := range []string{"Claude", " · ", "5小时限额", "2小时00分"} {
		if !strings.Contains(title, seg) {
			t.Fatalf("title segment %q missing from %q", seg, title)
		}
	}
	// The card is as wide as its longest row: name cell(claude-main) + the
	// fixed columns, floored by the name column's minimum.
	width := wantCardWidth("claude-main")
	if got := render.DisplayWidth(title); got != width {
		t.Fatalf("card width = %d, want %d: %q", got, width, title)
	}
	// The whole title line is pinned byte for byte: dim border, plain text,
	// dim dash fill — and nothing else.
	fill := width - 3 - 1 - render.DisplayWidth("Claude · 5小时限额 · 2小时00分") - 1
	wantTitle := ansiDim + "╭─ " + ansiReset +
		"Claude · 5小时限额 · 2小时00分" +
		ansiDim + " " + strings.Repeat("─", fill) + "╮" + ansiReset
	rawTitle := strings.SplitN(colored, "\n", 2)[0]
	if rawTitle != wantTitle {
		t.Fatalf("title line mismatch\n got: %q\nwant: %q", rawTitle, wantTitle)
	}
	// No bold anywhere in the card.
	if strings.Contains(colored, "\x1b[1m") {
		t.Fatalf("card must not carry bold:\n%q", colored)
	}
	// Color changes nothing but the escapes.
	if render.StripANSI(colored) != plain {
		t.Fatalf("color changed the visible text\nplain:\n%q\ncolored:\n%q",
			plain, render.StripANSI(colored))
	}
	if strings.Contains(plain, "\x1b") {
		t.Fatalf("no-color render still carries escapes:\n%q", plain)
	}
	cardLines(t, plain)
	cardLines(t, colored)
}

// TestRenderCardsUnifiedSingleRowEveryProvider is the headline contract of
// this change: Gemini, SuperGrok and ClinePass all render the ONE single-row
// layout — a title carrying the provider and the window only, one row per
// account starting with the dot and the account name, and title + N rows +
// border lines — with no 已用/总额 detail text, no 估算池 marker, no "~" and
// no 已达限额 footer anywhere.
func TestRenderCardsUnifiedSingleRowEveryProvider(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3*time.Hour + 12*time.Minute)
	cases := []struct {
		name   string
		snap   Snapshot
		title  string
		head   string // row prefix: dot + account display name + space
		tail   string // row tail: pct + token pair
		window string // window label on the title
		count  string // countdown segment on the title; "" without a reset
	}{
		{
			name: "gemini",
			snap: Snapshot{Provider: "gemini", Accounts: []string{"gemini-acct"},
				Windows: []Window{{Name: "5h", Status: "ok", Percent: 34, ResetsAt: &reset}}},
			title:  "╭─ Gemini · 5小时限额 · 3小时12分 ",
			window: "5小时限额",
			count:  "3小时12分",
			head:   "│ · gemini-acct ",
			tail:   rowTail("34%", "-"),
		},
		{
			name: "supergrok",
			snap: Snapshot{Provider: "xai", Accounts: []string{"grok-acct"},
				Windows: []Window{{Name: "weekly", Status: "ok", Percent: 34, LimitTokensEstimate: 1_000_000_000, ResetsAt: &reset}}},
			title:  "╭─ SuperGrok · 周限额 · 3小时12分 ",
			window: "周限额",
			count:  "3小时12分",
			head:   "│ · grok-acct ",
			tail:   rowTail("34%", "3.4亿/10亿"),
		},
		{
			name: "clinepass",
			snap: Snapshot{Provider: "clinepass", Accounts: []string{"cline-user"},
				Windows: []Window{{Name: "monthly", Status: "ok", Percent: 34, LimitTokensEstimate: 1_000_000_000, ResetsAt: &reset}}},
			title:  "╭─ ClinePass · 月限额 · 3小时12分 ",
			window: "月限额",
			count:  "3小时12分",
			head:   "│ · cline-user ",
			tail:   rowTail("34%", "3.4亿/10亿"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderCards([]Snapshot{tc.snap}, now, CardOptions{NoColor: true})
			if n := strings.Count(got, "╭─ "); n != 1 {
				t.Fatalf("want ONE card, got %d:\n%s", n, got)
			}
			if n := strings.Count(got, "╰"); n != 1 {
				t.Fatalf("want ONE bottom border, got %d:\n%s", n, got)
			}
			lines := cardLines(t, got)
			if len(lines) != 3 { // title + N(1) rows + border
				t.Fatalf("want 3 lines (title + 1 row + border), got %d:\n%s", len(lines), got)
			}
			assertCardTitle(t, lines[0], providerDisplayName(tc.snap.Provider), tc.window, tc.count)
			if !strings.HasPrefix(lines[0], tc.title) {
				t.Fatalf("title = %q, want prefix %q", lines[0], tc.title)
			}
			for _, acc := range tc.snap.Accounts {
				if strings.Contains(lines[0], acc) {
					t.Fatalf("the title must not carry the account %q: %q", acc, lines[0])
				}
			}
			if !strings.HasPrefix(lines[1], tc.head) {
				t.Fatalf("row = %q, want prefix %q", lines[1], tc.head)
			}
			if !strings.HasSuffix(lines[1], tc.tail) {
				t.Fatalf("row = %q, want tail %q", lines[1], tc.tail)
			}
			// The old two-row card's vocabulary is gone for EVERY provider.
			for _, bad := range []string{"已用", "总额", "词元", "估算池", "已达限额", "~"} {
				if strings.Contains(got, bad) {
					t.Fatalf("unified card must not carry %q:\n%s", bad, got)
				}
			}
		})
	}
}

// TestRenderCardsGroupsAccountsIntoOneCard: the accounts of one provider +
// window are ROWS of one card (N accounts of the group = N rows + the title
// and the border), ordered like the rest of the report (by account name), and
// every row keeps its OWN numbers in the shared columns.
func TestRenderCardsGroupsAccountsIntoOneCard(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	pct := map[string]int{"gm-a": 20, "gm-b": 10, "gm-c": 30}
	var snaps []Snapshot
	for _, name := range []string{"gm-b", "gm-a", "gm-c"} { // deliberately unsorted
		snaps = append(snaps, Snapshot{
			Provider: "gemini",
			Accounts: []string{name},
			Windows: []Window{{
				Name: "weekly", Status: "ok", Percent: pct[name],
				LimitTokensEstimate: 1_000_000_000, ResetsAt: &reset,
			}},
		})
	}
	got := RenderCards(snaps, now, CardOptions{NoColor: true})
	if n := strings.Count(got, "╭─ "); n != 1 {
		t.Fatalf("want ONE card for the three accounts, got %d:\n%s", n, got)
	}
	lines := cardLines(t, got)
	if len(lines) != 5 { // title + 3 rows + border
		t.Fatalf("want 5 lines (title + 3 rows + border), got %d:\n%s", len(lines), got)
	}
	want := []struct{ name, pct, metric string }{
		{"gm-a", "20%", "2亿/10亿"},
		{"gm-b", "10%", "1亿/10亿"},
		{"gm-c", "30%", "3亿/10亿"},
	}
	for i, w := range want {
		row := lines[1+i]
		if !strings.HasPrefix(row, "│ · "+w.name+" ") {
			t.Fatalf("row %d = %q, want the dot + %q", i, row, w.name)
		}
		if !strings.HasSuffix(row, rowTail(w.pct, w.metric)) {
			t.Fatalf("row %d = %q, want tail %q", i, row, rowTail(w.pct, w.metric))
		}
	}
}

// TestRenderCardsMetricField is the metric matrix:
//
//   - a weekly/monthly/5h window with a pool (or a measured consumption)
//     shows the used/total pair in 亿 units with NO "~" marker;
//   - a 5h window with no pool and no measured tokens reads "-" — its
//     countdown is on the title, never repeated per row;
//   - a drained (100 %) token window is forced to X/X;
//   - the opencode-go "rolling" USD window is not a token window, so it
//     reads "-" whatever a reset instant says.
func TestRenderCardsMetricField(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3*time.Hour + 12*time.Minute)
	past := now.Add(-time.Minute)
	cases := []struct {
		name   string
		win    Window
		pct    string
		metric string
		drain  bool
	}{
		{name: "5h token pair", win: Window{Name: "5h", Status: "ok", Percent: 34, LimitTokensEstimate: 1_000_000_000, ResetsAt: &reset},
			pct: "34%", metric: "3.4亿/10亿"},
		// No pool and no measured consumption: the honest "-". The countdown
		// the title carries is not repeated on the row.
		{name: "5h without pool", win: Window{Name: "5h", Status: "ok", Percent: 34, ResetsAt: &reset},
			pct: "34%", metric: "-"},
		{name: "5h without reset either", win: Window{Name: "5h", Status: "ok", Percent: 34},
			pct: "34%", metric: "-"},
		// rolling is the opencode-go USD window: not a token window at all.
		{name: "rolling is not a token window", win: Window{Name: "rolling", Status: "ok", Percent: 7, ResetsAt: &reset},
			pct: "7%", metric: "-"},
		{name: "5h after reset", win: Window{Name: "5h", Status: "ok", Percent: 7, ResetsAt: &past},
			pct: "7%", metric: "-"},
		{name: "weekly token pair", win: Window{Name: "weekly", Status: "ok", Percent: 34, LimitTokensEstimate: 1_000_000_000},
			pct: "34%", metric: "3.4亿/10亿"},
		{name: "monthly measured pair", win: Window{Name: "monthly", Status: "ok", Percent: 50, MeasuredTokens: 800_000_000},
			pct: "50%", metric: "4亿/8亿"},
		{name: "weekly with neither tokens nor reset",
			win: Window{Name: "weekly", Status: "ok", Percent: 34},
			pct: "34%", metric: "-"},
		{name: "weekly drained", win: Window{Name: "weekly", Status: "used up", Percent: 100,
			LimitTokensEstimate: 1_700_000_000, MeasuredTokens: 1_700_000_000},
			pct: "100%", metric: "17亿/17亿", drain: true},
		{name: "weekly drained measured only", win: Window{Name: "weekly", Status: "ok", Percent: 100,
			MeasuredTokens: 4_200_000_000},
			pct: "100%", metric: "42亿/42亿", drain: true},
		// A drained 5-hour window that HAS a measured total is forced to the
		// full X/X pair like every other drained token window.
		{name: "5h drained shows X/X", win: Window{Name: "5h", Status: "rate-limited", Percent: 100,
			MeasuredTokens: 4_200_000_000, ResetsAt: &reset},
			pct: "100%", metric: "42亿/42亿", drain: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderCards([]Snapshot{{
				Provider: "gemini",
				Accounts: []string{"gemini"},
				Windows:  []Window{tc.win},
			}}, now, CardOptions{NoColor: true})
			lines := cardLines(t, got)
			if len(lines) != 3 {
				t.Fatalf("want 3 lines, got %d:\n%s", len(lines), got)
			}
			if !strings.HasSuffix(lines[1], rowTail(tc.pct, tc.metric)) {
				t.Fatalf("row = %q, want tail %q", lines[1], rowTail(tc.pct, tc.metric))
			}
			if strings.Contains(got, "~") {
				t.Fatalf("the token pair must not carry a ~ marker:\n%s", got)
			}
			used := strings.Count(rowCapsule(t, lines[1]), capUsed)
			if tc.drain {
				if used != cardCapCells {
					t.Fatalf("a drained window must draw a solid capsule, got %d cells:\n%s", used, got)
				}
			}
		})
	}

	// The drained capsule is RED (in color mode), the drain signal the token
	// pair and the percentage share.
	colored := RenderCards([]Snapshot{{
		Provider: "gemini",
		Accounts: []string{"gemini"},
		Windows:  []Window{{Name: "weekly", Status: "used up", Percent: 100, MeasuredTokens: 4_200_000_000}},
	}}, now)
	if !strings.Contains(colored, ansiRed+strings.Repeat(capUsed, cardCapCells)+ansiReset) {
		t.Fatalf("drained capsule must be solid red:\n%q", colored)
	}
}

// TestRenderCardsTokenPairUnitLadder pins the unit of a token pair against the
// window's TOTAL, and that both halves of the pair share it:
//
//   - the unit is the largest one the total reaches (亿 at its own divisor and up,
//     then 万, then 千 below that), so a pool just above a unit's divisor reads in
//     that unit while a small pool reads as a number of its own — the readings the
//     cases below state — instead of both collapsing into zeros.
//
// That collapse was the bug: with the 亿 unit alone, a small window's card read
// zeros while its /admin/quota JSON carried the real numbers — the display, not
// the data, was wrong, and the two surfaces disagreed about the same window. A pair
// is also one comparison, so the unit is picked ONCE from the total and the used
// side is rendered in it too (never two halves the reader would have to convert
// before comparing).
func TestRenderCardsTokenPairUnitLadder(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		win    Window
		metric string
	}{
		{
			// 亿 from its own divisor up: unchanged by the ladder (the big pools the
			// card was originally sized for).
			name:   "exactly 1e8 stays 亿",
			win:    Window{Name: "weekly", Status: "ok", Percent: 10, LimitTokensEstimate: 100_000_000},
			metric: "0.1亿/1亿",
		},
		{
			name:   "just below 1e8 is 万",
			win:    Window{Name: "weekly", Status: "ok", Percent: 50, LimitTokensEstimate: 99_000_000},
			metric: "4950万/9900万",
		},
		{
			// The measured-only shape: no pool, so the consumption IS the total
			// (and a drained window writes it twice).
			name:   "a measured 1299 is 1.3千, not 0亿",
			win:    Window{Name: "monthly", Status: "ok", Percent: 100, MeasuredTokens: 1_299},
			metric: "1.3千/1.3千",
		},
		{
			// 万 for the total, and the used side stays in the same unit even
			// though its own value is tiny (two decimals, see FormatTokensUnit).
			name:   "a tiny share keeps the total's unit",
			win:    Window{Name: "weekly", Status: "ok", Percent: 1, LimitTokensEstimate: 20_000},
			metric: "0.02万/2万",
		},
		{
			// Below the smallest unit the ladder stops at 千: a small pool is still a
			// number.
			name:   "below 1e4 is 千",
			win:    Window{Name: "5h", Status: "ok", Percent: 50, LimitTokensEstimate: 500},
			metric: "0.2千/0.5千",
		},
		{
			// The boundary guard: a total that a coarse step's own rounding carries
			// into one integer digit too many for the field — the reader would see a
			// number that is not the total — steps up to the finer unit instead.
			name:   "a total that rounds past four 万 digits steps up to 亿",
			win:    Window{Name: "weekly", Status: "ok", Percent: 34, LimitTokensEstimate: 99_999_999},
			metric: "0.3亿/1亿",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderCards([]Snapshot{{
				Provider: "clinepass",
				Accounts: []string{"clinepass#aaaaaaaaaa"},
				Windows:  []Window{tc.win},
			}}, now, CardOptions{NoColor: true})
			if !strings.Contains(got, tc.metric) {
				t.Fatalf("cards missing %q:\n%s", tc.metric, got)
			}
		})
	}

	// No unit step, however small the pool, may push the metric field past its
	// clineNumberWidth reservation: the card's width invariant is what clineNumberWidth
	// exists for, and a 千 pair must not widen a border.
	got := RenderCards([]Snapshot{{
		Provider: "clinepass",
		Accounts: []string{"clinepass#aaaaaaaaaa"},
		Windows:  []Window{{Name: "5h", Status: "ok", Percent: 100, MeasuredTokens: 940}},
	}}, now, CardOptions{NoColor: true})
	lines := cardLines(t, got)
	for _, l := range lines {
		if w, cardW := render.DisplayWidth(l), render.DisplayWidth(lines[0]); w != cardW {
			t.Fatalf("line %q is %d columns, want the card's %d:\n%s", l, w, cardW, got)
		}
	}
	if !strings.Contains(lines[1], "0.9千/0.9千") {
		t.Fatalf("row = %q, want the 千 pair", lines[1])
	}
}

// TestFormatTokenPairUnitLadderBudget pins the C4 fix at the formatter: the
// used/total pair walks the ladder 亿 / 万 / 千 / 原值 (see clinePairUnits) and the
// step is SCORED (see clinePairCandidate.beats), so that
//
//   - a non-zero half is never rendered as "0<unit>" — the card/JSON
//     disagreement C4 reported: a unit too large for the window's numbers read
//     them as zeros while /admin/quota's JSON carried the real ones;
//   - the pair is inside the columns the row reserves for it (clineNumberWidth)
//     wherever some step of EITHER ladder fits there, so the row's Truncate never
//     cuts a digit for those pairs: a 万 step whose halves need more than the
//     field holds used to be written anyway and the card showed the cut. Above
//     the scan's upper bound no step fits at all and the width is no longer
//     bounded (see TestFormatTokenPairInt64Tail).
//
// Both halves of a pair share ONE step, and among the steps that fit and are
// honest the step nearest the total's own unit wins, which is what keeps the
// units the table below pins. This is the guard for the ladder's scoring; the
// widths and the residual above the scan's upper bound are pinned by
// TestFormatTokenPairInt64Tail, and the second pass by
// TestFormatTokenPairBackupLadder.
//
// The pair is the FORMATTER's contract, so the mandated cases are stated as
// direct pairs: the card derives its used side from the percent
// (clineMetricField), which cannot produce every pair a unit boundary turns on
// (a one-token used side under a small total has no percent).
func TestFormatTokenPairUnitLadderBudget(t *testing.T) {
	cases := []struct {
		name  string
		used  int64
		total int64
		want  string
	}{
		{"a one-token pair drops to the raw step", 1, 1, "1/1"},
		{"a four-token pair drops to the raw step", 4, 4, "4/4"},
		{"a tiny numerator drops to the raw step", 1, 1_299, "1/1299"},
		{"a small drained pair keeps 千", 1_299, 1_299, "1.3千/1.3千"},
		{"a total that rounds past four 万 digits steps up to 亿", 99_999_999, 99_999_999, "1亿/1亿"},
		{"a 万 pair the field would cut steps up to 亿", 9_999_500, 99_995_000, "0.1亿/1亿"},
		{"a share of a boundary total reads in 亿", 33_999_999, 99_999_999, "0.3亿/1亿"},
		{"a drained 999.5万 pool steps up to 亿", 9_995_000, 9_995_000, "0.1亿/0.1亿"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTokenPair(tc.used, tc.total); got != tc.want {
				t.Fatalf("formatTokenPair(%d, %d) = %q, want %q", tc.used, tc.total, got, tc.want)
			}
		})
	}

	// The card side of the same fix: a window whose 万 pair the metric field used
	// to cut now reads a pair the field holds whole, and no line of the card
	// carries the truncation marker.
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	cards := RenderCards([]Snapshot{{
		Provider: "clinepass",
		Accounts: []string{"clinepass#aaaaaaaaaa"},
		Windows: []Window{{
			Name: "weekly", Status: "ok", Percent: 10, LimitTokensEstimate: 99_995_000,
		}},
	}}, now, CardOptions{NoColor: true})
	if strings.Contains(cards, "…") {
		t.Fatalf("the metric field may not truncate a pair:\n%s", cards)
	}
	if !strings.Contains(cards, "0.1亿/1亿") {
		t.Fatalf("cards missing the 亿 pair of the 99_995_000 pool:\n%s", cards)
	}

	// ── the property scan ───────────────────────────────────────────────
	// Whatever the ladder picks has to keep BOTH values readable, use ONE unit
	// for both halves and stay inside the field's columns. Totals cover every
	// turn-over the ladder has (raw/千, 千/万, a 万 total that rounds into the
	// next decade, 万/亿), plus a dense strip of the 万/亿 boundary. The upper
	// bound is where the raw step's own digits stop fitting the field next to a
	// one-token numerator (the ladder's documented residual, see
	// TestFormatTokenPairInt64Tail).
	//
	// The per-side split is asserted as a CONDITIONAL property — the ladder may
	// never pass over a step that is honest, inside the field AND inside the
	// split, but it is allowed to end up outside the split when no step can have
	// all three: a small share of a 万 total needs one column more than the split
	// allows for its 万 half and has no honest 亿 half to step up to (the 亿 step
	// reads that share as zero), so the 万 pair — inside the field's whole budget,
	// and the shape the card wrote before this fix — is the honest answer there.
	narrowReachable := func(used, total int64) bool {
		for _, c := range clinePairCandidates(used, total) {
			if c.fits && c.plain && c.narrow {
				return true
			}
		}
		return false
	}
	check := func(used, total int64) {
		t.Helper()
		pair := formatTokenPair(used, total)
		if w := render.DisplayWidth(pair); w > clineNumberWidth {
			t.Fatalf("formatTokenPair(%d, %d) = %q: %d display columns, the field holds %d — the row would truncate it",
				used, total, pair, w, clineNumberWidth)
		}
		left, right, ok := strings.Cut(pair, "/")
		if !ok {
			t.Fatalf("formatTokenPair(%d, %d) = %q: not a pair", used, total, pair)
		}
		if lu, ru := pairUnitOf(left), pairUnitOf(right); lu != ru {
			t.Fatalf("formatTokenPair(%d, %d) = %q: one unit per pair, got %q and %q", used, total, pair, lu, ru)
		}
		if used != 0 && !hasNonZeroDigit(left) {
			t.Fatalf("formatTokenPair(%d, %d) = %q: a non-zero used side read as zero", used, total, pair)
		}
		if total != 0 && !hasNonZeroDigit(right) {
			t.Fatalf("formatTokenPair(%d, %d) = %q: a non-zero total read as zero", used, total, pair)
		}
		if narrowReachable(used, total) {
			if w := render.DisplayWidth(left); w > clinePairSideBudget {
				t.Fatalf("formatTokenPair(%d, %d) = %q: used side is %d columns, but a %d-column split was reachable",
					used, total, pair, w, clinePairSideBudget)
			}
			if w := render.DisplayWidth(right); w > clinePairSideBudget {
				t.Fatalf("formatTokenPair(%d, %d) = %q: total side is %d columns, but a %d-column split was reachable",
					used, total, pair, w, clinePairSideBudget)
			}
		}
	}

	totals := []int64{
		1, 2, 4, 5, 49, 50, 499, 500, 999, 1_000, 1_001, 1_299, 4_999, 5_000,
		9_999, 10_000, 19_047, 20_000, 99_999, 100_000, 500_000, 999_999,
		1_000_000, 4_999_999, 5_000_000, 9_990_000, 9_999_999, 19_800_000,
		99_000_000, 99_990_000, 99_995_000, 99_999_000, 99_999_999,
		100_000_000, 190_476_000, 999_999_999, 1_000_000_000, 5_000_000_000,
		10_000_000_000, 99_999_999_999,
	}
	for i := int64(99_990_000); i <= 99_999_000; i += 1_000 {
		totals = append(totals, i)
	}
	for i := int64(9_994_999); i <= 9_995_010; i++ { // the 999.5万 rounding edge
		totals = append(totals, i)
	}
	pcts := []int{0, 1, 5, 10, 34, 50, 99, 100}
	for _, total := range totals {
		// The pairs a window derives (used = total × percent) are the ones the
		// card writes, so every one of them goes through the whole property set.
		for _, pct := range pcts {
			check(int64(float64(total)*float64(pct)/100), total)
		}
		// Hostile absolute numerators no percent produces on a big total: the
		// ladder still has to keep them readable and inside the field — the raw
		// step is what covers them, and the width check above is what proves it.
		for _, used := range []int64{1, 4, total - 1, total} {
			if used < 0 {
				continue
			}
			if used > total {
				used = total
			}
			check(used, total)
		}
	}

	// Past the scan's upper bound the raw step stops fitting next to a one-token
	// numerator, so the field's invariant
	// turns into the CONDITIONAL form of rule 1: whenever SOME step of EITHER
	// ladder (the main one or the 万亿/亿亿 backup) is inside the field, the winner
	// is inside it too — 宁舍精度也不许截断, i.e. a step that hides that one token
	// is preferred to a step clineRowLine's Truncate would cut. The cut is reachable
	// only for a (used, total) PAIR that has no fitting step on either ladder — the
	// loop below decides that per pair, since one total can have a drained pair
	// with no fitting step while a one-token numerator of the same total still has
	// one.
	// TestFormatTokenPairInt64Tail pins those widths, the wider tail above them and
	// the zero-reading blocks below the scan. Even here the key order is the first
	// pass's, unchanged: fits
	// still outranks plain, so a step that reads a non-zero count as "0<unit>" can
	// beat a wider honest one — 读 0 可能优先于截断 (the same trade the main ladder
	// has always made for a large total, not a separate honesty rule). What this
	// tail pins
	// is the width and the shape, not a promise that the number is always kept.
	for _, total := range []int64{
		100_000_000_000, 1_000_000_000_000, 99_999_999_999_999, 1_000_000_000_000_000_000,
		9_223_372_036_854_775_807,
	} {
		for _, used := range []int64{1, 4, total / 10, total - 1, total} {
			pair := formatTokenPair(used, total)
			left, right, ok := strings.Cut(pair, "/")
			if !ok {
				t.Fatalf("formatTokenPair(%d, %d) = %q: not a pair", used, total, pair)
			}
			if lu, ru := pairUnitOf(left), pairUnitOf(right); lu != ru {
				t.Fatalf("formatTokenPair(%d, %d) = %q: one unit per pair, got %q and %q", used, total, pair, lu, ru)
			}
			fitsReachable := false
			for _, c := range clinePairCandidates(used, total) {
				if c.fits {
					fitsReachable = true
				}
			}
			if !fitsReachable {
				for _, c := range clinePairBackupCandidates(used, total) {
					if c.fits {
						fitsReachable = true
					}
				}
			}
			if w := render.DisplayWidth(pair); fitsReachable && w > clineNumberWidth {
				t.Fatalf("formatTokenPair(%d, %d) = %q: %d display columns though a fitting step exists — the row would truncate it",
					used, total, pair, w)
			}
			if !fitsReachable && used != 0 && !hasNonZeroDigit(left) {
				t.Fatalf("formatTokenPair(%d, %d) = %q: a non-zero used side read as zero and no step fits anyway", used, total, pair)
			}
		}
	}

	// What the loop above deliberately does NOT flag: at the residual's magnitude
	// the one-token shape reads as zeros on both sides — a non-zero count hidden
	// behind a step that DOES fit.
	// That is the same trade the main ladder has always made for a large total and
	// is intentional (fits is the first key), not a regression; the other used
	// shapes land where the number survives and so would never have caught it.
	// Recorded here so no later reader "fixes" the surviving step back into a
	// truncation: when a step fits, reading a non-zero count as 0 is an accepted
	// trade, not a bug.
	if got := formatTokenPair(1, 1e18); got != "0亿亿/100亿亿" {
		t.Fatalf("formatTokenPair(1, 1e18) = %q, want the intentional %q (fits outranks plain)", got, "0亿亿/100亿亿")
	}
}

// TestFormatTokenPairInt64Tail pins the two regimes no real window
// reaches (a window's pool is orders of magnitude below them), all measured on
// this tree; the comment on formatTokenPair sets out the mechanism.
//
//   - at the residual's magnitude neither ladder has a fitting step for the pairs
//     the table below states, and each comes back wider than the field — the
//     widths clineRowLine's Truncate cuts;
//   - above it the width is NOT capped at the residual pair's own width: a total
//     whose 亿 half gains a fraction comes back wider than its predecessor, and
//     the table below states the widths the tail reaches. The scan below locates
//     that first total;
//   - below that, both the one-token numerator and the drained pool read a
//     NON-ZERO count as zeros on both sides — but only in blocks, because the only
//     step that
//     fits inside the field is 亿亿's whole-number rendering. The test pins one
//     measured example of each shape plus one further point: it does
//     NOT claim they are the only blocks, nor that the pinned ones are the earliest
//     — the region repeats block-wise. Both examples live in the constants below.
//
// The numbers live in the table and the constants below; this comment does not
// restate them.
func TestFormatTokenPairInt64Tail(t *testing.T) {
	const (
		firstWideTotal  = int64(1000000000005000001)
		firstWideBefore = firstWideTotal - 1
		firstZeroBlock  = int64(100_000_005_000_000)
		laterZeroBlock  = int64(998_443_705_000_000)
		drainedZeroTop  = int64(9_500_000_000_000_001)
	)
	for _, tc := range []struct {
		name        string
		used, total int64
		want        string
		columns     int
	}{
		{"a tenth of a 1e18 pool", 1e18 / 10, 1e18, "1000000000亿/10000000000亿", 26},
		{"half a 1e18 pool", 1e18 / 2, 1e18, "5000000000亿/10000000000亿", 26},
		{"a 1e18 pool one token short", 1e18 - 1, 1e18, "10000000000亿/10000000000亿", 27},
		{"a drained 1e18 pool", 1e18, 1e18, "10000000000亿/10000000000亿", 27},
		{"a one-token numerator of a 1e18 pool", 1, 1e18, "0亿亿/100亿亿", 13},
		{"the widest total below the 1e18 residual", firstWideBefore, firstWideBefore, "10000000000亿/10000000000亿", 27},
		{"the first total above 1e18 whose 亿 half gains a fraction", firstWideTotal, firstWideTotal, "10000000000.1亿/10000000000.1亿", 31},
		{"a later crossing of the same kind", 1515344939205000063, 1515344939205000063, "15153449392亿/15153449392亿", 27},
		{"a later crossing of the same kind, one token on", 1515344939205000064, 1515344939205000064, "15153449392.1亿/15153449392.1亿", 31},
		{"a drained pool at int64's top", math.MaxInt64, math.MaxInt64, "92233720368.5亿/92233720368.5亿", 31},
		{"a one-token numerator, one token below the first zero block", 1, firstZeroBlock - 1, "0亿/1000000亿", 13},
		{"a one-token numerator at the first zero block", 1, firstZeroBlock, "0亿亿/0亿亿", 11},
		{"a one-token numerator, one token below a later zero block", 1, laterZeroBlock - 1, "0亿/9984437亿", 13},
		{"a one-token numerator inside a later zero block", 1, laterZeroBlock, "0亿亿/0亿亿", 11},
		{"a drained pool one decade below its zero block", 99_999_999_999_999, 99_999_999_999_999, "99万亿/99万亿", 13},
		{"a drained pool at its zero block's lower edge", 1e14, 1e14, "0亿亿/0亿亿", 11},
		{"a drained pool at its zero block's upper edge", drainedZeroTop, drainedZeroTop, "0亿亿/0亿亿", 11},
		{"the first drained total above the block that reads a digit", drainedZeroTop + 1, drainedZeroTop + 1, "1亿亿/1亿亿", 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTokenPair(tc.used, tc.total)
			if got != tc.want {
				t.Fatalf("formatTokenPair(%d, %d) = %q, want %q", tc.used, tc.total, got, tc.want)
			}
			if w := render.DisplayWidth(got); w != tc.columns {
				t.Fatalf("formatTokenPair(%d, %d) = %q is %d columns, want %d", tc.used, tc.total, got, w, tc.columns)
			}
		})
	}

	// The first drained total above the residual that is WIDER than the residual's
	// own width: every 1000th total is sampled and then every single one inside the
	// step that first came back wide — the wide band is millions of totals long, so
	// a step of 1000 cannot overshoot it. The width compared against and the total
	// expected are the constants above.
	const residualWidth = 27
	firstWide := int64(-1)
	for x := int64(1e18); x <= 1e18+20_000_000; x += 1000 {
		if render.DisplayWidth(formatTokenPair(x, x)) > residualWidth {
			for y := x - 1000; y <= x; y++ {
				if render.DisplayWidth(formatTokenPair(y, y)) > residualWidth {
					firstWide = y
					break
				}
			}
			break
		}
	}
	if firstWide != firstWideTotal {
		t.Fatalf("the first drained total above the residual that is over %d columns is %d, want %d", residualWidth, firstWide, firstWideTotal)
	}

	// Same scan for the one-token shape's first all-zero block, plus the decade
	// below it sampled coarsely (no all-zero reading below the block).
	firstZero := int64(-1)
	for x := int64(1e14); x <= 1e14+20_000_000; x += 1000 {
		if formatTokenPair(1, x) == "0亿亿/0亿亿" {
			for y := x - 1000; y <= x; y++ {
				if formatTokenPair(1, y) == "0亿亿/0亿亿" {
					firstZero = y
					break
				}
			}
			break
		}
	}
	if firstZero != firstZeroBlock {
		t.Fatalf("the first all-zero one-token block starts at %d, want %d", firstZero, firstZeroBlock)
	}
	for x := int64(1e13); x < 1e14; x += 1e7 {
		if got := formatTokenPair(1, x); got == "0亿亿/0亿亿" {
			t.Fatalf("a one-token numerator of %d reads %q, want a digit", x, got)
		}
	}
	// The block is a block: the same reading holds across the block's opening
	// stretch of totals.
	for _, off := range []int64{1, 1000, 1_000_000, 4_999_999, 5_000_000, 9_999_999} {
		if got := formatTokenPair(1, firstZeroBlock+off); got != "0亿亿/0亿亿" {
			t.Fatalf("the zero block starting at %d stops at +%d: %q", firstZeroBlock, off, got)
		}
	}
}

// TestFormatTokenPairBackupLadder pins the SECOND pass of the ladder: a pair the
// main ladder has no fitting step for gets the 万亿 / 亿亿 steps and their
// whole-number renderings (see clinePairBackupUnits), while a pair the main ladder
// already fits is untouched — the first pass returns before the backup one is
// reached (TestFormatTokenPairUnitLadderBudget above is the guard for that side).
// The property loop below is the backup pass's own guard.
func TestFormatTokenPairBackupLadder(t *testing.T) {
	cases := []struct {
		name  string
		used  int64
		total int64
		want  string
	}{
		{"a drained 10205000000 pool fits 亿 as it always did", 10_205_000_000, 10_205_000_000, "102亿/102亿"},
		{"a drained 10205000001 pool drops 亿's fraction", 10_205_000_001, 10_205_000_001, "102亿/102亿"},
		{"the same pool one token short of drained keeps the fraction", 10_205_000_000, 10_205_000_001, "102亿/102.1亿"},
		{"a one-token numerator still prefers the raw step — it fits", 1, 10_205_000_001, "1/10205000001"},
		{"a drained 1e12 pool reaches 万亿", 1e12, 1e12, "1万亿/1万亿"},
		{"a tenth of a 1e12 pool reads 0.1万亿", 1e11, 1e12, "0.1万亿/1万亿"},
		{"a one-token numerator of a 1e12 pool keeps the 亿 step", 1, 1e12, "0亿/10000亿"},
		{"a drained 1e13 pool reads 10万亿", 1e13, 1e13, "10万亿/10万亿"},
		{"a drained 1e16 pool reads 1亿亿", 1e16, 1e16, "1亿亿/1亿亿"},
		{"a drained 1e17 pool reads 10亿亿", 1e17, 1e17, "10亿亿/10亿亿"},
		// A magnitude the field cannot hold honestly in ONE unit: the 万亿 pair
		// needs more columns than the field has and the 亿 step needs more still, so
		// only the whole-number rendering of the coarser step is inside the budget
		// (its fraction rounds away). The JSON keeps the exact count either way.
		{"a drained 1e15 pool falls back to the whole 亿亿 step", 1e15, 1e15, "0亿亿/0亿亿"},
		// The documented residual: past the point where no step of EITHER ladder
		// fits, the main winner stands and clineRowLine's Truncate is what cuts it.
		{"a drained 1e18 pool is the residual the row truncates", 1e18, 1e18, "10000000000亿/10000000000亿"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTokenPair(tc.used, tc.total); got != tc.want {
				t.Fatalf("formatTokenPair(%d, %d) = %q, want %q", tc.used, tc.total, got, tc.want)
			}
		})
	}

	// The property the second pass exists for: over the band of totals below, for
	// each of the used shapes the loop feeds, the pair comes back inside the field,
	// carries no ellipsis, keeps ONE unit for both halves and is the same string
	// every time it is asked. The totals and shapes themselves are the slices
	// below; the band is where the main ladder alone has no step that fits.
	band := []int64{1e11, 10_205_000_001, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17}
	for _, total := range band {
		for _, used := range []int64{1, 4, total / 10, total / 2, total - 1, total} {
			if used > total {
				continue
			}
			pair := formatTokenPair(used, total)
			if w := render.DisplayWidth(pair); w > clineNumberWidth {
				t.Fatalf("formatTokenPair(%d, %d) = %q: %d columns, the field holds %d", used, total, pair, w, clineNumberWidth)
			}
			if strings.Contains(pair, "…") {
				t.Fatalf("formatTokenPair(%d, %d) = %q: the pair may not carry the truncation marker", used, total, pair)
			}
			left, right, ok := strings.Cut(pair, "/")
			if !ok {
				t.Fatalf("formatTokenPair(%d, %d) = %q: not a pair", used, total, pair)
			}
			if lu, ru := pairUnitOf(left), pairUnitOf(right); lu != ru {
				t.Fatalf("formatTokenPair(%d, %d) = %q: one unit per pair, got %q and %q", used, total, pair, lu, ru)
			}
			if again := formatTokenPair(used, total); again != pair {
				t.Fatalf("formatTokenPair(%d, %d) = %q then %q: the ladder is not a function of its input", used, total, pair, again)
			}
		}
	}

	// A step change as the total grows is fine; for the four shapes below
	// (drained / a tenth / a half / one token) a flip BACK is not, and the
	// totals around every step boundary may not read A-B-A.
	modes := []func(int64) int64{
		func(total int64) int64 { return total },
		func(total int64) int64 { return total / 10 },
		func(total int64) int64 { return total / 2 },
		func(total int64) int64 { return 1 },
	}
	bases := []int64{10_005_000_001, 10_205_000_001, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17}
	for _, base := range bases {
		for _, usedOf := range modes {
			prev2, prev1 := "", ""
			for total := base - 50; total <= base+50; total++ {
				cur := formatTokenPair(usedOf(total), total)
				if cur == prev2 && cur != prev1 {
					t.Fatalf("the pair oscillates around total=%d: %q then %q then %q", total, prev2, prev1, cur)
				}
				prev2, prev1 = prev1, cur
			}
		}
	}

	// The total−1 shape is the one exception, and it is pinned HERE rather than
	// by the A-B-A loop above. The numerator one token short of the total makes
	// the 亿 step's own rounding read its right half a decimal higher than its left
	// half at isolated totals: the spike's shape is a whole 亿 count against the
	// same count with a tenth (a block index k and its k.1), and it sits at one of
	// two offsets inside every block of totals that share the same 亿 integer part.
	// The loop below pins that shape over every block it walks: exactly one
	// field-wide single-point spike per block, its offset counted into the split
	// the constants below state, and the narrower equal pair on either side. Those
	// are single-point SPIKES that RETURN to the value around them — not the A-B-A
	// oscillations the loop above forbids — so an unconditional "no A-B-A" would be
	// FALSE for this shape, which is why it is not fed to the loop above. The
	// measured counts themselves live in the constants below, not in this comment.
	const wantSpikesAt5_000_001, wantSpikesAt5_000_000 = 516, 384 // the split the loop below counts into
	var at5_000_001, at5_000_000 int
	for k := int64(100); k <= 999; k++ {
		base := k*1e8 + 5_000_000
		hits, offset := 0, int64(0)
		for d := int64(-16); d <= 16; d++ {
			if render.DisplayWidth(formatTokenPair(base+d-1, base+d)) == clineNumberWidth {
				hits++
				offset = d
			}
		}
		if hits != 1 {
			t.Fatalf("the total−1 window around %d holds %d field-wide totals, want a single spike", base, hits)
		}
		spike := base + offset
		want := fmt.Sprintf("%d亿/%d.1亿", k, k)
		if got := formatTokenPair(spike-1, spike); got != want {
			t.Fatalf("the total−1 spike at %d reads %q, want the documented shape %q", spike, got, want)
		}
		before := formatTokenPair(spike-2, spike-1)
		after := formatTokenPair(spike, spike+1)
		if before != after {
			t.Fatalf("the total−1 spike at %d does not return: %q vs %q", spike, before, after)
		}
		if w := render.DisplayWidth(before); w != clineNumberWidth-2 {
			t.Fatalf("the total−1 spike at %d falls back to %d columns, want %d: %q", spike, w, clineNumberWidth-2, before)
		}
		switch offset {
		case 1:
			at5_000_001++
		case 0:
			at5_000_000++
		default:
			t.Fatalf("the total−1 spike in the block starting at %d sits at offset %d, want +5_000_000 or +5_000_001", base, offset)
		}
	}
	if at5_000_001 != wantSpikesAt5_000_001 || at5_000_000 != wantSpikesAt5_000_000 {
		t.Fatalf("the total−1 spikes split %d at +5_000_001 / %d at +5_000_000, want %d / %d",
			at5_000_001, at5_000_000, wantSpikesAt5_000_001, wantSpikesAt5_000_000)
	}
	for _, base := range bases {
		for total := base - 50; total <= base+50; total++ {
			pair := formatTokenPair(total-1, total)
			if w := render.DisplayWidth(pair); w > clineNumberWidth {
				t.Fatalf("formatTokenPair(%d, %d) = %q: %d display columns around the step boundary %d",
					total-1, total, pair, w, base)
			}
			if again := formatTokenPair(total-1, total); again != pair {
				t.Fatalf("formatTokenPair(%d, %d) = %q then %q: the ladder is not a function of its input", total-1, total, pair, again)
			}
		}
	}
}

// pairUnitOf is the unit suffix a rendered pair half ends with ("" for the raw
// step, see clinePairUnits); the pair test uses it to pin "one unit per pair".
func pairUnitOf(side string) string {
	for i := len(side) - 1; i >= 0; i-- {
		if side[i] >= '0' && side[i] <= '9' || side[i] == '.' || side[i] == '-' {
			return side[i+1:]
		}
	}
	return side
}

// TestRenderCardsCapsuleGeometry walks every percentage the user can hit —
// including the exhausted paths and a sub-percent window refined through
// UsedFraction — and checks the capsule length, the percentage label and the
// per-card width invariant in one go.
func TestRenderCardsCapsuleGeometry(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3*time.Hour + 12*time.Minute)
	cases := []struct {
		name    string
		win     Window
		wantUse int
		wantPct string
	}{{
		name: "zero", win: Window{Percent: 0},
		wantUse: 0, wantPct: "0%",
	}, {
		name: "one", win: Window{Percent: 1},
		wantUse: 1, wantPct: "1%",
	}, {
		name: "ten is one cell", win: Window{Percent: 10},
		wantUse: 1, wantPct: "10%",
	}, {
		// 59 % of 10 cells: ceil(5.9) = 6, so the coarse gauge rounds UP.
		name: "fifty-nine", win: Window{Percent: 59},
		wantUse: 6, wantPct: "59%",
	}, {
		name: "thirty-four", win: Window{Percent: 34},
		wantUse: 4, wantPct: "34%",
	}, {
		name: "ninety-nine", win: Window{Percent: 99},
		wantUse: cardCapCells, wantPct: "99%",
	}, {
		name: "hundred", win: Window{Percent: 100},
		wantUse: cardCapCells, wantPct: "100%",
	}, {
		name: "used up", win: Window{Status: "used up", Percent: 100},
		wantUse: cardCapCells, wantPct: "100%",
	}, {
		name: "rate limited", win: Window{Status: "rate-limited", Percent: 40},
		wantUse: cardCapCells, wantPct: "40%",
	}, {
		name: "sub-percent fraction", win: Window{Percent: 0, UsedFraction: 0.004},
		wantUse: 1, wantPct: "1%",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.win
			w.Name = "5h"
			w.ResetsAt = &h
			got := RenderCards([]Snapshot{{
				Provider: "claude",
				Accounts: []string{"claude-main"},
				Windows:  []Window{w},
			}}, now)
			// Every line — colored or not — is the card's own width.
			lines := cardLines(t, got)
			if len(lines) != 3 {
				t.Fatalf("the old extra rows are gone: want 3 lines, got %d:\n%q", len(lines), lines)
			}
			bar := rowCapsule(t, lines[1])
			if n := strings.Count(bar, capUsed); n != tc.wantUse {
				t.Fatalf("used cells = %d, want %d: %q", n, tc.wantUse, bar)
			}
			if n := strings.Count(bar, capEmpty); n != cardCapCells-tc.wantUse {
				t.Fatalf("remaining cells = %d, want %d: %q", n, cardCapCells-tc.wantUse, bar)
			}
			if !strings.HasSuffix(lines[1], rowTail(tc.wantPct, "-")) {
				t.Fatalf("row = %q, want tail %q", lines[1], rowTail(tc.wantPct, "-"))
			}
			// The colors off render must be the same layout, escapes stripped.
			plain := RenderCards([]Snapshot{{
				Provider: "claude",
				Accounts: []string{"claude-main"},
				Windows:  []Window{w},
			}}, now, CardOptions{NoColor: true})
			if render.StripANSI(got) != plain {
				t.Fatalf("no-color render is not the colored render minus escapes:\ngot:\n%q\nwant:\n%q",
					plain, render.StripANSI(got))
			}
			if strings.Contains(plain, "\x1b") {
				t.Fatalf("no-color render still carries escapes:\n%q", plain)
			}
		})
	}
}

// TestRenderCardsAllRowsWidthAtEveryPercent is the user's headline complaint —
// "边框被顶出、表格错位". It renders every percentage and every exhaustion path
// at once — colored and not — and asserts EVERY line of every card is exactly
// that card's width, plus the card count (a windowless snapshot and a
// windowless unknown provider each keep ONE card).
func TestRenderCardsAllRowsWidthAtEveryPercent(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3*time.Hour + 12*time.Minute)
	window := func(name string, w Window) Window {
		w.Name = name
		if w.ResetsAt == nil {
			w.ResetsAt = &h
		}
		return w
	}
	snaps := []Snapshot{
		{Provider: "gemini", Accounts: []string{"gemini-acct"}, Windows: []Window{
			window("rolling", Window{Status: "ok", Percent: 0}),
			window("weekly", Window{Status: "ok", Percent: 1}),
			window("monthly", Window{Status: "ok", Percent: 34, LimitTokensEstimate: 3_500_000_000}),
			window("weird-upstream-name", Window{Status: "ok", Percent: 7}),
		}},
		{Provider: "xai", Accounts: []string{"acct"}, Err: "fetch_failed", Windows: []Window{
			window("weekly", Window{Status: "ok", Percent: 99}),
			window("monthly", Window{Status: "used up", Percent: 100, LimitTokensEstimate: 3_500_000_000}),
		}},
		{Provider: "opencode-go", Accounts: []string{"a1"}, Err: "unauthorized"},
		{Provider: "unknown-provider", Accounts: nil, Stale: true},
	}
	for _, noColor := range []bool{false, true} {
		got := RenderCards(snaps, now, CardOptions{NoColor: noColor})
		lines := cardLines(t, got)
		wantCards := 4 + 2 + 1 + 1 // gemini's four windows, xai's two, two windowless
		if n := strings.Count(got, "╭─ "); n != wantCards {
			t.Fatalf("noColor=%v: got %d cards, want %d:\n%s", noColor, n, wantCards, got)
		}
		if len(lines) == 0 {
			t.Fatal("no lines")
		}
	}
}

// TestRenderCardsNoColorStripsEverything renders a mixed snapshot with colors
// off and asserts not a single escape survives while the layout stays
// identical to the colored render.
func TestRenderCardsNoColorStripsEverything(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3*time.Hour + 12*time.Minute)
	snaps := []Snapshot{{
		Provider: "gemini",
		Accounts: []string{"a1"},
		Windows: []Window{
			{Name: "5h", Percent: 34, ResetsAt: &h},
			{Name: "weekly", Status: "used up", Percent: 100, LimitTokensEstimate: 3_500_000_000},
		},
	}}
	colored := RenderCards(snaps, now)
	plain := RenderCards(snaps, now, CardOptions{NoColor: true})
	if strings.Contains(plain, "\x1b") {
		t.Fatalf("no-color render still carries escapes:\n%q", plain)
	}
	if !strings.Contains(colored, "\x1b") {
		t.Fatalf("colored render lost its colors:\n%q", colored)
	}
	if render.StripANSI(colored) != plain {
		t.Fatalf("layout changed between color modes:\ngot:\n%q\nwant:\n%q", plain, render.StripANSI(colored))
	}
	// The width invariant holds in the stripped mode too.
	cardLines(t, plain)
}

// TestRenderCardsExhausted: a drained window shows the solid red capsule, the
// 100 % percentage and the X/X pair — and NOTHING else. The 已达限额 footer is
// gone, so the yellow (#F4A261) the footer used never reaches a card.
func TestRenderCardsExhausted(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderCards([]Snapshot{{
		Provider: "xai",
		Accounts: []string{"acct-1"},
		Windows: []Window{{
			Name: "weekly", Status: "used up", Percent: 100,
			LimitTokensEstimate: 1_700_000_000, MeasuredTokens: 1_700_000_000,
		}},
	}}, now)
	lines := cardLines(t, got)
	if len(lines) != 3 {
		t.Fatalf("want 3 lines (no footer any more), got %d:\n%q", len(lines), lines)
	}

	// The whole capsule is one solid red ▰ run — a drained pill, not a hollow
	// one: hollow would read as "nothing used", the opposite of an exhausted
	// window.
	row := lines[1]
	if n := strings.Count(row, capUsed); n != cardCapCells {
		t.Fatalf("solid red cells = %d, want %d:\n%q", n, cardCapCells, row)
	}
	if strings.ContainsAny(row, capEmpty) {
		t.Fatalf("exhausted capsule must be solid:\n%q", row)
	}
	if !strings.Contains(got, ansiRed+strings.Repeat(capUsed, cardCapCells)+ansiReset) {
		t.Fatalf("red ANSI capsule missing:\n%q", got)
	}
	// The drain state is spelled by the percentage and the X/X pair.
	if !strings.HasSuffix(row, rowTail("100%", "17亿/17亿")) {
		t.Fatalf("drained row tail wrong:\n%q", row)
	}
	for _, bad := range []string{"已达限额", "已耗尽", "限流", "总额"} {
		if strings.Contains(got, bad) {
			t.Fatalf("the drain state must not be spelled with %q:\n%q", bad, got)
		}
	}
	if strings.Contains(got, ansiYellow) {
		t.Fatalf("the yellow footer is gone, its color must not appear on a drained card:\n%q", got)
	}
}

func TestRenderCardsRateLimited(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2*time.Hour + 1*time.Minute)
	got := RenderCards([]Snapshot{{
		Provider: "xai",
		Accounts: []string{"acct"},
		Windows:  []Window{{Name: "5h", Status: "rate-limited", Percent: 50, ResetsAt: &h2}},
	}}, now)
	lines := cardLines(t, got)
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d:\n%q", len(lines), lines)
	}
	// Same determination as the drained token window: a solid red capsule and
	// the honest percentage next to it. The 5-hour window has no pool here, so
	// its token-pair column reads "-" and its countdown rides on the title.
	if !strings.Contains(got, ansiRed+strings.Repeat(capUsed, cardCapCells)+ansiReset) {
		t.Fatalf("rate-limited capsule must be solid red:\n%q", got)
	}
	if !strings.HasSuffix(lines[1], rowTail("50%", "-")) {
		t.Fatalf("rate-limited row tail wrong:\n%q", lines[1])
	}
	if !strings.HasPrefix(lines[0], "╭─ SuperGrok · 5小时限额 · 2小时01分 ") {
		t.Fatalf("the countdown must ride on the title:\n%q", lines[0])
	}
	if strings.Contains(got, "已达限额") || strings.Contains(lines[1], "限流") {
		t.Fatalf("rate-limited window must not claim a footer or a status word:\n%q", got)
	}
}

// The capsule ramp's stop boundaries, its monotonicity and the shared cell
// arithmetic live with the implementation itself, in
// internal/render/capsule_test.go (TestCapsuleRampBoundaries,
// TestCapsuleUsedCells, TestCapsuleLevel). The tests below pin what the QUOTA
// cards do with the shared primitive: the direction of the gradient (a
// consumed share warms toward red) over the card's own 10-cell capsule.
//
// With only 10 cells each one stands for 10 %, so the ramp now reaches its
// stops within a few cells: cell 4 is the 40 % green stop and cell 10 is the
// 100 % red. A high-but-not-exhausted capsule therefore DOES carry a red cell
// — that is the ramp, not the exhaustion signal (the signal is the capsule
// going FULLY solid red, see TestRenderCardsExhausted).
//
// TestCapsuleGradientInBar checks that the ramp really reaches the rendered
// capsule: a healthy bar is flat green, a nearly-full bar warms from green to
// red cell by cell. The ramp stops at 40 % (green) and 100 % (red), and a
// 10-cell capsule puts the first cell at 10 % and the last at 100 %.
func TestCapsuleGradientInBar(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	low := RenderCards([]Snapshot{
		{Provider: "gemini", Accounts: []string{"a"}, Windows: []Window{{Name: "5h", Percent: 34}}},
	}, now)
	if !strings.Contains(low, ansiGreen+strings.Repeat(capUsed, 4)+ansiReset) {
		t.Fatalf("34%% must be one flat green run (4 cells):\n%q", low)
	}
	if strings.Contains(low, ansiRed) || strings.Contains(low, ansiYellow) {
		t.Fatalf("34%% must stay in the green band:\n%q", low)
	}

	high := RenderCards([]Snapshot{
		{Provider: "gemini", Accounts: []string{"a"}, Windows: []Window{{Name: "5h", Percent: 99}}},
	}, now)
	if !strings.Contains(high, ansiGreen+strings.Repeat(capUsed, 4)+ansiReset) {
		t.Fatalf("99%% must start flat green (4 cells):\n%q", high)
	}
	if !strings.Contains(high, ansiRed+capUsed+ansiReset) {
		t.Fatalf("99%% must reach red on its last cell:\n%q", high)
	}
	// The used cells must carry more than one distinct color: the gradient
	// is per cell, not one flat color for the whole bar.
	if runs := strings.Count(high, "\x1b[38;2;"); runs < 4 {
		t.Fatalf("99%% capsule has %d color runs, want a per-cell gradient", runs)
	}
}

// TestCapsuleGlyphWidth guards the capsule's cell arithmetic: ▰ and ▱ are
// both exactly one display column, and the card capsule is 23 of them.
func TestCapsuleGlyphWidth(t *testing.T) {
	if w := render.DisplayWidth(capUsed); w != 1 {
		t.Errorf("DisplayWidth(%q) = %d, want 1", capUsed, w)
	}
	if w := render.DisplayWidth(capEmpty); w != 1 {
		t.Errorf("DisplayWidth(%q) = %d, want 1", capEmpty, w)
	}
	if w := render.DisplayWidth(strings.Repeat(capUsed, cardCapCells)); w != cardCapCells {
		t.Errorf("capsule width = %d, want %d", w, cardCapCells)
	}
}

// TestRenderCardsWindowCards checks that one (provider, window) group is one
// card: every window gets its own title (with its own window label), its own
// row and its own metric, and the account travels with its row.
func TestRenderCardsWindowCards(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3*time.Hour + 12*time.Minute)
	w := now.Add(49 * time.Hour)
	got := RenderCards([]Snapshot{{
		Provider: "gemini",
		Accounts: []string{"gemini"},
		Windows: []Window{
			{Name: "5h", Status: "ok", Percent: 34, ResetsAt: &h},
			{Name: "weekly", Status: "ok", Percent: 94, LimitTokensEstimate: 3_500_000_000, ResetsAt: &w},
		},
	}}, now)
	lines := cardLines(t, got)
	if len(lines) != 7 { // 2 cards × 3 lines + 1 blank separator
		t.Fatalf("want 7 lines, got %d:\n%q", len(lines), lines)
	}
	assertCardTitle(t, lines[0], "Gemini", "5小时限额", "3小时12分")
	assertCardTitle(t, lines[4], "Gemini", "周限额", "2天1小时")

	// 34 % → 4 ▰ of the 10-cell capsule, and the 5-hour window has no pool
	// here, so its token-pair column reads "-".
	five := lines[1]
	if n := strings.Count(rowCapsule(t, five), capUsed); n != 4 {
		t.Fatalf("5h used cells = %d, want 4:\n%q", n, five)
	}
	if !strings.HasSuffix(five, rowTail("34%", "-")) {
		t.Fatalf("5h row tail wrong:\n%q", five)
	}
	// 94 % → 10 ▰ (a solid capsule), and the weekly metric is the token pair
	// (no "~").
	weekly := lines[5]
	if n := strings.Count(rowCapsule(t, weekly), capUsed); n != 10 {
		t.Fatalf("weekly used cells = %d, want 10:\n%q", n, weekly)
	}
	if !strings.HasSuffix(weekly, rowTail("94%", "32.9亿/35亿")) {
		t.Fatalf("weekly row tail wrong:\n%q", weekly)
	}

	// The account identifier is on the ROW of every card and never on a title.
	if n := strings.Count(got, "gemini"); n != 2 {
		t.Fatalf("the account must appear once per row, got %d:\n%s", n, got)
	}
	for _, title := range cardTitleLines(lines) {
		if strings.Contains(title, "gemini") {
			t.Fatalf("account leaked onto a title:\n%q", title)
		}
	}

	// 94 % is NOT exhausted: no SOLID red capsule, no footer, no cycle dots. A
	// single red CELL on the last cell is the ramp, not the drain signal.
	if strings.Contains(got, ansiRed+strings.Repeat(capUsed, cardCapCells)+ansiReset) ||
		strings.Contains(got, "已达限额") {
		t.Fatalf("94%% must not look exhausted:\n%q", got)
	}
	if strings.ContainsAny(got, "○●◆") {
		t.Fatalf("unfabricated extras must stay omitted:\n%q", got)
	}
}

// TestRenderCardsZeroUsedWindow: 0 % used draws an all-hollow capsule — the
// value is readable from the capsule's length alone — and a window with no
// reset instant and no pool reads "-" in the token-pair column and carries
// no countdown on its title.
func TestRenderCardsZeroUsedWindow(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderCards([]Snapshot{{
		Provider: "gemini",
		Accounts: []string{"Gemini"},
		Windows:  []Window{{Name: "5h", Status: "ok", Percent: 0}},
	}}, now)
	lines := cardLines(t, got)
	bar := rowCapsule(t, lines[1])
	if n := strings.Count(bar, capUsed); n != 0 {
		t.Fatalf("0%% used must draw no ▰ at all, got %d: %q", n, bar)
	}
	if n := strings.Count(bar, capEmpty); n != cardCapCells {
		t.Fatalf("remaining cells = %d, want %d: %q", n, cardCapCells, bar)
	}
	if !strings.Contains(got, ansiDim+strings.Repeat(capEmpty, cardCapCells)+ansiReset) {
		t.Fatalf("the whole capsule must be dim gray:\n%q", got)
	}
	// No ResetsAt and no pool: the pair is the honest "-" and the title
	// carries no countdown segment at all.
	if !strings.HasSuffix(lines[1], rowTail("0%", "-")) {
		t.Fatalf("no ResetsAt must render the '-' placeholder:\n%q", lines[1])
	}
	assertCardTitle(t, lines[0], "Gemini", "5小时限额", "")
}

// TestRenderCardsTitleNeverCarriesAccount is the title half of the unified
// contract: an account name — short, equal to the provider display name, or
// very long — is NEVER a title segment, so the account can no longer be
// truncated with an ellipsis and no other title segment can be squeezed by
// it either.
func TestRenderCardsTitleNeverCarriesAccount(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3 * time.Hour)
	cases := []struct{ name, account string }{
		{"short", "a1"},
		{"same as the provider", "Gemini"},
		{"long ascii", "very-long-account-name-that-just-keeps-going-and-going"},
		{"long cjk", strings.Repeat("账", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderCards([]Snapshot{{
				Provider: "gemini",
				Accounts: []string{tc.account},
				Windows:  []Window{{Name: "5h", Status: "ok", Percent: 12, ResetsAt: &h}},
			}}, now, CardOptions{NoColor: true})
			lines := cardLines(t, got) // title + row + border, all one width
			if len(lines) != 3 {
				t.Fatalf("want 3 lines, got %d:\n%s", len(lines), got)
			}
			assertCardTitle(t, lines[0], "Gemini", "5小时限额", "3小时00分")
			// The account is not a title segment: assertCardTitle above proves the
			// title holds nothing but the provider and the window, so the only
			// occurrence left to check is the one of an account whose name differs
			// from the provider's (an equal name IS the provider segment).
			if tc.account != "Gemini" && strings.Contains(lines[0], tc.account) {
				t.Fatalf("the account must stay out of the title: %q", lines[0])
			}
			if strings.Contains(got, "…") {
				t.Fatalf("nothing may be truncated on a card sized from its own content:\n%s", got)
			}
			// The account name renders in FULL on its own row, and the card is
			// as wide as that name + the fixed columns needs — floored by the
			// name column's minimum (a short name no longer narrows the card;
			// see TestRenderCardsUniformWidthAcrossProviders).
			if !strings.Contains(lines[1], clineDisplayName(clineRow{name: tc.account})) {
				t.Fatalf("the account must render in full on its row: %q", lines[1])
			}
			want := wantCardWidth(clineDisplayName(clineRow{name: tc.account}))
			if w := render.DisplayWidth(lines[0]); w != want {
				t.Fatalf("card width = %d, want %d:\n%s", w, want, got)
			}
		})
	}
}

// TestRenderCardsOverLongWindowNameTitle is the title half of the width pits:
// an unknown upstream window name can be arbitrarily long (the known ones are
// short and fixed). The card is sized from the title's own need, so a long
// window name WIDENS the card instead of being truncated, and the fill stays
// dashes-only with the ╮ intact — that old safety net ate the right border and
// could leave the card one column short.
func TestRenderCardsOverLongWindowNameTitle(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2 * time.Hour)
	names := []string{
		strings.Repeat("w", 70),  // ASCII, 70 columns
		strings.Repeat("窗口", 35), // CJK, 140 columns
		"mixed-窗口-" + strings.Repeat("x", 60),
		strings.Repeat("🔥", 40),
	}
	for _, name := range names {
		got := RenderCards([]Snapshot{{
			Provider: "opencode-go",
			Accounts: []string{"acct"},
			Windows:  []Window{{Name: name, Status: "ok", Percent: 7, ResetsAt: &h2}},
		}}, now, CardOptions{NoColor: true})
		// cardLines asserts EVERY line — title included — is the card's width.
		lines := cardLines(t, got)
		title := lines[0]
		if !strings.HasPrefix(title, "╭─ Opus · ") {
			t.Fatalf("window name %q: title head wrong: %q", name, title)
		}
		if !strings.Contains(title, name) {
			t.Fatalf("window name %q: the label must render in full: %q", name, title)
		}
		if !strings.HasSuffix(title, "╮") {
			t.Fatalf("window name %q: the ╮ was cut off: %q", name, title)
		}
		if strings.Contains(title, "…") {
			t.Fatalf("window name %q: the card sizes itself, nothing is truncated: %q", name, title)
		}
		// After the window label: " · <countdown> " and dash fill only, then ╮.
		rest := strings.TrimPrefix(title, "╭─ Opus · "+name)
		rest = strings.TrimPrefix(rest, " · 2小时00分")
		if !strings.HasPrefix(rest, " ") || !strings.Contains(rest, "─") ||
			strings.Trim(rest, " ─╮") != "" {
			t.Fatalf("window name %q: dash fill malformed: %q", name, title)
		}
		// The row is untouched by the long window name and still lines up.
		if !strings.HasPrefix(lines[1], "│ · acct ") || !strings.HasSuffix(lines[1], rowTail("7%", "-")) {
			t.Fatalf("window name %q: row wrong: %q", name, lines[1])
		}
	}
}

// TestRenderCardsProviderSortAndWindowCards pins the card ORDER: accounts of
// one provider+window group into ONE card whose rows follow the report's
// account order, and the provider groups keep the curated order.
func TestRenderCardsProviderSortAndWindowCards(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2 * time.Hour)
	got := RenderCards([]Snapshot{
		{Provider: "opencode-go", Accounts: []string{"acct-z"}, Windows: []Window{
			{Name: "rolling", Status: "ok", Percent: 12, ResetsAt: &h2},
			{Name: "weekly", Status: "ok", Percent: 8},
		}},
		{Provider: "opencode-go", Accounts: []string{"acct-a"}, Windows: []Window{
			{Name: "rolling", Status: "ok", Percent: 1, ResetsAt: &h2},
		}},
	}, now)
	lines := cardLines(t, got)

	titles := cardTitleLines(lines)
	if len(titles) != 2 {
		t.Fatalf("want 2 cards (one per window), got %d:\n%q", len(titles), titles)
	}
	// The first card is the rolling window, carrying BOTH accounts as rows
	// (acct-a first: the report sorts accounts by name).
	assertCardTitle(t, titles[0], "Opus", "5小时限额", "2小时00分")
	assertCardTitle(t, titles[1], "Opus", "周限额", "")
	fiveHour := render.StripANSI(strings.Split(strings.TrimSuffix(got, "\n"), "\n\n")[0])
	card := strings.Split(fiveHour, "\n")
	rows := card[1 : len(card)-1] // drop the title and the bottom border
	if len(rows) != 2 {
		t.Fatalf("want 2 rows on the first card, got %d:\n%s", len(rows), fiveHour)
	}
	if !strings.HasPrefix(rows[0], "│ · acct-a ") ||
		!strings.HasPrefix(rows[1], "│ · acct-z ") {
		t.Fatalf("rows must follow the account order:\n%s", fiveHour)
	}
	if !strings.HasSuffix(rows[0], rowTail("1%", "-")) ||
		!strings.HasSuffix(rows[1], rowTail("12%", "-")) {
		t.Fatalf("each row must carry its own numbers:\n%s", fiveHour)
	}
	// The weekly card holds acct-z only: acct-a has no weekly window.
	weekly := render.StripANSI(strings.Split(strings.TrimSuffix(got, "\n"), "\n\n")[1])
	if n := strings.Count(weekly, "│ · "); n != 1 {
		t.Fatalf("weekly card must hold one row, got %d:\n%s", n, weekly)
	}
	if !strings.Contains(weekly, "│ · acct-z ") {
		t.Fatalf("weekly card must hold acct-z:\n%s", weekly)
	}
	if !strings.HasSuffix(strings.Split(weekly, "\n")[1], rowTail("8%", "-")) {
		t.Fatalf("weekly row tail wrong:\n%s", weekly)
	}
}

// TestCardCountdown pins the Chinese countdown wording the card title uses:
// minutes zero-padded inside a day ("2小时01分"), a day+hour form across days
// ("6天23小时", a whole number of days reads "3天"), "不足1分" below a minute
// and "已重置" once the window has rolled over.
func TestCardCountdown(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		d    time.Duration
		want string
	}{
		{2*time.Hour + 1*time.Minute, "2小时01分"},
		{4*time.Hour + 51*time.Minute, "4小时51分"},
		{2 * time.Hour, "2小时00分"},
		{45 * time.Minute, "45分"},
		{30 * time.Second, "不足1分"},
		{3*24*time.Hour + 4*time.Hour, "3天4小时"},
		{6*24*time.Hour + 23*time.Hour, "6天23小时"},
		{3 * 24 * time.Hour, "3天"},
		{-time.Minute, "已重置"},
	}
	for _, tc := range cases {
		at := now.Add(tc.d)
		if got := cardCountdown(now, at); got != tc.want {
			t.Errorf("%v: got %q want %q", tc.d, got, tc.want)
		}
	}
}

// TestTitleCountdown pins the title's countdown segment: the window-level
// countdown wording, "已重置" once the window has rolled over, and "" when the
// window reported no reset instant at all (a card then has no countdown
// segment).
func TestTitleCountdown(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3*time.Hour + 12*time.Minute)
	past := now.Add(-time.Minute)
	if got := titleCountdown(&h, now); got != "3小时12分" {
		t.Errorf("future reset = %q", got)
	}
	if got := titleCountdown(&past, now); got != "已重置" {
		t.Errorf("elapsed reset = %q", got)
	}
	if got := titleCountdown(nil, now); got != "" {
		t.Errorf("missing reset = %q", got)
	}
	zero := time.Time{}
	if got := titleCountdown(&zero, now); got != "" {
		t.Errorf("zero reset = %q", got)
	}
}

// TestRenderCardsLocalizedCardText pins the Chinese wording of the card
// texts: the title's reset countdown and the fetch-failure note. The data
// layer keeps the English code; the RENDER layer maps it, so the card shows
// Chinese for every mapped code and passes an unmapped one through behind
// the ⚠ prefix. The note carries the ACCOUNT, because the title no longer
// does. No old English string may survive anywhere in a rendered card, and
// the deleted 已达限额 footer must not come back. The countdown is on the
// TITLE only: "3小时12分" appears ONCE per card though the card has one row.
func TestRenderCardsLocalizedCardText(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3*time.Hour + 12*time.Minute)
	past := now.Add(-time.Minute)
	got := RenderCards([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"acct"},
		Err:      "unauthorized",
		Windows:  []Window{{Name: "rolling", Status: "used up", Percent: 100, ResetsAt: &h}},
	}, {
		Provider: "xai",
		Accounts: []string{"b2"},
		Err:      "no_subscription",
		Windows:  []Window{{Name: "weekly", Percent: 12, ResetsAt: &past}},
	}}, now)
	cardLines(t, got) // every row still the card's own width
	for _, want := range []string{"3小时12分", "已重置", "⚠ acct: 未授权", "⚠ b2: 无订阅"} {
		if !strings.Contains(got, want) {
			t.Fatalf("localized card text %q missing:\n%q", want, got)
		}
	}
	// The countdown is window-level: ONE occurrence per card (the title), not
	// one per row.
	if n := strings.Count(got, "3小时12分"); n != 1 {
		t.Fatalf("countdown occurs %d times, want 1 (the title):\n%q", n, got)
	}
	for _, bad := range []string{
		"limit reached", "resets in", "resets now", "unauthorized",
		"no_subscription", "unexpected_status", "fetch_failed", "已达限额",
		"后重置", "3h 12m", "2h 01m", "<1m", "3d 4h", "已到",
	} {
		if strings.Contains(got, bad) {
			t.Fatalf("English or deleted card text %q survived:\n%q", bad, got)
		}
	}

	// The remaining mapped codes, and an unmapped code that must pass
	// through unchanged behind the ⚠ prefix.
	for _, tc := range []struct{ code, want string }{
		{"unexpected_status", "⚠ c3: 上游状态异常"},
		{"fetch_failed", "⚠ c3: 拉取失败"},
		{"timeout", "⚠ c3: timeout"},
	} {
		plain := RenderCards([]Snapshot{{
			Provider: "gemini",
			Accounts: []string{"c3"},
			Err:      tc.code,
		}}, now, CardOptions{NoColor: true})
		if !strings.Contains(plain, tc.want) {
			t.Fatalf("code %q: want %q in card:\n%q", tc.code, tc.want, plain)
		}
		cardLines(t, plain)
	}
}

// TestRenderCardsErrorAndEmpty: a snapshot without windows still shows its
// account — as a ROW, with the metric columns blank — plus the failure note
// (which carries the account attribution) and the border. The 旧 marker of a
// stale snapshot rides in the name cell of the row, and the name column is
// sized from the MARKED cell, so a stale row stays aligned.
func TestRenderCardsErrorAndEmpty(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)

	// Error, no windows: title (no window label) + row + note + bottom.
	got := RenderCards([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"acct"},
		Err:      "unauthorized",
	}}, now, CardOptions{NoColor: true})
	lines := cardLines(t, got)
	assertCardTitle(t, lines[0], "Opus", "", "")
	if len(lines) != 4 {
		t.Fatalf("error card should have 4 lines, got %d:\n%q", len(lines), lines)
	}
	if !strings.HasPrefix(lines[1], "│ · acct ") || !strings.HasSuffix(lines[1], " │") {
		t.Fatalf("windowless row must be the dot + account, padded to the card:\n%q", lines[1])
	}
	if strings.ContainsAny(lines[1], "▰▱%") {
		t.Fatalf("a windowless row has no metric to show:\n%q", lines[1])
	}
	if !strings.Contains(lines[2], "⚠ acct: 未授权") {
		t.Fatalf("error note missing:\n%q", lines[2])
	}

	// No windows, no error: title + row + bottom only, no spurious note.
	got = RenderCards([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"acct"},
	}}, now, CardOptions{NoColor: true})
	lines = cardLines(t, got)
	if len(lines) != 3 {
		t.Fatalf("empty card should have 3 lines, got %d:\n%q", len(lines), lines)
	}
	if strings.Contains(got, "⚠") {
		t.Fatalf("spurious warning note:\n%q", got)
	}

	// Stale + error + window: the stale marker lives in the ROW's name cell
	// (the title no longer holds the account), and the error note rides along.
	h := now.Add(45 * time.Minute)
	got = RenderCards([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"acct"},
		Stale:    true,
		Err:      "fetch_failed",
		Windows:  []Window{{Name: "rolling", Status: "ok", Percent: 3, ResetsAt: &h}},
	}}, now, CardOptions{NoColor: true})
	lines = cardLines(t, got)
	assertCardTitle(t, lines[0], "Opus", "5小时限额", "45分")
	if n := strings.Count(got, "acct 旧"); n != 1 {
		t.Fatalf("stale marker occurs %d times, want 1 (the row's name cell):\n%q", n, got)
	}
	if !strings.HasPrefix(lines[1], "│ · acct 旧 ") {
		t.Fatalf("stale marker must ride in the row's name cell:\n%q", lines[1])
	}
	if !strings.HasSuffix(lines[1], rowTail("3%", "-")) {
		t.Fatalf("window row missing its data:\n%q", lines[1])
	}
	if !strings.Contains(got, "⚠ acct: 拉取失败") {
		t.Fatalf("error note missing from the window card:\n%q", got)
	}
}

// ── the shared palette & window labels ───────────────────────────────────

func TestWindowTitle(t *testing.T) {
	cases := map[string]string{
		"rolling": "5小时限额",
		"5h":      "5小时限额",
		"weekly":  "周限额",
		"monthly": "月限额",
		"custom":  "custom",
		"":        "--",
	}
	for in, want := range cases {
		if got := windowTitle(in); got != want {
			t.Errorf("windowTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAccountColor(t *testing.T) {
	palette := []string{"00FFFF", "FF00FF", "FFFF00", "00FF00", "FF8800", "8800FF"}
	cases := []struct {
		fp   string
		want string
	}{
		{"", ""},
		{"short", ""}, // < 8 chars must not panic
	}
	for _, tc := range cases {
		if got := accountColor(tc.fp); got != tc.want {
			t.Errorf("accountColor(%q) = %q, want %q", tc.fp, got, tc.want)
		}
	}
	// Non-empty 8+ hex fingerprints must return a palette color.
	for _, fp := range []string{"1234567890ABCDEF", "FFFFFFFFFFFFFFFF", "0000000000000000"} {
		c := accountColor(fp)
		found := false
		for _, p := range palette {
			if c == p {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("accountColor(%q) = %q not in palette", fp, c)
		}
	}
}

// ── per-account rows (the merged group shape, now the only shape) ────────
//
// One card per (provider, window) holds one ROW per account. Everything below
// pins that row: the account name (display name + colour), the row identity,
// the X/X of a drained token window and the exact columns.

// clinePassSnap is one ClinePass account's snapshot: one account and its own
// key fingerprint (the shape the poller produces — one snapshot per account
// key), mirroring what the CLI assembles in cmd/prism/quota.go.
func clinePassSnap(fp, account string, windows ...Window) Snapshot {
	s := Snapshot{Provider: "clinepass", Accounts: []string{account}, Windows: windows}
	s.SetAccountFPs([]string{fp})
	return s
}

// TestRenderCardsClinePassMergesSameWindowAccounts is the headline contract of
// the merged shape: two accounts of one plan are two snapshots, and they must
// land in ONE card with exactly TWO rows (never four, never a duplicated name)
// when they share a window. The title carries the provider, the window and
// the countdown, the row carries the account, and no row carries a detail
// text, a 估算池 marker or a 已达限额 footer.
func TestRenderCardsClinePassMergesSameWindowAccounts(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	resets := now.Add(6*24*time.Hour + 23*time.Hour)
	full := Window{Name: "weekly", Status: "used up", Percent: 100, LimitTokensEstimate: 4_200_000_000, MeasuredTokens: 4_200_000_000, ResetsAt: &resets}
	part := Window{Name: "weekly", Status: "ok", Percent: 34, LimitTokensEstimate: 1_000_000_000, ResetsAt: &resets}
	got := RenderCards([]Snapshot{
		clinePassSnap("aaaa1111bbbb2222", "cline-user", full),
		clinePassSnap("cccc3333dddd4444", "cline-user", part),
	}, now)

	if n := strings.Count(got, "╭─ "); n != 1 {
		t.Fatalf("want ONE merged card, got %d:\n%s", n, render.StripANSI(got))
	}
	lines := cardLines(t, got)
	if len(lines) != 4 { // title + 2 rows + bottom border
		t.Fatalf("want 4 lines (title + 2 rows + border), got %d:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "╭─ ClinePass · 周限额 · 6天23小时") {
		t.Fatalf("title must be provider + window + countdown: %q", lines[0])
	}
	if strings.Contains(lines[0], "cline-user") {
		t.Fatalf("the account must not appear on a title: %q", lines[0])
	}

	rows := lines[1:3]
	for i, r := range rows {
		if !strings.HasPrefix(r, "│ · cline-user ") {
			t.Fatalf("row %d must be dot + account name: %q", i, r)
		}
	}
	if !strings.HasSuffix(rows[0], rowTail("100%", "42亿/42亿")) {
		t.Fatalf("drained row wrong: %q", rows[0])
	}
	if !strings.HasSuffix(rows[1], rowTail("34%", "3.4亿/10亿")) {
		t.Fatalf("34%% row wrong: %q", rows[1])
	}

	// The row has no detail text, no marker and no footer. The countdown is on
	// the title, so "已重置" never reaches a card whose window has not rolled.
	for _, bad := range []string{"已用", "总额", "词元", "后重置", "已重置", "估算池", "已达限额", "~"} {
		if strings.Contains(got, bad) {
			t.Fatalf("card must not carry %q:\n%s", bad, render.StripANSI(got))
		}
	}
	if strings.Count(got, "cline-user") != 2 {
		t.Fatalf("the account name must appear exactly once per row:\n%s", render.StripANSI(got))
	}

	// Colour: the dot and the name share the account's palette colour.
	for _, fp := range []string{"aaaa1111bbbb2222", "cccc3333dddd4444"} {
		color := accountColor(fp)
		if color == "" {
			t.Fatalf("fingerprint %q must map to a palette colour", fp)
		}
		if !strings.Contains(got, render.FgFromHex(color, "·")) ||
			!strings.Contains(got, render.FgFromHex(color, "cline-user")) {
			t.Fatalf("dot and name must both carry %s:\n%q", color, got)
		}
	}

	// No-colour output is the same layout minus the escapes.
	plain := RenderCards([]Snapshot{
		clinePassSnap("aaaa1111bbbb2222", "cline-user", full),
		clinePassSnap("cccc3333dddd4444", "cline-user", part),
	}, now, CardOptions{NoColor: true})
	if plain != render.StripANSI(got) {
		t.Fatalf("no-colour render differs from the coloured one\ngot:\n%q\nwant:\n%q", plain, render.StripANSI(got))
	}
}

// TestRenderCardsClinePassExhaustedShowsXOverX pins the exhausted wording: a
// 100 % TOKEN window ALWAYS shows X/X — the measured consumption as both sides
// when the pool is unknown — and never "-".
func TestRenderCardsClinePassExhaustedShowsXOverX(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		w    Window
		want string
	}{
		{
			// The estimate pipeline sets LimitTokensEstimate = MeasuredTokens
			// on a drained window.
			name: "measured pair",
			w:    Window{Name: "weekly", Status: "used up", Percent: 100, LimitTokensEstimate: 3_800_000_000, MeasuredTokens: 3_800_000_000},
			want: "38亿/38亿",
		},
		{
			// Unknown pool: the measured consumption IS the total.
			name: "measured only",
			w:    Window{Name: "monthly", Status: "ok", Percent: 100, MeasuredTokens: 4_200_000_000},
			want: "42亿/42亿",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderCards([]Snapshot{clinePassSnap("aaaa1111bbbb2222", "cline-user", tc.w)}, now)
			lines := cardLines(t, got)
			if len(lines) != 3 {
				t.Fatalf("want 3 lines, got %d:\n%s", len(lines), got)
			}
			if !strings.HasSuffix(lines[1], rowTail("100%", tc.want)) {
				t.Fatalf("row tail = %q, want %q", lines[1], rowTail("100%", tc.want))
			}
			if strings.Contains(lines[1], "  -") || strings.HasSuffix(lines[1], "- │") {
				t.Fatalf("a drained row must not fall back to \"-\": %q", lines[1])
			}
			// The one exhausted signal: the capsule went solid red.
			if !strings.Contains(got, ansiRed+strings.Repeat(capUsed, clineCapCells)+ansiReset) {
				t.Fatalf("drained capsule must be solid red:\n%q", got)
			}
		})
	}
}

// TestRenderCardsClinePassRowIdentity pins the row identity rules: the row is
// keyed by (window, account name, fingerprint), so an account whose name list
// is repeated across snapshots is still ONE row (the duplicated roster is what
// used to double a 2-account card into 4 rows), while two DIFFERENT accounts
// that happen to share a name stay two rows.
func TestRenderCardsClinePassRowIdentity(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	w := Window{Name: "weekly", Status: "ok", Percent: 34, LimitTokensEstimate: 10_000_000}

	rowsOf := func(t *testing.T, snaps []Snapshot) int {
		t.Helper()
		lines := cardLines(t, RenderCards(snaps, now))
		if len(lines) < 3 {
			t.Fatalf("no rows:\n%v", lines)
		}
		return len(lines) - 2 // title + bottom border
	}

	// Two accounts with the same name and different fingerprints: 2 rows.
	if n := rowsOf(t, []Snapshot{
		clinePassSnap("aaaa1111bbbb2222", "cline-user", w),
		clinePassSnap("cccc3333dddd4444", "cline-user", w),
	}); n != 2 {
		t.Fatalf("same-name accounts = %d rows, want 2", n)
	}

	// The SAME account listed by two snapshots (a roster repeated across
	// snapshots): still 2 rows, not 4 — the old doubling regression.
	if n := rowsOf(t, []Snapshot{
		clinePassSnap("aaaa1111bbbb2222", "cline-user", w),
		clinePassSnap("cccc3333dddd4444", "other-user", w),
		clinePassSnap("aaaa1111bbbb2222", "cline-user", w),
		clinePassSnap("cccc3333dddd4444", "other-user", w),
	}); n != 2 {
		t.Fatalf("repeated roster = %d rows, want 2", n)
	}

	// No fingerprints at all: two same-name accounts are indistinguishable, so
	// the row is keyed by position and both stay visible.
	if n := rowsOf(t, []Snapshot{
		{Provider: "clinepass", Accounts: []string{"cline-user"}, Windows: []Window{w}},
		{Provider: "clinepass", Accounts: []string{"cline-user"}, Windows: []Window{w}},
	}); n != 2 {
		t.Fatalf("fingerprint-less same-name accounts = %d rows, want 2", n)
	}

	// One snapshot with two accounts sharing one key: two rows.
	if n := rowsOf(t, []Snapshot{
		{Provider: "clinepass", Accounts: []string{"cline-user", "other-user"}, Windows: []Window{w}},
	}); n != 2 {
		t.Fatalf("two accounts of one snapshot = %d rows, want 2", n)
	}
}

// TestRenderCardsDisplayNameDropsNumericSuffix pins the DISPLAY name of a row:
// the trailing pure-digit suffix is dropped, so the two accounts of one plan
// read as ONE name (Cline and Cline2 both show "Cline"). Stripping is
// display-only: the row identity (clineRowID) still keys on the FULL name plus
// the fingerprint, so the pair stays exactly TWO rows (also without a
// fingerprint, where the key falls back to the position), and the dot plus the
// name colour are what tells the two same-reading rows apart. The card is as
// wide as the DISPLAY name, so the dropped suffix reserves no invisible
// column.
func TestRenderCardsDisplayNameDropsNumericSuffix(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	w := Window{Name: "weekly", Status: "ok", Percent: 34, LimitTokensEstimate: 10_000_000}
	fps := []string{"aaaa1111bbbb2222", "cccc3333dddd4444"}

	got := RenderCards([]Snapshot{
		clinePassSnap(fps[0], "Cline", w),
		clinePassSnap(fps[1], "Cline2", w),
	}, now)

	if n := strings.Count(got, "╭─ "); n != 1 {
		t.Fatalf("want ONE merged card, got %d:\n%s", n, render.StripANSI(got))
	}
	lines := cardLines(t, got)
	if len(lines) != 4 { // title + 2 rows + bottom border
		t.Fatalf("want 4 lines (title + 2 rows + border), got %d:\n%s", len(lines), render.StripANSI(got))
	}
	rows := lines[1:3]
	for i, r := range rows {
		// The prefix ends with a SPACE, so no digit can sit right after the
		// name: the suffix is gone, not merely moved.
		if !strings.HasPrefix(r, "│ · Cline ") {
			t.Fatalf("row %d must read the suffix-free name Cline: %q", i, r)
		}
	}
	// Two rows, one visible text: after the strip there is nothing but the
	// colour left to tell them apart (the row count is unchanged because the
	// identity still keys on the full name + fingerprint).
	if rows[0] != rows[1] {
		t.Fatalf("the two rows must share one visible text:\n%q\n%q", rows[0], rows[1])
	}
	for _, gone := range []string{"Cline2", "Cline1"} {
		if strings.Contains(got, gone) {
			t.Fatalf("the digit suffix must not reach the card (%q found):\n%s", gone, render.StripANSI(got))
		}
	}
	// Same text, different colour: each account keeps its own palette colour
	// on BOTH the dot and the name, which is what tells the pair apart.
	colors := map[string]bool{}
	for _, fp := range fps {
		color := accountColor(fp)
		if color == "" {
			t.Fatalf("fingerprint %q must map to a palette colour", fp)
		}
		if !strings.Contains(got, render.FgFromHex(color, "·")) ||
			!strings.Contains(got, render.FgFromHex(color, "Cline")) {
			t.Fatalf("dot and name must both carry %s:\n%q", color, got)
		}
		colors[color] = true
	}
	if len(colors) != 2 {
		t.Fatalf("the two same-reading rows must carry different colours: %v", colors)
	}
	// The card is as wide as its longest DISPLAY name, not the full one —
	// floored by the name column's minimum.
	if want := wantCardWidth("Cline"); render.DisplayWidth(lines[0]) != want {
		t.Fatalf("card width = %d, want %d (no column for the dropped suffix):\n%s",
			render.DisplayWidth(lines[0]), want, render.StripANSI(got))
	}

	// No fingerprint at all: the position-keyed rows still stay TWO, and both
	// read the same stripped name (plain · and an uncoloured name).
	plain := RenderCards([]Snapshot{
		{Provider: "clinepass", Accounts: []string{"Cline1"}, Windows: []Window{w}},
		{Provider: "clinepass", Accounts: []string{"Cline2"}, Windows: []Window{w}},
	}, now, CardOptions{NoColor: true})
	plainLines := cardLines(t, plain)
	if len(plainLines) != 4 {
		t.Fatalf("fingerprint-less pair: want 4 lines (2 rows), got %d:\n%s", len(plainLines), plain)
	}
	for i, r := range plainLines[1:3] {
		if !strings.HasPrefix(r, "│ · Cline ") {
			t.Fatalf("fingerprint-less row %d must read the stripped name: %q", i, r)
		}
	}
	if plainLines[1] != plainLines[2] {
		t.Fatalf("fingerprint-less rows must share one visible text:\n%q\n%q", plainLines[1], plainLines[2])
	}
}

// TestRenderCardsDisplayNameEveryProvider: the display-name contract is ONE
// contract, so a NON-ClinePass account with a trailing digit run reads the
// same stripped name a ClinePass one does (the hyphen is not a digit and
// stays: "gemini-acct-1" → "gemini-acct-"). The name is display-only; the row
// identity keeps the full name plus the fingerprint.
func TestRenderCardsDisplayNameEveryProvider(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	w := Window{Name: "weekly", Status: "ok", Percent: 34, LimitTokensEstimate: 10_000_000}
	got := RenderCards([]Snapshot{{
		Provider: "gemini",
		Accounts: []string{"gemini-acct-1"},
		Windows:  []Window{w},
	}}, now, CardOptions{NoColor: true})
	lines := cardLines(t, got)
	if !strings.HasPrefix(lines[1], "│ · gemini-acct- ") {
		t.Fatalf("the trailing digit run must be dropped (%q):\n%s", "gemini-acct-1", got)
	}
	if want := wantCardWidth("gemini-acct-"); render.DisplayWidth(lines[0]) != want {
		t.Fatalf("card width = %d, want %d:\n%s", render.DisplayWidth(lines[0]), want, got)
	}
}

// TestStripNumericSuffix pins the display-name transform and its all-digit
// fallback: a trailing pure-digit suffix is dropped, a digit run in the MIDDLE
// of a name is not touched, and a name that is ONLY digits keeps its name.
// Returning "" there would blank the account column of the row (nothing but
// the colour dot left), so the fallback is part of the contract, not an
// accident of the loop.
func TestStripNumericSuffix(t *testing.T) {
	cases := map[string]string{
		"Cline":       "Cline",
		"Cline1":      "Cline",
		"Cline2":      "Cline",
		"account123":  "account",
		"Cline-1":     "Cline-", // only the trailing digits go; the hyphen stays
		"cline-user2": "cline-user",
		"Cline1x":     "Cline1x", // the digit run is not trailing
		"deepseek-v2": "deepseek-v",
		"":            "",
		"12345":       "12345", // all digits: keep the name, never strip to ""
		"0":           "0",
	}
	for in, want := range cases {
		if got := stripNumericSuffix(in); got != want {
			t.Errorf("stripNumericSuffix(%q) = %q, want %q", in, got, want)
		}
	}

	// End to end: an all-digit account name must still render its name on the
	// row (dot + name), not a dot followed by blanks.
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	w := Window{Name: "weekly", Status: "ok", Percent: 34, LimitTokensEstimate: 10_000_000}
	got := RenderCards([]Snapshot{clinePassSnap("aaaa1111bbbb2222", "12345", w)}, now)
	lines := cardLines(t, got)
	if !strings.HasPrefix(lines[1], "│ · 12345 ") {
		t.Fatalf("an all-digit account name must stay visible: %q", lines[1])
	}
}

// TestRenderCardsClinePassPlainTokenPairAndFiveHourCountdown pins the two
// display rules of the metric column and where the countdown went:
//
//   - a weekly/monthly row shows the used/total pair in 亿 units PLAINLY: no
//     "~" prefix on the total and no 估算池 segment on the title, even though
//     the pool is the inference ApplyClinePassEstimates derives (an inferred
//     total is still the only total that window has);
//   - the COUNTDOWN moved to the TITLE (" · 2小时15分") and is no longer
//     repeated per row;
//   - a 5-hour row WITHOUT a pool (magpie log unreadable / a fresh window) shows
//     "-" in the token-pair column instead of a countdown, and WITH a pool it
//     shows its own reversal — 5h is a token window like the other two.
func TestRenderCardsClinePassPlainTokenPairAndFiveHourCountdown(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2*time.Hour + 15*time.Minute)

	weekly := Window{Name: "weekly", Status: "ok", Percent: 20, LimitTokensEstimate: 2_000_000_000, ResetsAt: &reset}
	monthly := Window{Name: "monthly", Status: "ok", Percent: 60, LimitTokensEstimate: 4_000_000_000, ResetsAt: &reset}
	got := RenderCards([]Snapshot{clinePassSnap("aaaa1111bbbb2222", "Cline", weekly, monthly)}, now, CardOptions{NoColor: true})

	for _, want := range []string{
		"╭─ ClinePass · 周限额 · 2小时15分 ",
		"╭─ ClinePass · 月限额 · 2小时15分 ",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("pool card title missing %q:\n%s", want, got)
		}
	}
	for _, want := range []string{rowTail("20%", "4亿/20亿"), rowTail("60%", "24亿/40亿")} {
		if !strings.Contains(got, want) {
			t.Fatalf("token pair %q missing:\n%s", want, got)
		}
	}
	for _, gone := range []string{"~", "估算池"} {
		if strings.Contains(got, gone) {
			t.Fatalf("%q must be gone from the cards:\n%s", gone, got)
		}
	}
	// The countdown is on the TITLE, once per card — never on a row.
	if n := strings.Count(got, "2小时15分"); n != 2 {
		t.Fatalf("countdown occurs %d times, want 2 (one title per card):\n%s", n, got)
	}

	// The 5-hour window with no pool: the row reads "-" and the countdown is
	// the title's job.
	fiveHour := Window{Name: "5h", Status: "ok", Percent: 12, ResetsAt: &reset}
	got = RenderCards([]Snapshot{clinePassSnap("aaaa1111bbbb2222", "Cline", fiveHour)}, now, CardOptions{NoColor: true})
	lines := cardLines(t, got)
	if !strings.HasPrefix(lines[0], "╭─ ClinePass · 5小时限额 · 2小时15分 ") {
		t.Fatalf("the 5-hour card title is wrong: %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], rowTail("12%", "-")) {
		t.Fatalf("a pool-less 5h row must read '-': %q", lines[1])
	}
	// ...and WITH its own reversal the 5h row shows its own pair, like the
	// weekly and monthly rows do.
	fiveHourPooled := Window{Name: "5h", Status: "ok", Percent: 12, LimitTokensEstimate: 400_000_000, ResetsAt: &reset}
	got = RenderCards([]Snapshot{clinePassSnap("aaaa1111bbbb2222", "Cline", fiveHourPooled)}, now, CardOptions{NoColor: true})
	lines = cardLines(t, got)
	if !strings.HasSuffix(lines[1], rowTail("12%", "0.5亿/4亿")) {
		t.Fatalf("a 5h window with a pool must show its own pair: %q", lines[1])
	}
}

// TestRenderCardsLongNameWidensCard: the account name is never truncated — a
// long name widens the whole card, and every row of that card stays aligned. A
// long name must not leak onto the title either.
func TestRenderCardsLongNameWidensCard(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	long := strings.Repeat("账", 20) // 40 display columns
	w := Window{Name: "weekly", Status: "ok", Percent: 34, LimitTokensEstimate: 10_000_000}
	got := RenderCards([]Snapshot{
		clinePassSnap("aaaa1111bbbb2222", "ab", w),
		clinePassSnap("cccc3333dddd4444", long, w),
	}, now)

	lines := cardLines(t, got) // every line of the render shares one width
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %d:\n%s", len(lines), render.StripANSI(got))
	}
	wantWidth := wantCardWidth(long)
	if w := render.DisplayWidth(lines[0]); w != wantWidth {
		t.Fatalf("card width = %d, want %d:\n%s", w, wantWidth, render.StripANSI(got))
	}
	if !strings.Contains(lines[2], long) {
		t.Fatalf("the long account name must render in full on its row: %q", lines[2])
	}
	if strings.Contains(got, "…") {
		t.Fatalf("rows must never be truncated:\n%s", render.StripANSI(got))
	}
	if strings.Contains(lines[0], long) {
		t.Fatalf("the account name must stay out of the title: %q", lines[0])
	}
}

// TestRenderCardsDegradesWithoutFingerprint pins the palette's fallback: an
// empty or short fingerprint renders the plain · and an uncoloured name
// instead of reading past the fingerprint (the old out-of-range read), so it
// must not panic and must not carry colour.
func TestRenderCardsDegradesWithoutFingerprint(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	w := Window{Name: "weekly", Status: "ok", Percent: 34, LimitTokensEstimate: 10_000_000}
	for _, tc := range []struct{ name, fp string }{{"empty", ""}, {"short", "abc123"}} {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderCards([]Snapshot{clinePassSnap(tc.fp, "cline-user", w)}, now)
			lines := cardLines(t, got)
			if !strings.HasPrefix(lines[1], "│ · cline-user ") {
				t.Fatalf("want a plain · and an uncoloured name: %q", lines[1])
			}
			if !strings.Contains(got, "│ ") {
				t.Fatalf("border missing:\n%q", got)
			}
			for _, color := range []string{"00FFFF", "FF00FF", "FFFF00", "00FF00", "FF8800", "8800FF"} {
				if strings.Contains(got, render.FgFromHex(color, "·")) ||
					strings.Contains(got, render.FgFromHex(color, "cline-user")) {
					t.Fatalf("fingerprint %q must not be coloured:\n%q", tc.fp, got)
				}
			}
		})
	}
}

// TestRenderCardsStaleMarkerRidesInTheRow: a stale snapshot keeps its 旧
// marker — the title no longer holds the account, so the marker moved into the
// row's name cell, and the name column is sized from the MARKED cell, so the
// row keeps its columns.
func TestRenderCardsStaleMarkerRidesInTheRow(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	fresh := Snapshot{Provider: "clinepass", Accounts: []string{"Cline"}, Stale: false,
		Windows: []Window{{Name: "5h", Percent: 34, ResetsAt: &reset}}}
	stale := Snapshot{Provider: "clinepass", Accounts: []string{"ClineOther"}, Stale: true,
		Windows: []Window{{Name: "5h", Percent: 34, ResetsAt: &reset}}}
	got := RenderCards([]Snapshot{fresh, stale}, now, CardOptions{NoColor: true})
	lines := cardLines(t, got)
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %d:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[1], "│ · Cline ") {
		t.Fatalf("the fresh row is wrong: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "│ · ClineOther 旧 ") {
		t.Fatalf("the stale row must carry the 旧 marker in its name cell: %q", lines[2])
	}
	// The marked cell sizes the name column, so both rows still line up.
	if !strings.HasSuffix(lines[1], rowTail("34%", "-")) ||
		!strings.HasSuffix(lines[2], rowTail("34%", "-")) {
		t.Fatalf("the two rows must share their columns:\n%q\n%q", lines[1], lines[2])
	}
	// The countdown is window-level: on the title, once, not on either row.
	assertCardTitle(t, lines[0], "ClinePass", "5小时限额", "2小时00分")
	if n := strings.Count(got, "2小时00分"); n != 1 {
		t.Fatalf("countdown occurs %d times, want 1 (the title):\n%s", n, got)
	}
	want := wantCardWidth("ClineOther 旧")
	if w := render.DisplayWidth(lines[0]); w != want {
		t.Fatalf("card width = %d, want %d (the marked cell sizes the column):\n%s", w, want, got)
	}
}

// TestRenderCardsUniformWidthAcrossProviders is the headline contract of the
// shared width: a render holding THREE providers — short account names next to
// a 9-column one ("SuperGrok", the longest name this package knows) — gives
// every card ONE width, and that width is the name column's floor (45
// columns), not each card's own longest name. Under the old per-card layout the
// three cards came out at their own widths instead — 54 / 55 / 58 columns at the
// time — so the right border and the
// capsule start of each card sat at a different column.
func TestRenderCardsUniformWidthAcrossProviders(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	fiveHour := func() Window {
		return Window{Name: "5h", Status: "ok", Percent: 34, ResetsAt: &reset}
	}
	// One card per provider (a different PROVIDER each, so no two snapshots
	// can merge), with name cells of 6, 8 and 9 columns — every one of them
	// SHORT enough for the floor to decide the width.
	snaps := []Snapshot{
		clinePassSnap("aaaa1111bbbb2222", "cline-1", fiveHour()), // displays "cline-", 6 columns
		{Provider: "gemini", Accounts: []string{"gemini-x"}, Windows: []Window{fiveHour()}},
		{Provider: "xai", Accounts: []string{"SuperGrok"}, Windows: []Window{fiveHour()}},
	}
	got := RenderCards(snaps, now, CardOptions{NoColor: true})
	lines := cardLines(t, got) // ALL lines of ALL cards share one width
	titles := cardTitleLines(lines)
	if len(titles) != 3 {
		t.Fatalf("want 3 cards, got %d:\n%s", len(titles), got)
	}

	// Every card's border line — and every other line of it — is exactly 45
	// display columns wide.
	for i, title := range titles {
		if w := render.DisplayWidth(title); w != 45 {
			t.Fatalf("card %d title: width %d, want 45:\n%q", i, w, title)
		}
	}
	for i, l := range lines {
		if l == "" {
			continue
		}
		if w := render.DisplayWidth(l); w != 45 {
			t.Fatalf("line %d: width %d, want the shared 45:\n%q", i, w, l)
		}
	}
	// The width is the floor the common case is pinned at, and it is NOT the
	// numeric coincidence of one card's own name.
	if want := wantCardWidth("cline-", "gemini-x", "SuperGrok"); want != 45 {
		t.Fatalf("the three name cells must ask for the 45-column floor, got %d", want)
	}

	// The floor decides the width on its own as soon as the roster holds only
	// SHORT names: the same render minus the 9-column account still comes out
	// 45 columns wide — the width does not follow the longest name down, so
	// the same command cannot jump between widths from run to run.
	shortOnly := RenderCards(snaps[:2], now, CardOptions{NoColor: true})
	for i, l := range cardLines(t, shortOnly) {
		if l == "" {
			continue
		}
		if w := render.DisplayWidth(l); w != 45 {
			t.Fatalf("short-name-only render, line %d: width %d, want the 45-column floor:\n%q", i, w, l)
		}
	}

	// Every row's capsule starts at the SAME display column, on every card:
	// that is what the padded name column buys. The rows are the lines that
	// carry a capsule cell.
	start := -1
	rows := 0
	for _, l := range lines {
		i := strings.Index(l, capUsed)
		if i < 0 {
			continue // title / border / note lines carry no capsule
		}
		rows++
		if c := render.DisplayWidth(l[:i]); start == -1 {
			start = c
		} else if c != start {
			t.Fatalf("capsule starts at column %d, want the shared %d:\n%q", c, start, l)
		}
	}
	if rows != 3 {
		t.Fatalf("want 3 data rows (one per card), got %d:\n%s", rows, got)
	}
	// "│ " + dot + " " + the 9-column name cell + " " is 14 columns, i.e.
	// width - clineRowFixed + 1.
	if wantStart := 45 - clineRowFixed + 1; start != wantStart {
		t.Fatalf("capsule starts at column %d, want %d", start, wantStart)
	}

	// The short names are padded, never shortened, and every name still reads
	// in full on its own row.
	for _, name := range []string{"cline-", "gemini-x", "SuperGrok"} {
		if !strings.Contains(got, "│ · "+name+" ") {
			t.Fatalf("row for %q missing:\n%s", name, got)
		}
	}
	if strings.Contains(got, "…") {
		t.Fatalf("nothing may be truncated:\n%s", got)
	}

	// The WIDEST row a card can hold lands exactly ON the card's width: a
	// 100 % share ("100%", clinePctWidth columns) next to the widest token pair
	// a window can print — a pair exactly as wide as clineNumberWidth — fills
	// BOTH reserved segments completely, so the row needs no pad at all
	// and the right border sits where the short rows put it. That is the bound
	// the reservations exist for: every shorter row is padded inside them to
	// this same width.
	widest := RenderCards([]Snapshot{{
		Provider: "gemini",
		Accounts: []string{"gemini-x"},
		Windows: []Window{{
			Name: "weekly", Status: "used up", Percent: 100,
			LimitTokensEstimate: 999_900_000_000,
		}},
	}}, now, CardOptions{NoColor: true})
	widestLines := cardLines(t, widest)
	if w, cardW := render.DisplayWidth(widestLines[1]), render.DisplayWidth(widestLines[0]); w != cardW {
		t.Fatalf("the widest row (100%% + a 13-column pair) is %d columns, want the card's %d:\n%s", w, cardW, widest)
	}
	if !strings.HasSuffix(widestLines[1], rowTail("100%", "9999亿/9999亿")) {
		t.Fatalf("the widest row must end with the zero-fill tail %q:\n%q",
			rowTail("100%", "9999亿/9999亿"), widestLines[1])
	}
}

// TestRenderCardsNameColumnFloorWidensEveryCard: the floor (clineNameColMin) is
// a MINIMUM, never a cap. An account name LONGER than it widens EVERY card of
// the render — the short card grows with the long one, so the cards stay
// exactly as wide as each other — and the long name is still rendered in full.
func TestRenderCardsNameColumnFloorWidensEveryCard(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	long := "SuperGrok-extra-long-name" // 25 columns, far past the 9-column floor
	fiveHour := Window{Name: "5h", Status: "ok", Percent: 34, ResetsAt: &reset}
	snaps := []Snapshot{
		clinePassSnap("aaaa1111bbbb2222", "Cline-1", fiveHour), // displays "Cline-", 6 columns
		{Provider: "xai", Accounts: []string{long}, Windows: []Window{fiveHour}},
	}
	got := RenderCards(snaps, now, CardOptions{NoColor: true})
	lines := cardLines(t, got) // both cards, one width
	titles := cardTitleLines(lines)
	if len(titles) != 2 {
		t.Fatalf("want 2 cards, got %d:\n%s", len(titles), got)
	}
	want := 4 + clineRowFixed + render.DisplayWidth(long)
	if want <= 4+clineRowFixed+clineNameColMin {
		t.Fatalf("the test name must exceed the floor: %d", want)
	}
	for i, title := range titles {
		if w := render.DisplayWidth(title); w != want {
			t.Fatalf("card %d title: width %d, want %d (every card widens together):\n%q",
				i, w, want, title)
		}
	}
	if !strings.Contains(got, "│ · "+long+" ") {
		t.Fatalf("the long account name must render in full:\n%s", got)
	}
	if strings.Contains(got, "…") {
		t.Fatalf("rows must never be truncated:\n%s", got)
	}
}
