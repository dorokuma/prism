package planusage

import (
	"fmt"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/render"
)

const reportIndent = "  "

// RenderTable is the default CLI / format=table report. It is a compact
// single-line table with the same visual rules as the usage report: every
// output block is indented two spaces, columns are separated by one space,
// each window is one row, names render in full (never truncated) and the
// layout never depends on the terminal width.
func RenderTable(snaps []Snapshot) string {
	return RenderTableAt(snaps, time.Now())
}

// RenderTableAt is RenderTable with an injectable clock (tests). Each
// window is one row; every account name renders once per window group,
// vertically centered on the weekly row when one exists, otherwise on
// the group's middle row (see accountRowIndex). Load-balanced plans are
// never merged. Snapshots without windows keep the account line and the
// error line, if any. Error lines always carry the account attribution
// ("  provider/account: fetch_failed") so failures from different
// accounts stay distinguishable.
func RenderTableAt(snaps []Snapshot, now time.Time) string {
	if len(snaps) == 0 {
		return "  没有套餐数据\n"
	}

	cols := []render.Column{
		{Title: "账号", Align: render.AlignLeft},
		{Title: "窗口", Align: render.AlignLeft},
		{Title: "状态", Align: render.AlignLeft},
		{Title: "占用", Align: render.AlignRight},
		{Title: "重置", Align: render.AlignRight},
		{Title: "限额估算", Align: render.AlignRight},
	}

	var rows [][]string
	var notes strings.Builder

	multiProvider := hasMultipleProviders(snaps)
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
			account := accountCell(s, title, multiProvider)
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
			accountRow := accountRowIndex(s.Windows)
			for i, w := range s.Windows {
				cell := ""
				if i == accountRow {
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

// hasMultipleProviders reports whether the snapshots span more than one
// provider; the account cell then prefixes the provider so sources stay
// distinguishable.
func hasMultipleProviders(snaps []Snapshot) bool {
	seen := ""
	for _, s := range snaps {
		if s.Provider == "" {
			continue
		}
		if seen != "" && s.Provider != seen {
			return true
		}
		if seen == "" {
			seen = s.Provider
		}
	}
	return false
}

// accountCell is the first-column value for one account. A stale snapshot
// keeps the 旧 marker, and a multi-provider report prefixes the provider.
func accountCell(s Snapshot, title string, multiProvider bool) string {
	account := title
	if multiProvider && s.Provider != "" {
		account = s.Provider + "/" + title
	}
	if s.Stale {
		account += " 旧"
	}
	return account
}

// accountRowIndex returns the row within one account's window group that
// carries the account name, so the name renders once and is vertically
// centered in the group. The weekly window is the preferred home whenever
// it exists — even when the group is not a plain three-window
// rolling/weekly/monthly stack (missing or custom windows). Without a
// weekly window the middle row of the actual window count wins: the
// unique middle for odd counts, the lower middle (len/2, 0-based) for
// even counts. The caller never passes an empty group (len == 0 is
// handled before rows are built); 0 is returned defensively.
func accountRowIndex(windows []Window) int {
	for i, w := range windows {
		if w.Name == "weekly" {
			return i
		}
	}
	return len(windows) / 2
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

// formatEstimate renders the window quota limit: LimitUSDEstimate is the
// upstream's documented dollar limit for the window, not the current
// spend. It shows the field only when the upstream marks it estimated —
// never a real spend figure.
func formatEstimate(w Window) string {
	if w.LimitUSDEstimate > 0 && w.USDStatus == "estimated" {
		return fmt.Sprintf("$%d.00", w.LimitUSDEstimate)
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
