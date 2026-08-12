package usage

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/util"
)

// fakeStore is a Store implementation for writer tests that can be told to
// panic or fail on InsertBatch, to prove the writer survives.
type fakeStore struct {
	panicOnInsert bool
	errOnInsert   bool
	inserted      int
	mu            sync.Mutex
}

func (f *fakeStore) Open() error                   { return nil }
func (f *fakeStore) Close() error                  { return nil }
func (f *fakeStore) Migrate(context.Context) error { return nil }
func (f *fakeStore) Summary(context.Context, SummaryQuery) ([]SummaryRow, error) {
	return nil, nil
}
func (f *fakeStore) Overview(context.Context, SummaryQuery) (*Overview, error) {
	return nil, nil
}
func (f *fakeStore) DeleteBefore(context.Context, int64) (int64, error) { return 0, nil }

func (f *fakeStore) InsertBatch(_ context.Context, events []Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicOnInsert {
		panic("fake store boom")
	}
	if f.errOnInsert {
		return fmt.Errorf("fake insert failure")
	}
	f.inserted += len(events)
	return nil
}

func (f *fakeStore) insertedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inserted
}

// TestRecordNonBlockingWhenChannelFull is the acceptance test for the
// non-blocking Record: with a full channel (no worker draining), Record must
// return immediately and count the drop.
func TestRecordNonBlockingWhenChannelFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	r := NewRecorder(Config{Enabled: true, DBPath: path, ChannelSize: 1, BatchSize: 1000, BatchFlushMS: 60000}, &fakeStore{})
	// never started: nothing drains the channel, the single slot stays full

	before := util.MetricsUsageEventsDropped.Value()
	r.Record(Event{Model: "m1"}) // fills the only slot

	start := time.Now()
	r.Record(Event{Model: "m2"}) // must not block
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Record blocked for %v, want non-blocking drop", elapsed)
	}
	if got := util.MetricsUsageEventsDropped.Value(); got != before+1 {
		t.Fatalf("dropped counter = %d, want %d", got, before+1)
	}

	// hammer: still no blocking
	start = time.Now()
	for i := 0; i < 10000; i++ {
		r.Record(Event{Model: "m3"})
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("10000 Records took %v, want fast non-blocking drops", elapsed)
	}
	r.Close() // never started: must be a no-op that does not hang
}

func TestNilRecorderIsNoOp(t *testing.T) {
	var r *Recorder
	r.Record(Event{}) // must not panic
	r.Start()         // must not panic
	r.Close()         // must not panic
}

func TestDisabledRecorderIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store := &SQLiteStore{path: path}
	r := NewRecorder(Config{Enabled: false, DBPath: path}, store)
	r.Start()
	r.Record(Event{Model: "m"})
	r.Close()
	if store.writePool() != nil {
		t.Fatal("disabled recorder must not open the store")
	}
}

// TestRecorderFlushAndClose is the acceptance test for "Close 后数据已
// flush": events recorded before Close must all be persisted.
func TestRecorderFlushAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store := &SQLiteStore{path: path}
	price := &Price{Input: 1, Output: 2}
	c, _ := costOf(10, 5, 0, 0, "", price)
	r := NewRecorder(Config{Enabled: true, DBPath: path, ChannelSize: 256, BatchSize: 10, BatchFlushMS: 10}, store)
	r.Start()
	const n = 37 // odd number, exercises partial final batch
	for i := 0; i < n; i++ {
		r.Record(Event{
			Ts: time.Now(), RequestID: fmt.Sprintf("req-%d", i), Model: "m",
			Provider: "p", Account: "a", KeyID: "k", Success: true, Status: 200,
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			Cost: c, CostStatus: CostStatusOK,
		})
	}
	writtenBefore := util.MetricsUsageEventsWritten.Value()
	droppedBefore := util.MetricsUsageEventsDropped.Value()
	r.Close()

	// verify through a fresh store on the same file
	check := &SQLiteStore{path: path}
	if err := check.Open(); err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := check.Summary(context.Background(), SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != n {
		t.Fatalf("persisted %+v, want exactly %d events", rows, n)
	}
	if got := util.MetricsUsageEventsWritten.Value(); got != writtenBefore+n {
		t.Fatalf("usage_events_written delta = %d, want %d", got-writtenBefore, n)
	}
	// Normal close: nothing may be counted dropped (the drain persisted
	// everything), keeping the identity written + dropped == total.
	if got := util.MetricsUsageEventsDropped.Value(); got != droppedBefore {
		t.Fatalf("usage_events_dropped delta = %d, want 0 on the normal path", got-droppedBefore)
	}
}

