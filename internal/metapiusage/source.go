package metapiusage

import (
	"context"
	"expvar"
	"log/slog"
	"sync"
)

// Source degradation metrics, published on /metrics. sourceErrors counts
// failure INCIDENTS (one per failed sum call); sourceStatus is the current
// lifecycle state ("ok" / "degraded") for management visibility, mirroring
// usage_recorder_status in internal/util.
var (
	sourceErrors = expvar.NewInt("clinepass_usage_source_errors")
	sourceStatus = expvar.NewString("clinepass_usage_source_status")
)

func init() { sourceStatus.Set("ok") }

// Source is the long-lived ClinePass consumption source the service poller
// wires into planusage. Unlike a persistent sql.DB handle it keeps NO
// connection: every SumClinePassTokens call opens a fresh short-lived
// read-only Store (open → shape-check + sum → close), so
//
//   - a metapi database swap / file replacement / data-plane rollback is
//     read on the next refresh round. A persistent handle would keep
//     reading the replaced file's inode and serve frozen, wrong totals
//     indefinitely;
//   - a database that was missing or unreadable when prism started (boot
//     ordering, a permission window) recovers by itself on a later round
//     instead of being disabled for the process lifetime.
//
// The cost is one open (+ ping under a 5 s busy_timeout) per window sum —
// milliseconds, dominated by the sum query itself — against a refresh
// interval of two minutes by default.
//
// Failures degrade this round's estimates only: SumClinePassTokens reports
// them to the caller (planusage leaves the windows without a total, never
// touching Snapshot.Err). The degraded state is logged once per transition
// into and out of it (never once per round) and mirrored in expvar.
type Source struct {
	path string

	mu       sync.Mutex
	degraded bool
}

// NewSource returns a source for path. The database is deliberately not
// touched here: each sum makes its own open attempt, so an unavailable
// database at construction time is not a permanent state.
func NewSource(path string) *Source { return &Source{path: path} }

// SumClinePassTokens opens path read-only, sums the ClinePass rows in
// [fromUnix, toUnix] and closes the connection. Errors are the same as
// Store.SumClinePassTokens (missing file, permission denied, absent
// proxy_logs table, created_at format drift); each failure is counted and
// the healthy→degraded transition logs a WARN, while a success after a
// failure logs the recovery. Callers degrade to "no estimate" and must
// never fail the quota fetch.
func (s *Source) SumClinePassTokens(ctx context.Context, fromUnix, toUnix int64) (int64, error) {
	n, err := s.sumOnce(ctx, fromUnix, toUnix)
	s.observe(err)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Source) sumOnce(ctx context.Context, fromUnix, toUnix int64) (int64, error) {
	st, err := Open(s.path)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	return st.SumClinePassTokens(ctx, fromUnix, toUnix)
}

// observe moves the degradation state machine: the first failure after a
// healthy state logs a WARN and flips the status, further failures only
// count (no per-round spam), and the first success after a failure logs
// the recovery and flips the status back.
func (s *Source) observe(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		if s.degraded {
			s.degraded = false
			sourceStatus.Set("ok")
			slog.Info("clinepass usage source recovered", "path", s.path)
		}
		return
	}
	sourceErrors.Add(1)
	if !s.degraded {
		s.degraded = true
		sourceStatus.Set("degraded")
		slog.Warn("clinepass usage source unavailable, total estimate disabled", "path", s.path, "error", err)
	}
}
