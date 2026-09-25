package metapiusage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// metapiFixtureRow is one proxy_logs row for the test fixture; nil
// pointers map to SQL NULL.
type metapiFixtureRow struct {
	model      string
	createdAt  time.Time
	prompt     *int64
	completion *int64
	total      *int64
}

func i64(v int64) *int64 { return &v }

// createdAtFixtureLayout is the fixture-side copy of metapi's created_at
// shape, written as a literal on purpose: fixtures must not certify the
// production constant they pin (see TestCreatedAtLayoutLiteral), or a
// drifted metapiCreatedAtLayout would keep every fixture green.
const createdAtFixtureLayout = "2006-01-02 15:04:05"

// createProxyLogsTable creates the proxy_logs subset this package reads.
func createProxyLogsTable(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE proxy_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		model_requested TEXT,
		prompt_tokens INTEGER,
		completion_tokens INTEGER,
		total_tokens INTEGER,
		created_at TEXT DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatal(err)
	}
}

// writeFixtureAt creates a proxy_logs fixture at path and inserts rows.
// Returns the database path.
func writeFixtureAt(t *testing.T, path string, rows []metapiFixtureRow) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createProxyLogsTable(t, db)
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO proxy_logs (model_requested, prompt_tokens, completion_tokens, total_tokens, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			r.model, r.prompt, r.completion, r.total,
			r.createdAt.UTC().Format(createdAtFixtureLayout),
		); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// writeFixture creates a proxy_logs fixture in its own temp dir. Returns
// the database path.
func writeFixture(t *testing.T, rows []metapiFixtureRow) string {
	t.Helper()
	return writeFixtureAt(t, filepath.Join(t.TempDir(), "hub.db"), rows)
}

// TestSumClinePassTokens pins the query contract: only
// model_requested LIKE 'cline-pass/%' rows inside the inclusive time window
// count; the per-row total is total_tokens, or prompt+completion when
// total_tokens is NULL.
func TestSumClinePassTokens(t *testing.T) {
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	from := base.Unix()
	to := base.Add(time.Hour).Unix()

	path := writeFixture(t, []metapiFixtureRow{
		// counted: stored total
		{model: "cline-pass/deepseek-v4.1-flash", createdAt: base.Add(10 * time.Minute), prompt: i64(60), completion: i64(40), total: i64(100)},
		// counted: NULL total falls back to prompt+completion
		{model: "cline-pass/kimi-k3", createdAt: base.Add(20 * time.Minute), prompt: i64(7), completion: i64(3), total: nil},
		// counted: both window bounds are inclusive
		{model: "cline-pass/a", createdAt: base, total: i64(1)},
		{model: "cline-pass/b", createdAt: base.Add(time.Hour), total: i64(2)},
		// excluded: outside the window
		{model: "cline-pass/a", createdAt: base.Add(-time.Second), total: i64(1000)},
		{model: "cline-pass/a", createdAt: base.Add(time.Hour + time.Second), total: i64(2000)},
		// excluded: other models, including a cline-pass prefix without the
		// slash separator and a similar-looking id
		{model: "gpt-5", createdAt: base.Add(time.Minute), total: i64(5000)},
		{model: "cline-pass-extra/x", createdAt: base.Add(time.Minute), total: i64(5000)},
		{model: "xcline-pass/a", createdAt: base.Add(time.Minute), total: i64(5000)},
	})

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	got, err := st.SumClinePassTokens(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if got != 113 {
		t.Fatalf("sum = %d, want 113", got)
	}

	// A non-positive lower bound is a no-op, matching usage.SumTokensLike.
	if got, err = st.SumClinePassTokens(context.Background(), 0, to); err != nil || got != 0 {
		t.Fatalf("from<=0: got (%d, %v), want (0, nil)", got, err)
	}

	// An empty window sums to 0, not an error.
	if got, err = st.SumClinePassTokens(context.Background(), to+10, to+20); err != nil || got != 0 {
		t.Fatalf("empty window: got (%d, %v), want (0, nil)", got, err)
	}
}

// TestSumMissingTable pins the degrade path: a database without the
// proxy_logs table opens (ping succeeds) but the sum returns an error,
// which the quota wiring turns into "no estimate" — never a failure.
func TestSumMissingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.SumClinePassTokens(context.Background(), 1, 2); err == nil {
		t.Fatal("sum over a missing proxy_logs table must error")
	}
}

func TestOpenMissingFile(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope", "hub.db")); err == nil {
		t.Fatal("open of a missing file must fail")
	}
	if _, err := Open("  "); err == nil {
		t.Fatal("open of an empty path must fail")
	}
}

// TestStoreIsReadOnly pins mode=ro: a write through the opened handle must
// be rejected by SQLite, so prism can never modify metapi's database.
func TestStoreIsReadOnly(t *testing.T) {
	path := writeFixture(t, nil)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.db.Exec(`INSERT INTO proxy_logs (model_requested) VALUES ('cline-pass/x')`); err == nil {
		t.Fatal("write through a read-only store must fail")
	}
}

