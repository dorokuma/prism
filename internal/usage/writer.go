package usage

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dorokuma/prism/internal/util"
)

// Event is one usage record produced by a proxied request. KeyID carries the
// API key name — never the key material.
//
// Cost and CostStatus are computed ONCE, on the synchronous request
// finalization path (middleware.EmitAudit → the wiring-stage pricer), and
// carried on the event; the writer persists them as-is and never re-prices,
// so the audit log amount and the stored amount are exactly the same value.
// A nil Cost means the model had no known price (cost stored as NULL, status
// missing_price).
//
// Source records which upstream wire format the token counts came from
// (SourceOpenAI or SourceAnthropic). It selects the cost formula: OpenAI
// prompt_tokens includes cached tokens (so the cached portion is repriced at
// CacheRead), while Anthropic input_tokens excludes the cache counters
// entirely. It is persisted so the pricing basis of any row can be audited.
type Event struct {
	Ts               time.Time
	RequestID        string
	Path             string
	Model            string
	Provider         string
	Account          string
	KeyID            string
	Stream           bool
	Success          bool
	Status           int
	ErrorType        string
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CachedTokens     int64
	ReasoningTokens  int64
	CacheWriteTokens int64
	DurationMS       float64
	Source           string
	Cost             *float64
	CostStatus       string
}

// Config configures the Recorder. It deliberately does not depend on
// internal/config; the wiring stage maps the application configuration onto
// this struct.
type Config struct {
	Enabled       bool
	DBPath        string
	RetentionDays int
	ChannelSize   int
	BatchSize     int
	BatchFlushMS  int
}

const (
	defaultChannelSize = 1024
	defaultBatchSize   = 50
	defaultBatchFlush  = 200 * time.Millisecond
)

// closeFlushTimeout bounds the total time Close spends waiting for the
// background worker to drain the queue and flush. It is a var so tests can
// shorten it; production uses 5 seconds.
var closeFlushTimeout = 5 * time.Second

// cleanupInterval is the retention cleanup cadence. It is a var so tests can
// shorten it; production uses one hour.
var cleanupInterval = 1 * time.Hour

// Recorder accepts usage events on a bounded channel and persists them in
// batches from a single background worker. Record never blocks: when the
// channel is full the event is dropped and counted. A nil *Recorder or a
// disabled one is a safe no-op.
//
// Record/Close synchronization protocol (why Record can never panic and
// Close can never lose an accepted event silently):
//
//   - r.ch is NEVER closed. Sending on a closed channel is the one panic
//     this design eliminates outright.
//   - r.mu guards the boundary between Record and Close. Record holds it
//     while checking r.closed and doing the (non-blocking) send; Close
//     holds it while setting r.closed. The critical section is a bool
//     check plus a non-blocking channel send, so Record's worst-case wait
//     is the few nanoseconds Close holds the lock — it can never block on
//     the request finalization path.
//   - Close sets r.closed under mu and only then closes r.done. By the
//     mutex happens-before chain, every send that completed before that
//     point is visible to the drain loop the worker runs after receiving
//     from r.done, and every later Record observes r.closed and counts
//     itself dropped instead of sending. The worker therefore drains
//     exactly the accepted events, and no event is lost silently.
//
// Event accounting (the three expvar counters, see util/metrics.go):
//
//   - usage_events_written: +1 per event persisted (incremented only after
//     the batch transaction commits).
//   - usage_events_dropped: +1 per event that will never be persisted,
//     counted at the point the loss becomes known: the Record path (queue
//     full / recorder already closed), a flush panic (whole batch), a
//     failed InsertBatch (whole batch — the transaction rolls back, so
//     nothing of it persists), a per-event insert failure inside an
//     otherwise successful batch, and the Close path when the worker never
//     started. Identity: written + dropped == total Record calls, exactly,
//     with no event counted twice. The one exception is documented in
//     Close: when the worker is stuck forever and Close times out, the
//     still-buffered events are NOT counted (a snapshot could double-count
//     if the worker later drains them); that pathological loss is signaled
//     by the close-timeout error log.
//   - usage_write_errors: +1 per failure INCIDENT (open/migrate, cleanup,
//     flush panic, failed batch, per-event insert failure). It is NOT an
//     event counter: one failed batch of N events is one incident (the N
//     events are accounted by dropped). An incident that loses events
//     therefore shows up in BOTH dropped (event accounting) and
//     write_errors (incident accounting) — orthogonal axes, not a double
//     count of events.
type Recorder struct {
	store Store
	cfg   Config

	ch      chan Event
	stopped atomic.Bool

	// started is set once by Start right before the worker goroutine is
	// launched. Close uses it to distinguish "worker never launched" (the
	// channel will never be drained; buffered events can be counted as
	// dropped exactly) from "worker launched but possibly stuck" (counting
	// buffered events would be a racing snapshot).
	started atomic.Bool

	mu     sync.Mutex
	closed bool

	// done is closed once by Close to tell the worker and the cleanup loop
	// to exit. The worker drains everything still buffered before it stops.
	done chan struct{}

	// workerDone is closed exactly once, by the worker's own exit path,
	// strictly after the store has been closed (the worker owns the store
	// lifecycle). Close waits on it instead of spawning a wg.Wait waiter
	// goroutine, so a worker that never exits cannot leak a second
	// goroutine (and Close cannot block on it past its deadline).
	workerDone chan struct{}

	// abortCtx is cancelled EARLY only when Close times out, to abort an
	// in-flight store operation (InsertBatch/DeleteBefore) so the worker can
	// exit; on the normal path Close cancels it once, at the very end, after
	// the worker has exited (resource release). It is deliberately NOT
	// cancelled at the start of Close: an insert that is already running
	// must be allowed to finish so buffered events are not lost on the
	// normal shutdown path.
	abortCtx    context.Context
	abortCancel context.CancelFunc

	closeOnce sync.Once
}