// TestCloseDrainsBufferedEvents: with batch size and flush interval far above
// the event count, Close must still drain the channel and flush everything.
func TestCloseDrainsBufferedEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store := &SQLiteStore{path: path}
	r := NewRecorder(Config{Enabled: true, DBPath: path, ChannelSize: 1024, BatchSize: 1000, BatchFlushMS: 60000}, store)
	r.Start()
	const n = 15
	c, _ := costOf(1, 1, 0, 0, "", &Price{Input: 1, Output: 1})
	for i := 0; i < n; i++ {
		r.Record(Event{
			Ts: time.Now(), RequestID: fmt.Sprintf("req-%d", i), Model: "m",
			PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2,
			Cost: c, CostStatus: CostStatusOK,
		})
	}
	r.Close()

	check := &SQLiteStore{path: path}
	if err := check.Open(); err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := check.Summary(context.Background(), SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != n {
		t.Fatalf("after Close: %+v, want %d events drained and flushed", rows, n)
	}
}

// TestConcurrentRecordAndSummary runs the real Recorder (channel + worker +
// batched writes) while readers run Summary on the same store: no
// "database is locked" may surface.
func TestConcurrentRecordAndSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	store := &SQLiteStore{path: path}
	price := &Price{Input: 0.14, Output: 0.42}
	c, _ := costOf(10, 5, 0, 0, "", price)
	r := NewRecorder(Config{Enabled: true, DBPath: path, ChannelSize: 1024, BatchSize: 50, BatchFlushMS: 20}, store)
	r.Start()

	const writers = 4
	const perWriter = 200
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				r.Record(Event{
					Ts: time.Now(), RequestID: fmt.Sprintf("w%d-%d", w, i), Model: "m",
					Provider: "p", Account: "a", KeyID: "k", Stream: true, Success: true,
					Status: 200, PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
					Cost: c, CostStatus: CostStatusOK,
				})
			}
		}(w)
	}

	// concurrent readers on the same store's read pool while the worker flushes
	errCh := make(chan error, 4)
	var rwg sync.WaitGroup
	for i := 0; i < 4; i++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for j := 0; j < 40; j++ {
				if _, err := store.Summary(context.Background(), SummaryQuery{GroupBy: []string{"model"}}); err != nil {
					errCh <- err
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}

	wg.Wait()
	rwg.Wait()
	close(errCh)
	for err := range errCh {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "locked") || strings.Contains(lower, "busy") {
			t.Errorf("SQLITE_BUSY surfaced: %v", err)
		}
		t.Error(err)
	}
	r.Close()

	// the recorder closed its store; verify through a fresh store on the file
	check := &SQLiteStore{path: path}
	if err := check.Open(); err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := check.Summary(context.Background(), SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	want := writers * perWriter
	if len(rows) != 1 || rows[0].Requests != int64(want) {
		t.Fatalf("persisted %+v, want %d events", rows, want)
	}
}

// TestWriterSurvivesInsertPanic: a panic inside InsertBatch must drop the
// batch, count it, and keep the writer goroutine alive.
func TestWriterSurvivesInsertPanic(t *testing.T) {
	store := &fakeStore{panicOnInsert: true}
	r := NewRecorder(Config{Enabled: true, ChannelSize: 64, BatchSize: 5, BatchFlushMS: 5}, store)
	r.Start()
	c, _ := costOf(0, 0, 0, 0, "", &Price{Input: 1, Output: 1})
	errorsBefore := util.MetricsUsageWriteErrors.Value()
	for i := 0; i < 100; i++ {
		r.Record(Event{Model: "m", Cost: c, CostStatus: CostStatusOK})
	}
	deadline := time.Now().Add(5 * time.Second)
	for util.MetricsUsageWriteErrors.Value() <= errorsBefore && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if util.MetricsUsageWriteErrors.Value() == errorsBefore {
		t.Fatal("writer never hit the panic path")
	}
	// worker must still be alive and Close must not hang
	r.Record(Event{Model: "m2"})
	done := make(chan struct{})
	go func() {
		r.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung after writer panic")
	}
}

