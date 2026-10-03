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
// pointers map to SQL NULL. accountID is the metapi account the row belongs
// to (NULL for rows metapi recorded without one), which is what the
// per-account sum isolates on.
type metapiFixtureRow struct {
	accountID  *int64
	model      string
	createdAt  time.Time
	prompt     *int64
	completion *int64
	total      *int64
	// cacheRead is metapi's cache_read_tokens, the part of total_tokens the
	// per-account sum must NOT count (the upstream percent it is reversed
	// against is a prompt+completion 口径).
	cacheRead *int64
}

func i64(v int64) *int64 { return &v }

// metapiAccountFixture is one accounts row: site 49 is ClinePass, status is
// metapi's own enum ('active' / 'disabled', default 'active').
type metapiAccountFixture struct {
	id       int64
	siteID   int64
	username string
	status   string
	apiToken *string // nil = SQL NULL (an account without a usable token)
}

func sptr(v string) *string { return &v }

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
		account_id INTEGER,
		model_requested TEXT,
		prompt_tokens INTEGER,
		completion_tokens INTEGER,
		total_tokens INTEGER,
		cache_read_tokens INTEGER,
		created_at TEXT DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatal(err)
	}
}

// createAccountsTable creates the accounts subset this package reads: id,
// site_id, username, status and api_token. The production table's other
// columns (tokens, quota, oauth linkage, …) are deliberately absent — prism
// never reads them.
func createAccountsTable(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id INTEGER NOT NULL,
		username TEXT,
		status TEXT DEFAULT 'active',
		api_token TEXT
	)`); err != nil {
		t.Fatal(err)
	}
}

// insertProxyLogs writes the fixture rows into proxy_logs. The column list is
// explicit (account_id included) so a fixture row with a NULL account_id is
// the same as metapi's own NULL.
func insertProxyLogs(t *testing.T, db *sql.DB, rows []metapiFixtureRow) {
	t.Helper()
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO proxy_logs (account_id, model_requested, prompt_tokens, completion_tokens, total_tokens, cache_read_tokens, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.accountID, r.model, r.prompt, r.completion, r.total, r.cacheRead,
			r.createdAt.UTC().Format(createdAtFixtureLayout),
		); err != nil {
			t.Fatal(err)
		}
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
	insertProxyLogs(t, db, rows)
	return path
}

// writeHubFixtureAt creates BOTH tables this package reads (accounts and
// proxy_logs) at path and returns the path. It is the fixture of the
// account-aware paths: the roster (ListClinePassAccounts) and the per-account
// token sum.
func writeHubFixtureAt(t *testing.T, path string, accounts []metapiAccountFixture, rows []metapiFixtureRow) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createProxyLogsTable(t, db)
	createAccountsTable(t, db)
	for _, a := range accounts {
		if _, err := db.Exec(
			`INSERT INTO accounts (id, site_id, username, status, api_token) VALUES (?, ?, ?, ?, ?)`,
			a.id, a.siteID, a.username, a.status, a.apiToken,
		); err != nil {
			t.Fatal(err)
		}
	}
	insertProxyLogs(t, db, rows)
	return path
}

// writeHubFixture is writeHubFixtureAt in its own temp dir.
func writeHubFixture(t *testing.T, accounts []metapiAccountFixture, rows []metapiFixtureRow) string {
	t.Helper()
	return writeHubFixtureAt(t, filepath.Join(t.TempDir(), "hub.db"), accounts, rows)
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

// TestSumClinePassTokensByAccount pins two properties at once: per-account
// isolation — only the rows of the requested metapi account count, another
// account's ClinePass traffic in the SAME window must not leak in (each
// subscription's pool is its own) — and the molecule, prompt_tokens +
// completion_tokens with metapi's cache_read_tokens EXCLUDED (total_tokens
// WOULD count the cache, which the upstream percent does not).
func TestSumClinePassTokensByAccount(t *testing.T) {
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	from := base.Unix()
	to := base.Add(time.Hour).Unix()
	acctA, acctB := int64(34), int64(38)

	path := writeHubFixture(t, nil, []metapiFixtureRow{
		// account 34: counted (prompt+completion), no cache on the row.
		{accountID: &acctA, model: "cline-pass/x", createdAt: base.Add(10 * time.Minute), prompt: i64(60), completion: i64(40), total: i64(100)},
		// account 34 with a NULL total: prompt+completion still count.
		{accountID: &acctA, model: "cline-pass/y", createdAt: base.Add(20 * time.Minute), prompt: i64(7), completion: i64(3), total: nil},
		// account 34, cache-heavy: metapi's total (9820) includes 9800
		// cache-read tokens, which the sum MUST drop (20 + 0 counts).
		{accountID: &acctA, model: "cline-pass/z", createdAt: base.Add(25 * time.Minute), prompt: i64(20), total: i64(9_820), cacheRead: i64(9_800)},
		// account 38 in the same window: excluded from account 34's sum.
		{accountID: &acctB, model: "cline-pass/x", createdAt: base.Add(15 * time.Minute), prompt: i64(5_000), total: i64(9_999), cacheRead: i64(4_999)},
		// NULL account_id (a row metapi recorded without an account): never
		// attributed to a specific account.
		{model: "cline-pass/x", createdAt: base.Add(16 * time.Minute), prompt: i64(5_000), total: i64(5_000)},
		// account 34 outside the window / with another model: excluded.
		{accountID: &acctA, model: "cline-pass/x", createdAt: base.Add(-time.Second), prompt: i64(1_000), total: i64(1_000)},
		{accountID: &acctA, model: "gpt-5", createdAt: base.Add(5 * time.Minute), prompt: i64(2_000), total: i64(2_000)},
	})

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// 60+40 + 7+3 + 20+0 = 130: the two cache-heavy rows contribute nothing
	// of their cache to this account.
	if got, err := st.SumClinePassTokensByAccount(ctx, acctA, from, to); err != nil || got != 130 {
		t.Fatalf("account 34 sum = (%d, %v), want (130, nil) — prompt+completion only", got, err)
	}
	// 5000 + 0: the other account's own cache is dropped too, and 4200/7000
	// of the first account never leak here.
	if got, err := st.SumClinePassTokensByAccount(ctx, acctB, from, to); err != nil || got != 5_000 {
		t.Fatalf("account 38 sum = (%d, %v), want (5000, nil) — the other account must not leak", got, err)
	}
	// An account with no traffic in the window sums to 0, not an error.
	if got, err := st.SumClinePassTokensByAccount(ctx, 41, from, to); err != nil || got != 0 {
		t.Fatalf("unknown account sum = (%d, %v), want (0, nil)", got, err)
	}
	// A non-positive lower bound is a no-op, like the subscription-wide sum.
	if got, err := st.SumClinePassTokensByAccount(ctx, acctA, 0, to); err != nil || got != 0 {
		t.Fatalf("from<=0: got (%d, %v), want (0, nil)", got, err)
	}

	// The subscription-wide sum still sees all accounts — and still counts
	// total_tokens (cache included): 100 + 10 + 9820 + 9999 + 5000.
	if got, err := st.SumClinePassTokens(ctx, from, to); err != nil || got != 24_929 {
		t.Fatalf("subscription-wide sum = (%d, %v), want (24929, nil)", got, err)
	}
}

// TestListClinePassAccountsFiltersDisabled pins the SQL-side roster filter:
// site 49 AND status 'active'. The production site 49 holds one active and one
// disabled account, and a disabled account must not be polled (it has no live
// subscription) nor rendered. Rows without an api_token and rows of another
// site are excluded too. The Source variant (the one cmd/prism calls) must
// return the same roster.
func TestListClinePassAccountsFiltersDisabled(t *testing.T) {
	path := writeHubFixture(t, []metapiAccountFixture{
		{id: 34, siteID: 49, username: "Cline", status: "disabled", apiToken: sptr("tok-disabled")},
		{id: 38, siteID: 49, username: "Cline2", status: "active", apiToken: sptr("tok-active-2")},
		{id: 39, siteID: 49, username: "no-token", status: "active", apiToken: nil},
		{id: 40, siteID: 9, username: "other-site", status: "active", apiToken: sptr("tok-other-site")},
		{id: 41, siteID: 49, username: "Cline3", status: "disabled", apiToken: sptr("tok-disabled-3")},
	}, nil)

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	got, err := st.ListClinePassAccountsWithToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("roster = %+v, want exactly the one active site-49 account", got)
	}
	if got[0].AccountID != 38 || got[0].Username != "Cline2" || got[0].APIToken != "tok-active-2" {
		t.Fatalf("roster entry = %+v, want account 38 Cline2", got[0])
	}

	// The service-side listing (Source) must agree: ONE implementation decides
	// which accounts exist, so CLI and service cannot drift.
	src := NewSource(path)
	fromSrc, err := src.ListClinePassAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromSrc) != 1 || fromSrc[0].AccountID != 38 || fromSrc[0].APIToken != "tok-active-2" {
		t.Fatalf("source roster = %+v, want the same single active account", fromSrc)
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
