// Package usage records per-request token usage and cost into a SQLite
// database and serves aggregated usage summaries. The storage layer is
// defined by the Store interface and backed by the pure-Go modernc.org/sqlite
// driver; nothing in this package requires CGO.
package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/dorokuma/prism/internal/util"
	_ "modernc.org/sqlite"
)

// Store is the persistence layer for usage events. The concrete SQLite
// implementation can be swapped for another driver-backed implementation; the
// rest of the package only depends on database/sql types.
type Store interface {
	// Open establishes connections to the underlying database. Idempotent.
	Open() error
	// Close releases all database connections. Idempotent.
	Close() error
	// Migrate applies pending schema migrations. Idempotent; safe to run on
	// every startup.
	Migrate(ctx context.Context) error
	// InsertBatch persists a batch of events in a single transaction. Cost is
	// NOT computed here: it was computed once on the synchronous request path
	// and carried on the event (single pricing point); the writer persists it
	// as-is. A failing single event is dropped without aborting the batch.
	InsertBatch(ctx context.Context, events []Event) error
	// Summary runs an aggregated usage query over the read connection.
	Summary(ctx context.Context, q SummaryQuery) ([]SummaryRow, error)
	// Overview returns a single global aggregation over the whole filtered
	// range (no group_by, no limit) on the read connection. Filters are
	// shared with Summary; GroupBy and Limit in q are ignored.
	Overview(ctx context.Context, q SummaryQuery) (*Overview, error)
	// DeleteBefore removes events older than the given unix timestamp and
	// returns the number of deleted rows.
	DeleteBefore(ctx context.Context, tsUnix int64) (int64, error)
}

// Migration is a single schema migration step.
type Migration struct {
	Version int
	SQL     string
}

// migrations is the ordered list of schema migrations. Only append new
// entries; never edit or reorder existing ones, since applied versions are
// recorded in schema_migrations and re-running a modified old migration would
// corrupt the schema.
var migrations = []Migration{
	{
		Version: 1,
		// request_id intentionally has NO UNIQUE constraint: X-Request-ID is
		// client-controlled, and a unique constraint would let a client reuse
		// one ID to silently drop its own usage rows via INSERT OR IGNORE
		// style deduplication, i.e. bill evasion. It is a plain queryable
		// column only.
		SQL: `CREATE TABLE IF NOT EXISTS usage_events (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    ts_unix            INTEGER NOT NULL,
    ts                 TEXT,
    request_id         TEXT,
    path               TEXT,
    model              TEXT,
    provider           TEXT,
    account            TEXT,
    key_id             TEXT,
    stream             INTEGER,
    success            INTEGER,
    status             INTEGER,
    error_type         TEXT,
    prompt_tokens      INTEGER NOT NULL DEFAULT 0,
    completion_tokens  INTEGER NOT NULL DEFAULT 0,
    total_tokens       INTEGER NOT NULL DEFAULT 0,
    cached_tokens      INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd           REAL,
    cost_status        TEXT,
    duration_ms        REAL
);
CREATE INDEX IF NOT EXISTS idx_usage_events_ts_unix ON usage_events(ts_unix);
CREATE INDEX IF NOT EXISTS idx_usage_events_key_id_ts ON usage_events(key_id, ts_unix);
CREATE INDEX IF NOT EXISTS idx_usage_events_model_ts ON usage_events(model, ts_unix);
CREATE INDEX IF NOT EXISTS idx_usage_events_provider_ts ON usage_events(provider, ts_unix);
CREATE INDEX IF NOT EXISTS idx_usage_events_account_ts ON usage_events(account, ts_unix);
CREATE INDEX IF NOT EXISTS idx_usage_events_success_ts ON usage_events(success, ts_unix);
`,
	},
	{
		Version: 2,
		// usage_source records the upstream wire format the token counts were
		// parsed from ("openai" / "anthropic"): it selects the cost formula
		// (OpenAI prompt_tokens includes cached tokens, Anthropic input_tokens
		// excludes the cache counters) and lets any row's pricing basis be
		// audited. NULL = legacy rows / unknown basis (priced with the OpenAI
		// formula).
		SQL: `ALTER TABLE usage_events ADD COLUMN usage_source TEXT;`,
	},
}

