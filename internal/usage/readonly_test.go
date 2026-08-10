package usage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadOnlyDSN(t *testing.T) {
	got := roDSN("/var/lib/prism/usage.db")
	if !strings.HasPrefix(got, "file:") {
		t.Errorf("ro DSN must use the file: URI form, got %q", got)
	}
	if !strings.Contains(got, "mode=ro") {
		t.Errorf("ro DSN must open strictly read-only, got %q", got)
	}
	if !strings.Contains(got, "_pragma=busy_timeout(5000)") {
		t.Errorf("ro DSN must set busy_timeout, got %q", got)
	}
	if strings.Contains(got, "journal_mode") {
		t.Errorf("ro DSN must NOT set journal_mode (fails on non-WAL files; WAL is persistent in the header), got %q", got)
	}
	// the path is escaped so spaces/special chars survive URI parsing
	if !strings.Contains(got, "%2F") {
		t.Errorf("ro DSN must escape the path, got %q", got)
	}
}

// TestReadOnlyRejectsWrites: the read-only store must never be able to write:
// Migrate/InsertBatch/DeleteBefore fail with "store not open" (no write
// pool), and the file stays untouched.
func TestReadOnlyRejectsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	w := NewSQLiteStore(path)
	if err := w.Open(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := w.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.InsertBatch(ctx, []Event{testEvent(time.Now(), "m", nil)}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	ro := NewReadOnlyStore(path)
	if err := ro.Open(); err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	defer ro.Close()
	if err := ro.Migrate(ctx); err == nil {
		t.Error("Migrate on a read-only store must fail")
	}
	if err := ro.InsertBatch(ctx, []Event{testEvent(time.Now(), "m", nil)}); err == nil {
		t.Error("InsertBatch on a read-only store must fail")
	}
	if _, err := ro.DeleteBefore(ctx, time.Now().Unix()); err == nil {
		t.Error("DeleteBefore on a read-only store must fail")
	}
	// reads still work
	rows, err := ro.Summary(ctx, SummaryQuery{})
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 {
		t.Errorf("read-only Summary = %+v, %v; want the 1 inserted row", rows, err)
	}
}

// TestReadOnlyConcurrentWithLiveWriter is the acceptance test for the CLI
// read path: a real Recorder keeps writing to the database while a
// read-only store (the exact connection the prism usage subcommand uses)
// queries the same file concurrently. It must never hit "database is
// locked" and must not disturb the writer: every recorded event is
// persisted and every concurrent query sees consistent aggregate data.
func TestReadOnlyConcurrentWithLiveWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store := NewSQLiteStore(path)
	rec := NewRecorder(Config{
		Enabled:      true,
		DBPath:       path,
		BatchSize:    8,
		BatchFlushMS: 5,
	}, store)
	rec.Start()

	var recorded atomic.Int64
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			rec.Record(Event{
				Ts:               time.Now(),
				RequestID:        fmt.Sprintf("r%d", i),
				Model:            "concurrent-model",
				PromptTokens:     100,
				CompletionTokens: 50,
				TotalTokens:      150,
				Success:          true,
			})
			recorded.Add(1)
			i++
			time.Sleep(time.Millisecond)
		}
	}()

	ro := NewReadOnlyStore(path)
	if err := ro.Open(); err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	ctx := context.Background()
	deadline := time.Now().Add(2 * time.Second)
	queries := 0
	var lastRequests int64 = -1
	for time.Now().Before(deadline) {
		rows, err := ro.Summary(ctx, SummaryQuery{})
		if err != nil {
			t.Fatalf("read-only Summary during live writes: %v", err)
		}
		ov, err := ro.Overview(ctx, SummaryQuery{})
		if err != nil {
			t.Fatalf("read-only Overview during live writes: %v", err)
		}
		if strings.Contains(fmt.Sprint(err), "locked") {
			t.Fatalf("read-only query hit a lock error: %v", err)
		}
		// The two queries are separate read transactions, so the writer may
		// commit between them; counts can only grow (monotonicity), never
		// regress or disagree in a way a snapshot would not allow.
		if len(rows) == 1 && ov.Requests < rows[0].Requests {
			t.Fatalf("Overview (%d) behind Summary (%d) — a snapshot consistency violation", ov.Requests, rows[0].Requests)
		}
		lastRequests = ov.Requests
		queries++
		time.Sleep(2 * time.Millisecond)
	}
	close(stop)
	<-writerDone
	rec.Close() // drain: every accepted event must be persisted

	if queries < 50 {
		t.Fatalf("only %d concurrent queries ran, want at least 50", queries)
	}
	if lastRequests == 0 {
		t.Fatal("read-only queries never observed any written events")
	}

	// The writer must not have been disturbed: with a 1024-slot queue and
	// ~1ms cadence nothing can overflow, so every Record call must have been
	// persisted (written == recorded, zero drops).
	want := recorded.Load()
	check := NewSQLiteStore(path)
	if err := check.Open(); err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	rows, err := check.Summary(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != want {
		t.Fatalf("persisted %+v (requests=%d), want exactly %d recorded events — the concurrent reader disturbed the writer", rows, rows[0].Requests, want)
	}
}

// TestReadOnlyOpenMissingFile: opening a nonexistent file read-only must
// fail cleanly (the CLI turns this into the friendly usage.enabled hint
// before ever calling Open, but the store itself must not panic or hang).
func TestReadOnlyOpenMissingFile(t *testing.T) {
	ro := NewReadOnlyStore(filepath.Join(t.TempDir(), "nope", "usage.db"))
	if err := ro.Open(); err == nil {
		t.Fatal("open of a missing file must fail")
	}
}
