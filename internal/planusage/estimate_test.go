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
	got := WeekStartUnix([]Snapshot{{Provider: "xai", Windows: []Window{{
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
	got = WeekStartUnix([]Snapshot{{Provider: "xai", Windows: []Window{{
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

// TestRollWeekStartFallbackAndFuture pins the two defensive branches of
// rollWeekStart directly: a non-positive span falls back to 7 days, and an
// anchor that lies in the future (clock skew / upstream oddity) is returned
// unchanged instead of being rolled backwards or panicking.
func TestRollWeekStartFallbackAndFuture(t *testing.T) {
	anchor := time.Date(2026, 8, 22, 8, 48, 26, 0, time.UTC)
	now := time.Date(2026, 8, 29, 22, 57, 0, 0, time.UTC)

	if got := rollWeekStart(anchor, 0, now); got.Unix() != anchor.Add(7*24*time.Hour).Unix() {
		t.Fatalf("zero span falls back to 7d: got %s want %s", got, anchor.Add(7*24*time.Hour))
	}
	if got := rollWeekStart(anchor, -time.Hour, now); got.Unix() != anchor.Add(7*24*time.Hour).Unix() {
		t.Fatalf("negative span falls back to 7d: got %s want %s", got, anchor.Add(7*24*time.Hour))
	}

	future := now.Add(48 * time.Hour)
	if got := rollWeekStart(future, weekFallback, now); !got.Equal(future) {
		t.Fatalf("future anchor must not roll: got %s want %s", got, future)
	}

	// A multi-week roll must also terminate for a far-past anchor (no
	// runaway loop): 3 years of weekly spans is a bounded iteration count.
	farPast := anchor.AddDate(-3, 0, 0)
	got := rollWeekStart(farPast, weekFallback, now)
	if got.After(now) || now.Sub(got) >= 7*24*time.Hour {
		t.Fatalf("far-past anchor rolls into current week: got %s (now %s)", got, now)
	}
}

// TestWeekStartUnixIgnoresGeminiWeekly pins the default usage range to the
// SuperGrok week: a Gemini weekly snapshot (own period start) must never
// win the anchor — the map-ordered cache could otherwise flip the
// /admin/usage/summary default window between the two providers.
func TestWeekStartUnixIgnoresGeminiWeekly(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	gemStart := now.Add(-2 * time.Hour)
	xaiStart := now.Add(-5 * 24 * time.Hour)
	snaps := []Snapshot{
		{Provider: "gemini", Windows: []Window{{
			Name: "weekly", PeriodStart: &gemStart,
		}}},
		{Provider: "xai", Windows: []Window{{
			Name: "weekly", PeriodStart: &xaiStart,
		}}},
	}
	got := WeekStartUnix(snaps, "", now)
	if got != xaiStart.Unix() {
		t.Fatalf("WeekStartUnix = %d, want xai week %d (gemini must not win)", got, xaiStart.Unix())
	}
}

func TestGeminiWeekStartUnixIgnoresXAI(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	gemStart := now.Add(-2 * time.Hour)
	xaiStart := now.Add(-5 * 24 * time.Hour)
	snaps := []Snapshot{
		{Provider: "xai", Windows: []Window{{
			Name: "weekly", PeriodStart: &xaiStart,
		}}},
		{Provider: "gemini", Windows: []Window{{
			Name: "weekly", PeriodStart: &gemStart,
		}}},
	}
	got := GeminiWeekStartUnix(snaps, "", now)
	if got != gemStart.Unix() {
		t.Fatalf("GeminiWeekStartUnix = %d, want gemini week %d (xai must not win)", got, gemStart.Unix())
	}
	// The SuperGrok helper must still ignore the gemini window.
	if got := WeekStartUnix(snaps, "", now); got != xaiStart.Unix() {
		t.Fatalf("WeekStartUnix = %d, want xai week %d", got, xaiStart.Unix())
	}
}

func TestWeekStartUnixGeminiOnlyFallsBack(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	gemStart := now.Add(-2 * time.Hour)
	snaps := []Snapshot{{Provider: "gemini", Windows: []Window{{
		Name: "weekly", PeriodStart: &gemStart,
	}}}}
	want := now.Add(-7 * 24 * time.Hour).Unix()
	if got := WeekStartUnix(snaps, "", now); got != want {
		t.Fatalf("gemini-only must not win WeekStartUnix: got %d want 7d fallback %d", got, want)
	}
	if got := GeminiWeekStartUnix(snaps, "", now); got != gemStart.Unix() {
		t.Fatalf("GeminiWeekStartUnix = %d want %d", got, gemStart.Unix())
	}
}

func TestApplyWeekEstimateEatsCombinedAgySum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gem.json")
	start := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	snap := Snapshot{Provider: "gemini", Windows: []Window{{
		Name: "weekly", Percent: 50, PeriodStart: &start, ResetsAt: &end,
	}}}
	usageSum := func(context.Context, int64, int64) (int64, error) { return 400, nil }
	agySum := func(context.Context, int64, int64) (int64, error) { return 600, nil }
	got := ApplyWeekEstimate(context.Background(), snap, CombineTokenSums(usageSum, agySum), path, start.Add(time.Hour))
	// 400+600 = 1000 tokens at 50% → pool 2000. ApplyWeekEstimate itself
	// is unchanged; the combined sum is what feeds it.
	if got.Windows[0].LimitTokensEstimate != 2000 {
		t.Fatalf("combined estimate = %d, want 2000", got.Windows[0].LimitTokensEstimate)
	}
}

func TestApplyWeekEstimateUsesUnflooredFraction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gem.json")
	start := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	// 12.7% used floors to Percent=12. Integer reversal would be
	// 1270*100/12 = 10583; unfloored 1270/0.127 = 10000.
	snap := Snapshot{Provider: "gemini", Windows: []Window{{
		Name: "weekly", Percent: 12, UsedFraction: 0.127,
		PeriodStart: &start, ResetsAt: &end,
	}}}
	sum := func(context.Context, int64, int64) (int64, error) { return 1270, nil }
	got := ApplyWeekEstimate(context.Background(), snap, sum, path, start.Add(time.Hour))
	if got.Windows[0].LimitTokensEstimate != 10000 {
		t.Fatalf("unfloored estimate = %d, want 10000", got.Windows[0].LimitTokensEstimate)
	}
}

func TestApplyWeekEstimateSubPercentStillInverts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gem.json")
	start := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	// 0.4% used floors to Percent=0; old path left the estimate empty.
	snap := Snapshot{Provider: "gemini", Windows: []Window{{
		Name: "weekly", Percent: 0, UsedFraction: 0.004,
		PeriodStart: &start, ResetsAt: &end,
	}}}
	sum := func(context.Context, int64, int64) (int64, error) { return 4000, nil }
	got := ApplyWeekEstimate(context.Background(), snap, sum, path, start.Add(time.Hour))
	if got.Windows[0].LimitTokensEstimate != 1_000_000 {
		t.Fatalf("sub-percent estimate = %d, want 1000000", got.Windows[0].LimitTokensEstimate)
	}
}

func TestCombineTokenSumsNil(t *testing.T) {
	if CombineTokenSums() != nil || CombineTokenSums(nil, nil) != nil {
		t.Fatal("all-nil must return nil so ApplyWeekEstimate no-ops")
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

func TestApplyGrokWeekEstimateShowsLiveAfterRollover(t *testing.T) {
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
	// LIVE reversal: 10 tokens at 1% used → pool ≈ 1000, not the previous
	// period's frozen value.
	if got.Windows[0].LimitTokensEstimate != 1000 {
		t.Fatalf("after rollover = %d, want live 1000", got.Windows[0].LimitTokensEstimate)
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

func TestApplyGrokWeekEstimateSumErrorLeavesEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "est.json")
	start := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	win := Window{Name: "weekly", Percent: 50, PeriodStart: &start, ResetsAt: &end}

	failSum := func(context.Context, int64, int64) (int64, error) { return 0, errors.New("db busy") }
	got := ApplyGrokWeekEstimate(context.Background(), Snapshot{Windows: []Window{win}}, failSum, path, start.Add(time.Hour))
	if got.Windows[0].LimitTokensEstimate != 0 {
		t.Fatalf("sum error must leave estimate empty, got %d", got.Windows[0].LimitTokensEstimate)
	}
}

func TestApplyGrokWeekEstimateLiveEveryPeriod(t *testing.T) {
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

	// The same (older) snapshot queried again still reverses live from its
	// own period data — there is no stale freeze any more.
	liveSum := func(context.Context, int64, int64) (int64, error) { return 5700, nil }
	got := ApplyGrokWeekEstimate(context.Background(), Snapshot{Windows: []Window{{
		Name: "weekly", Percent: 57, PeriodStart: &p1, ResetsAt: &p1end,
	}}}, liveSum, path, p1.Add(6*24*time.Hour))
	if got.Windows[0].LimitTokensEstimate != 10000 {
		t.Fatalf("older snapshot reversed live = %d, want 10000", got.Windows[0].LimitTokensEstimate)
	}
}
