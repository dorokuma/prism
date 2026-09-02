package planusage

import (
	"strings"
	"testing"
	"time"
)

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
	// Module layout: each account name appears on the module's FIRST
	// window row only; the module's other rows leave the account cell
	// empty so windows stay grouped under their account.
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

// TestRenderTableModuleFirstRow pins the module layout: the account name
// sits on the module's FIRST window row, every later row of the module
// leaves the account cell empty (windows stay grouped under their
// account, never mistaken for a neighbour's).
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

// TestRenderTableModuleSortAndEmptyCells pins that modules are sorted by
// account name (never interleaving) and that only each module's first
// window row carries the account name.
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
	// a1 module first (sorted), then z9 module.
	if !strings.Contains(lines[1], "a1") || !strings.Contains(lines[2], "z9") {
		t.Fatalf("modules not sorted:\n%s", got)
	}
	// z9 module: account on first window row, empty on the second.
	if !strings.Contains(lines[2], "z9") || strings.Contains(lines[3], "z9") {
		t.Fatalf("z9 module layout wrong:\n%s", got)
	}
}

// TestRenderTableStaleModuleFirstRow pins that the stale 旧 marker rides
// the account cell on the module's first window row (no provider prefix
// any more), while error notes keep the plain account attribution.
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
	// Error notes keep the plain account attribution (no provider prefix).
	for _, s := range []string{"a1 旧: fetch_failed", "b2 旧: timeout"} {
		if !strings.Contains(got, s) {
			t.Fatalf("error attribution %q missing:\n%s", s, got)
		}
	}
	if strings.Contains(got, "opencode-go/a1") || strings.Contains(got, "acme/b2") {
		t.Fatalf("provider prefix leaked:\n%s", got)
	}
	// The stale account cell appears once on the module's first window
	// row plus once on the error note.
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

// TestRenderTableErrorAttribution pins the per-account error ownership:
// stale snapshots that still carry windows must not degrade into bare,
// indistinguishable error lines — each error keeps its account.
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
	// Each error line carries its own account attribution (no provider
	// prefix any more).
	for _, s := range []string{"a1 旧: fetch_failed", "b2 旧: timeout"} {
		if !strings.Contains(got, s) {
			t.Fatalf("error attribution %q missing:\n%s", s, got)
		}
	}
	if strings.Contains(got, "opencode-go/a1") || strings.Contains(got, "acme/b2") {
		t.Fatalf("provider prefix leaked:\n%s", got)
	}
	// No bare error line may survive without its attribution.
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if strings.Contains(line, "fetch_failed") && !strings.Contains(line, "a1 旧: ") {
			t.Fatalf("bare error line: %q", line)
		}
		if strings.Contains(line, "timeout") && !strings.Contains(line, "b2 旧: ") {
			t.Fatalf("bare error line: %q", line)
		}
	}
	// Normal window rows and the stale markers are preserved.
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
	// Module layout: account on the first (5h) row, weekly row leaves the
	// account cell empty so both rows read as one Gemini module.
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

// TestRenderTableNoProviderPrefix pins that the provider prefix is gone
// from the account cell even across providers: account names are globally
// unique in prism, and every window row carries its own account.
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