func TestRodsn(t *testing.T) {
	got := roDSN("/var/lib/metapi/data/hub.db")
	if !strings.HasPrefix(got, "file:") {
		t.Fatalf("ro DSN must use the file: URI form, got %q", got)
	}
	if !strings.Contains(got, "mode=ro") {
		t.Fatalf("ro DSN must open strictly read-only, got %q", got)
	}
	if !strings.Contains(got, "_pragma=busy_timeout(5000)") {
		t.Fatalf("ro DSN must carry the busy timeout, got %q", got)
	}
}

// TestCreatedAtLayoutLiteral is the independent contract pin for
// proxy_logs.created_at (oracle M11): the expectation is a hand-written
// literal string, deliberately NOT metapiCreatedAtLayout, so changing the
// production constant fails here instead of certifying itself.
func TestCreatedAtLayoutLiteral(t *testing.T) {
	ts := time.Date(2026, 9, 25, 21, 41, 3, 0, time.UTC)
	got := formatCreatedAt(ts.Unix())
	if got != "2026-09-25 21:41:03" {
		t.Fatalf("formatCreatedAt = %q, want %q (metapi datetime('now') UTC text)", got, "2026-09-25 21:41:03")
	}
	// The range comparison in SumClinePassTokens is a TEXT comparison, so
	// the layout must sort lexicographically in time order. A layout that
	// ignores the ordering (or drops the space at index 10) is caught here.
	if len(got) != 19 || got[10] != ' ' {
		t.Fatalf("layout %q must be 19 chars with a space at index 10, got %q", metapiCreatedAtLayout, got)
	}
	if a, b := formatCreatedAt(ts.Unix()), formatCreatedAt(ts.Add(time.Second).Unix()); a >= b {
		t.Fatalf("layout %q is not lexicographically increasing: %q !< %q", metapiCreatedAtLayout, a, b)
	}
}

// writeCreatedAtLiteralFixture seeds one cline-pass row per literal
// created_at value. The values are passed raw by the caller, never
// rendered through the production layout, so the shape guard is pinned
// against literal text instead of certifying itself.
func writeCreatedAtLiteralFixture(t *testing.T, values ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createProxyLogsTable(t, db)
	for _, v := range values {
		if _, err := db.Exec(
			`INSERT INTO proxy_logs (model_requested, total_tokens, created_at) VALUES ('cline-pass/x', 1, ?)`, v,
		); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// TestCheckCreatedAtShape pins the drift guard: MAX(created_at) must look
// like the UTC `YYYY-MM-DD HH:MM:SS` text (space at index 10) the TEXT
// range comparison depends on. Drift must fail BOTH the probe and every
// sum, so a round degrades to "no estimate" instead of silently
// mis-bounding the window; an empty table is skipped.
func TestCheckCreatedAtShape(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		drift  bool
	}{
		{name: "empty table skipped"},
		{name: "literal utc accepted", values: []string{"2026-09-25 21:41:03"}},
		{name: "rfc3339 rejected", values: []string{"2026-09-25T21:41:03Z"}, drift: true},
		{name: "epoch seconds as text rejected", values: []string{"1758831663"}, drift: true},
		{name: "newest drifted row wins", values: []string{"2026-09-25 21:41:03", "2026-09-25T22:00:00Z"}, drift: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := Open(writeCreatedAtLiteralFixture(t, tc.values...))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			probeErr := st.checkCreatedAtShape(context.Background())
			if tc.drift && !errors.Is(probeErr, ErrCreatedAtShape) {
				t.Fatalf("shape probe = %v, want ErrCreatedAtShape", probeErr)
			}
			if !tc.drift && probeErr != nil {
				t.Fatalf("shape probe = %v, want nil", probeErr)
			}

			_, sumErr := st.SumClinePassTokens(context.Background(), 1, 2)
			if tc.drift && !errors.Is(sumErr, ErrCreatedAtShape) {
				t.Fatalf("sum over drifted created_at = %v, want ErrCreatedAtShape (this round must produce no estimate)", sumErr)
			}
			if !tc.drift && sumErr != nil {
				t.Fatalf("sum error = %v, want nil", sumErr)
			}
		})
	}
}

// TestCheckCreatedAtShapeDriftDegradesOlderValidRows pins the mixed case
// harder: older rows in the expected shape must not mask a newer drifted
// sample — MAX picks the drifted row and the sum is refused.
func TestCheckCreatedAtShapeDriftDegradesOlderValidRows(t *testing.T) {
	path := writeCreatedAtLiteralFixture(t,
		"2026-09-25 10:00:00",
		"2026-09-25T11:00:00Z", // lexicographically largest: T > space
	)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got, err := st.SumClinePassTokens(context.Background(), 1, 2); !errors.Is(err, ErrCreatedAtShape) || got != 0 {
		t.Fatalf("sum = (%d, %v), want (0, ErrCreatedAtShape)", got, err)
	}
}