// TestWriterSurvivesInsertError: a plain InsertBatch error is logged and
// counted, the writer keeps going.
func TestWriterSurvivesInsertError(t *testing.T) {
	store := &fakeStore{errOnInsert: true}
	r := NewRecorder(Config{Enabled: true, ChannelSize: 64, BatchSize: 5, BatchFlushMS: 5}, store)
	r.Start()
	c, _ := costOf(0, 0, 0, 0, "", &Price{Input: 1, Output: 1})
	errorsBefore := util.MetricsUsageWriteErrors.Value()
	for i := 0; i < 50; i++ {
		r.Record(Event{Model: "m", Cost: c, CostStatus: CostStatusOK})
	}
	deadline := time.Now().Add(5 * time.Second)
	for util.MetricsUsageWriteErrors.Value() <= errorsBefore && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if util.MetricsUsageWriteErrors.Value() == errorsBefore {
		t.Fatal("insert errors were never counted")
	}
	done := make(chan struct{})
	go func() {
		r.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung after insert errors")
	}
}

// TestConcurrentRecordAndCloseRace is the acceptance test for the Record vs
// Close synchronization protocol: 50 goroutines hammer Record while another
// goroutine calls Close, run under -race across multiple rounds. No panic
// may surface and no data race may be reported, and every event must be
// accounted for exactly once: persisted (written) or counted as dropped.
// This pins the two properties the old design lacked: sending on the closed
// channel (panic) and silently losing events that raced the drain.
func TestConcurrentRecordAndCloseRace(t *testing.T) {
	const goroutines = 50
	const perG = 100
	const rounds = 5

	for round := 0; round < rounds; round++ {
		path := filepath.Join(t.TempDir(), "usage.db")
		store := &SQLiteStore{path: path}
		r := NewRecorder(Config{Enabled: true, DBPath: path, ChannelSize: 256, BatchSize: 16, BatchFlushMS: 5}, store)
		r.Start()
		c, _ := costOf(10, 5, 0, 0, SourceOpenAI, &Price{Input: 1, Output: 1})

		writtenBefore := util.MetricsUsageEventsWritten.Value()
		droppedBefore := util.MetricsUsageEventsDropped.Value()

		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				for i := 0; i < perG; i++ {
					r.Record(Event{
						Ts: time.Now(), RequestID: fmt.Sprintf("r%d-g%d-i%d", round, g, i),
						Model: "m", Provider: "p", Account: "a", KeyID: "k",
						Success: true, Status: 200,
						PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
						Source: SourceOpenAI, Cost: c, CostStatus: CostStatusOK,
					})
				}
			}(g)
		}
		close(start)
		// Let some events land, then Close while the goroutines are still
		// recording — this is the race window under test.
		time.Sleep(2 * time.Millisecond)
		r.Close()
		wg.Wait()

		written := util.MetricsUsageEventsWritten.Value() - writtenBefore
		dropped := util.MetricsUsageEventsDropped.Value() - droppedBefore
		total := int64(goroutines * perG)
		if written+dropped != total {
			t.Fatalf("round %d: written %d + dropped %d = %d, want %d (every event accounted exactly once)",
				round, written, dropped, written+dropped, total)
		}
		// closeOnce: a second Close is an idempotent no-op that must not
		// panic or hang.
		r.Close()
	}
}

// hangStore blocks InsertBatch until its context is cancelled (simulating a
// store that is stuck past the close timeout) and records whether a store
// Close ever lands while an insert is in flight — the use-after-close hazard
// the old Close had on its timeout path.
type hangStore struct {
	mu            sync.Mutex
	closeCalls    int
	inFlight      int
	useAfterClose bool
}

func (h *hangStore) Open() error                   { return nil }
func (h *hangStore) Close() error                  { h.mu.Lock(); defer h.mu.Unlock(); h.closeCalls++; return nil }
func (h *hangStore) Migrate(context.Context) error { return nil }
func (h *hangStore) Summary(context.Context, SummaryQuery) ([]SummaryRow, error) {
	return nil, nil
}
func (h *hangStore) Overview(context.Context, SummaryQuery) (*Overview, error) {
	return nil, nil
}
func (h *hangStore) DeleteBefore(context.Context, int64) (int64, error) { return 0, nil }

func (h *hangStore) InsertBatch(ctx context.Context, events []Event) error {
	h.mu.Lock()
	h.inFlight++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.inFlight--
		h.mu.Unlock()
	}()
	<-ctx.Done() // block until Close's abort cancels the context
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closeCalls > 0 {
		h.useAfterClose = true
	}
	return ctx.Err()
}

