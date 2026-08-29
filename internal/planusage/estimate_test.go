package planusage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestWeekStartUnix(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "est.json")
	st := grokWeekEstimate{PeriodStart: start.UTC().Format(time.RFC3339Nano)}
	if err := saveGrokWeekEstimate(path, st); err != nil {
		t.Fatal(err)
	}

	live := start.Add(-time.Hour)
	got := WeekStartUnix([]Snapshot{{Windows: []Window{{
		Name: "weekly", PeriodStart: &live,
	}}}}, path, now)
	if got != live.Unix() {
		t.Fatalf("snapshot wins: got %d want %d", got, live.Unix())
	}

	got = WeekStartUnix(nil, path, now)
	if got != start.Unix() {
		t.Fatalf("estimate file: got %d want %d", got, start.Unix())
	}

	got = WeekStartUnix(nil, filepath.Join(t.TempDir(), "missing.json"), now)
	want := now.Add(-7 * 24 * time.Hour).Unix()
	if got != want {
		t.Fatalf("7d fallback: got %d want %d", got, want)
	}

	if _, ok := StoredPeriodStart(""); ok {
		t.Fatal("empty path must miss")
	}
}

// TestWeekStartUnixRollsPastReset pins the weekly rollover: when the anchor
// (live snapshot or estimate file) is stale and the weekly boundary has
// already passed, WeekStartUnix must roll forward to the latest period start
// not after now — the default usage range resets on schedule without waiting
// for a fresh fetch or a writable estimate path.
func TestWeekStartUnixRollsPastReset(t *testing.T) {
	anchor := time.Date(2026, 8, 22, 8, 48, 26, 0, time.UTC)
	now := time.Date(2026, 8, 29, 22, 57, 0, 0, time.UTC) // past the 08-29 08:48 reset
	path := filepath.Join(t.TempDir(), "est.json")
	st := grokWeekEstimate{PeriodStart: anchor.UTC().Format(time.RFC3339Nano)}
	if err := saveGrokWeekEstimate(path, st); err != nil {
		t.Fatal(err)
	}

	next := anchor.Add(7 * 24 * time.Hour)
	got := WeekStartUnix(nil, path, now)
	if got != next.Unix() {
		t.Fatalf("stale estimate rolls past reset: got %d want %d", got, next.Unix())
	}

	// Live snapshot: the span is ResetsAt-PeriodStart; the roll uses it, so
	// the result lands exactly on the live window boundary even when the
	// span is not exactly 7 days.
	p2 := anchor.Add(7*24*time.Hour + 30*time.Minute)
	got = WeekStartUnix([]Snapshot{{Windows: []Window{{
		Name: "weekly", PeriodStart: &anchor, ResetsAt: &p2,
	}}}}, path, now)
	if got != p2.Unix() {
		t.Fatalf("live snapshot rolls by its own span: got %d want %d", got, p2.Unix())
	}

	// A multi-week stale anchor still rolls to the current period.
	old := anchor.Add(-14 * 24 * time.Hour)
	st2 := grokWeekEstimate{PeriodStart: old.UTC().Format(time.RFC3339Nano)}
	if err := saveGrokWeekEstimate(path, st2); err != nil {
		t.Fatal(err)
	}
	if got = WeekStartUnix(nil, path, now); got != next.Unix() {
		t.Fatalf("multi-week stale anchor: got %d want %d", got, next.Unix())
	}

	// Exactly at the reset moment the roll enters the new (empty) period.
	atReset := anchor.Add(7 * 24 * time.Hour)
	if got = WeekStartUnix(nil, path, atReset); got != atReset.Unix() {
		t.Fatalf("roll at exact reset: got %d want %d", got, atReset.Unix())
	}

	// Before the first boundary nothing rolls.
	before := anchor.Add(time.Hour)
	if got = WeekStartUnix(nil, path, before); got != anchor.Unix() {
		t.Fatalf("no roll before boundary: got %d want %d", got, anchor.Unix())
	}
}

func TestApplyGrokWeekEstimateFirstPeriodUsesLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "est.json")
	start := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	snap := Snapshot{Windows: []Window{{
		Name: "weekly", Percent: 50, PeriodStart: &start, ResetsAt: &end,
	}}}
	sum := func(context.Context, int64, int64) (int64, error) { return 1000, nil }
	got := ApplyGrokWeekEstimate(context.Background(), snap, sum, path, start.Add(time.Hour))
	if got.Windows[0].LimitTokensEstimate != 2000 {
		t.Fatalf("first period live estimate = %d, want 2000", got.Windows[0].LimitTokensEstimate)
	}
}

