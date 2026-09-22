package planusage

import (
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

// ── card TUI tests (流光胶囊 / streaming capsule) ───────────────────────
//
// These tests guard the layout invariants: every card line is EXACTLY 60
// display columns (ANSI stripped) at every percentage the user can hit
// (0, 1, 34, 59, 99, 100) and for the exhaustion paths (≥100 %, used up,
// rate-limited); the capsule is ▰ (used) + ▱ (remaining) with a per-cell
// green→yellow→red ramp and a solid red capsule for exhausted windows;
// the title is Brand(service) + Dim(account) + Brand(window label) with a
// right-only dash fill; the account never leaks into the bar or detail
// rows; no-color output is the same layout minus the escapes; and no
// cycle dots are invented.

const (
	ansiBrandCyan = "\x1b[38;2;0;180;216m"
	ansiGreen     = "\x1b[38;2;82;183;136m"
	ansiYellow    = "\x1b[38;2;244;162;97m" // #F4A261: ramp stop + footer
	ansiRed       = "\x1b[38;2;230;57;70m"
	ansiDim       = "\x1b[38;2;102;102;102m"
	cardLineWidth = 60
	cardBarCells  = 51 // capsule = cardInner(56) − indent(0) − pct(4) − gap(1)
	ansiReset     = "\x1b[0m"
)

// cardLines splits a card render into lines and asserts every non-empty
// line is exactly cardLineWidth display columns wide (ANSI counted as 0).
// Lines are returned ANSI-stripped.
func cardLines(t *testing.T, got string) []string {
	t.Helper()
	raw := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	out := make([]string, 0, len(raw))
	for i, l := range raw {
		if l == "" {
			out = append(out, "")
			continue // blank separator between cards
		}
		plain := render.StripANSI(l)
		if w := render.DisplayWidth(plain); w != cardLineWidth {
			t.Fatalf("line %d: width %d, want %d:\n%q", i, w, cardLineWidth, plain)
		}
		out = append(out, plain)
	}
	return out
}

// assertCardTitle checks a stripped title line: "╭─ <service> <account> ·
// <window>" followed by one space, ONLY dash fill, then "╮". No left-side
// fill, no ╶, and the window label is never truncated.
func assertCardTitle(t *testing.T, line, service, account, window string) {
	t.Helper()
	head := "╭─ " + service
	if account != "" {
		head += " " + account
	}
	if window != "" {
		head += " · " + window
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

// barArea extracts the capsule zone (the last cardBarCells runes) of a
// stripped bar row. Only ▰ and ▱ appear there. The trailing percentage
// label is trimmed first, so the helper does not depend on the label's
// width.
func barArea(t *testing.T, row string) string {
	t.Helper()
	rest := []rune(strings.TrimRight(strings.TrimSuffix(row, " │"), " %0123456789"))
	if len(rest) < cardBarCells {
		t.Fatalf("row too short for a capsule: %q", row)
	}
	return string(rest[len(rest)-cardBarCells:])
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

	// Title: brand service + dim account + brand window label + right-only
	// dash fill.
	assertCardTitle(t, lines[0], "Claude", "claude-main", "5小时限额")
	if !strings.Contains(got, ansiBrandCyan+"Claude"+ansiReset) {
		t.Fatalf("cyan service name missing:\n%q", got)
	}
	if !strings.Contains(got, ansiDim+"claude-main"+ansiReset) {
		t.Fatalf("dim account missing:\n%q", got)
	}
	if !strings.Contains(got, ansiBrandCyan+"5小时限额"+ansiReset) {
		t.Fatalf("cyan window label missing:\n%q", got)
	}
	if strings.Contains(got, "╶") {
		t.Fatalf("left-side title fill artifact (╶) present:\n%q", got)
	}

	// Bar row: the capsule, one space, right-aligned percent.
	row := lines[1]
	if !strings.HasPrefix(row, "│ ") {
		t.Fatalf("bar row must start with the border gutter (no indent of its own):\n%q", row)
	}
	if !strings.HasSuffix(row, " 59% │") {
		t.Fatalf("bar row must end with the right-aligned pct:\n%q", row)
	}
	// The capsule's FILLED LENGTH is the used share: 59 % → 31 ▰ + 20 ▱.
	bar := barArea(t, row)
	if n := strings.Count(bar, capUsed); n != 31 {
		t.Fatalf("used cells = %d, want 31: %q", n, bar)
	}
	if n := strings.Count(bar, capEmpty); n != cardBarCells-31 {
		t.Fatalf("remaining cells = %d, want %d: %q", n, cardBarCells-31, bar)
	}
	// The used run keeps the healthy band flat green; the rest is dim gray.
	if !strings.Contains(got, ansiGreen+strings.Repeat(capUsed, 20)+ansiReset) {
		t.Fatalf("flat green used run missing:\n%q", got)
	}
	if !strings.Contains(got, ansiDim+strings.Repeat(capEmpty, 20)+ansiReset) {
		t.Fatalf("dim remaining run missing:\n%q", got)
	}
	if strings.Contains(got, ansiRed) {
		t.Fatalf("59 %% is not exhausted, red must not appear:\n%q", got)
	}

	// Detail row: used/total left, reset countdown right.
	detail := lines[2]
	if !strings.HasPrefix(detail, "│ 已用 59%") {
		t.Fatalf("detail left text wrong:\n%q", detail)
	}
	if !strings.HasSuffix(detail, "resets in 2h 01m │") {
		t.Fatalf("detail right text wrong:\n%q", detail)
	}

	// No invented cycle dots, no exhausted footer.
	if strings.ContainsAny(got, "○●◆") {
		t.Fatalf("cycle dots must not be fabricated:\n%q", got)
	}
	if strings.Contains(got, "limit reached") {
		t.Fatalf("exhausted footer must not appear:\n%q", got)
	}

	// Bottom border: ╰ + 58 dashes + ╯.
	bottom := lines[len(lines)-1]
	if !strings.HasPrefix(bottom, "╰") || !strings.HasSuffix(bottom, "╯") ||
		strings.Count(bottom, "─") != cardLineWidth-2 {
		t.Fatalf("bottom border malformed:\n%q", bottom)
	}
}

// TestRenderCardsCapsuleGeometry walks every percentage the user can hit
// — including the exhausted paths and a sub-percent window refined through
// UsedFraction — and checks the capsule length, the percentage label, the
// detail text and the 60-column invariant in one go.
func TestRenderCardsCapsuleGeometry(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3*time.Hour + 12*time.Minute)
	cases := []struct {
		name        string
		win         Window
		wantUsed    int
		wantPct     string
		wantDetail  string
		wantExhaust bool
	}{{
		name: "zero", win: Window{Percent: 0},
		wantUsed: 0, wantPct: "  0%", wantDetail: "已用 0%",
	}, {
		name: "one", win: Window{Percent: 1},
		wantUsed: 1, wantPct: "  1%", wantDetail: "已用 1%",
	}, {
		name: "thirty-four", win: Window{Percent: 34},
		wantUsed: 18, wantPct: " 34%", wantDetail: "已用 34%",
	}, {
		name: "fifty-nine", win: Window{Percent: 59},
		wantUsed: 31, wantPct: " 59%", wantDetail: "已用 59%",
	}, {
		name: "ninety-nine", win: Window{Percent: 99},
		wantUsed: 51, wantPct: " 99%", wantDetail: "已用 99%",
	}, {
		name: "hundred", win: Window{Percent: 100},
		wantUsed: 51, wantPct: "100%", wantDetail: "已用 100%", wantExhaust: true,
	}, {
		name: "used up", win: Window{Status: "used up", Percent: 100},
		wantUsed: 51, wantPct: "100%", wantDetail: "已耗尽 100%", wantExhaust: true,
	}, {
		name: "rate limited", win: Window{Status: "rate-limited", Percent: 40},
		wantUsed: 51, wantPct: " 40%", wantDetail: "限流 40%", wantExhaust: true,
	}, {
		name: "sub-percent fraction", win: Window{Percent: 0, UsedFraction: 0.004},
		wantUsed: 1, wantPct: "  1%", wantDetail: "已用 1%",
	}, {
		name: "token pool", win: Window{Percent: 34, LimitTokensEstimate: 3_500_000},
		wantUsed: 18, wantPct: " 34%", wantDetail: "已用 34% / 总额 3.5M tok",
	}, {
		name: "dollar estimate", win: Window{Percent: 12, LimitUSDEstimate: 60, USDStatus: "estimated"},
		wantUsed: 7, wantPct: " 12%", wantDetail: "额度 12% / $60.00",
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
			// Every line — colored or not — is exactly 60 columns.
			lines := cardLines(t, got)
			wantLines := 4 // title, bar, detail, bottom
			if tc.wantExhaust {
				wantLines++ // the "! limit reached" footer
			}
			if len(lines) != wantLines {
				t.Fatalf("want %d lines, got %d:\n%q", wantLines, len(lines), lines)
			}
			bar := barArea(t, lines[1])
			if n := strings.Count(bar, capUsed); n != tc.wantUsed {
				t.Fatalf("used cells = %d, want %d: %q", n, tc.wantUsed, bar)
			}
			if n := strings.Count(bar, capEmpty); n != cardBarCells-tc.wantUsed {
				t.Fatalf("remaining cells = %d, want %d: %q", n, cardBarCells-tc.wantUsed, bar)
			}
			if !strings.HasSuffix(strings.TrimSuffix(lines[1], " │"), tc.wantPct) {
				t.Fatalf("pct label = %q, want %q", lines[1], tc.wantPct)
			}
			if !strings.HasPrefix(strings.TrimPrefix(lines[2], "│ "), tc.wantDetail) {
				t.Fatalf("detail = %q, want %q", lines[2], tc.wantDetail)
			}
			if !strings.HasSuffix(lines[2], "resets in 3h 12m │") {
				t.Fatalf("reset countdown missing: %q", lines[2])
			}
			if tc.wantExhaust && !strings.Contains(lines[3], "! limit reached") {
				t.Fatalf("exhausted window needs the footer: %q", lines[3])
			}
			// The colors off render must be the same layout, escapes stripped.
			plain := RenderCards([]Snapshot{{
				Provider: "claude",
				Accounts: []string{"claude-main"},
				Windows:  []Window{w},
			}}, now, CardOptions{NoColor: true})
			if render.StripANSI(got) != plain {
				t.Fatalf("no-color render is not the colored render minus escapes:\ngot:\n%q\nwant:\n%q", plain, render.StripANSI(got))
			}
			if strings.Contains(plain, "\x1b") {
				t.Fatalf("no-color render still carries escapes:\n%q", plain)
			}
		})
	}
}

// TestRenderCardsAllRowsWidthAtEveryPercent is the user's headline
// complaint — "边框被顶出、表格错位". It renders one snapshot that carries
// every percentage and every exhaustion path at once, in both color modes,
// and asserts EVERY line is exactly cardWidth columns.
func TestRenderCardsAllRowsWidthAtEveryPercent(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3*time.Hour + 12*time.Minute)
	windows := []Window{
		{Name: "rolling", Percent: 0, ResetsAt: &h},
		{Name: "weekly", Percent: 1, ResetsAt: &h},
		{Name: "monthly", Percent: 34, ResetsAt: &h, LimitTokensEstimate: 3_500_000},
		{Name: "rolling", Percent: 59, ResetsAt: &h, UsedFraction: 0.594},
		{Name: "weekly", Percent: 99, ResetsAt: &h},
		{Name: "monthly", Status: "used up", Percent: 100, ResetsAt: &h},
		{Name: "rolling", Status: "rate-limited", Percent: 40, ResetsAt: &h},
		{Name: "weekly", Status: "ok", Percent: 12, LimitUSDEstimate: 60, USDStatus: "estimated"},
		{Name: "weird-upstream-name", Status: "ok", Percent: 7},
	}
	snaps := []Snapshot{
		{Provider: "gemini", Accounts: []string{"gemini-acct-1"}, Windows: windows},
		{Provider: "xai", Accounts: []string{"acct-1"}, Err: "fetch_failed", Windows: windows},
		{Provider: "opencode-go", Accounts: []string{"a1"}, Err: "unauthorized"},
		{Provider: "unknown-provider", Accounts: nil, Stale: true},
	}
	for _, noColor := range []bool{false, true} {
		got := RenderCards(snaps, now, CardOptions{NoColor: noColor})
		lines := cardLines(t, got)
		wantCards := len(windows)*2 + 2 // 2 accounts with windows + 2 empty ones
		if n := strings.Count(got, "╭─ "); n != wantCards {
			t.Fatalf("noColor=%v: got %d cards, want %d:\n%s", noColor, n, wantCards, got)
		}
		if len(lines) == 0 {
			t.Fatal("no lines")
		}
	}
}

