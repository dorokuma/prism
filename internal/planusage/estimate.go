package planusage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// DefaultGrokEstimatePath is the on-disk freeze of last week's inferred
// SuperGrok pool size. Displayed during the current week so a fresh
// reset is not estimated from empty usage.
const DefaultGrokEstimatePath = "/var/lib/prism/quota/grok-week-estimate.json"

// DefaultGeminiEstimatePath is the on-disk freeze of the Gemini week-pool
// inference (usage-db gemini-* tokens ÷ week percent). The estimate only
// exists while usage rows for gemini-* models are recorded (currently
// none: Antigravity usage does not pass through prism, so the column stays
// -- until a consumption source lands).
const DefaultGeminiEstimatePath = "/var/lib/prism/quota/gemini-week-estimate.json"

// GrokBuildImportWindow is the [from,to] unix range for Grok Build session
// import: previous SuperGrok week plus the current week through now.
func GrokBuildImportWindow(snap Snapshot, now time.Time) (from, to int64) {
	to = now.Unix()
	from = now.Add(-14 * 24 * time.Hour).Unix()
	for _, w := range snap.Windows {
		if w.Name != "weekly" || w.PeriodStart == nil {
			continue
		}
		span := 7 * 24 * time.Hour
		if w.ResetsAt != nil {
			if d := w.ResetsAt.Sub(*w.PeriodStart); d > 0 {
				span = d
			}
		}
		return w.PeriodStart.Add(-span).Unix(), to
	}
	return from, to
}

const weekFallback = 7 * 24 * time.Hour

// rollWeekStart advances a weekly period start by whole window spans until
// it is the latest start that is not after now. The default usage range then
// resets at the weekly boundary on schedule — even when the live snapshot or
// the estimate file is stale (upstream fetch failures, read-only estimate
// path) — instead of freezing on the last known period forever. The anchor
// (wall-clock moment of the last confirmed reset) is preserved, so a 7-day
// roll lands on the same time-of-day as the upstream window. span <= 0 falls
// back to 7 days.
func rollWeekStart(start time.Time, span time.Duration, now time.Time) time.Time {
	if span <= 0 {
		span = weekFallback
	}
	for end := start.Add(span); !end.After(now); end = start.Add(span) {
		start = end
	}
	return start
}

// WeekStartUnix is the SuperGrok week-window start used as the default
// usage range. Live weekly snapshots of the xai provider win, then the
// estimate file, then now minus 7 days so a cold start still bounds the
// query. Whichever source provides the anchor, it is rolled forward to the
// latest weekly boundary not after now, so the range resets in step with
// the grok quota period even when the source is stale. Only xai weekly
// windows are considered: Gemini (and any future provider) has its own
// weekly window with a different period start, and the usage default must
// stay pinned to the SuperGrok week (documented contract).
func WeekStartUnix(snaps []Snapshot, estimatePath string, now time.Time) int64 {
	for _, snap := range snaps {
		if snap.Provider != "xai" {
			continue
		}
		for _, w := range snap.Windows {
			if w.Name != "weekly" || w.PeriodStart == nil || w.PeriodStart.IsZero() {
				continue
			}
			span := weekFallback
			if w.ResetsAt != nil && !w.ResetsAt.IsZero() {
				if d := w.ResetsAt.Sub(*w.PeriodStart); d > 0 {
					span = d
				}
			}
			return rollWeekStart(*w.PeriodStart, span, now).Unix()
		}
	}
	if t, ok := StoredPeriodStart(estimatePath); ok {
		return rollWeekStart(t, weekFallback, now).Unix()
	}
	return now.Add(-weekFallback).Unix()
}

