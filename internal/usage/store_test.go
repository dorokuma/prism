package usage

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// openTestStore opens a SQLiteStore on a fresh temp file and applies
// migrations. t.TempDir is used (never :memory:) so WAL behavior is real.
func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	s := &SQLiteStore{path: path}
	if err := s.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

// costOf prices a test event's token counts through the single production
// pricing function (ComputeCost), exactly like the synchronous request path
// does — test events never reimplement the formula.
func costOf(prompt, completion, cached, cacheWrite int64, source string, price *Price) (*float64, string) {
	return ComputeCost(prompt, completion, cached, cacheWrite, source, price)
}

func testEvent(ts time.Time, model string, price *Price) Event {
	cost, status := costOf(100, 50, 0, 0, "", price)
	return Event{
		Ts: ts, RequestID: "req-" + model, Path: "/v1/chat/completions",
		Model: model, Provider: "test-provider", Account: "acc1", KeyID: "key-name-1",
		Stream: true, Success: true, Status: 200,
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
		DurationMS: 12.5, Cost: cost, CostStatus: status,
	}
}

func TestMigrateIdempotent(t *testing.T) {
	s := openTestStore(t) // migrated once by the helper
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("third migrate: %v", err)
	}
	db, err := sql.Open("sqlite", dsn(s.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("schema_migrations has %d rows, want 2 (idempotent re-run of v1+v2)", count)
	}
	var version int
	var appliedAt string
	if err := db.QueryRow(`SELECT version, applied_at FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &appliedAt); err != nil {
		t.Fatal(err)
	}
	if version != 2 || appliedAt == "" {
		t.Fatalf("latest migration row = %d %q, want version 2 with applied_at", version, appliedAt)
	}
}

func TestUsageEventsSchema(t *testing.T) {
	s := openTestStore(t)
	db, err := sql.Open("sqlite", dsn(s.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// request_id must have NO unique constraint (client-controlled ID would
	// allow bill evasion via ID reuse).
	var tableSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='usage_events'`).Scan(&tableSQL); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(tableSQL), "UNIQUE") {
		t.Fatalf("usage_events contains a UNIQUE constraint, request_id must not be unique: %s", tableSQL)
	}

	// exactly the six expected indexes, none unique
	rows, err := db.Query(`PRAGMA index_list('usage_events')`)
	if err != nil {
		t.Fatal(err)
	}
	indexes := map[string]bool{}
	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		if unique != 0 {
			t.Errorf("index %s is UNIQUE", name)
		}
		indexes[name] = true
	}
	rows.Close()
	for _, want := range []string{
		"idx_usage_events_ts_unix",
		"idx_usage_events_key_id_ts",
		"idx_usage_events_model_ts",
		"idx_usage_events_provider_ts",
		"idx_usage_events_account_ts",
		"idx_usage_events_success_ts",
	} {
		if !indexes[want] {
			t.Errorf("missing index %s (have %v)", want, indexes)
		}
	}
	if len(indexes) != 6 {
		t.Errorf("unexpected index set: %v", indexes)
	}

	// required columns with NOT NULL on ts_unix and token counters
	cols := map[string]string{}
	rows2, err := db.Query(`PRAGMA table_info('usage_events')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows2.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows2.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[name] = fmt.Sprintf("nn=%d default=%q pk=%d", notnull, dflt.String, pk)
	}
	rows2.Close()
	for _, want := range []string{
		"id", "ts_unix", "ts", "request_id", "path", "model", "provider",
		"account", "key_id", "stream", "success", "status", "error_type",
		"prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens",
		"reasoning_tokens", "cache_write_tokens", "cost_usd", "cost_status",
		"duration_ms", "usage_source",
	} {
		if _, ok := cols[want]; !ok {
			t.Errorf("missing column %s", want)
		}
	}
	if !strings.HasPrefix(cols["ts_unix"], "nn=1") {
		t.Errorf("ts_unix must be NOT NULL, got %s", cols["ts_unix"])
	}
	if !strings.HasPrefix(cols["prompt_tokens"], "nn=1") {
		t.Errorf("prompt_tokens must be NOT NULL DEFAULT 0, got %s", cols["prompt_tokens"])
	}
	if !strings.Contains(cols["prompt_tokens"], `default="0"`) {
		t.Errorf("prompt_tokens must default to 0, got %s", cols["prompt_tokens"])
	}
	// INTEGER PRIMARY KEY is a rowid alias: pragma reports nn=0, but the
	// AUTOINCREMENT declaration is visible in the CREATE TABLE text.
	if !strings.Contains(cols["id"], "pk=1") {
		t.Errorf("id must be INTEGER PRIMARY KEY, got %s", cols["id"])
	}
	if !strings.Contains(tableSQL, "AUTOINCREMENT") {
		t.Errorf("id must be AUTOINCREMENT, table SQL: %s", tableSQL)
	}

	// behavioral proof: two events sharing one request_id both persist
	ctx := context.Background()
	now := time.Now()
	ev1 := testEvent(now, "m1", nil)
	ev2 := testEvent(now, "m2", nil)
	ev2.RequestID = ev1.RequestID // same client-supplied ID
	if err := s.InsertBatch(ctx, []Event{ev1, ev2}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE request_id = ?`, ev1.RequestID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("same request_id persisted %d rows, want 2 (no UNIQUE constraint)", n)
	}
}