// NewRecorder builds a Recorder. It does not open the store; call Start.
// Zero or negative config values fall back to the defaults (channel 1024,
// batch 50, flush 200ms).
func NewRecorder(cfg Config, store Store) *Recorder {
	if cfg.ChannelSize <= 0 {
		cfg.ChannelSize = defaultChannelSize
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.BatchFlushMS <= 0 {
		cfg.BatchFlushMS = int(defaultBatchFlush / time.Millisecond)
	}
	abortCtx, abortCancel := context.WithCancel(context.Background())
	return &Recorder{
		store:       store,
		cfg:         cfg,
		ch:          make(chan Event, cfg.ChannelSize),
		done:        make(chan struct{}),
		workerDone:  make(chan struct{}),
		abortCtx:    abortCtx,
		abortCancel: abortCancel,
	}
}

// Start opens the store, applies migrations and launches the background
// writer and the retention cleanup loop. It never returns an error: any
// failure (open, migrate) is logged and counted and the recorder degrades to
// a no-op so request proxying is never affected.
func (r *Recorder) Start() {
	if r == nil || !r.cfg.Enabled {
		return
	}
	if r.store == nil {
		slog.Error("usage: recorder disabled, no store configured")
		util.RecordUsageWriteErrors()
		r.stopped.Store(true)
		return
	}
	if err := r.store.Open(); err != nil {
		slog.Error("usage: open store failed, recorder disabled", "error", err, "path", r.cfg.DBPath)
		util.RecordUsageWriteErrors()
		r.stopped.Store(true)
		return
	}
	if err := r.store.Migrate(r.abortCtx); err != nil {
		slog.Error("usage: migrate failed, recorder disabled", "error", err, "path", r.cfg.DBPath)
		util.RecordUsageWriteErrors()
		r.stopped.Store(true)
		_ = r.store.Close()
		return
	}
	r.started.Store(true)
	go r.run()
	go r.cleanupLoop()
}

// Record queues one event for asynchronous persistence. It never blocks: if
// the queue is full the event is dropped and counted, making it safe to call
// on HTTP request finalization paths. A nil receiver or a disabled recorder
// is a no-op. An event recorded after Close is counted as dropped, so
// shutdown-time loss is always observable.
func (r *Recorder) Record(e Event) {
	if r == nil || !r.cfg.Enabled {
		return
	}
	if r.stopped.Load() {
		// Failed to start or already closed: nothing will ever persist this
		// event. Count it so the loss is observable rather than silent.
		util.RecordUsageEventsDropped()
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		util.RecordUsageEventsDropped()
		return
	}
	select {
	case r.ch <- e:
	default:
		util.RecordUsageEventsDropped()
	}
	r.mu.Unlock()
}

// run is the single background writer: it batches events and flushes on
// batch size, on the flush ticker, and when Close signals done (shutdown
// drain). A panic anywhere in the loop must not kill the goroutine. On exit
// it closes the store: the worker is the only goroutine that ever calls
// store.Close, and it does so strictly after its last InsertBatch returned,
// so Close can never race a live write with a store close (use-after-close),
// even on its own timeout path.
func (r *Recorder) run() {
	// Exit order (defers run LIFO): the recover runs first, then the store
	// is closed (the worker is the only goroutine that ever closes it), and
	// only then is workerDone closed, so a Close that observes workerDone
	// knows the store is already closed and no write is in flight.
	defer close(r.workerDone)
	defer func() {
		if r.store != nil {
			if err := r.store.Close(); err != nil {
				slog.Error("usage: store close failed", "error", err)
			}
		}
	}()
	defer func() {
		if p := recover(); p != nil {
			slog.Error("usage: writer panic", "panic", p)
			util.RecordUsageWriteErrors()
		}
	}()

	ticker := time.NewTicker(time.Duration(r.cfg.BatchFlushMS) * time.Millisecond)
	defer ticker.Stop()
	batch := make([]Event, 0, r.cfg.BatchSize)

	// flush persists the current batch. The recover is per-flush (not just at
	// run level): a panic while persisting one batch drops that batch and
	// counts it, but the writer goroutine must survive and keep processing.
	// The batch is handed to InsertBatch with the recorder's abort context:
	// on the normal path it stays live until the worker has exited (the
	// final inserts finish; events are not lost), while a stuck insert is
	// aborted early on Close timeout so the worker can exit.
	flush := func() {
		if len(batch) == 0 {
			return
		}
		events := batch
		batch = batch[:0]
		func() {
			defer func() {
				if p := recover(); p != nil {
					slog.Error("usage: writer flush panic, dropping batch", "panic", p, "events", len(events))
					util.RecordUsageWriteErrors()
					countDropped(len(events))
				}
			}()
			if err := r.store.InsertBatch(r.abortCtx, events); err != nil {
				slog.Error("usage: batch insert failed, dropping batch", "error", err, "events", len(events))
				util.RecordUsageWriteErrors()
				countDropped(len(events))
			}
		}()
	}

	for {
		select {
		case e := <-r.ch:
			batch = append(batch, e)
			if len(batch) >= r.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-r.done:
			// Drain everything still buffered, then flush and exit. No new
			// event can arrive after done is closed: every accepted send is
			// ordered before Close's closed flag (mutex happens-before), and
			// close(done) happens after that, so a receive here observes
			// every accepted event (channel semantics: a send that
			// happens-before a receive is seen by it).
			for {
				select {
				case e := <-r.ch:
					batch = append(batch, e)
					if len(batch) >= r.cfg.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// cleanupLoop periodically deletes events older than RetentionDays. It stops
// when Close closes done. RetentionDays <= 0 disables deletion entirely.
func (r *Recorder) cleanupLoop() {
	if r.cfg.RetentionDays <= 0 {
		return
	}
	cutoff := func() int64 {
		return time.Now().Add(-time.Duration(r.cfg.RetentionDays) * 24 * time.Hour).Unix()
	}
	r.deleteBefore(r.abortCtx, cutoff())
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			// During shutdown the worker may already have closed the store;
			// skip the pass when done was closed while the ticker fired.
			select {
			case <-r.done:
				return
			default:
			}
			r.deleteBefore(r.abortCtx, cutoff())
		}
	}
}

func (r *Recorder) deleteBefore(ctx context.Context, ts int64) {
	if r.store == nil || ctx.Err() != nil {
		// ctx.Err() != nil means shutdown is in progress; the caller no longer
		// cares about this pass.
		return
	}
	n, err := r.store.DeleteBefore(ctx, ts)
	if err != nil {
		slog.Warn("usage: retention cleanup failed", "error", err)
		util.RecordUsageWriteErrors()
		return
	}
	if n > 0 {
		slog.Info("usage: retention cleanup deleted events", "count", n, "before_unix", ts)
	}
}

// Close stops accepting events, tells the worker to drain the queue and
// flush, and waits up to closeFlushTimeout for it. On timeout the in-flight
// store operation is aborted (abort context) and Close waits another
// closeFlushTimeout for the worker to really exit (the review requirement:
// abort, then wait for exit, never close the store under a live write); if
// the worker still has not exited, Close returns WITHOUT closing the store —
// the worker owns the store lifecycle and closes it on exit, so a store
// close can never race a live write. Worst-case Close duration is
// 2×closeFlushTimeout, and only when the store itself is stuck; the normal
// path returns in milliseconds. Safe to call multiple times and on a
// never-started recorder.
//
// Buffered-event accounting: after the worker exits its drain loop has
// emptied the channel (every accepted event is drained before exit), so on
// the normal path nothing is counted here. When the worker never started,
// nothing will ever drain the channel, so the buffered events are counted as
// dropped exactly. When the worker is stuck past both timeouts, the buffer
// is NOT counted: a len(r.ch) snapshot would have no happens-before edge to
// the worker and could double-count events the worker drains and persists
// later; that loss is signaled by the close-timeout error log (and by the
// writer's own dropped/write-error accounting for any batch that does
// eventually fail).
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		// Release the abort context resources unconditionally, but only at
		// the very END of Close: cancelling it earlier could abort the
		// worker's last InsertBatch on the normal path and lose buffered
		// events. CancelFunc is idempotent, so the timeout path that calls
		// abortCancel() earlier is unaffected.
		defer r.abortCancel()
		r.stopped.Store(true)
		if !r.cfg.Enabled || r.store == nil {
			return
		}
		// Stop accepting events. The closed flag is set under mu so that a
		// concurrent Record either enqueues before this point (and is then
		// guaranteed visible to the worker's drain) or observes closed and
		// counts itself dropped. Only then is done closed.
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		close(r.done)

		if !r.started.Load() {
			// Start never launched the worker (never called, or failed before
			// launch): nothing can ever drain the channel. No Record can send
			// anymore (r.closed), so len(r.ch) is a stable, exact count — no
			// worker exists to double-count it.
			if n := len(r.ch); n > 0 {
				slog.Warn("usage: dropping unflushed events at close", "dropped", n)
				countDropped(n)
			}
			return
		}

		deadline := time.Now().Add(closeFlushTimeout)
		if !waitBefore(r.workerDone, deadline) {
			// The worker is stuck in a store operation (slow disk, busy
			// lock). Abort the in-flight operation so it can exit. This is
			// the only early abortCancel use: a normal shutdown lets the
			// in-flight insert finish so buffered events are not lost.
			r.abortCancel()
			if !waitBefore(r.workerDone, time.Now().Add(closeFlushTimeout)) {
				// The worker still owns the store: closing it here could hit
				// a live write (use-after-close). Leave the store to the
				// worker's exit path and report the loss instead.
				slog.Error("usage: close timed out, worker still flushing; store close deferred to worker")
			}
		}
		// Worker exited: its drain loop emptied the channel, so there is
		// nothing left to count here (see the accounting comment above).
	})
}

// countDropped records n events as dropped. It is used when a whole batch is
// lost (flush panic or failed InsertBatch). Counting the full batch is exact:
// InsertBatch returns an error only when its transaction rolled back, so not
// a single event of the batch was persisted, and the written counter is only
// incremented after a successful commit — dropped and written can never
// double-count.
func countDropped(n int) {
	for i := 0; i < n; i++ {
		util.RecordUsageEventsDropped()
	}
}

// waitBefore reports whether ch is closed before the deadline. It never
// blocks past the deadline.
func waitBefore(ch <-chan struct{}, deadline time.Time) bool {
	d := time.Until(deadline)
	if d <= 0 {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}