// TestCloseTimeoutAbortsStuckInsertWithoutUseAfterClose is the acceptance
// test for the Close timeout path: when the worker is stuck inside
// InsertBatch past closeFlushTimeout, Close must abort the in-flight
// operation via the cancellable context, wait for the worker to really
// exit, and must never close the store while a write could still be in
// flight (use-after-close). The store is closed exactly once, by the
// worker, after its last insert; the loss is observable via the write-error
// counter and the per-batch error logs.
func TestCloseTimeoutAbortsStuckInsertWithoutUseAfterClose(t *testing.T) {
	old := closeFlushTimeout
	closeFlushTimeout = 50 * time.Millisecond
	defer func() { closeFlushTimeout = old }()

	store := &hangStore{}
	r := NewRecorder(Config{Enabled: true, ChannelSize: 64, BatchSize: 4, BatchFlushMS: 60000}, store)
	r.Start()
	c, _ := costOf(0, 0, 0, 0, "", &Price{Input: 1, Output: 1})
	// The worker picks these up and blocks inside InsertBatch (batch size 4
	// triggers the first flush; the 60s ticker never fires).
	for i := 0; i < 20; i++ {
		r.Record(Event{RequestID: fmt.Sprintf("r-%d", i), Model: "m", Cost: c, CostStatus: CostStatusOK})
	}
	// Wait until the worker is actually stuck inside InsertBatch.
	deadline := time.Now().Add(5 * time.Second)
	for {
		store.mu.Lock()
		inflight := store.inFlight
		store.mu.Unlock()
		if inflight > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never entered InsertBatch")
		}
		time.Sleep(time.Millisecond)
	}

	errorsBefore := util.MetricsUsageWriteErrors.Value()
	droppedBefore := util.MetricsUsageEventsDropped.Value()
	writtenBefore := util.MetricsUsageEventsWritten.Value()
	start := time.Now()
	r.Close()
	elapsed := time.Since(start)

	// Phase 1 waits closeFlushTimeout, then the abort unsticks the worker
	// and phase 2 confirms the exit: Close returns well inside 2× the
	// timeout plus slack.
	if elapsed > 2*closeFlushTimeout+time.Second {
		t.Fatalf("Close took %v, want bounded by the close budget", elapsed)
	}
	store.mu.Lock()
	useAfterClose := store.useAfterClose
	closeCalls := store.closeCalls
	inflight := store.inFlight
	store.mu.Unlock()
	if useAfterClose {
		t.Fatal("store.Close raced an in-flight InsertBatch (use-after-close)")
	}
	if inflight != 0 {
		t.Fatal("worker still inside InsertBatch after Close returned")
	}
	// The worker closes the store exactly once, strictly after its last
	// insert returned.
	if closeCalls != 1 {
		t.Fatalf("store.Close calls = %d, want exactly 1 (from the worker)", closeCalls)
	}
	// Loss is observable: every aborted/dropped batch is counted as a write
	// error and logged ("batch insert failed, dropping batch").
	errorsDelta := util.MetricsUsageWriteErrors.Value() - errorsBefore
	if errorsDelta == 0 {
		t.Fatal("close-timeout loss was not counted (write-error counter unchanged)")
	}

	// Accounting identity on the timeout path (no double counting): all 20
	// events were lost, each counted exactly once as dropped (one full batch
	// of 4 per failed flush: the in-flight batch aborted + 4 drain batches
	// rejected by the cancelled context); nothing was written; and the 5
	// failed batches are the 5 write-error incidents. written + dropped
	// reconciles the total Record count, and the incidents explain the
	// batches.
	dropped := util.MetricsUsageEventsDropped.Value() - droppedBefore
	written := util.MetricsUsageEventsWritten.Value() - writtenBefore
	if written != 0 {
		t.Errorf("written delta = %d, want 0 (no batch ever committed)", written)
	}
	if dropped != 20 {
		t.Errorf("dropped delta = %d, want 20 (every lost event counted exactly once)", dropped)
	}
	if written+dropped != 20 {
		t.Errorf("written+dropped = %d, want 20 (every Record accounted exactly once, no double count)", written+dropped)
	}
	if errorsDelta != 5 {
		t.Errorf("write-error delta = %d, want 5 (one incident per failed batch of 4)", errorsDelta)
	}
}

