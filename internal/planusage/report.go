package planusage

import (
	"fmt"
	"strings"
	"time"
)

// RenderTable is a plain-text report for CLI and format=table.
func RenderTable(snaps []Snapshot) string {
	if len(snaps) == 0 {
		return "quota: 没有匹配的 provider（当前只支持 OpenCode Go）\n"
	}
	var b strings.Builder
	b.WriteString("provider      accounts              window    status         percent  ~usd     resets_at\n")
	b.WriteString("---------------------------------------------------------------------------------------------\n")
	for _, s := range snaps {
		acc := strings.Join(s.Accounts, ",")
		if acc == "" {
			acc = "-"
		}
		if len(s.Windows) == 0 {
			fmt.Fprintf(&b, "%-13s %-21s %-9s %-14s %7s  %-7s  %s\n",
				s.Provider, truncate(acc, 21), "-", s.Err, "-", "-", staleMark(s))
			continue
		}
		for i, w := range s.Windows {
			prov := s.Provider
			accounts := acc
			if i > 0 {
				prov = ""
				accounts = ""
			}
			est := ""
			if w.LimitUSDEstimate > 0 {
				est = fmt.Sprintf("~%.2f", float64(w.Percent)/100*float64(w.LimitUSDEstimate))
			}
			reset := "-"
			if w.ResetsAt != nil && !w.ResetsAt.IsZero() {
				reset = w.ResetsAt.UTC().Format(time.RFC3339)
			}
			status := w.Status
			if i == 0 && s.Err != "" {
				status = status + "/" + s.Err
			}
			if i == 0 && s.Stale {
				reset = reset + " (stale)"
			}
			fmt.Fprintf(&b, "%-13s %-21s %-9s %-14s %7d  %-7s  %s\n",
				truncate(prov, 13), truncate(accounts, 21), w.Name, truncate(status, 14), w.Percent, est, reset)
		}
	}
	b.WriteString("\npercent 是上游整数百分比；~usd 按文档额度估算（5h $12 / 周 $30 / 月 $60），不是账单。\n")
	return b.String()
}

func staleMark(s Snapshot) string {
	if s.Err != "" {
		return s.Err
	}
	return "-"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
