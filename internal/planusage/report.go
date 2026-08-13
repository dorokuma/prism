package planusage

import (
	"fmt"
	"strings"
	"time"
)

// RenderTable is the default CLI / format=table report. It is stacked for
// a ~40-column phone terminal: no header row, no ISO timestamps.
func RenderTable(snaps []Snapshot) string {
	return RenderTableAt(snaps, time.Now())
}

// RenderTableAt is RenderTable with an injectable clock (tests).
func RenderTableAt(snaps []Snapshot, now time.Time) string {
	if len(snaps) == 0 {
		return "没有套餐数据\n"
	}
	var b strings.Builder
	for i, s := range snaps {
		if i > 0 {
			b.WriteByte('\n')
		}
		title := s.Provider
		if title == "" {
			title = "unknown"
		}
		if s.Stale {
			title += "  旧"
		}
		b.WriteString(title)
		b.WriteByte('\n')
		if len(s.Accounts) > 0 {
			b.WriteString(strings.Join(s.Accounts, ", "))
			b.WriteByte('\n')
		}
		if s.Err != "" && len(s.Windows) == 0 {
			b.WriteString(s.Err)
			b.WriteByte('\n')
			continue
		}
		if s.Err != "" {
			b.WriteString(s.Err)
			b.WriteByte('\n')
		}
		for _, w := range s.Windows {
			label := windowLabel(w.Name)
			remain := formatRemain(now, w.ResetsAt)
			extra := ""
			if w.Status == "rate-limited" {
				extra = " 限流"
			}
			fmt.Fprintf(&b, "%-3s %3d%%%s  %s\n", label, w.Percent, extra, remain)
		}
	}
	return b.String()
}

func windowLabel(name string) string {
	switch name {
	case "rolling":
		return "5h"
	case "weekly":
		return "周"
	case "monthly":
		return "月"
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
