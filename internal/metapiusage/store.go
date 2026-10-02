// Package metapiusage reads ClinePass token consumption and account
// metadata from the metapi hub database. ClinePass traffic is routed
// through metapi (a separate gateway deployment), so prism's own usage
// ledger never records it; the ClinePass quota reversal
// (internal/planusage) therefore takes its consumed-token sum from
// metapi's proxy_logs.
//
// The database is opened STRICTLY read-only (mode=ro) with a busy timeout:
// metapi owns and writes the file, and this package must never take a
// write lock on it. Every failure — the file missing, no read permission,
// the table or database schema absent — is reported as an error and
// degraded by the caller to "no 总额 for this window": it must never
// affect quota fetching, snapshot state or display.
//
// Accounts are read from the metapi accounts table (site_id=49 for
// ClinePass). Only ACTIVE accounts are listed: metapi keeps disabled rows in
// the table (the production site 49 holds one active and one disabled
// account), and a disabled account must neither be polled nor rendered —
// there is no live subscription behind it. Each account's api_token is used
// to poll cline.bot, and its consumption is summed from proxy_logs by
// account_id. A persistent Store holds NO connection: the service Source
// opens a fresh Store per operation, so a replaced database FILE (a restore /
// swap / data-plane rollback) or a database that was unavailable at startup
// is picked up on the next refresh round instead of being served stale from a
// frozen inode.
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

// clinePassAccountWithToken is one ClinePass subscription account from the
// metapi accounts table (site_id=49), including the raw api_token for callers
// that need to authenticate with the upstream. It is deliberately
// unexported: only callers within this module can request it, and they must
// never log, render or persist the token (the display colour is derived from
// planusage.KeyFingerprint of the key — the SHA-256 first 8 bytes hex — never
// from the token itself).
//
// The account's status is NOT carried: the roster is already restricted to
// status = 'active' in SQL (see ListClinePassAccountsWithToken), so a status
// field here would be a value nobody could act on — and a caller-side filter
// would be a second place to get the rule wrong.
type clinePassAccountWithToken struct {
	AccountID int64
	Username  string
	APIToken  string
}

// ListClinePassAccountsWithToken returns every ACTIVE account in the metapi
// accounts table whose site_id is 49 (ClinePass), together with its raw
// api_token. The token is what the upstream poll authenticates with; it must
// never leave the immediate caller (cmd/prism) and must never be logged. A
// missing accounts table or an unreadable database is returned as an error
// for the caller to degrade to "no clinepass accounts".
//
// A disabled account is EXCLUDED in SQL rather than filtered by the caller:
// metapi's own status enum is the single truth ('active' / 'disabled',
// default 'active', never NULL in the production table), and a status the
// caller could not read would silently become a polled account again.
func (s *Store) ListClinePassAccountsWithToken(ctx context.Context) ([]clinePassAccountWithToken, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("metapiusage: store not open")
	}
	const q = `SELECT id, username, api_token FROM accounts WHERE site_id = 49 AND status = 'active'`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []clinePassAccountWithToken
	for rows.Next() {
		var id sql.NullInt64
		var username, apiToken sql.NullString
		if err := rows.Scan(&id, &username, &apiToken); err != nil {
			return nil, err
		}
		if !id.Valid || !username.Valid || !apiToken.Valid {
			continue
		}
		out = append(out, clinePassAccountWithToken{
			AccountID: id.Int64,
			Username:  username.String,
			APIToken:  apiToken.String,
		})
	}
	return out, rows.Err()
}

// SumClinePassTokensByAccount sums 词元 of ClinePass rows in
// [fromUnix, toUnix] (unix seconds, inclusive) for ONE metapi account.
// The per-row total is total_tokens with prompt+completion as the
// fallback for NULL rows, mirroring the usage-db ledger expression
// (internal/usage SumTokensLike). A non-positive lower bound returns 0
// without querying. A missing proxy_logs table is returned as an error
// for the caller to degrade to "no estimate"; it is never fatal.
//
// Before summing, the created_at sample is shape-checked (see
// ErrCreatedAtShape): with a drifted layout the TEXT range comparison
// would silently mis-bound the window, so drift is returned as an error
// and the caller degrades this round to "no estimate" — the same
// contract as a missing table.
func (s *Store) SumClinePassTokensByAccount(ctx context.Context, accountID int64, fromUnix, toUnix int64) (int64, error) {
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
		WHERE account_id = ? AND model_requested LIKE ? AND created_at >= ? AND created_at <= ?`
	var n int64
	if err := s.db.QueryRowContext(ctx, q,
		accountID, clinePassModelLike, formatCreatedAt(fromUnix), formatCreatedAt(toUnix)).Scan(&n); err != nil {
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