// TestRetentionZeroDisablesCleanup is the acceptance test for the
// retention_days=0 semantics (config layer now preserves an explicit 0):
// the cleanup loop must not delete anything, so even a year-old event
// survives multiple cleanup passes.
func TestRetentionZeroDisablesCleanup(t *testing.T) {
	old := cleanupInterval
	cleanupInterval = 20 * time.Millisecond
	defer func() { cleanupInterval = old }()

	path := filepath.Join(t.TempDir(), "usage.db")
	store := &SQLiteStore{path: path}
	r := NewRecorder(Config{Enabled: true, DBPath: path, RetentionDays: 0, ChannelSize: 64, BatchSize: 100, BatchFlushMS: 10}, store)
	r.Start()
	c, _ := costOf(0, 0, 0, 0, "", &Price{Input: 1, Output: 1})
	r.Record(Event{Ts: time.Now().Add(-365 * 24 * time.Hour), RequestID: "ancient", Model: "m", Cost: c, CostStatus: CostStatusOK})
	r.Record(Event{Ts: time.Now(), RequestID: "recent", Model: "m", Cost: c, CostStatus: CostStatusOK})
	// Let several cleanup passes run; none may delete anything.
	time.Sleep(300 * time.Millisecond)
	r.Close()

	check := &SQLiteStore{path: path}
	if err := check.Open(); err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := check.Summary(context.Background(), SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 2 {
		t.Fatalf("after retention_days=0: %+v, want both events kept (cleanup disabled)", rows)
	}
}

// TestRetentionCleanupDeletesOldEvents: with a shortened cleanup interval,
// events older than RetentionDays are removed by the background loop.
func TestRetentionCleanupDeletesOldEvents(t *testing.T) {
	old := cleanupInterval
	cleanupInterval = 20 * time.Millisecond
	defer func() { cleanupInterval = old }()

	path := filepath.Join(t.TempDir(), "usage.db")
	store := &SQLiteStore{path: path}
	r := NewRecorder(Config{Enabled: true, DBPath: path, RetentionDays: 1, ChannelSize: 64, BatchSize: 100, BatchFlushMS: 10}, store)
	r.Start()
	c, _ := costOf(0, 0, 0, 0, "", &Price{Input: 1, Output: 1})
	r.Record(Event{Ts: time.Now().Add(-48 * time.Hour), RequestID: "old", Model: "m", Cost: c, CostStatus: CostStatusOK})
	r.Record(Event{Ts: time.Now(), RequestID: "recent", Model: "m", Cost: c, CostStatus: CostStatusOK})
	// wait for the periodic cleanup to run (initial pass runs at Start, before
	// the events were flushed; the 20ms ticker pass runs after)
	time.Sleep(300 * time.Millisecond)
	r.Close()

	check := &SQLiteStore{path: path}
	if err := check.Open(); err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := check.Summary(context.Background(), SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatalf("after retention cleanup: %+v, want exactly the recent event", rows)
	}
}

// abortAwareStore records whether any InsertBatch ever observed an already
// cancelled abort context — i.e. whether Close cancelled the context before
// the worker's last insert had finished.
type abortAwareStore struct {
	mu        sync.Mutex
	cancelled bool
}

func (s *abortAwareStore) Open() error                   { return nil }
func (s *abortAwareStore) Close() error                  { return nil }
func (s *abortAwareStore) Migrate(context.Context) error { return nil }
func (s *abortAwareStore) Summary(context.Context, SummaryQuery) ([]SummaryRow, error) {
	return nil, nil
}
func (s *abortAwareStore) Overview(context.Context, SummaryQuery) (*Overview, error) {
	return nil, nil
}
func (s *abortAwareStore) DeleteBefore(context.Context, int64) (int64, error) { return 0, nil }

func (s *abortAwareStore) InsertBatch(ctx context.Context, events []Event) error {
	if ctx.Err() != nil {
		s.mu.Lock()
		s.cancelled = true
		s.mu.Unlock()
	}
	return nil
}

func (s *abortAwareStore) cancelledBeforeInsert() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

// TestCloseCancelsAbortContext is the acceptance test for the abort-context
// release (fix 3): Close must ALWAYS cancel the abort context — releasing
// the context.WithCancel resources — on the normal path and on the
// never-started path alike, and must do so only AFTER the worker's last
// InsertBatch has finished (never before it, which would abort the final
// write on the normal shutdown path).
func TestCloseCancelsAbortContext(t *testing.T) {
	// Normal path: recorder started, events flushed by Close's drain.
	store := &abortAwareStore{}
	r := NewRecorder(Config{Enabled: true, ChannelSize: 64, BatchSize: 100, BatchFlushMS: 60000}, store)
	r.Start()
	c, _ := costOf(0, 0, 0, 0, "", &Price{Input: 1, Output: 1})
	for i := 0; i < 10; i++ {
		r.Record(Event{RequestID: fmt.Sprintf("r-%d", i), Model: "m", Cost: c, CostStatus: CostStatusOK})
	}
	r.Close()
	if r.abortCtx.Err() == nil {
		t.Fatal("abort context must be cancelled by Close (context.WithCancel resources must be released)")
	}
	if store.cancelledBeforeInsert() {
		t.Fatal("abort context was cancelled before the worker's last InsertBatch finished (would abort the final write)")
	}

	// Never-started path: Close still owns the context and must release it.
	r2 := NewRecorder(Config{Enabled: true, ChannelSize: 4, BatchSize: 10, BatchFlushMS: 60000}, &fakeStore{})
	r2.Record(Event{Model: "m"}) // buffered; nothing will ever drain it
	r2.Close()
	if r2.abortCtx.Err() == nil {
		t.Fatal("abort context must be cancelled by Close on the never-started path too")
	}
	if !r2.stopped.Load() {
		t.Fatal("never-started recorder must be marked stopped by Close")
	}
}

// neverExitsStore blocks in InsertBatch forever, IGNORING context
// cancellation: the worker can never exit even after Close's abort. This is
// the pathological shutdown scenario the Close waiter must not leak a
// goroutine in.
type neverExitsStore struct {
	entered chan struct{}
	once    sync.Once
}

func (s *neverExitsStore) Open() error                   { return nil }
func (s *neverExitsStore) Close() error                  { return nil }
func (s *neverExitsStore) Migrate(context.Context) error { return nil }
func (s *neverExitsStore) Summary(context.Context, SummaryQuery) ([]SummaryRow, error) {
	return nil, nil
}
func (s *neverExitsStore) Overview(context.Context, SummaryQuery) (*Overview, error) {
	return nil, nil
}
func (s *neverExitsStore) DeleteBefore(context.Context, int64) (int64, error) { return 0, nil }

func (s *neverExitsStore) InsertBatch(context.Context, []Event) error {
	s.once.Do(func() { close(s.entered) })
	select {} // block forever, ignoring ctx cancellation
}

// TestCloseNoWaiterGoroutineLeak: when the worker can never exit (store
// ignores context cancellation), Close must return within its bounded budget
// and must NOT leave a long-lived waiter goroutine behind. The old design
// spawned a goroutine blocked on wg.Wait() in this scenario, which leaked
// forever; the new design waits on the worker's own exit channel and creates
// no goroutine at all. (The worker goroutine itself is legitimately still
// blocked inside the store — hence the +1 allowance.)
func TestCloseNoWaiterGoroutineLeak(t *testing.T) {
	old := closeFlushTimeout
	closeFlushTimeout = 30 * time.Millisecond
	defer func() { closeFlushTimeout = old }()

	store := &neverExitsStore{entered: make(chan struct{})}
	r := NewRecorder(Config{Enabled: true, ChannelSize: 64, BatchSize: 4, BatchFlushMS: 60000}, store)
	r.Start()
	c, _ := costOf(0, 0, 0, 0, "", &Price{Input: 1, Output: 1})
	for i := 0; i < 8; i++ {
		r.Record(Event{RequestID: fmt.Sprintf("r-%d", i), Model: "m", Cost: c, CostStatus: CostStatusOK})
	}
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never entered InsertBatch")
	}

	baseline := runtime.NumGoroutine()
	start := time.Now()
	r.Close()
	if elapsed := time.Since(start); elapsed > 2*closeFlushTimeout+time.Second {
		t.Fatalf("Close took %v, want bounded by the close budget", elapsed)
	}
	// Let the count settle: the still-stuck worker keeps exactly +1
	// goroutine. Any additional goroutine (the old wg.Wait waiter) would
	// never go away.
	settle := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+1 && time.Now().Before(settle) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline+1 {
		t.Fatalf("goroutines after Close = %d, baseline %d: a waiter goroutine leaked", n, baseline)
	}
}

