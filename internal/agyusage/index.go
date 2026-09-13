package agyusage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultIndexPath is the sidecar sqlite used as a hot-path cache of
// parsed agy generations. HTTP queries hit only this file.
const DefaultIndexPath = "/var/lib/prism/agy-usage.db"

const (
	Provider = "gemini"
	Account  = "agy-local"
	pruneAge = 14 * 24 * time.Hour
)

// DefaultConversationsDir is ~/.gemini/antigravity-cli/conversations.
func DefaultConversationsDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
}

var groupByColumns = map[string]string{
	"model":    "model",
	"provider": "'" + Provider + "'",
	"account":  "'" + Account + "'",
	"key_id":   "''",
	"stream":   "0",
	"success":  "1",
	"hour":     "(ts_unix/3600)*3600",
	"day":      "(ts_unix/86400)*86400",
}

// Query is the index-side aggregation. Fields mirror usage.SummaryQuery so
// the CLI/handler can copy them without this package importing usage.
type Query struct {
	From     int64
	To       int64
	GroupBy  []string
	Model    string
	Provider string
	Account  string
	KeyID    string
	Stream   *bool
	Success  *bool
}

// Row is one aggregated group, ready to convert into usage.SummaryRow.
type Row struct {
	Groups             map[string]any
	Requests           int64
	PromptTokens       int64
	CachedTokens       int64
	CompletionTokens   int64
	ReasoningTokens    int64
	TotalTokens        int64
	HitRateInputTokens int64
}

// Index is a writable sidecar of parsed generations. Source conversation
// databases are opened read-only (mode=ro) and never copied into usage_events.
type Index struct {
	path    string
	convDir string

	mu sync.Mutex
	db *sql.DB
}

// Open creates (or opens) the sidecar index. The caller degrades on error
// — this must not crash the process. convDir is the agy conversations
// directory; tests inject a temp dir.
func Open(indexPath, convDir string) (*Index, error) {
	if indexPath == "" {
		return nil, errors.New("agyusage: empty index path")
	}
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o700); err != nil {
		return nil, fmt.Errorf("agyusage: mkdir: %w", err)
	}
	db, err := sql.Open("sqlite", dsn(indexPath))
	if err != nil {
		return nil, fmt.Errorf("agyusage: open index: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("agyusage: ping index: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(indexPath, 0o600)
	return &Index{path: indexPath, convDir: convDir, db: db}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS generations (
    conversation_id    TEXT NOT NULL,
    request_id         TEXT NOT NULL,
    ts_unix            INTEGER NOT NULL,
    model              TEXT,
    prompt_tokens      INTEGER NOT NULL DEFAULT 0,
    cached_tokens      INTEGER NOT NULL DEFAULT 0,
    completion_tokens  INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tokens       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (conversation_id, request_id)
);
CREATE INDEX IF NOT EXISTS idx_generations_ts ON generations(ts_unix);
CREATE TABLE IF NOT EXISTS sources (
    path     TEXT PRIMARY KEY,
    mtime_ns INTEGER NOT NULL,
    size     INTEGER NOT NULL
);`)
	if err != nil {
		return fmt.Errorf("agyusage: migrate: %w", err)
	}
	return nil
}

// Close releases the index. Idempotent.
func (idx *Index) Close() error {
	if idx == nil {
		return nil
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.db == nil {
		return nil
	}
	err := idx.db.Close()
	idx.db = nil
	return err
}

func (idx *Index) dbOrNil() *sql.DB {
	if idx == nil {
		return nil
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.db
}

// Refresh incrementally re-reads conversation *.db files whose mtime/size
// changed. Missing source files keep already-indexed rows (deleted
// conversations must not under-count the current week). Rows older than
// 14 days are pruned.
func (idx *Index) Refresh(ctx context.Context) error {
	db := idx.dbOrNil()
	if db == nil {
		return errors.New("agyusage: index not open")
	}
	if idx.convDir == "" {
		return idx.prune(ctx, db, time.Now())
	}
	matches, err := filepath.Glob(filepath.Join(idx.convDir, "*.db"))
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(matches))
	for _, path := range matches {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fi, err := os.Stat(path)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		seen[path] = struct{}{}
		mtime, size := sourceStamp(path, fi)
		var prevM, prevS int64
		err = db.QueryRowContext(ctx, `SELECT mtime_ns, size FROM sources WHERE path = ?`, path).Scan(&prevM, &prevS)
		if err == nil && prevM == mtime && prevS == size {
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		gens, perr := readConversation(path)
		if perr != nil {
			continue
		}
		if err := idx.replaceConversation(ctx, db, path, mtime, size, gens); err != nil {
			return err
		}
	}
	// Drop source fingerprints for vanished files; keep generations.
	rows, err := db.QueryContext(ctx, `SELECT path FROM sources`)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		if _, ok := seen[p]; !ok {
			gone = append(gone, p)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, p := range gone {
		if _, err := db.ExecContext(ctx, `DELETE FROM sources WHERE path = ?`, p); err != nil {
			return err
		}
	}
	return idx.prune(ctx, db, time.Now())
}

func sourceStamp(path string, fi os.FileInfo) (mtimeNs, size int64) {
	mtimeNs = fi.ModTime().UnixNano()
	size = fi.Size()
	if wal, err := os.Stat(path + "-wal"); err == nil {
		if wt := wal.ModTime().UnixNano(); wt > mtimeNs {
			mtimeNs = wt
		}
		size += wal.Size()
	}
	return mtimeNs, size
}

func (idx *Index) replaceConversation(ctx context.Context, db *sql.DB, path string, mtime, size int64, gens []Generation) error {
	convID := strings.TrimSuffix(filepath.Base(path), ".db")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `DELETE FROM generations WHERE conversation_id = ?`, convID); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO generations (
		conversation_id, request_id, ts_unix, model,
		prompt_tokens, cached_tokens, completion_tokens, reasoning_tokens, total_tokens
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, g := range gens {
		if _, err := stmt.ExecContext(ctx, g.ConversationID, g.RequestID, g.TsUnix, g.Model,
			g.PromptTokens, g.CachedTokens, g.CompletionTokens, g.ReasoningTokens, g.TotalTokens); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sources(path, mtime_ns, size) VALUES(?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET mtime_ns = excluded.mtime_ns, size = excluded.size`,
		path, mtime, size); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	ok = true
	return nil
}