// StoredPeriodStart reads period_start from the grok week-estimate file.
func StoredPeriodStart(path string) (time.Time, bool) {
	if path == "" {
		return time.Time{}, false
	}
	st, err := loadGrokWeekEstimate(path)
	if err != nil {
		return time.Time{}, false
	}
	s := strings.TrimSpace(st.PeriodStart)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil && !t.IsZero() {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil && !t.IsZero() {
		return t, true
	}
	return time.Time{}, false
}

// GrokTokenSum returns grok-* token totals in [fromUnix, toUnix].
type GrokTokenSum func(ctx context.Context, fromUnix, toUnix int64) (int64, error)

type grokWeekEstimate struct {
	PeriodStart  string `json:"period_start"`
	LiveTokens   int64  `json:"live_tokens"`
	LivePercent  int    `json:"live_percent"`
	LiveEstimate int64  `json:"live_estimate"`
}

func withEstimateLock(path string, fn func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	_ = chownLike(lockPath, dir)
	return fn()
}

// ApplyWeekEstimate fills LimitTokensEstimate on the weekly window of
// any provider with the LIVE reversal: consumed tokens in the current
// period ÷ the period's used percent × 100. The estimate therefore moves
// with real usage (percent and consumption), unlike the old frozen
// previous-week value. The on-disk file keeps period_start (used by
// WeekStartUnix) plus the live snapshot for inspection; sum errors leave
// the estimate empty rather than showing a stale value.
func ApplyWeekEstimate(ctx context.Context, snap Snapshot, sum GrokTokenSum, path string, now time.Time) Snapshot {
	if sum == nil || path == "" {
		return snap
	}
	idx := -1
	for i := range snap.Windows {
		if snap.Windows[i].Name == "weekly" {
			idx = i
			break
		}
	}
	if idx < 0 || snap.Windows[idx].PeriodStart == nil {
		return snap
	}
	w := snap.Windows[idx]
	from := w.PeriodStart.Unix()
	to := now.Unix()
	if w.ResetsAt != nil && !w.ResetsAt.After(now) {
		to = w.ResetsAt.Unix()
	}
	tokens, err := sum(ctx, from, to)
	if err != nil {
		slog.Warn("quota week token sum failed", "error", err)
		return snap
	}
	if w.Percent > 0 && tokens > 0 {
		snap.Windows[idx].LimitTokensEstimate = tokens * 100 / int64(w.Percent)
	}
	if err := withEstimateLock(path, func() error {
		st, _ := loadGrokWeekEstimate(path)
		st.PeriodStart = w.PeriodStart.UTC().Format(time.RFC3339Nano)
		st.LiveTokens = tokens
		st.LivePercent = w.Percent
		if w.Percent > 0 && tokens > 0 {
			st.LiveEstimate = tokens * 100 / int64(w.Percent)
		} else {
			st.LiveEstimate = 0
		}
		return saveGrokWeekEstimate(path, st)
	}); err != nil {
		slog.Warn("quota week estimate lock failed", "error", err)
	}
	return snap
}

// ApplyGrokWeekEstimate fills LimitTokensEstimate on the SuperGrok weekly
// window (see ApplyWeekEstimate).
func ApplyGrokWeekEstimate(ctx context.Context, snap Snapshot, sum GrokTokenSum, path string, now time.Time) Snapshot {
	return ApplyWeekEstimate(ctx, snap, sum, path, now)
}

func loadGrokWeekEstimate(path string) (grokWeekEstimate, error) {
	var st grokWeekEstimate
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return grokWeekEstimate{}, err
	}
	return st, nil
}

func saveGrokWeekEstimate(path string, st grokWeekEstimate) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if parent := filepath.Dir(dir); parent != "" && parent != dir {
		_ = chownLike(dir, parent)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".grok-est-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if err := chownLike(path, dir); err != nil {
		return fmt.Errorf("chown grok estimate: %w", err)
	}
	ok = true
	return nil
}

func chownLike(path, like string) error {
	fi, err := os.Stat(like)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	wantUID, wantGID := int(st.Uid), int(st.Gid)
	cur, err := os.Stat(path)
	if err != nil {
		return err
	}
	if cst, ok := cur.Sys().(*syscall.Stat_t); ok && int(cst.Uid) == wantUID && int(cst.Gid) == wantGID {
		return nil
	}
	return os.Chown(path, wantUID, wantGID)
}
