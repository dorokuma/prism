package metapiusage

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSourcePicksUpReplacedFile pins oracle item 1(a): the source must
// re-open the database every round. Replacing the FILE under the same path
// (a metapi restore or data-plane rollback: a new inode) must be visible to
// the next sum; a persistent handle would keep reading the old inode and
// serve frozen totals.
func TestSourcePicksUpReplacedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	from, to := base.Unix(), base.Add(time.Hour).Unix()

	writeFixtureAt(t, path, []metapiFixtureRow{
		{model: "cline-pass/a", createdAt: base.Add(time.Minute), total: i64(100)},
	})
	src := NewSource(path)
	got, err := src.SumClinePassTokens(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if got != 100 {
		t.Fatalf("sum = %d, want 100", got)
	}

	next := filepath.Join(dir, "hub-replacement.db")
	writeFixtureAt(t, next, []metapiFixtureRow{
		{model: "cline-pass/a", createdAt: base.Add(time.Minute), total: i64(7)},
		{model: "cline-pass/b", createdAt: base.Add(2 * time.Minute), total: i64(8)},
	})
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	got, err = src.SumClinePassTokens(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if got != 15 {
		t.Fatalf("sum after file replacement = %d, want 15 (the replaced file must be read, not a frozen inode)", got)
	}
}

// TestSourceStartupUnavailableThenRecovers pins oracle item 1(b): a
// database that is unavailable while prism starts (boot ordering, a
// permission window) must be retried on later rounds, not disabled for the
// process lifetime.
func TestSourceStartupUnavailableThenRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "hub.db") // parent dir absent
	src := NewSource(path)
	ctx := context.Background()
	base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	from, to := base.Unix(), base.Add(time.Hour).Unix()

	if _, err := src.SumClinePassTokens(ctx, from, to); err == nil {
		t.Fatal("sum with the database absent must fail")
	}

	// metapi comes up later: the very next round must see it.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureAt(t, path, []metapiFixtureRow{
		{model: "cline-pass/a", createdAt: base.Add(time.Minute), total: i64(42)},
	})
	got, err := src.SumClinePassTokens(ctx, from, to)
	if err != nil {
		t.Fatalf("recovery sum: %v", err)
	}
	if got != 42 {
		t.Fatalf("sum after recovery = %d, want 42", got)
	}
}

// TestSourceTransitionLogging pins the observability contract: the WARN is
// emitted once per healthy→degraded transition (never once per round), the
// error counter counts every failed sum, the status expvar follows the
// state, and the first success after a failure logs the recovery exactly
// once.
func TestSourceTransitionLogging(t *testing.T) {
	var buf bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(oldDefault) })

	errorsBefore := sourceErrors.Value()
	path := filepath.Join(t.TempDir(), "data", "hub.db")
	src := NewSource(path)
	ctx := context.Background()
	base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	from, to := base.Unix(), base.Add(time.Hour).Unix()

	// A single refresh round sums once per ClinePass window (5h / weekly /
	// monthly): three failures in a row are one degraded state, not three
	// warnings.
	for i := 0; i < 3; i++ {
		if _, err := src.SumClinePassTokens(ctx, from, to); err == nil {
			t.Fatal("sum with the database absent must fail")
		}
	}
	if got := strings.Count(buf.String(), "clinepass usage source unavailable"); got != 1 {
		t.Fatalf("degradation WARNs = %d, want exactly 1 per transition:\n%s", got, buf.String())
	}
	if got := sourceErrors.Value() - errorsBefore; got != 3 {
		t.Fatalf("error counter delta = %d, want 3 (one per failed sum)", got)
	}
	if got := sourceStatus.Value(); got != "degraded" {
		t.Fatalf("source status = %q, want \"degraded\"", got)
	}

	// Recovery: the first success logs once and flips the status back.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureAt(t, path, []metapiFixtureRow{
		{model: "cline-pass/a", createdAt: base.Add(time.Minute), total: i64(1)},
	})
	if _, err := src.SumClinePassTokens(ctx, from, to); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "clinepass usage source recovered"); got != 1 {
		t.Fatalf("recovery logs = %d, want 1:\n%s", got, buf.String())
	}
	if got := sourceStatus.Value(); got != "ok" {
		t.Fatalf("source status after recovery = %q, want \"ok\"", got)
	}

	// Later successes stay silent: recovery is a transition, not a report.
	if _, err := src.SumClinePassTokens(ctx, from, to); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "clinepass usage source recovered"); got != 1 {
		t.Fatalf("recovery logs after a second success = %d, want 1", got)
	}

	// A second failure is a new transition: exactly one more WARN.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := src.SumClinePassTokens(ctx, from, to); err == nil {
		t.Fatal("sum after removal must fail")
	}
	if got := strings.Count(buf.String(), "clinepass usage source unavailable"); got != 2 {
		t.Fatalf("degradation WARNs after a second transition = %d, want 2:\n%s", got, buf.String())
	}
	if got := sourceErrors.Value() - errorsBefore; got != 4 {
		t.Fatalf("error counter delta = %d, want 4", got)
	}
}