// TestRenderCardsNoColorStripsEverything renders a mixed snapshot with
// colors off and asserts not a single escape survives while the layout
// stays identical to the colored render.
func TestRenderCardsNoColorStripsEverything(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3*time.Hour + 12*time.Minute)
	snaps := []Snapshot{{
		Provider: "gemini",
		Accounts: []string{"a1"},
		Windows: []Window{
			{Name: "5h", Percent: 34, ResetsAt: &h},
			{Name: "weekly", Status: "used up", Percent: 100, LimitTokensEstimate: 3_500_000},
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

func TestRenderCardsExhausted(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderCards([]Snapshot{{
		Provider: "xai",
		Accounts: []string{"acct-1"},
		Windows:  []Window{{Name: "weekly", Status: "used up", Percent: 100}},
	}}, now)
	lines := cardLines(t, got)

	// The whole capsule is one solid red ▰ run — a drained pill, not a
	// hollow one: hollow would read as "nothing used", the opposite of an
	// exhausted window.
	row := lines[1]
	if n := strings.Count(row, capUsed); n != cardBarCells {
		t.Fatalf("solid red cells = %d, want %d:\n%q", n, cardBarCells, row)
	}
	if strings.ContainsAny(row, capEmpty) {
		t.Fatalf("exhausted capsule must be solid:\n%q", row)
	}
	if !strings.Contains(got, ansiRed+strings.Repeat(capUsed, cardBarCells)+ansiReset) {
		t.Fatalf("red ANSI capsule missing:\n%q", got)
	}
	if !strings.Contains(lines[2], "已耗尽 100%") {
		t.Fatalf("detail '已耗尽 100%%' missing:\n%q", lines[2])
	}

	// Footer: #F4A261 "! limit reached".
	if !strings.Contains(got, ansiYellow+"! limit reached"+ansiReset) {
		t.Fatalf("#F4A261 footer missing:\n%q", got)
	}
	if lines[3] == "" || !strings.HasPrefix(lines[3], "│ ! limit reached") {
		t.Fatalf("footer row malformed:\n%q", lines[3])
	}
}

func TestRenderCardsRateLimited(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2*time.Hour + 1*time.Minute)
	got := RenderCards([]Snapshot{{
		Provider: "xai",
		Accounts: []string{"acct-2"},
		Windows:  []Window{{Name: "5h", Status: "rate-limited", Percent: 50, ResetsAt: &h2}},
	}}, now)
	lines := cardLines(t, got)

	// The detail row must not lie with "remains" for a rate-limited window.
	detail := lines[2]
	if !strings.Contains(detail, "限流 50%") {
		t.Fatalf("detail '限流 50%%' missing for rate-limited:\n%q", detail)
	}
	if strings.Contains(detail, "remains") {
		t.Fatalf("rate-limited window must not claim 'remains':\n%q", detail)
	}
	// Same determination as the footer: solid red capsule, and the honest
	// percentage stays visible next to it.
	if !strings.Contains(got, ansiRed+strings.Repeat(capUsed, cardBarCells)+ansiReset) {
		t.Fatalf("rate-limited capsule must be solid red:\n%q", got)
	}
	if !strings.HasSuffix(detail, "resets in 2h 01m │") {
		t.Fatalf("rate-limited pct missing:\n%q", detail)
	}
	// Reset countdown still shown (ResetsAt exists).
	if !strings.HasSuffix(lines[2], "resets in 2h 01m │") {
		t.Fatalf("countdown missing for rate-limited:\n%q", lines[2])
	}
	if !strings.Contains(got, "! limit reached") {
		t.Fatalf("rate-limited must trigger the exhausted footer:\n%q", got)
	}
}

// The capsule ramp's stop boundaries, its monotonicity and the shared cell
// arithmetic now live with the implementation itself, in
// internal/render/capsule_test.go (TestCapsuleRampBoundaries,
// TestCapsuleUsedCells, TestCapsuleLevel). The tests below pin what the
// QUOTA cards do with the shared primitive: the direction of the gradient
// (a consumed share warms toward red), the level arithmetic bound to the
// 49-cell card geometry, and the exhausted paths.

// TestCapsuleGradientInBar checks that the ramp really reaches the
// rendered capsule: a healthy bar is flat green, a nearly-full bar warms
// from green to red cell by cell.
func TestCapsuleGradientInBar(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	low := RenderCards([]Snapshot{
		{Provider: "gemini", Accounts: []string{"a"}, Windows: []Window{{Name: "5h", Percent: 34}}},
	}, now)
	if !strings.Contains(low, ansiGreen+strings.Repeat(capUsed, 18)+ansiReset) {
		t.Fatalf("34%% must be one flat green run:\n%q", low)
	}
	if strings.Contains(low, ansiRed) || strings.Contains(low, ansiYellow) {
		t.Fatalf("34%% must stay in the green band:\n%q", low)
	}

	high := RenderCards([]Snapshot{
		{Provider: "gemini", Accounts: []string{"a"}, Windows: []Window{{Name: "5h", Percent: 99}}},
	}, now)
	if !strings.Contains(high, ansiGreen+strings.Repeat(capUsed, 20)+ansiReset) {
		t.Fatalf("99%% must start flat green:\n%q", high)
	}
	if !strings.Contains(high, ansiRed+capUsed+ansiReset) {
		t.Fatalf("99%% must reach red on its last cell:\n%q", high)
	}
	// The used cells must carry more than one distinct color: the gradient
	// is per cell, not one flat color for the whole bar.
	runs := strings.Count(high, "\x1b[38;2;")
	if runs < 5 {
		t.Fatalf("99%% capsule has %d color runs, want a per-cell gradient", runs)
	}
}

// TestCapsuleGlyphWidth guards the capsule's cell arithmetic: ▰ and ▱ are
// both exactly one display column.
func TestCapsuleGlyphWidth(t *testing.T) {
	if w := render.DisplayWidth(capUsed); w != 1 {
		t.Errorf("DisplayWidth(%q) = %d, want 1", capUsed, w)
	}
	if w := render.DisplayWidth(capEmpty); w != 1 {
		t.Errorf("DisplayWidth(%q) = %d, want 1", capEmpty, w)
	}
	if w := render.DisplayWidth(strings.Repeat(capUsed, cardBarCells)); w != cardBarCells {
		t.Errorf("capsule width = %d, want %d", w, cardBarCells)
	}
}

// TestCapsuleUsedCells locks ceil(pct/100 * barCells) with clamping.
func TestCapsuleUsedCells(t *testing.T) {
	cases := []struct{ pct, want int }{
		{-10, 0}, {0, 0}, {1, 1}, {2, 2}, {3, 2},
		{34, 18}, {50, 26}, {59, 31}, {98, 50}, {99, 51}, {100, 51}, {140, 51},
	}
	for _, tc := range cases {
		if got := capsuleUsedCells(tc.pct); got != tc.want {
			t.Errorf("capsuleUsedCells(%d) = %d, want %d", tc.pct, got, tc.want)
		}
	}
}

// TestCapsuleLevel covers the per-cell level the ramp is evaluated at.
func TestCapsuleLevel(t *testing.T) {
	if got := capsuleLevel(0); got != 1 {
		t.Errorf("capsuleLevel(0) = %d, want 1 (100/51)", got)
	}
	if got := capsuleLevel(cardBarCells - 1); got != 100 {
		t.Errorf("capsuleLevel(last) = %d, want 100", got)
	}
}

// TestRenderCardsWindowCards checks that one window is one card: every
// window gets its own title (with its own window label), its own capsule
// and its own detail row, and the account never leaks into a row.
func TestRenderCardsWindowCards(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3*time.Hour + 12*time.Minute)
	w := now.Add(49 * time.Hour)
	got := RenderCards([]Snapshot{{
		Provider: "gemini",
		Accounts: []string{"gemini-acct-1"},
		Windows: []Window{
			{Name: "5h", Status: "ok", Percent: 34, ResetsAt: &h},
			{Name: "weekly", Status: "ok", Percent: 94, LimitTokensEstimate: 3_500_000, ResetsAt: &w},
		},
	}}, now)
	lines := cardLines(t, got)
	if len(lines) != 9 { // 2 cards × 4 lines + 1 blank separator
		t.Fatalf("want 9 lines, got %d:\n%q", len(lines), lines)
	}
	assertCardTitle(t, lines[0], "Gemini", "gemini-acct-1", "5小时限额")
	assertCardTitle(t, lines[5], "Gemini", "gemini-acct-1", "周限额")

	five := lines[1]
	if n := strings.Count(barArea(t, five), capUsed); n != 18 {
		t.Fatalf("5h used cells = %d, want 18:\n%q", n, five)
	}
	if !strings.HasSuffix(five, " 34% │") {
		t.Fatalf("5h pct wrong:\n%q", five)
	}
	if !strings.HasPrefix(strings.TrimPrefix(lines[2], "│ "), "已用 34%") {
		t.Fatalf("5h detail wrong:\n%q", lines[2])
	}

	weekly := lines[6]
	if n := strings.Count(barArea(t, weekly), capUsed); n != 48 {
		t.Fatalf("weekly used cells = %d, want 48:\n%q", n, weekly)
	}
	if !strings.HasSuffix(weekly, " 94% │") {
		t.Fatalf("weekly pct wrong:\n%q", weekly)
	}
	if !strings.HasPrefix(strings.TrimPrefix(lines[7], "│ "), "已用 94% / 总额 3.5M tok") {
		t.Fatalf("weekly detail wrong:\n%q", lines[7])
	}
	if !strings.HasSuffix(lines[7], "resets in 2d 1h │") {
		t.Fatalf("weekly reset wrong:\n%q", lines[7])
	}

	// The account identifier never leaks into a card row (the titles are
	// the only place it belongs).
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "╭─ ") && strings.Contains(l, "gemini-acct-1") {
			t.Fatalf("account leaked into a card row:\n%q", l)
		}
	}
	// 94 % is NOT exhausted: no red capsule, no footer, no cycle dots.
	if strings.Contains(got, ansiRed) || strings.Contains(got, "limit reached") {
		t.Fatalf("94%% must not look exhausted:\n%q", got)
	}
	if strings.ContainsAny(got, "○●◆") {
		t.Fatalf("unfabricated extras must stay omitted:\n%q", got)
	}
}

