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

// WeekStartUnix is the SuperGrok week-window start used as the default
// usage range. Live weekly snapshots win, then the estimate file, then
// now minus 7 days so a cold start still bounds the query.
func WeekStartUnix(snaps []Snapshot, estimatePath string, now time.Time) int64 {
	for _, snap := range snaps {
		for _, w := range snap.Windows {
			if w.Name != "weekly" || w.PeriodStart == nil || w.PeriodStart.IsZero() {
				continue
			}
			return w.PeriodStart.Unix()
		}
	}
	if t, ok := StoredPeriodStart(estimatePath); ok {
		return t.Unix()
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
	PeriodStart     string `json:"period_start"`
	LiveTokens      int64  `json:"live_tokens"`
	LivePercent     int    `json:"live_percent"`
	LiveEstimate    int64  `json:"live_estimate"`
	DisplayEstimate int64  `json:"display_estimate"`
}

func (st grokWeekEstimate) shown() int64 {
	if st.DisplayEstimate > 0 {
		return st.DisplayEstimate
	}
	return st.LiveEstimate
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

// ApplyGrokWeekEstimate fills LimitTokensEstimate on the weekly window.
// Live tokens/percent update every call; the shown value is the previous
// period's estimate. The first period (no freeze yet) shows the live
// estimate so the column is not empty until the first rollover.
func ApplyGrokWeekEstimate(ctx context.Context, snap Snapshot, sum GrokTokenSum, path string, now time.Time) Snapshot {
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
	if err := withEstimateLock(path, func() error {
		st, _ := loadGrokWeekEstimate(path)
		if stored, err := time.Parse(time.RFC3339Nano, st.PeriodStart); err == nil && w.PeriodStart.Before(stored) {
			snap.Windows[idx].LimitTokensEstimate = st.shown()
			return nil
		}
		tokens, err := sum(ctx, from, to)
		if err != nil {
			slog.Warn("quota grok token sum failed", "error", err)
			snap.Windows[idx].LimitTokensEstimate = st.shown()
			return nil
		}
		newStart := w.PeriodStart.UTC().Format(time.RFC3339Nano)
		if st.PeriodStart != "" && st.PeriodStart != newStart && st.LiveEstimate > 0 {
			st.DisplayEstimate = st.LiveEstimate
		}
		st.PeriodStart = newStart
		st.LiveTokens = tokens
		st.LivePercent = w.Percent
		if w.Percent > 0 && tokens > 0 {
			st.LiveEstimate = tokens * 100 / int64(w.Percent)
		} else {
			st.LiveEstimate = 0
		}
		if err := saveGrokWeekEstimate(path, st); err != nil {
			slog.Warn("quota grok estimate save failed", "error", err)
		}
		snap.Windows[idx].LimitTokensEstimate = st.shown()
		return nil
	}); err != nil {
		slog.Warn("quota grok estimate lock failed", "error", err)
	}
	return snap
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
