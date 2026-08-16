package planusage

import (
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/render"
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
		"  账号 窗口 状态 占用  重置 限额估算\n" +
		"       短期 ok    12% 2h13m        -\n" +
		"  go-1 中期 ok     8%  3d4h        -\n" +
		"       长期 ok    40%   12d        -\n" +
		"       短期 限流 100% 2h13m        -\n" +
		"  go-2 中期 ok    30%  3d4h        -\n" +
		"       长期 ok    55%   12d        -\n"
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
	pctCol := colOf(winLines[0], "%")
	// The remain column is right-aligned, so the token's right edge is the
	// stable anchor, not its start.
	remainEnd := colOf(winLines[0], "2h13m") + render.DisplayWidth("2h13m")
	if pctCol < 0 || remainEnd < 0 {
		t.Fatalf("anchor missing: %q", winLines[0])
	}
	for _, line := range winLines {
		if c := colOf(line, "%"); c != pctCol {
			t.Errorf("%q: %% at col %d, want %d", line, c, pctCol)
		}
		rest := line[strings.Index(line, "%")+1:]
		rest = strings.TrimLeft(rest, " ")
		tok := strings.Fields(rest)
		if len(tok) == 0 {
			t.Errorf("%q: no remain", line)
			continue
		}
		if end := colOf(line, tok[0]) + render.DisplayWidth(tok[0]); end != remainEnd {
			t.Errorf("%q: remain %q ends at col %d, want %d", line, tok[0], end, remainEnd)
		}
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
		"  a1   短期 ok     1%    -        -\n" +
		"  a2   短期 ok     1%    -        -\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func colOf(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return render.DisplayWidth(s[:i])
}

// TestRenderTableAccountRowPlacement pins where the account name sits
// inside one window group: the weekly row whenever a weekly window
// exists (even when that is not the group's middle row), otherwise the
// middle row of the actual window count — the lower middle (len/2,
// 0-based) for even counts. Every other window row leaves the account
// cell empty.
func TestRenderTableAccountRowPlacement(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	h2 := now.Add(2*time.Hour + 13*time.Minute)
	d3 := now.Add(3*24*time.Hour + 4*time.Hour)
	d12 := now.Add(12 * 24 * time.Hour)
	header := "  账号 窗口 状态 占用  重置 限额估算\n"
	cases := []struct {
		name    string
		windows []Window
		want    string
	}{
		{
			name: "three windows, weekly is the middle row",
			windows: []Window{
				{Name: "rolling", Status: "ok", Percent: 12, ResetsAt: &h2},
				{Name: "weekly", Status: "ok", Percent: 8, ResetsAt: &d3},
				{Name: "monthly", Status: "ok", Percent: 40, ResetsAt: &d12},
			},
			want: header +
				"       短期 ok    12% 2h13m        -\n" +
				"  a1   中期 ok     8%  3d4h        -\n" +
				"       长期 ok    40%   12d        -\n",
		},
		{
			name: "weekly present but not the middle row",
			windows: []Window{
				{Name: "rolling", Status: "ok", Percent: 12, ResetsAt: &h2},
				{Name: "monthly", Status: "ok", Percent: 40, ResetsAt: &d12},
				{Name: "weekly", Status: "ok", Percent: 8, ResetsAt: &d3},
			},
			want: header +
				"       短期 ok    12% 2h13m        -\n" +
				"       长期 ok    40%   12d        -\n" +
				"  a1   中期 ok     8%  3d4h        -\n",
		},
		{
			name: "two windows, no weekly: lower middle row",
			windows: []Window{
				{Name: "rolling", Status: "ok", Percent: 12, ResetsAt: &h2},
				{Name: "monthly", Status: "ok", Percent: 40, ResetsAt: &d12},
			},
			want: header +
				"       短期 ok    12% 2h13m        -\n" +
				"  a1   长期 ok    40%   12d        -\n",
		},
		{
			name:    "single window",
			windows: []Window{{Name: "rolling", Status: "ok", Percent: 12, ResetsAt: &h2}},
			want: header +
				"  a1   短期 ok    12% 2h13m        -\n",
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

// TestRenderTableAccountOncePerGroup pins that with several accounts on
// one snapshot each name still renders exactly once, on its own weekly
// row — never repeated across the group's other windows.
func TestRenderTableAccountOncePerGroup(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderTableAt([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1", "a2"},
		Windows: []Window{
			{Name: "rolling", Status: "ok", Percent: 12},
			{Name: "weekly", Status: "ok", Percent: 8},
			{Name: "monthly", Status: "ok", Percent: 40},
		},
	}}, now)
	for _, acct := range []string{"a1", "a2"} {
		var hits []string
		for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
			if strings.Contains(line, "%") && strings.Contains(line, acct) {
				hits = append(hits, line)
			}
		}
		if len(hits) != 1 {
			t.Fatalf("%s appears in %d window rows, want exactly 1: %q\n%s", acct, len(hits), hits, got)
		}
		if !strings.Contains(hits[0], "中期") {
			t.Fatalf("%s not on the weekly row: %q", acct, hits[0])
		}
	}
}

// TestRenderTableStaleProviderPlacement pins that the provider prefix and
// the stale 旧 marker ride the account cell and appear only on the
// account's single centered row, while error notes keep the full
// provider/account attribution.
func TestRenderTableStaleProviderPlacement(t *testing.T) {
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
	// Error notes keep the complete provider/account attribution.
	for _, s := range []string{"opencode-go/a1 旧: fetch_failed", "acme/b2 旧: timeout"} {
		if !strings.Contains(got, s) {
			t.Fatalf("error attribution %q missing:\n%s", s, got)
		}
	}
	// Each full account cell (provider prefix + stale marker) appears
	// exactly twice in the whole report: once on its single window row
	// and once on its error note — never on the group's other rows.
	for _, acct := range []string{"opencode-go/a1 旧", "acme/b2 旧"} {
		if n := strings.Count(got, acct); n != 2 {
			t.Fatalf("%q occurs %d times, want 2 (one window row + one error note):\n%s", acct, n, got)
		}
	}
	// a1's only window is rolling; b2's group has a weekly window, so the
	// name sits on the 中期 row.
	var a1Row, b2Row string
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		switch {
		case strings.Contains(line, "opencode-go/a1 旧") && strings.Contains(line, "%"):
			a1Row = line
		case strings.Contains(line, "acme/b2 旧") && strings.Contains(line, "%"):
			b2Row = line
		}
	}
	if !strings.Contains(a1Row, "短期") {
		t.Fatalf("a1 not on its single window row: %q", a1Row)
	}
	if !strings.Contains(b2Row, "中期") {
		t.Fatalf("b2 not on the weekly row: %q", b2Row)
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
// indistinguishable error lines — each error keeps its provider/account.
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
	// Each error line carries its own provider/account attribution.
	for _, s := range []string{"opencode-go/a1 旧: fetch_failed", "acme/b2 旧: timeout"} {
		if !strings.Contains(got, s) {
			t.Fatalf("error attribution %q missing:\n%s", s, got)
		}
	}
	// No bare error line may survive without its attribution.
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if strings.Contains(line, "fetch_failed") && !strings.Contains(line, "opencode-go/a1 旧: ") {
			t.Fatalf("bare error line: %q", line)
		}
		if strings.Contains(line, "timeout") && !strings.Contains(line, "acme/b2 旧: ") {
			t.Fatalf("bare error line: %q", line)
		}
	}
	// Normal window rows and the stale markers are preserved.
	if !strings.Contains(got, "opencode-go/a1 旧") || !strings.Contains(got, "acme/b2 旧") {
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

func TestRenderTableProviderPrefix(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	win := []Window{{Name: "rolling", Status: "ok", Percent: 1}}
	got := RenderTableAt([]Snapshot{
		{Provider: "opencode-go", Accounts: []string{"a1"}, Windows: win},
		{Provider: "acme", Accounts: []string{"a1"}, Windows: win},
	}, now)
	if !strings.Contains(got, "opencode-go/a1") || !strings.Contains(got, "acme/a1") {
		t.Fatalf("provider prefix missing:\n%s", got)
	}
	// Same provider: account names alone are enough.
	got = RenderTableAt([]Snapshot{
		{Provider: "opencode-go", Accounts: []string{"a1"}, Windows: win},
		{Provider: "opencode-go", Accounts: []string{"b2"}, Windows: win},
	}, now)
	if strings.Contains(got, "opencode-go/a1") || strings.Contains(got, "opencode-go/b2") {
		t.Fatalf("unneeded provider prefix:\n%s", got)
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