// Pool sizing: the write pool is capped at one connection so writers
// serialize and SQLITE_BUSY between writers is structurally impossible; the
// read pool keeps concurrent Summary queries flowing. Do not "fix" busy
// errors by dropping the read pool to one connection as well — that would
// serialize reads behind the writer for no reason.
const (
	writeMaxOpenConns = 1
	readMaxOpenConns  = 4
	busyTimeoutMS     = 5000
)

// SQLiteStore is a Store backed by a SQLite database file via the pure-Go
// modernc.org/sqlite driver (no CGO). Two *sql.DB pools point at the same
// file: a single-connection write pool and a read pool. Both run in WAL mode
// with NORMAL synchronous and a 5s busy timeout. A read-only store (see
// NewReadOnlyStore) keeps only the read pool and opens the file strictly
// read-only.
type SQLiteStore struct {
	path     string
	readOnly bool

	mu      sync.Mutex
	writeDB *sql.DB
	readDB  *sql.DB
}

// NewSQLiteStore creates a SQLite-backed Store for the given database file
// path. The store is not opened until Open (or Recorder.Start) is called;
// an Open failure leaves the store unusable (Summary returns an error) but
// never affects request proxying.
func NewSQLiteStore(path string) *SQLiteStore {
	return &SQLiteStore{path: path}
}

// NewReadOnlyStore creates a store that opens the database strictly
// read-only (mode=ro in the DSN): it is meant for offline querying, e.g. the
// prism usage CLI reading the database while the prism service is running
// and writing to it. WAL mode makes this safe: the file header persists the
// WAL setting, so a read-only connection reads through the -wal file without
// ever taking a write lock, and the busy_timeout pragma absorbs the rare
// recovery/checkpoint edge. Write operations (Migrate, InsertBatch,
// DeleteBefore) fail with "store not open" because there is no write pool.
func NewReadOnlyStore(path string) *SQLiteStore {
	return &SQLiteStore{path: path, readOnly: true}
}

// dsn returns a file DSN that applies the required pragmas on every new
// connection: WAL journaling, NORMAL synchronous, and a 5s busy timeout. The
// modernc driver parses the query string itself (when the path has no file:
// prefix) or sqlite3 URI parsing applies; the file: prefix with an escaped
// path is the documented form and handles paths with spaces or special
// characters.
func dsn(path string) string {
	return "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
}

// roDSN is the read-only DSN. journal_mode(WAL) is deliberately NOT set
// here: it is persistent in the database header (the service always opens
// the file in WAL mode), and on a read-only connection the pragma is only a
// no-op when the file is already WAL — on a rollback-journal file it would
// fail with "attempt to write a readonly database". busy_timeout is what
// makes concurrent reads safe (SQLITE_BUSY only occurs in rare
// recovery/checkpoint moments and is retried for 5s instead of erroring).
func roDSN(path string) string {
	return "file:" + url.PathEscape(path) +
		"?mode=ro&_pragma=busy_timeout(5000)"
}

// tightenWritableFileModes chmods the usage database and its WAL/SHM
// sidecar files to 0600 (owner-only). SQLite creates files with the
// process umask (typically 0644), which would leave per-request token
// usage — model, key_id, cost — world-readable. It is called after a
// successful writable Open and again after Migrate (which is when the
// schema_migrations write actually creates the -wal/-shm sidecars on
// first run). Missing files are skipped (ENOENT is normal before the
// first write); every other error is returned — a store that cannot be
// secured must fail loudly, not serve with loose permissions. Read-only
// stores never call this: they must not touch a file owned by the live
// service.
func tightenWritableFileModes(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0600); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("usage: chmod %s to 0600: %w", p, err)
		}
	}
	return nil
}

// tightenFileModes is the file-mode tightening step used after successful
// write transactions (see tightenWritableFileModes). It is a variable so
// tests can inject a failing tightening and pin the commit-vs-tighten
// semantics; the production value is unchanged.
var tightenFileModes = tightenWritableFileModes

// reportTightenFailureAfterCommit is the post-commit failure path shared by
// InsertBatch and DeleteBefore: the data change is ALREADY durable, so the
// failure must not be returned as a failed write (the writer would count
// the batch dropped / the retention loop would retry a committed delete,
// contradicting what is actually in the database). It fails loud instead:
// an error-level log plus the write-error incident counter keep the
// security problem observable without lying about the data.
func reportTightenFailureAfterCommit(err error, path, op string) {
	slog.Error("usage: "+op+" committed but file modes could not be secured", "error", err, "path", path)
	util.RecordUsageWriteErrors()
}