// TestRenderCardsZeroUsedWindow: 0 % used draws an all-hollow capsule —
// the value is readable from the capsule's length alone.
func TestRenderCardsZeroUsedWindow(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderCards([]Snapshot{{
		Provider: "gemini",
		Accounts: []string{"Gemini"},
		Windows:  []Window{{Name: "5h", Status: "ok", Percent: 0}},
	}}, now)
	lines := cardLines(t, got)
	bar := barArea(t, lines[1])
	if n := strings.Count(bar, capUsed); n != 0 {
		t.Fatalf("0%% used must draw no ▰ at all, got %d: %q", n, bar)
	}
	if n := strings.Count(bar, capEmpty); n != cardBarCells {
		t.Fatalf("remaining cells = %d, want %d: %q", n, cardBarCells, bar)
	}
	if !strings.Contains(got, ansiDim+strings.Repeat(capEmpty, cardBarCells)+ansiReset) {
		t.Fatalf("the whole capsule must be dim gray:\n%q", got)
	}
	if !strings.HasSuffix(lines[2], "- │") {
		t.Fatalf("no ResetsAt must render the '-' placeholder:\n%q", lines[2])
	}
}

// TestRenderCardsTitleDeduplicatesServiceAndAccount: when the account name
// is exactly the provider display name, the title carries the name ONCE
// ("Gemini · 5小时限额") instead of the redundant "Gemini Gemini". An account
// that differs from the service keeps the usual brand + dim account pair.
func TestRenderCardsTitleDeduplicatesServiceAndAccount(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderCards([]Snapshot{{
		Provider: "gemini",
		Accounts: []string{"Gemini"},
		Windows:  []Window{{Name: "5h", Status: "ok", Percent: 12}},
	}}, now)
	lines := cardLines(t, got)
	assertCardTitle(t, lines[0], "Gemini", "", "5小时限额")
	if n := strings.Count(lines[0], "Gemini"); n != 1 {
		t.Fatalf("service must appear exactly once, got %d:\n%q", n, lines[0])
	}

	// Same provider, different account: both segments stay.
	got = RenderCards([]Snapshot{{
		Provider: "gemini",
		Accounts: []string{"gemini-acct-1"},
		Windows:  []Window{{Name: "5h", Status: "ok", Percent: 12}},
	}}, now)
	lines = cardLines(t, got)
	assertCardTitle(t, lines[0], "Gemini", "gemini-acct-1", "5小时限额")
	if !strings.Contains(lines[0], "Gemini gemini-acct-1") {
		t.Fatalf("distinct account must keep both title segments:\n%q", lines[0])
	}
}

