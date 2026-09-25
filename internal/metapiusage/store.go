// Package metapiusage reads ClinePass token consumption from the metapi
// hub database. ClinePass traffic is routed through metapi (a separate
// gateway deployment), so prism's own usage ledger never records it; the
// ClinePass quota reversal (internal/planusage) therefore takes its
// consumed-token sum from metapi's proxy_logs.
//
// The database is opened STRICTLY read-only (mode=ro) with a busy timeout:
// metapi owns and writes the file, and this package must never take a
// write lock on it. Every failure — the file missing, no read permission,
// the table or database schema absent — is reported as an error and
// degraded by the caller to "no 总额 for this window": it must never
// affect quota fetching, snapshot state or display.
//
// The sum is subscription-wide: it totals every cline-pass/% row in the
// window. With a single ClinePass subscription that is exactly the
// account's traffic; a second subscription (another prism account with
// its own metapi upstream) would mix both and needs a per-account scope
// review.
//
// A Store is one read-only connection: open, sum, close. The service
// poller uses Source, which opens a fresh Store per sum, so a replaced
// database FILE (a restore / swap / data-plane rollback) or a database
// that was unavailable at startup is picked up on the next refresh round
// instead of being served stale from a frozen inode.
package metapiusage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultDBPath is the metapi production hub database on this host. It is
// a fixed default, not a config key — the same pattern as the agy usage
// index (agyusage.DefaultIndexPath): deployments without metapi simply
// have no such file and lose only the ClinePass 总额 estimate.
const DefaultDBPath = "/var/lib/metapi/data/hub.db"

// clinePassModelLike selects the ClinePass subscription traffic in
// proxy_logs: metapi records those requests as cline-pass/<model>.
const clinePassModelLike = "cline-pass/%"

// metapiCreatedAtLayout is the format metapi writes into
// proxy_logs.created_at (SQLite datetime('now')): UTC, second precision.
// The column is TEXT and string comparisons are range-compatible with
// this layout.
const metapiCreatedAtLayout = "2006-01-02 15:04:05"

// Store is a read-only handle on the metapi hub database.
type Store struct {
	path string
	db   *sql.DB
}

// Open opens path read-only. A missing file or a failing ping is an
// error; callers skip the estimate instead of failing quota fetches.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("metapiusage: empty db path")
	}
	db, err := sql.Open("sqlite", roDSN(path))
	if err != nil {
		return nil, fmt.Errorf("metapiusage: open: %w", err)
	}
	// One connection per handle is enough for a single sum (the service
	// Source opens a handle per sum and closes it) and keeps the number of
	// readers registered against metapi's WAL low.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("metapiusage: ping: %w", err)
	}
	return &Store{path: path, db: db}, nil
}

// Close releases the handle. Idempotent.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// SumClinePassTokens sums 词元 of ClinePass rows in [fromUnix, toUnix]
// (unix seconds, inclusive). The per-row total is total_tokens with
// prompt+completion as the fallback for NULL rows, mirroring the
// usage-db ledger expression (internal/usage SumTokensLike). A
// non-positive lower bound returns 0 without querying. A missing
// proxy_logs table is returned as an error for the caller to degrade to
// "no estimate"; it is never fatal.
//
// Before summing, the created_at sample is shape-checked (see
// ErrCreatedAtShape): with a drifted layout the TEXT range comparison
// would silently mis-bound the window, so drift is returned as an error
// and the caller degrades this round to "no estimate" — the same contract
// as a missing table.
func (s *Store) SumClinePassTokens(ctx context.Context, fromUnix, toUnix int64) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("metapiusage: store not open")
	}
	if fromUnix <= 0 {
		return 0, nil
	}
	if err := s.checkCreatedAtShape(ctx); err != nil {
		return 0, err
	}
	const q = `SELECT COALESCE(SUM(CASE WHEN total_tokens IS NULL
			THEN COALESCE(prompt_tokens, 0) + COALESCE(completion_tokens, 0)
			ELSE total_tokens END), 0)
		FROM proxy_logs
		WHERE model_requested LIKE ? AND created_at >= ? AND created_at <= ?`
	var n int64
	if err := s.db.QueryRowContext(ctx, q,
		clinePassModelLike, formatCreatedAt(fromUnix), formatCreatedAt(toUnix)).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ErrCreatedAtShape marks a proxy_logs.created_at sample that no longer
// looks like the UTC datetime('now') text the sum windows compare
// against. The window bounds are TEXT range comparisons, so a drifted
// layout (RFC3339, epoch seconds, a changed precision) silently mis-bounds
// every sum; a caller that sees this error must degrade to "no estimate"
// rather than risk a wrong number.
var ErrCreatedAtShape = errors.New("metapiusage: created_at format drift")

// checkCreatedAtShape samples MAX(created_at) and verifies the metapi wire
// shape: the UTC `2006-01-02 15:04:05` text has a space at index 10 (its
// 11th character). A non-empty sample that is shorter than 11 characters
// cannot be that layout either (e.g. epoch seconds written as text), and
// comparing it as text would silently exclude every row, so it is drift
// too. An empty table (NULL sample) is skipped: there is nothing to sum
// and nothing to certify.
//
// The probe is `MAX` on an indexed created_at column (metapi's schema
// carries proxy_logs_created_at_idx), so it rides the index instead of
// scanning the log table. It runs on every sum — the service Source opens
// a fresh connection each refresh round — so a format change introduced
// while prism is running degrades the next round instead of silently
// under-counting.
func (s *Store) checkCreatedAtShape(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("metapiusage: store not open")
	}
	var sample sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(created_at) FROM proxy_logs`).Scan(&sample); err != nil {
		return err
	}
	if !sample.Valid || sample.String == "" {
		return nil
	}
	if v := sample.String; len(v) < 11 || v[10] != ' ' {
		return fmt.Errorf("%w: sampled created_at %q", ErrCreatedAtShape, v)
	}
	return nil
}

// formatCreatedAt renders a unix bound as the UTC text metapi compares
// against. metapi writes proxy_logs.created_at with datetime('now'), which
// is UTC; the sum windows must use the same clock.
func formatCreatedAt(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(metapiCreatedAtLayout)
}

// roDSN is the strictly read-only DSN, mirroring internal/usage's roDSN:
// mode=ro forbids writes at the SQLite level, busy_timeout absorbs the
// rare recovery/checkpoint moment while metapi is writing. journal_mode is
// deliberately not set: the pragma persists in the file header, and on a
// rollback-journal file a WAL pragma would fail on a read-only connection.
func roDSN(path string) string {
	return "file:" + url.PathEscape(path) +
		"?mode=ro&_pragma=busy_timeout(5000)"
}