// Open creates the connection pools. Both are pinged so a bad path or
// pragma fails fast here instead of on the first query. A read-only store
// opens only the read pool (mode=ro); the write pool stays nil so
// Migrate/InsertBatch/DeleteBefore fail with "store not open" instead of
// ever touching the file. A successful writable open tightens the file
// modes to 0600 (see tightenWritableFileModes).
func (s *SQLiteStore) Open() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readDB != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.readOnly {
		rdb, err := sql.Open("sqlite", roDSN(s.path))
		if err != nil {
			return fmt.Errorf("usage: open read-only pool: %w", err)
		}
		rdb.SetMaxOpenConns(readMaxOpenConns)
		rdb.SetMaxIdleConns(readMaxOpenConns)
		if err := rdb.PingContext(ctx); err != nil {
			rdb.Close()
			return fmt.Errorf("usage: ping read-only pool: %w", err)
		}
		s.readDB = rdb
		return nil
	}
	writeDB, err := sql.Open("sqlite", dsn(s.path))
	if err != nil {
		return fmt.Errorf("usage: open write pool: %w", err)
	}
	writeDB.SetMaxOpenConns(writeMaxOpenConns)
	writeDB.SetMaxIdleConns(writeMaxOpenConns)
	readDB, err := sql.Open("sqlite", dsn(s.path))
	if err != nil {
		writeDB.Close()
		return fmt.Errorf("usage: open read pool: %w", err)
	}
	readDB.SetMaxOpenConns(readMaxOpenConns)
	readDB.SetMaxIdleConns(readMaxOpenConns)
	if err := writeDB.PingContext(ctx); err != nil {
		writeDB.Close()
		readDB.Close()
		return fmt.Errorf("usage: ping write pool: %w", err)
	}
	if err := readDB.PingContext(ctx); err != nil {
		writeDB.Close()
		readDB.Close()
		return fmt.Errorf("usage: ping read pool: %w", err)
	}
	s.writeDB = writeDB
	s.readDB = readDB
	// The file exists now: tighten the main database (and any sidecars
	// already created) to owner-only BEFORE any data is written. A failure
	// here aborts the open — a usage database with loose permissions is a
	// real leak, not a degradation.
	if err := tightenFileModes(s.path); err != nil {
		writeDB.Close()
		readDB.Close()
		s.writeDB = nil
		s.readDB = nil
		return err
	}
	return nil
}

// Close releases both pools. Idempotent.
func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.writeDB != nil {
		err = s.writeDB.Close()
		s.writeDB = nil
	}
	if s.readDB != nil {
		if cerr := s.readDB.Close(); err == nil {
			err = cerr
		}
		s.readDB = nil
	}
	return err
}

func (s *SQLiteStore) writePool() *sql.DB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeDB
}

func (s *SQLiteStore) readPool() *sql.DB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readDB
}

// Migrate creates the schema_migrations bookkeeping table and applies every
// pending migration inside its own transaction, recording the version. Runs
// on every startup and is idempotent: already-applied versions are skipped.
// On success the file modes are re-tightened (see
// tightenWritableFileModes): the first migration's transaction is what
// creates the -wal/-shm sidecars on a fresh database.
func (s *SQLiteStore) Migrate(ctx context.Context) error {
	db := s.writePool()
	if db == nil {
		return errors.New("usage: store not open")
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("usage: create schema_migrations: %w", err)
	}
	var applied int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&applied); err != nil {
		return fmt.Errorf("usage: read applied migrations: %w", err)
	}
	for _, m := range migrations {
		if m.Version <= applied {
			continue
		}
		if err := s.applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("usage: apply migration %d: %w", m.Version, err)
		}
		applied = m.Version
	}
	return tightenFileModes(s.path)
}

