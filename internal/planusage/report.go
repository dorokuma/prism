package planusage

import (
	"fmt"
	"sort"
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
// account is one MODULE: its windows are consecutive rows, the account
// name sits on the module's first window row, later rows leave the
// account cell empty. Modules are sorted by account name so they never
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

	// Stable sort by the first account name so modules stay contiguous
	// and in a readable order (Cache.List is map-ordered).
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

// accountSortKey orders snapshots by their first account name (provider
// as fallback), so modules never interleave in the table.
func accountSortKey(s Snapshot) string {
	if len(s.Accounts) > 0 {
		return s.Accounts[0]
	}
	return s.Provider
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
