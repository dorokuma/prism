package planusage

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Phone cards stay inside ~40 display columns. Columns are padded by
// terminal cell width (CJK = 2), not by Go's %s rune count.
const (
	labelMinCols = 2
	pctCols      = 4
)

// RenderTable is the default CLI / format=table report. It is stacked for
// a ~40-column phone terminal: no header row, no ISO timestamps.
func RenderTable(snaps []Snapshot) string {
	return RenderTableAt(snaps, time.Now())
}

// RenderTableAt is RenderTable with an injectable clock (tests).
// Each account is its own card. Load-balanced plans are never merged.
func RenderTableAt(snaps []Snapshot, now time.Time) string {
	if len(snaps) == 0 {
		return "没有套餐数据\n"
	}
	var b strings.Builder
	first := true
	for _, s := range snaps {
		titles := s.Accounts
		if len(titles) == 0 {
			title := s.Provider
			if title == "" {
				title = "unknown"
			}
			titles = []string{title}
		}
		for _, title := range titles {
			if !first {
				b.WriteByte('\n')
			}
			first = false
			writeCard(&b, title, s, now)
		}
	}
	return b.String()
}

func writeCard(b *strings.Builder, title string, s Snapshot, now time.Time) {
	if s.Stale {
		title += "  旧"
	}
	b.WriteString(title)
	b.WriteByte('\n')
	if s.Err != "" && len(s.Windows) == 0 {
		b.WriteString(s.Err)
		b.WriteByte('\n')
		return
	}
	if s.Err != "" {
		b.WriteString(s.Err)
		b.WriteByte('\n')
	}
	labelW := labelMinCols
	for _, w := range s.Windows {
		if dw := dispWidth(windowLabel(w.Name)); dw > labelW {
			labelW = dw
		}
	}
	for _, w := range s.Windows {
		label := padRight(windowLabel(w.Name), labelW)
		pct := padLeft(fmt.Sprintf("%d%%", w.Percent), pctCols)
		remain := formatRemain(now, w.ResetsAt)
		line := label + "  " + pct + "  " + remain
		if w.Status == "rate-limited" {
			line += "  限流"
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

func windowLabel(name string) string {
	switch name {
	case "rolling":
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

func dispWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeDispWidth(r)
	}
	return n
}

func runeDispWidth(r rune) int {
	switch {
	case r == 0 || r < ' ':
		return 0
	case r < 0x7F:
		return 1
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r):
		return 0
	case unicode.Is(unicode.Han, r),
		unicode.Is(unicode.Hangul, r),
		unicode.Is(unicode.Hiragana, r),
		unicode.Is(unicode.Katakana, r):
		return 2
	case r >= 0x3000 && r <= 0x303F, // CJK punctuation
		r >= 0xFF01 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6:
		return 2
	default:
		return 1
	}
}

func padRight(s string, w int) string {
	n := dispWidth(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

func padLeft(s string, w int) string {
	n := dispWidth(s)
	if n >= w {
		return s
	}
	return strings.Repeat(" ", w-n) + s
}