func (s *SQLiteStore) applyMigration(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.Version, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

// InsertBatch persists events in one transaction on the single write
// connection. Cost is NOT computed here: it was computed once on the
// synchronous request path (middleware.EmitAudit → the wiring-stage pricer)
// and carried on each event; the writer persists it as-is, so the audit log
// and the database store exactly the same amount. A nil Cost (or a pricing
// panic on the request path) is persisted as NULL with the missing_price
// status. A failed single insert drops that event only, logs it, and counts
// it (dropped + write error), so one bad record never kills the writer or
// the batch.
func (s *SQLiteStore) InsertBatch(ctx context.Context, events []Event) error {
	db := s.writePool()
	if db == nil {
		return errors.New("usage: store not open")
	}
	if len(events) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO usage_events (
		ts_unix, ts, request_id, path, model, provider, account, key_id,
		stream, success, status, error_type,
		prompt_tokens, completion_tokens, total_tokens, cached_tokens,
		reasoning_tokens, cache_write_tokens,
		cost_usd, cost_status, duration_ms, usage_source
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	written := 0
	for _, e := range events {
		ts := e.Ts
		if ts.IsZero() {
			ts = time.Now()
		}
		cost := e.Cost
		status := e.CostStatus
		if cost == nil {
			// No cost was computed (no known price, or a pricing panic on
			// the request path): persist NULL with the missing_price status
			// so the row's meaning is unambiguous.
			status = CostStatusMissingPrice
		} else if status == "" {
			status = CostStatusOK
		}
		if _, err := stmt.ExecContext(ctx,
			ts.Unix(), ts.UTC().Format(time.RFC3339),
			e.RequestID, e.Path, e.Model, e.Provider, e.Account, e.KeyID,
			boolInt(e.Stream), boolInt(e.Success), e.Status, e.ErrorType,
			e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.CachedTokens,
			e.ReasoningTokens, e.CacheWriteTokens,
			cost, status, e.DurationMS, e.Source,
		); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// Rate-limited like every other loss signal: a sustained
			// per-event failure cannot flood the log while the counters
			// (write error + dropped) keep the loss observable.
			if flushLogThrottle.allow() {
				slog.Warn("usage: insert event failed, dropping record", "error", err,
					"request_id", e.RequestID, "model", e.Model)
			}
			util.RecordUsageWriteErrors()
			util.RecordUsageEventsDropped()
			continue
		}
		written++
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for i := 0; i < written; i++ {
		util.RecordUsageEventsWritten()
	}
	// The write transaction may have CREATED or RECREATED the main database
	// or its -wal/-shm sidecars (SQLite creates them with the process
	// umask, typically 0644 — e.g. after a checkpoint or when the journal
	// was reset), so re-tighten the file modes after every successful write
	// transaction, not just at Open/Migrate.
	//
	// A tightening failure here must NOT be returned as a batch failure:
	// the batch IS committed and counted as written, and an error return
	// would make the writer treat the whole batch as lost (dropped + write
	// error) and retry it — duplicating rows that are already in the
	// database and breaking the written+dropped accounting identity. The
	// failure is reported loudly on its own channel instead (error-level
	// log + write-error incident counter); the data result stays success.
	if err := tightenFileModes(s.path); err != nil {
		reportTightenFailureAfterCommit(err, s.path, "batch insert")
	}
	return nil
}

// DeleteBefore removes events with ts_unix < tsUnix and returns the number of
// deleted rows. VACUUM is deliberately not run automatically: it would block
// the whole file; the OS reclaims the space over time. Like InsertBatch, the
// file modes are re-tightened after the successful delete (SQLite may have
// recreated the -wal/-shm sidecars); a tightening failure is reported loudly
// (error-level log + incident counter) but never returned as a failed
// delete — the rows ARE already gone, and reporting an error would make the
// retention loop retry (and re-report) a committed deletion.
func (s *SQLiteStore) DeleteBefore(ctx context.Context, tsUnix int64) (int64, error) {
	db := s.writePool()
	if db == nil {
		return 0, errors.New("usage: store not open")
	}
	res, err := db.ExecContext(ctx, `DELETE FROM usage_events WHERE ts_unix < ?`, tsUnix)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tightenFileModes(s.path); err != nil {
		reportTightenFailureAfterCommit(err, s.path, "delete")
	}
	return n, nil
}

// DeleteKeyIDRange removes events for one key_id inside [fromUnix, toUnix]
// (inclusive). Used to refresh imported Grok Build rows for a week window.
func (s *SQLiteStore) DeleteKeyIDRange(ctx context.Context, keyID string, fromUnix, toUnix int64) (int64, error) {
	db := s.writePool()
	if db == nil {
		return 0, errors.New("usage: store not open")
	}
	res, err := db.ExecContext(ctx, `DELETE FROM usage_events WHERE key_id = ? AND ts_unix >= ? AND ts_unix <= ?`, keyID, fromUnix, toUnix)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tightenFileModes(s.path); err != nil {
		reportTightenFailureAfterCommit(err, s.path, "delete")
	}
	return n, nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