func TestWALMode(t *testing.T) {
	s := openTestStore(t)
	db, err := sql.Open("sqlite", dsn(s.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var syncMode string
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&syncMode); err != nil {
		t.Fatal(err)
	}
	if syncMode != "1" && syncMode != "NORMAL" {
		t.Fatalf("synchronous = %q, want NORMAL", syncMode)
	}
	var busy int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busy)
	}
}

func TestInsertBatchAndSummaryRoundtrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	price := &Price{Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 1}
	evA1 := testEvent(now, "model-a", price)
	evA2 := testEvent(now.Add(time.Second), "model-a-2", nil) // missing price
	evB := testEvent(now.Add(2*time.Second), "model-b", price)
	if err := s.InsertBatch(ctx, []Event{evA1, evA2, evB}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Summary(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("ungrouped summary: %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Requests != 3 || r.PromptTokens != 300 || r.CompletionTokens != 150 || r.TotalTokens != 450 {
		t.Fatalf("aggregates = %+v", r)
	}
	// evA1: (100/1e6*1)+(50/1e6*2) = 0.0002; evB same → 0.0004
	if r.CostUSD == nil || *r.CostUSD != 0.0004 {
		t.Fatalf("cost_usd = %v, want 0.0004", r.CostUSD)
	}
	if r.CostMissingRequests != 1 {
		t.Fatalf("cost_missing_requests = %d, want 1", r.CostMissingRequests)
	}

	byModel, err := s.Summary(ctx, SummaryQuery{GroupBy: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(byModel) != 3 {
		t.Fatalf("grouped by model: %d rows, want 3", len(byModel))
	}
	for _, row := range byModel {
		switch row.Groups["model"] {
		case "model-a":
			if row.Requests != 1 || row.CostMissingRequests != 0 || row.CostUSD == nil || *row.CostUSD != 0.0002 {
				t.Errorf("model-a row: %+v", row)
			}
		case "model-a-2":
			if row.Requests != 1 || row.CostMissingRequests != 1 || row.CostUSD != nil {
				t.Errorf("model-a-2 row (missing price): %+v", row)
			}
		case "model-b":
			if row.Requests != 1 || row.CostMissingRequests != 0 || row.CostUSD == nil || *row.CostUSD != 0.0002 {
				t.Errorf("model-b row: %+v", row)
			}
		default:
			t.Errorf("unexpected group %v", row.Groups)
		}
	}
}

// TestUsageSourcePersisted: the Source 口径 marker must be persisted so any
// row's pricing basis can be audited ("落库时口径也要存下来便于排查").
func TestUsageSourcePersisted(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	p := &Price{Input: 1, Output: 1}
	c1, _ := costOf(10, 0, 0, 0, SourceAnthropic, p)
	c2, _ := costOf(10, 0, 0, 0, SourceOpenAI, p)
	c3, _ := costOf(10, 0, 0, 0, "", p)
	if err := s.InsertBatch(ctx, []Event{
		{Ts: now, RequestID: "a", Model: "m", PromptTokens: 10, Source: SourceAnthropic, Cost: c1, CostStatus: CostStatusOK},
		{Ts: now, RequestID: "b", Model: "m", PromptTokens: 10, Source: SourceOpenAI, Cost: c2, CostStatus: CostStatusOK},
		{Ts: now, RequestID: "c", Model: "m", PromptTokens: 10, Cost: c3, CostStatus: CostStatusOK}, // legacy: no marker
	}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn(s.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT request_id, usage_source FROM usage_events ORDER BY request_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id string
		var src sql.NullString
		if err := rows.Scan(&id, &src); err != nil {
			t.Fatal(err)
		}
		got[id] = src.String
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got["a"] != SourceAnthropic {
		t.Errorf("event a usage_source = %q, want %q", got["a"], SourceAnthropic)
	}
	if got["b"] != SourceOpenAI {
		t.Errorf("event b usage_source = %q, want %q", got["b"], SourceOpenAI)
	}
	if got["c"] != "" {
		t.Errorf("event c usage_source = %q, want empty (legacy event without marker)", got["c"])
	}
}

func TestDeleteBefore(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	mid := now.Add(-24 * time.Hour)
	if err := s.InsertBatch(ctx, []Event{
		testEvent(old, "a", nil), testEvent(mid, "b", nil), testEvent(now, "c", nil),
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteBefore(ctx, now.Add(-36*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted %d rows, want 1", n)
	}
	rows, err := s.Summary(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Requests != 2 {
		t.Fatalf("remaining %d, want 2", rows[0].Requests)
	}
}

// TestConcurrentWriteReadNoLocked is the acceptance test for the two-pool WAL
// design: several writers hammer InsertBatch while readers run Summary, and
// no SQLITE_BUSY / "database is locked" may surface.
func TestConcurrentWriteReadNoLocked(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	price := &Price{Input: 0.14, Output: 0.42}
	c, _ := costOf(10, 5, 0, 0, "", price)

	const writers = 6
	const batches = 40
	const perBatch = 8
	const readers = 4
	const readsPerReader = 60

	var wg sync.WaitGroup
	errCh := make(chan error, writers+readers)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for b := 0; b < batches; b++ {
				evs := make([]Event, perBatch)
				for i := 0; i < perBatch; i++ {
					evs[i] = Event{
						Ts: time.Now(), RequestID: fmt.Sprintf("w%d-b%d-i%d", w, b, i),
						Model: "m", Provider: "p", Account: "a", KeyID: "k",
						Stream: true, Success: true, Status: 200,
						PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
						Cost:       c,
						CostStatus: CostStatusOK,
					}
				}
				if err := s.InsertBatch(ctx, evs); err != nil {
					errCh <- fmt.Errorf("writer %d: %w", w, err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < readsPerReader; i++ {
				if _, err := s.Summary(ctx, SummaryQuery{GroupBy: []string{"model"}}); err != nil {
					errCh <- fmt.Errorf("reader %d grouped: %w", r, err)
					return
				}
				if _, err := s.Summary(ctx, SummaryQuery{}); err != nil {
					errCh <- fmt.Errorf("reader %d ungrouped: %w", r, err)
					return
				}
			}
		}(r)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "locked") || strings.Contains(lower, "busy") {
			t.Errorf("SQLITE_BUSY surfaced under concurrent write/read: %v", err)
		}
		t.Error(err)
	}

	rows, err := s.Summary(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	want := writers * batches * perBatch
	if rows[0].Requests != int64(want) {
		t.Fatalf("persisted %d events, want %d", rows[0].Requests, want)
	}
}