// -------------------------------------------------------------------------
// Audit round 6, item 2: rate-limited loss logs + recorder status expvar
// -------------------------------------------------------------------------

// captureSlog redirects slog output into a buffer (Warn level) and returns
// the buffer plus a restore function.
func captureSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return &buf, func() { slog.SetDefault(old) }
}

// resetLossThrottles clears the package-level rate-limiter state so tests
// can assert exact log counts without cross-test interference.
func resetLossThrottles() {
	droppedLogThrottle.mu.Lock()
	droppedLogThrottle.last = time.Time{}
	droppedLogThrottle.mu.Unlock()
	flushLogThrottle.mu.Lock()
	flushLogThrottle.last = time.Time{}
	flushLogThrottle.mu.Unlock()
}

// TestRecordQueueFullLogsRateLimited pins the audit round 6 item 2
// requirement: a full queue drops the event (counter, never blocks) AND
// emits a visible log line — but rate-limited, so 1000 drops under a
// sustained full queue produce exactly one line per lossLogInterval, not a
// log flood. The counters stay event-exact regardless of the log
// throttling.
func TestRecordQueueFullLogsRateLimited(t *testing.T) {
	old := lossLogInterval
	lossLogInterval = time.Hour // long interval: one line for the whole burst
	defer func() { lossLogInterval = old; resetLossThrottles() }()
	resetLossThrottles()

	buf, restore := captureSlog(t)
	defer restore()

	r := NewRecorder(Config{Enabled: true, ChannelSize: 1, BatchSize: 1000, BatchFlushMS: 60000}, &fakeStore{})
	// never started: the single slot stays full after the first event
	droppedBefore := util.MetricsUsageEventsDropped.Value()
	r.Record(Event{Model: "m1"}) // fills the only slot
	for i := 0; i < 100; i++ {
		r.Record(Event{Model: "m2"})
	}
	if got := util.MetricsUsageEventsDropped.Value() - droppedBefore; got != 100 {
		t.Fatalf("dropped counter delta = %d, want 100 (event accounting must be exact regardless of log throttling)", got)
	}
	lines := strings.Count(buf.String(), "usage event queue full, dropping event")
	if lines != 1 {
		t.Errorf("queue-full log lines = %d, want exactly 1 (rate-limited)", lines)
	}

	// With a zero interval every loss is logged: the throttle is the only
	// suppression mechanism, and disabling it must surface every event.
	lossLogInterval = 0
	resetLossThrottles()
	buf.Reset()
	for i := 0; i < 10; i++ {
		r.Record(Event{Model: "m3"})
	}
	if lines := strings.Count(buf.String(), "usage event queue full, dropping event"); lines != 10 {
		t.Errorf("queue-full log lines with zero interval = %d, want 10 (throttle must be the only suppressor)", lines)
	}
	r.Close()
}

