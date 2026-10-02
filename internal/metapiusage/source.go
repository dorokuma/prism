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
// connection: every call opens a fresh short-lived read-only Store (open
// → shape-check + sum/account-list → close), so
//
//   - a metapi database swap / file replacement / data-plane rollback is
//     read on the next refresh round. A persistent handle would keep
//     reading the replaced file's inode and serve frozen, wrong totals
//     indefinitely;
//   - a database that was missing or unreadable when prism started (boot
//     ordering, a permission window) recovers by itself on a later round
//     instead of being disabled for the process lifetime.
//
// The cost is one open (+ ping under a 5 s busy_timeout) per operation
// — milliseconds, dominated by the query itself — against a refresh
// interval of two minutes by default.
//
// Failures degrade this round's estimates only: errors are reported to
// the caller (planusage leaves the windows without a total, never
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
	n, err := s.sumOnce(ctx, func(st *Store) (int64, error) {
		return st.SumClinePassTokens(ctx, fromUnix, toUnix)
	})
	s.observe(err)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// SumClinePassTokensByAccount opens path read-only, sums the ClinePass
// rows for one account_id in [fromUnix, toUnix] and closes the
// connection. Errors and degradation are the same as SumClinePassTokens.
func (s *Source) SumClinePassTokensByAccount(ctx context.Context, accountID int64, fromUnix, toUnix int64) (int64, error) {
	n, err := s.sumOnce(ctx, func(st *Store) (int64, error) {
		return st.SumClinePassTokensByAccount(ctx, accountID, fromUnix, toUnix)
	})
	s.observe(err)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ListClinePassAccounts opens path read-only, lists the ACTIVE ClinePass
// accounts (site_id=49) and closes the connection. The returned api_token is
// what cmd/prism authenticates against cline.bot with: it goes only to the
// immediate caller, is never logged and is never rendered (the display colour
// comes from planusage.KeyFingerprint of the key). Errors and degradation are
// the same as SumClinePassTokens.
//
// This is the ONE account-discovery implementation: the service roster
// (cmd/prism refreshQuotaAccounts, at startup and on SIGHUP) and the CLI
// (prism quota) both call it, so the two paths cannot drift on which accounts
// exist. A disabled account is filtered out in SQL (see Store).
func (s *Source) ListClinePassAccounts(ctx context.Context) ([]clinePassAccountWithToken, error) {
	var out []clinePassAccountWithToken
	_, err := s.sumOnce(ctx, func(st *Store) (int64, error) {
		var err error
		out, err = st.ListClinePassAccountsWithToken(ctx)
		return 0, err
	})
	s.observe(err)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Source) sumOnce(ctx context.Context, fn func(*Store) (int64, error)) (int64, error) {
	st, err := Open(s.path)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	return fn(st)
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