func TestRenderCardsErrorAndEmpty(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)

	// Error, no windows: title + note + bottom.
	got := RenderCards([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1"},
		Err:      "unauthorized",
	}}, now)
	lines := cardLines(t, got)
	assertCardTitle(t, lines[0], "Opus", "a1", "")
	if !strings.Contains(lines[1], "⚠ unauthorized") {
		t.Fatalf("error note missing:\n%q", lines[1])
	}
	if len(lines) != 3 {
		t.Fatalf("error card should have 3 lines, got %d:\n%q", len(lines), lines)
	}

	// No windows, no error: title + bottom only, no spurious note.
	got = RenderCards([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1"},
	}}, now)
	lines = cardLines(t, got)
	if len(lines) != 2 {
		t.Fatalf("empty card should have 2 lines, got %d:\n%q", len(lines), lines)
	}
	if strings.Contains(got, "⚠") {
		t.Fatalf("spurious warning note:\n%q", got)
	}

	// Stale + error + window: stale marker lives in the TITLE only, the
	// error note rides along on the window card.
	h := now.Add(45 * time.Minute)
	got = RenderCards([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1"},
		Stale:    true,
		Err:      "fetch_failed",
		Windows:  []Window{{Name: "rolling", Status: "ok", Percent: 3, ResetsAt: &h}},
	}}, now)
	lines = cardLines(t, got)
	if !strings.Contains(lines[0], "a1 旧 · 5小时限额") {
		t.Fatalf("stale marker missing from title:\n%q", lines[0])
	}
	if n := strings.Count(got, "a1 旧"); n != 1 {
		t.Fatalf("stale marker occurs %d times, want 1 (title only):\n%q", n, got)
	}
	if !strings.Contains(lines[3], "⚠ fetch_failed") {
		t.Fatalf("error note missing from the window card:\n%q", lines[3])
	}
	if !strings.Contains(lines[1], " 3%") || !strings.HasSuffix(lines[2], "resets in 45m │") {
		t.Fatalf("window card missing data:\n%q", lines)
	}
	if strings.Contains(lines[1], "a1") || strings.Contains(lines[2], "a1") {
		t.Fatalf("account leaked into a window card row:\n%q", lines)
	}
}