// TestRecordAfterCloseLogsRateLimited: events recorded after Close are
// counted dropped AND logged (rate-limited), so shutdown-time loss is never
// silent.
func TestRecordAfterCloseLogsRateLimited(t *testing.T) {
	old := lossLogInterval
	lossLogInterval = 0
	defer func() { lossLogInterval = old; resetLossThrottles() }()
	resetLossThrottles()

	buf, restore := captureSlog(t)
	defer restore()

	r := NewRecorder(Config{Enabled: true, ChannelSize: 8, BatchSize: 100, BatchFlushMS: 60000}, &fakeStore{})
	r.Start()
	r.Close()
	droppedBefore := util.MetricsUsageEventsDropped.Value()
	r.Record(Event{Model: "m"})
	if got := util.MetricsUsageEventsDropped.Value() - droppedBefore; got != 1 {
		t.Errorf("dropped delta after Close = %d, want 1", got)
	}
	if !strings.Contains(buf.String(), "recorder closed, usage event dropped") &&
		!strings.Contains(buf.String(), "recorder not started or already closed, usage event dropped") {
		t.Errorf("post-Close drop must be logged, got: %s", buf.String())
	}
}

// TestRecordNotStartedLogsRateLimited: a recorder that failed to start
// (e.g. no store configured) counts every drop AND logs it — the failure is
// never silent.
func TestRecordNotStartedLogsRateLimited(t *testing.T) {
	old := lossLogInterval
	lossLogInterval = 0
	defer func() { lossLogInterval = old; resetLossThrottles() }()
	resetLossThrottles()

	buf, restore := captureSlog(t)
	defer restore()

	r := NewRecorder(Config{Enabled: true, ChannelSize: 8, BatchSize: 100, BatchFlushMS: 60000}, nil)
	r.Start() // store == nil → recorder disabled, stopped=true
	droppedBefore := util.MetricsUsageEventsDropped.Value()
	r.Record(Event{Model: "m"})
	if got := util.MetricsUsageEventsDropped.Value() - droppedBefore; got != 1 {
		t.Errorf("dropped delta on not-started recorder = %d, want 1", got)
	}
	if !strings.Contains(buf.String(), "recorder not started or already closed") {
		t.Errorf("not-started drop must be logged, got: %s", buf.String())
	}
	r.Close()
}