func (idx *Index) prune(ctx context.Context, db *sql.DB, now time.Time) error {
	cutoff := now.Add(-pruneAge).Unix()
	_, err := db.ExecContext(ctx, `DELETE FROM generations WHERE ts_unix < ?`, cutoff)
	return err
}

// SumTokens sums total_tokens for ts_unix in [fromUnix, toUnix] (inclusive).
// Only gemini-* rows (and blank model, which is still Gemini in live dbs)
// feed the Gemini week reversal; other families would inflate the pool.
// fromUnix <= 0 returns 0, matching usage.SumTokensLike.
func (idx *Index) SumTokens(ctx context.Context, fromUnix, toUnix int64) (int64, error) {
	db := idx.dbOrNil()
	if db == nil {
		return 0, errors.New("agyusage: index not open")
	}
	if fromUnix <= 0 {
		return 0, nil
	}
	q := `SELECT COALESCE(SUM(total_tokens), 0) FROM generations
		WHERE ts_unix >= ? AND ts_unix <= ?
		AND (model = '' OR LOWER(model) LIKE 'gemini-%')`
	var n int64
	if err := db.QueryRowContext(ctx, q, fromUnix, toUnix).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Query aggregates indexed generations. HTTP callers must not Refresh here.
func (idx *Index) Query(ctx context.Context, q Query) ([]Row, error) {
	db := idx.dbOrNil()
	if db == nil {
		return nil, errors.New("agyusage: index not open")
	}
	if skipAgy(q) {
		return []Row{}, nil
	}

	var groupExprs []string
	var groupNames []string
	seen := make(map[string]bool, len(q.GroupBy))
	for _, g := range q.GroupBy {
		expr, ok := groupByColumns[g]
		if !ok {
			continue
		}
		if !seen[g] {
			seen[g] = true
			groupExprs = append(groupExprs, expr)
			groupNames = append(groupNames, g)
		}
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	if len(groupExprs) > 0 {
		sb.WriteString(strings.Join(groupExprs, ", "))
		sb.WriteString(", ")
	}
	sb.WriteString(`COUNT(*) AS requests,
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(reasoning_tokens), 0),
		COALESCE(SUM(total_tokens), 0)
	FROM generations WHERE 1=1`)
	args := make([]any, 0, 4)
	if q.From > 0 {
		sb.WriteString(" AND ts_unix >= ?")
		args = append(args, q.From)
	}
	if q.To > 0 {
		sb.WriteString(" AND ts_unix <= ?")
		args = append(args, q.To)
	}
	if q.Model != "" {
		sb.WriteString(" AND model = ?")
		args = append(args, q.Model)
	}
	if len(groupExprs) > 0 {
		sb.WriteString(" GROUP BY " + strings.Join(groupExprs, ", "))
	}

	rows, err := db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Row, 0, 8)
	for rows.Next() {
		row := Row{Groups: make(map[string]any, len(groupNames))}
		dest := make([]any, 0, len(groupNames)+6)
		strVals := make([]*string, len(groupNames))
		intVals := make([]*int64, len(groupNames))
		for i, name := range groupNames {
			switch name {
			case "hour", "day", "stream", "success":
				intVals[i] = new(int64)
				dest = append(dest, intVals[i])
			default:
				strVals[i] = new(string)
				dest = append(dest, strVals[i])
			}
		}
		var requests, pt, cached, ct, rt, tt int64
		dest = append(dest, &requests, &pt, &cached, &ct, &rt, &tt)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		for i, name := range groupNames {
			if intVals[i] != nil {
				row.Groups[name] = *intVals[i]
			} else {
				row.Groups[name] = *strVals[i]
			}
		}
		row.Requests = requests
		row.PromptTokens = pt
		row.CachedTokens = cached
		row.CompletionTokens = ct
		row.ReasoningTokens = rt
		row.TotalTokens = tt
		row.HitRateInputTokens = pt + cached
		if row.Requests == 0 && tt == 0 && pt == 0 {
			continue
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func skipAgy(q Query) bool {
	if q.KeyID != "" {
		return true
	}
	if q.Stream != nil {
		return true
	}
	if q.Success != nil && !*q.Success {
		return true
	}
	if q.Provider != "" && q.Provider != Provider {
		return true
	}
	if q.Account != "" && q.Account != Account {
		return true
	}
	return false
}