func TestRenderCardsLongTitleTruncates(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2 * time.Hour)
	long := "very-long-account-name-that-just-keeps-going-and-going-and-going"
	got := RenderCards([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{long},
		Windows:  []Window{{Name: "rolling", Status: "ok", Percent: 7, ResetsAt: &h2}},
	}}, now)
	lines := cardLines(t, got) // width invariant covers the whole card

	title := lines[0]
	if !strings.HasPrefix(title, "╭─ Opus ") {
		t.Fatalf("title prefix wrong:\n%q", title)
	}
	if !strings.Contains(title, "…") {
		t.Fatalf("over-long account must be truncated with an ellipsis:\n%q", title)
	}
	if strings.Contains(title, long) {
		t.Fatalf("account was not truncated:\n%q", title)
	}
	// The window label survives the truncation and stays readable.
	if !strings.Contains(title, "· 5小时限额 ─") {
		t.Fatalf("window label must survive title truncation:\n%q", title)
	}
	// Rows never carry the account, truncated or not.
	for _, l := range lines[1:] {
		if strings.Contains(l, "very-long") {
			t.Fatalf("account leaked into a card row:\n%q", l)
		}
	}
}

// TestRenderCardsOverLongWindowNameTitle is the title half of the
// width pits: an unknown upstream window name can be arbitrarily long (the
// known ones are short and fixed). The title must shrink the VARIABLE
// segments — account, then service, then the window label — and recompute
// the dash fill from the width the segments ACTUALLY occupy. It must never
// truncate the whole line: that old safety net ate the right border ╮ and
// could leave the card at 59 columns.
func TestRenderCardsOverLongWindowNameTitle(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2 * time.Hour)
	names := []string{
		strings.Repeat("w", 70),  // ASCII, 70 columns
		strings.Repeat("窗口", 35), // CJK, 70 columns
		"mixed-窗口-" + strings.Repeat("x", 60),
		strings.Repeat("🔥", 40),
	}
	for _, name := range names {
		got := RenderCards([]Snapshot{{
			Provider: "opencode-go",
			Accounts: []string{"a1"},
			Windows:  []Window{{Name: name, Status: "ok", Percent: 7, ResetsAt: &h2}},
		}}, now, CardOptions{NoColor: true})
		// cardLines asserts EVERY line — title included — is 60 columns.
		lines := cardLines(t, got)
		title := lines[0]
		if !strings.HasPrefix(title, "╭─ Opus · ") {
			// The account is the first thing dropped; the service and its
			// separator must survive the window-label truncation.
			t.Fatalf("window name %q: title head wrong: %q", name, title)
		}
		if !strings.HasSuffix(title, "╮") {
			t.Fatalf("window name %q: the ╮ was cut off: %q", name, title)
		}
		if !strings.Contains(title, "…") {
			t.Fatalf("window name %q: not truncated: %q", name, title)
		}
		// After the ellipsis: one space, dash fill only, then ╮.
		fill := title[strings.LastIndex(title, "…")+len("…"):]
		if !strings.HasPrefix(fill, " ") || !strings.Contains(fill, "─") ||
			strings.Trim(fill, " ─╮") != "" {
			t.Fatalf("window name %q: dash fill malformed: %q", name, title)
		}
		// The other card rows are untouched by the long window name.
		if !strings.HasPrefix(lines[1], "│ ") || !strings.HasSuffix(lines[1], " 7% │") {
			t.Fatalf("window name %q: bar row wrong: %q", name, lines[1])
		}
	}
}