// TestFlushFailureLogsRateLimited: a failing store logs each failed batch
// at most once per lossLogInterval (no log flood) while the write-error and
// dropped counters stay exact for EVERY batch.
func TestFlushFailureLogsRateLimited(t *testing.T) {
	old := lossLogInterval
	lossLogInterval = time.Hour
	defer func() { lossLogInterval = old; resetLossThrottles() }()
	resetLossThrottles()

	buf, restore := captureSlog(t)
	defer restore()

	store := &fakeStore{errOnInsert: true}
	r := NewRecorder(Config{Enabled: true, ChannelSize: 64, BatchSize: 5, BatchFlushMS: 5}, store)
	r.Start()
	c, _ := costOf(0, 0, 0, 0, "", &Price{Input: 1, Output: 1})
	errorsBefore := util.MetricsUsageWriteErrors.Value()
	droppedBefore := util.MetricsUsageEventsDropped.Value()
	// 20 events < ChannelSize 64: every event reaches the worker, so the
	// failure count is deterministic (4 batches of 5) instead of racing the
	// queue-full path.
	const n = 20
	for i := 0; i < n; i++ {
		r.Record(Event{Model: "m", Cost: c, CostStatus: CostStatusOK})
	}
	// Wait until ALL events were dropped (every batch failed): the counters
	// are the ground truth (20 events = 4 batches of 5), the log is the
	// throttled signal.
	deadline := time.Now().Add(5 * time.Second)
	for util.MetricsUsageEventsDropped.Value() < droppedBefore+n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	errorsDelta := util.MetricsUsageWriteErrors.Value() - errorsBefore
	droppedDelta := util.MetricsUsageEventsDropped.Value() - droppedBefore
	if droppedDelta != n {
		t.Fatalf("dropped delta = %d, want %d (every event of every failed batch counted)", droppedDelta, n)
	}
	if errorsDelta != n/5 {
		t.Errorf("write-error delta = %d, want %d (one incident per failed batch of 5)", errorsDelta, n/5)
	}
	lines := strings.Count(buf.String(), "batch insert failed, dropping batch")
	if lines != 1 {
		t.Errorf("batch-failure log lines = %d, want exactly 1 (rate-limited across %d failures)", lines, errorsDelta)
	}
	r.Close()
}

// TestRecorderStatusExpvar pins the management-observability surface (audit
// round 6, item 2): the usage_recorder_status expvar reflects the recorder
// lifecycle (started / stopped / disabled) so an operator can query whether
// usage persistence is live.
func TestRecorderStatusExpvar(t *testing.T) {
	// Disabled recorder → "disabled".
	r := NewRecorder(Config{Enabled: false}, &fakeStore{})
	r.Start()
	if got := util.MetricsUsageRecorderStatus.Value(); got != "disabled" {
		t.Errorf("disabled recorder status = %q, want disabled", got)
	}
	r.Close()

	// Started + closed → "started" then "stopped".
	r2 := NewRecorder(Config{Enabled: true, ChannelSize: 8, BatchSize: 100, BatchFlushMS: 60000}, &fakeStore{})
	r2.Start()
	if got := util.MetricsUsageRecorderStatus.Value(); got != "started" {
		t.Errorf("started recorder status = %q, want started", got)
	}
	r2.Record(Event{Model: "m"})
	r2.Close()
	if got := util.MetricsUsageRecorderStatus.Value(); got != "stopped" {
		t.Errorf("closed recorder status = %q, want stopped", got)
	}
}

// TestFlushPanicCountsWholeBatchDropped: a panic inside InsertBatch loses
// the whole batch; every event of that batch must be counted dropped (fix 4:
// previously only a single write error was recorded, making the loss
// unobservable at event granularity). written must stay 0 and the panic
// incidents must still be counted as write errors.
func TestFlushPanicCountsWholeBatchDropped(t *testing.T) {
	store := &fakeStore{panicOnInsert: true}
	r := NewRecorder(Config{Enabled: true, ChannelSize: 256, BatchSize: 5, BatchFlushMS: 5}, store)
	r.Start()
	c, _ := costOf(0, 0, 0, 0, "", &Price{Input: 1, Output: 1})
	droppedBefore := util.MetricsUsageEventsDropped.Value()
	writtenBefore := util.MetricsUsageEventsWritten.Value()
	errorsBefore := util.MetricsUsageWriteErrors.Value()
	const n = 100
	for i := 0; i < n; i++ {
		r.Record(Event{Model: "m", Cost: c, CostStatus: CostStatusOK})
	}
	deadline := time.Now().Add(5 * time.Second)
	for util.MetricsUsageEventsDropped.Value() < droppedBefore+n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := util.MetricsUsageEventsDropped.Value() - droppedBefore; got != n {
		t.Fatalf("dropped delta = %d, want %d (every event of every panicked batch counted)", got, n)
	}
	if got := util.MetricsUsageEventsWritten.Value() - writtenBefore; got != 0 {
		t.Fatalf("written delta = %d, want 0 (a panicked batch never persists)", got)
	}
	if util.MetricsUsageWriteErrors.Value() == errorsBefore {
		t.Fatal("panic incidents must be counted as write errors")
	}
	// The worker survived every panic; Close must not hang.
	done := make(chan struct{})
	go func() {
		r.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung after flush panics")
	}
}