func TestApplyGrokWeekEstimateShowsPreviousAfterRollover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "est.json")
	p1 := time.Date(2026, 8, 15, 8, 0, 0, 0, time.UTC)
	p1end := p1.Add(7 * 24 * time.Hour)
	sum1 := func(context.Context, int64, int64) (int64, error) { return 5700, nil }
	snap1 := Snapshot{Windows: []Window{{
		Name: "weekly", Percent: 57, PeriodStart: &p1, ResetsAt: &p1end,
	}}}
	ApplyGrokWeekEstimate(context.Background(), snap1, sum1, path, p1.Add(6*24*time.Hour))

	p2 := p1end
	p2end := p2.Add(7 * 24 * time.Hour)
	sum2 := func(context.Context, int64, int64) (int64, error) { return 10, nil }
	snap2 := Snapshot{Windows: []Window{{
		Name: "weekly", Percent: 1, PeriodStart: &p2, ResetsAt: &p2end,
	}}}
	got := ApplyGrokWeekEstimate(context.Background(), snap2, sum2, path, p2.Add(time.Hour))
	want := int64(5700 * 100 / 57)
	if got.Windows[0].LimitTokensEstimate != want {
		t.Fatalf("after rollover = %d, want previous %d", got.Windows[0].LimitTokensEstimate, want)
	}
}

func TestApplyGrokWeekEstimateIgnoresNonGrokSumZeroPercent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "est.json")
	start := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	snap := Snapshot{Windows: []Window{{
		Name: "weekly", Percent: 0, PeriodStart: &start,
	}}}
	sum := func(context.Context, int64, int64) (int64, error) { return 100, nil }
	got := ApplyGrokWeekEstimate(context.Background(), snap, sum, path, start.Add(time.Hour))
	if got.Windows[0].LimitTokensEstimate != 0 {
		t.Fatalf("zero percent must not estimate, got %d", got.Windows[0].LimitTokensEstimate)
	}
}

func TestApplyGrokWeekEstimateSumErrorUsesLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "est.json")
	start := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	win := Window{Name: "weekly", Percent: 50, PeriodStart: &start, ResetsAt: &end}
	okSum := func(context.Context, int64, int64) (int64, error) { return 1000, nil }
	ApplyGrokWeekEstimate(context.Background(), Snapshot{Windows: []Window{win}}, okSum, path, start.Add(time.Hour))

	failSum := func(context.Context, int64, int64) (int64, error) { return 0, errors.New("db busy") }
	got := ApplyGrokWeekEstimate(context.Background(), Snapshot{Windows: []Window{win}}, failSum, path, start.Add(2*time.Hour))
	if got.Windows[0].LimitTokensEstimate != 2000 {
		t.Fatalf("sum error shown = %d, want live 2000", got.Windows[0].LimitTokensEstimate)
	}
}

func TestApplyGrokWeekEstimateRejectsStalePeriod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "est.json")
	p1 := time.Date(2026, 8, 15, 8, 0, 0, 0, time.UTC)
	p1end := p1.Add(7 * 24 * time.Hour)
	sum1 := func(context.Context, int64, int64) (int64, error) { return 5700, nil }
	ApplyGrokWeekEstimate(context.Background(), Snapshot{Windows: []Window{{
		Name: "weekly", Percent: 57, PeriodStart: &p1, ResetsAt: &p1end,
	}}}, sum1, path, p1.Add(6*24*time.Hour))

	p2 := p1end
	p2end := p2.Add(7 * 24 * time.Hour)
	sum2 := func(context.Context, int64, int64) (int64, error) { return 10, nil }
	ApplyGrokWeekEstimate(context.Background(), Snapshot{Windows: []Window{{
		Name: "weekly", Percent: 1, PeriodStart: &p2, ResetsAt: &p2end,
	}}}, sum2, path, p2.Add(time.Hour))
	want := int64(5700 * 100 / 57)

	staleSum := func(context.Context, int64, int64) (int64, error) {
		t.Fatal("stale period must not query usage")
		return 0, nil
	}
	got := ApplyGrokWeekEstimate(context.Background(), Snapshot{Windows: []Window{{
		Name: "weekly", Percent: 57, PeriodStart: &p1, ResetsAt: &p1end,
	}}}, staleSum, path, p1.Add(6*24*time.Hour))
	if got.Windows[0].LimitTokensEstimate != want {
		t.Fatalf("stale period shown = %d, want frozen %d", got.Windows[0].LimitTokensEstimate, want)
	}
}