// TestRenderCardsCJKAccountTitleFill pins the OTHER half: the account is
// the first segment to shrink, and a CJK account whose truncation budget
// ends on an odd column stops one column short of it. The dash fill is
// derived from the MEASURED width, so the card stays at exactly 60
// columns — assuming the budget was consumed in full would push the right
// border out.
func TestRenderCardsCJKAccountTitleFill(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2 * time.Hour)
	got := RenderCards([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{strings.Repeat("账", 40)}, // 80 columns
		Windows:  []Window{{Name: "rolling", Status: "ok", Percent: 7, ResetsAt: &h2}},
	}}, now, CardOptions{NoColor: true})
	lines := cardLines(t, got) // title is 60 columns, ╮ intact
	title := lines[0]
	if !strings.HasPrefix(title, "╭─ Opus ") {
		t.Fatalf("title head wrong: %q", title)
	}
	if !strings.Contains(title, "…") {
		t.Fatalf("over-long CJK account must be truncated: %q", title)
	}
	if !strings.Contains(title, " · 5小时限额 ─") {
		t.Fatalf("window label and dash fill must survive: %q", title)
	}
}

func TestRenderCardsModuleSortAndWindowCards(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2 * time.Hour)
	got := RenderCards([]Snapshot{
		{Provider: "opencode-go", Accounts: []string{"z9"}, Windows: []Window{
			{Name: "rolling", Status: "ok", Percent: 12, ResetsAt: &h2},
			{Name: "weekly", Status: "ok", Percent: 8},
		}},
		{Provider: "opencode-go", Accounts: []string{"a1"}, Windows: []Window{
			{Name: "rolling", Status: "ok", Percent: 1, ResetsAt: &h2},
		}},
	}, now)
	lines := cardLines(t, got)

	// Collect title indices (stable sort: a1 before z9, one card per
	// window: a1 1 card, z9 2 cards).
	var titleIdx []int
	for i, l := range lines {
		if strings.HasPrefix(l, "╭─ ") {
			titleIdx = append(titleIdx, i)
		}
	}
	if len(titleIdx) != 3 {
		t.Fatalf("want 3 cards (1 + 2 windows), got %d:\n%q", len(titleIdx), lines)
	}
	if !strings.Contains(lines[titleIdx[0]], "a1") {
		t.Fatalf("cards not sorted by account:\n%q", lines)
	}
	if !strings.Contains(lines[titleIdx[1]], "z9") || !strings.Contains(lines[titleIdx[2]], "z9") {
		t.Fatalf("z9 window cards missing:\n%q", lines)
	}
	if !strings.Contains(lines[titleIdx[2]], "周限额") {
		t.Fatalf("second z9 card must carry the weekly window label:\n%q", lines[titleIdx[2]])
	}
	// Inside the z9 cards, no row after a title mentions the account (the
	// titles themselves legitimately carry it).
	for i := titleIdx[1] + 1; i < len(lines) && lines[i] != ""; i++ {
		if strings.Contains(lines[i], "z9") {
			t.Fatalf("account leaked into row %d:\n%q", i, lines[i])
		}
	}
}

