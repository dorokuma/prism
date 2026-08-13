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
	got := RenderTableAt([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1", "a2"},
		Windows: []Window{
			{Name: "rolling", Status: "ok", Percent: 12, ResetsAt: &h2},
			{Name: "weekly", Status: "ok", Percent: 8, ResetsAt: &d3},
			{Name: "monthly", Status: "rate-limited", Percent: 100, ResetsAt: &d12},
		},
	}}, now)
	if !strings.Contains(got, "opencode-go\n") || !strings.Contains(got, "a1, a2\n") {
		t.Fatalf("header: %q", got)
	}
	if !strings.Contains(got, "12%") || !strings.Contains(got, "2h13m") {
		t.Fatalf("5h line: %q", got)
	}
	if !strings.Contains(got, "8%") || !strings.Contains(got, "3d4h") {
		t.Fatalf("week line: %q", got)
	}
	if !strings.Contains(got, "100%") || !strings.Contains(got, "限流") || !strings.Contains(got, "12d") {
		t.Fatalf("month line: %q", got)
	}
	if strings.Contains(got, "T10:00:00Z") || strings.Contains(got, "---") {
		t.Fatalf("old wide/ISO format leaked: %q", got)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if len([]rune(line)) > 40 {
			t.Errorf("line too wide for phone (%d runes): %q", len([]rune(line)), line)
		}
	}
}

func TestRenderTableAtErrorAndStale(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	got := RenderTableAt([]Snapshot{{
		Provider: "opencode-go",
		Accounts: []string{"a1"},
		Err:      "unauthorized",
	}}, now)
	if got != "opencode-go\na1\nunauthorized\n" {
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
	if RenderTable(nil) != "没有套餐数据\n" {
		t.Fatal(RenderTable(nil))
	}
}
