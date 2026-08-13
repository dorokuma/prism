package planusage

import (
	"strings"
	"testing"
	"time"
)

func TestRenderTableAtMobile(t *testing.T) {
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
		"  go-1\n" +
		"  短期   12%  2h13m\n" +
		"  中期    8%  3d4h\n" +
		"  长期   40%  12d\n" +
		"\n" +
		"  go-2\n" +
		"  短期  100%  2h13m  限流\n" +
		"  中期   30%  3d4h\n" +
		"  长期   55%  12d\n"
	if got != want {
		t.Fatalf("layout\ngot:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "T10:00:00Z") || strings.Contains(got, "---") || strings.Contains(got, ", ") {
		t.Fatalf("old merged/ISO format leaked: %q", got)
	}
	var winLines []string
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if dw := dispWidth(line); dw > 40 {
			t.Errorf("line too wide for phone (%d cols): %q", dw, line)
		}
		if strings.Contains(line, "%") {
			winLines = append(winLines, line)
		}
	}
	if len(winLines) != 6 {
		t.Fatalf("window lines: %q", winLines)
	}
	pctCol := colOf(winLines[0], "%")
	remainCol := colOf(winLines[0], "2h13m")
	if pctCol < 0 || remainCol < 0 {
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
		if c := colOf(line, tok[0]); c != remainCol {
			t.Errorf("%q: remain %q at col %d, want %d", line, tok[0], c, remainCol)
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
	want := "  a1\n  短期    1%  -\n\n  a2\n  短期    1%  -\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func colOf(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return dispWidth(s[:i])
}

func TestRenderTableAtErrorAndStale(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderTableAt([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1"},
		Err:      "unauthorized",
	}}, now)
	if got != "  a1\n  unauthorized\n" {
		t.Fatalf("error card: %q", got)
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

func TestRenderTableEmpty(t *testing.T) {
	if RenderTable(nil) != "  没有套餐数据\n" {
		t.Fatal(RenderTable(nil))
	}
}

func TestDispWidthAndPad(t *testing.T) {
	if dispWidth("短期") != 4 || dispWidth("中期") != 4 || dispWidth("长期") != 4 || dispWidth("限流") != 4 {
		t.Fatalf("widths 短期=%d 中期=%d 长期=%d 限流=%d", dispWidth("短期"), dispWidth("中期"), dispWidth("长期"), dispWidth("限流"))
	}
	if padRight("中期", 4) != "中期" || padLeft("8%", 4) != "  8%" {
		t.Fatalf("pad %q %q", padRight("中期", 4), padLeft("8%", 4))
	}
}