func TestCardCountdownZeroPadsMinutes(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		d    time.Duration
		want string
	}{
		{2*time.Hour + 1*time.Minute, "2h 01m"},
		{4*time.Hour + 51*time.Minute, "4h 51m"},
		{45 * time.Minute, "45m"},
		{2 * time.Hour, "2h"},
		{3*24*time.Hour + 4*time.Hour, "3d 4h"},
		{30 * time.Second, "<1m"},
		{-time.Minute, "已到"},
	}
	for _, tc := range cases {
		at := now.Add(tc.d)
		if got := cardCountdown(now, at); got != tc.want {
			t.Errorf("%v: got %q want %q", tc.d, got, tc.want)
		}
	}
}

func TestResetText(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h := now.Add(3*time.Hour + 12*time.Minute)
	future := now.Add(12 * time.Hour)
	if got := resetText(Window{ResetsAt: &h}, now); got != "resets in 3h 12m" {
		t.Errorf("future reset = %q", got)
	}
	if got := resetText(Window{ResetsAt: &now}, now); got != "resets now" {
		t.Errorf("elapsed reset = %q", got)
	}
	if got := resetText(Window{ResetsAt: nil}, now); got != "-" {
		t.Errorf("missing reset = %q", got)
	}
	zero := time.Time{}
	if got := resetText(Window{ResetsAt: &zero}, now); got != "-" {
		t.Errorf("zero reset = %q", got)
	}
	if got := resetText(Window{ResetsAt: &future}, now); got != "resets in 12h" {
		t.Errorf("whole-hour reset = %q", got)
	}
}

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
