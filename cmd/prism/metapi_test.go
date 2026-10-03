package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/metapiusage"
	"github.com/dorokuma/prism/internal/planusage"
	"github.com/dorokuma/prism/internal/render"
	_ "modernc.org/sqlite"
)

// metapiRow is one proxy_logs row in the CLI test fixture. total is the
// stored total_tokens; invalid = SQL NULL (exercises the
// prompt+completion fallback). accountID is the metapi account the row
// belongs to; invalid = SQL NULL.
type metapiRow struct {
	accountID  sql.NullInt64
	model      string
	at         time.Time
	prompt     int64
	completion int64
	total      sql.NullInt64
}

// metapiAccountRow is one accounts row (site_id 49 = ClinePass). status is
// metapi's own enum; the production site 49 holds one active and one disabled
// account, and only the active ones are polled and rendered.
type metapiAccountRow struct {
	id       int64
	username string
	status   string
	apiToken string
}

// createMetapiHubFixture creates the two metapi tables prism reads: proxy_logs
// (with the account_id column the per-account sums isolate on) and accounts
// (the ClinePass roster).
func createMetapiHubFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE proxy_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER,
		model_requested TEXT,
		prompt_tokens INTEGER,
		completion_tokens INTEGER,
		total_tokens INTEGER,
		created_at TEXT DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatal(err)
	}
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

// insertMetapiRows writes the fixture rows into proxy_logs.
func insertMetapiRows(t *testing.T, db *sql.DB, rows []metapiRow) {
	t.Helper()
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO proxy_logs (account_id, model_requested, prompt_tokens, completion_tokens, total_tokens, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			r.accountID, r.model, r.prompt, r.completion, r.total,
			r.at.UTC().Format("2006-01-02 15:04:05"),
		); err != nil {
			t.Fatal(err)
		}
	}
}

// seedMetapiDB writes a minimal metapi hub database (the proxy_logs subset
// the estimate reads) and returns its path.
func seedMetapiDB(t *testing.T, rows []metapiRow) string {
	t.Helper()
	return seedMetapiAccountsDB(t, nil, rows)
}

// seedMetapiAccountsDB writes a metapi hub fixture with an accounts roster
// (site 49) and its proxy_logs rows, and returns the database path.
func seedMetapiAccountsDB(t *testing.T, accounts []metapiAccountRow, rows []metapiRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createMetapiHubFixture(t, db)
	for _, a := range accounts {
		if _, err := db.Exec(
			`INSERT INTO accounts (id, site_id, username, status, api_token) VALUES (?, 49, ?, ?, ?)`,
			a.id, a.username, a.status, a.apiToken,
		); err != nil {
			t.Fatal(err)
		}
	}
	insertMetapiRows(t, db, rows)
	return path
}

// accountID is the valid SQL account_id of one fixture proxy_logs row.
func accountID(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: true} }

func nullableInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// TestApplyQuotaClinePassEstimatePerAccount pins the accountID > 0 branch of
// the CLI estimate: with an account_id the sum is scoped to that account, so a
// second account's ClinePass traffic in the same window must not inflate the
// first account's 总额. It also pins the accountID <= 0 degrade: with no
// metapi account_id at all there is NO estimate — a subscription-wide sum would
// add one independent pool's traffic to the other's percent and print a 总额
// that belongs to neither (串账).
func TestApplyQuotaClinePassEstimatePerAccount(t *testing.T) {
	now := time.Now()
	path := seedMetapiAccountsDB(t, nil, []metapiRow{
		{accountID: accountID(38), model: "cline-pass/x", at: now.Add(-30 * time.Minute), prompt: 4_200, completion: 0, total: nullableInt(4_200)},
		{accountID: accountID(40), model: "cline-pass/x", at: now.Add(-30 * time.Minute), prompt: 7_000, completion: 0, total: nullableInt(7_000)},
	})
	prev := metapiUsageDBPath
	metapiUsageDBPath = path
	t.Cleanup(func() { metapiUsageDBPath = prev })

	start, end := now.Add(-time.Hour), now.Add(4*time.Hour)
	// A fresh snapshot per call: the estimator writes into the Window SLICE,
	// which the snapshot value shares with its caller.
	snapshot := func() planusage.Snapshot {
		return planusage.Snapshot{Provider: "clinepass", Windows: []planusage.Window{
			{Name: "weekly", Status: "ok", Percent: 34, PeriodStart: &start, ResetsAt: &end},
		}}
	}

	// Baseline of the shared skip counter: this test asserts DELTAS, so a
	// counter left over from another test cannot make it pass or fail.
	skippedBefore := clinepassEstimateSkipped.Value()

	got := applyQuotaClinePassEstimate(context.Background(), snapshot(), 38)
	if got.Windows[0].LimitTokensEstimate != 12_353 {
		t.Fatalf("account 38 estimate = %d, want 12353 (its own 4200 tokens ÷ 34%%)", got.Windows[0].LimitTokensEstimate)
	}
	other := applyQuotaClinePassEstimate(context.Background(), snapshot(), 40)
	if other.Windows[0].LimitTokensEstimate != 20_588 {
		t.Fatalf("account 40 estimate = %d, want 20588 (its own 7000 tokens ÷ 34%%)", other.Windows[0].LimitTokensEstimate)
	}
	// A scoped account is NOT a skip: the counter must not move for either
	// accountID > 0 call above.
	if d := clinepassEstimateSkipped.Value() - skippedBefore; d != 0 {
		t.Fatalf("accountID > 0 must not bump the skipped counter: delta = %d, want 0", d)
	}

	// The accountID <= 0 degrade is COUNTED (expvar
	// clinepass_quota_estimate_skipped_total, published on /metrics) so an
	// account that stopped producing a 总额 is visible instead of merely
	// blank: this ONE call must move the counter by exactly 1.
	noneBefore := clinepassEstimateSkipped.Value()
	none := applyQuotaClinePassEstimate(context.Background(), snapshot(), 0)
	if none.Windows[0].LimitTokensEstimate != 0 {
		t.Fatalf("accountID 0 must yield NO estimate (two pools must not be summed), got %d",
			none.Windows[0].LimitTokensEstimate)
	}
	if d := clinepassEstimateSkipped.Value() - noneBefore; d != 1 {
		t.Fatalf("skipped counter delta = %d, want exactly +1", d)
	}
}

// TestCLIAssemblyCarriesAccountFingerprints pins F1 on the CLI shape: the
// ClinePass accounts come from the real metapi discovery
// (readClinePassAccounts), the snapshots are completed by the same
// buildQuotaSnapshot runQuotaWith uses, and the result is rendered by the
// shared RenderCards. Before the wiring, the CLI never stamped the snapshots'
// fingerprints: a production `prism quota` drew the bare · instead of a colour
// dot, an uncoloured account name and position-keyed rows, so "two same-name
// accounts are told apart by their dot" was unreachable outside the service.
//
// The upstream fetch is the one part not exercised here (tests do not call
// cline.bot): the snapshot a fetcher returns for one account is built directly
// and then run through the CLI assembly.
func TestCLIAssemblyCarriesAccountFingerprints(t *testing.T) {
	now := time.Now()
	// Two accounts whose DISPLAY name is the same (Cline2 drops its digit
	// suffix) and whose tokens map to different palette colours — the fixture
	// token choice is deliberate, the palette itself is only 6 wide (see the
	// leftover list).
	const tokA, tokB = "cline-token-alpha", "cline-token-bravo"
	dbPath := seedMetapiAccountsDB(t,
		[]metapiAccountRow{
			// The production site-49 shape: one disabled account, the rest
			// active. Only the active ones may be polled or rendered.
			{id: 34, username: "Cline", status: "disabled", apiToken: "tok-disabled-1"},
			{id: 38, username: "Cline", status: "active", apiToken: tokA},
			{id: 40, username: "Cline2", status: "active", apiToken: tokB},
		},
		[]metapiRow{
			{accountID: accountID(38), model: "cline-pass/x", at: now.Add(-30 * time.Minute), prompt: 4_200, completion: 0, total: nullableInt(4_200)},
			{accountID: accountID(40), model: "cline-pass/x", at: now.Add(-30 * time.Minute), prompt: 7_000, completion: 0, total: nullableInt(7_000)},
		})
	prev := metapiUsageDBPath
	metapiUsageDBPath = dbPath
	t.Cleanup(func() { metapiUsageDBPath = prev })

	ctx := context.Background()
	views, err := readClinePassAccounts(ctx, metapiusage.NewSource(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("active accounts = %d, want 2 (the disabled site-49 account must be dropped)", len(views))
	}

	groups := planusage.GroupByKey(views, planusage.DefaultFetchers())
	if len(groups) != 2 {
		t.Fatalf("key groups = %d, want one snapshot per account key", len(groups))
	}
	start, end := now.Add(-time.Hour), now.Add(4*time.Hour)
	cfg := &config.Config{}
	snaps := make([]planusage.Snapshot, 0, len(groups))
	for _, g := range groups {
		fetched := planusage.Snapshot{Provider: "clinepass", FetchedAt: now, Windows: []planusage.Window{
			{Name: "weekly", Status: "ok", Percent: 34, PeriodStart: &start, ResetsAt: &end},
		}}
		snaps = append(snaps, buildQuotaSnapshot(ctx, cfg, g, fetched, nil))
	}

	// The fingerprint is the KEY fingerprint (the same 口径 as the service
	// poller), it is never empty, and the 总额 of each snapshot counts only its
	// own account's rows (accountID > 0 branch).
	byFP := map[string]planusage.Snapshot{}
	for _, s := range snaps {
		fps := s.AccountFPs()
		if len(fps) != 1 || len(s.Accounts) != 1 {
			t.Fatalf("snapshot must carry one account name and one fingerprint: %+v", s)
		}
		if fps[0] != planusage.KeyFingerprint(tokA) && fps[0] != planusage.KeyFingerprint(tokB) {
			t.Fatalf("snapshot fingerprint %q is not the api_token fingerprint", fps[0])
		}
		byFP[fps[0]] = s
	}
	if len(byFP) != 2 {
		t.Fatalf("fingerprints = %d, want 2 distinct", len(byFP))
	}
	for tok, want := range map[string]int64{tokA: 12_353, tokB: 20_588} {
		s, ok := byFP[planusage.KeyFingerprint(tok)]
		if !ok {
			t.Fatalf("no snapshot for the account keyed by %s", tok)
		}
		if got := s.Windows[0].LimitTokensEstimate; got != want {
			t.Fatalf("account %s estimate = %d, want %d (its own rows only)", tok, got, want)
		}
	}

	colored := planusage.RenderCards(snaps, now)
	plain := planusage.RenderCards(snaps, now, planusage.CardOptions{NoColor: true})
	if n := strings.Count(plain, "╭─ "); n != 1 {
		t.Fatalf("want ONE merged card, got %d:\n%s", n, plain)
	}
	var rows []string
	for _, line := range strings.Split(strings.TrimSuffix(colored, "\n"), "\n") {
		if strings.Contains(render.StripANSI(line), "│ · Cline ") {
			rows = append(rows, line)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("want two same-name rows (Cline / Cline2 both display \"Cline\"):\n%s", render.StripANSI(colored))
	}
	colors := map[string]bool{}
	for _, r := range rows {
		dot, name := sgrBefore(t, r, "·"), sgrBefore(t, r, "Cline")
		if dot == "" {
			t.Fatalf("the dot must be coloured, not the bare ·: %q", r)
		}
		if dot != name {
			t.Fatalf("the dot and the account name must share one colour: dot=%q name=%q\n%q", dot, name, r)
		}
		colors[dot] = true
	}
	if len(colors) != 2 {
		t.Fatalf("the two same-name rows must carry different colours:\n%s", render.StripANSI(colored))
	}
	if plain != render.StripANSI(colored) {
		t.Fatalf("the no-colour render must be the coloured one minus the escapes")
	}

	// Row identity is the fingerprint, not the position: the same assembled
	// snapshots delivered twice (a roster repeated across snapshots) still
	// render exactly two rows, where position-keyed rows would double to four.
	dup := append(append([]planusage.Snapshot(nil), snaps...), snaps...)
	if n := strings.Count(planusage.RenderCards(dup, now, planusage.CardOptions{NoColor: true}), "│ · Cline"); n != 2 {
		t.Fatalf("repeated roster = %d rows, want 2 (rows must be keyed by the fingerprint)", n)
	}
}

// sgrBefore returns the ANSI SGR sequence emitted immediately before the first
// occurrence of sub in s, or "" when sub is not colour-wrapped.
func sgrBefore(t *testing.T, s, sub string) string {
	t.Helper()
	i := strings.Index(s, sub)
	if i < 0 {
		t.Fatalf("%q not found in %q", sub, s)
	}
	j := strings.LastIndex(s[:i], "\x1b[")
	if j < 0 {
		return ""
	}
	end := strings.Index(s[j:], "m")
	if end < 0 {
		return ""
	}
	return s[j : j+end+1]
}

// stubQuotaView is a minimal planusage.AccountView for the roster rules.
type stubQuotaView struct{ name, provider string }

func (v stubQuotaView) Name() string         { return v.name }
func (v stubQuotaView) Provider() string     { return v.provider }
func (v stubQuotaView) BaseURL() string      { return "" }
func (v stubQuotaView) Key() string          { return "key-" + v.name }
func (v stubQuotaView) AuthHeader() string   { return "" }
func (v stubQuotaView) Client() *http.Client { return nil }

// TestNextQuotaViews pins the SIGHUP re-discovery rule: a successful discovery
// always wins — including an EMPTY one, which is how a disabled or removed
// account actually leaves the roster — while a FAILED discovery keeps the
// previous metapi-backed views, so a transient metapi error cannot make the
// whole ClinePass block silently disappear from the cards. Non-clinepass views
// are never carried over from prev: they are rebuilt from the pool every round
// (and must not be duplicated).
func TestNextQuotaViews(t *testing.T) {
	cfgViews := []planusage.AccountView{stubQuotaView{name: "gemini-1", provider: "gemini"}}
	prev := []planusage.AccountView{
		stubQuotaView{name: "gemini-1", provider: "gemini"},
		stubQuotaView{name: "Cline", provider: "clinepass"},
		stubQuotaView{name: "Cline2", provider: "clinepass"},
	}
	discovered := []planusage.AccountView{
		stubQuotaView{name: "Cline2", provider: "clinepass"},
		stubQuotaView{name: "Cline3", provider: "clinepass"},
	}

	cases := []struct {
		name       string
		discovered []planusage.AccountView
		err        error
		want       []string
	}{
		{
			name:       "successful discovery replaces the roster",
			discovered: discovered,
			want:       []string{"gemini-1", "Cline2", "Cline3"},
		},
		{
			name: "empty discovery removes the accounts",
			want: []string{"gemini-1"},
		},
		{
			name: "failed discovery keeps the previous clinepass views",
			err:  errors.New("metapi down"),
			want: []string{"gemini-1", "Cline", "Cline2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextQuotaViews(cfgViews, tc.discovered, prev, tc.err)
			var names []string
			for _, a := range got {
				names = append(names, a.Name())
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("roster = %v, want %v", names, tc.want)
			}
		})
	}
}

// TestClinePassRosterDelta pins the shrink-to-zero signal added for the three
// silent-shrink paths (O8/O9): a round that leaves an empty ClinePass roster
// again although the previous round had accounts must produce a reason (the
// WARN + clinepass_quota_roster_drops_total counter), and the count is the
// size of the new roster for clinepass_quota_accounts.
func TestClinePassRosterDelta(t *testing.T) {
	generic := []planusage.AccountView{stubQuotaView{name: "gemini-1", provider: "gemini"}}
	prev := []planusage.AccountView{
		stubQuotaView{name: "gemini-1", provider: "gemini"},
		stubQuotaView{name: "Cline", provider: "clinepass"},
		stubQuotaView{name: "Cline2", provider: "clinepass"},
	}
	cases := []struct {
		name       string
		prev, next []planusage.AccountView
		dbPresent  bool
		err        error
		wantCount  int
		wantReason string
	}{
		{
			name: "database file absent drops the block", prev: prev, next: generic,
			dbPresent: false, wantCount: 0, wantReason: "metapi database absent",
		},
		{
			// The O9 shapes (status third state/case change, NULL or empty
			// api_token row skipped, site_id rebuild) all surface as a
			// successful discovery that returned nothing.
			name: "successful discovery without accounts drops the block", prev: prev, next: generic,
			dbPresent: true, wantCount: 0, wantReason: "no clinepass accounts discovered",
		},
		{
			name: "failed discovery keeps the previous views and stays quiet", prev: prev, next: prev,
			dbPresent: true, err: errors.New("metapi down"), wantCount: 2, wantReason: "",
		},
		{
			// Nothing was removed: an already empty roster must not warn.
			name: "empty to empty stays quiet", prev: generic, next: generic,
			dbPresent: false, wantCount: 0, wantReason: "",
		},
		{
			name: "shrinking to a non-zero size stays quiet", prev: prev,
			next:      []planusage.AccountView{stubQuotaView{name: "Cline2", provider: "clinepass"}},
			dbPresent: true, wantCount: 1, wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, reason := clinePassRosterDelta(tc.prev, tc.next, tc.dbPresent, tc.err)
			if count != tc.wantCount || reason != tc.wantReason {
				t.Fatalf("delta = (%d, %q), want (%d, %q)", count, reason, tc.wantCount, tc.wantReason)
			}
		})
	}
}

// TestApplyQuotaClinePassEstimate pins the CLI path of `prism quota`: the
// ClinePass POOL windows (weekly / monthly) get ONE weekly-anchored 总额 from
// metapi's cline-pass consumption of THAT account — monthly is exactly twice
// the weekly pool — the 5-hour window gets none, and the result is identical
// to the shared planusage.ApplyClinePassEstimates the service poller uses
// (CLI and /admin/quota cannot drift).
func TestApplyQuotaClinePassEstimate(t *testing.T) {
	now := time.Now()
	end5h := now.Add(2 * time.Hour)
	start5h := end5h.Add(-5 * time.Hour)
	endW := now.Add(72 * time.Hour)
	startW := endW.Add(-7 * 24 * time.Hour)
	endM := now.Add(10 * 24 * time.Hour)
	startM := endM.AddDate(0, -1, 0)

	dbPath := seedMetapiDB(t, []metapiRow{
		// In the 5h, weekly AND monthly window: 20000 tokens (a NULL total
		// exercises the prompt+completion molecule).
		{accountID: accountID(34), model: "cline-pass/deepseek-v4.1-flash", at: now.Add(-time.Hour), prompt: 12000, completion: 8000, total: sql.NullInt64{}},
		// In the weekly and monthly window: 30000 more.
		{accountID: accountID(34), model: "cline-pass/kimi-k3", at: now.Add(-48 * time.Hour), prompt: 18000, completion: 12000, total: nullableInt(30000)},
		// Older than the weekly window, still in the monthly one: 40000 more.
		{accountID: accountID(34), model: "cline-pass/kimi-k3", at: now.Add(-10 * 24 * time.Hour), prompt: 25000, completion: 15000, total: nullableInt(40000)},
		// Ignored: another provider, another ACCOUNT (its pool is its own),
		// and a cline-pass row older than the monthly window.
		{model: "gpt-5", at: now.Add(-time.Hour), total: nullableInt(9999999)},
		{accountID: accountID(38), model: "cline-pass/x", at: now.Add(-time.Hour), total: nullableInt(9999999)},
		{accountID: accountID(34), model: "cline-pass/deepseek-v4.1-flash", at: now.Add(-40 * 24 * time.Hour), total: nullableInt(9999999)},
	})

	prev := metapiUsageDBPath
	metapiUsageDBPath = dbPath
	t.Cleanup(func() { metapiUsageDBPath = prev })

	snap := planusage.Snapshot{Provider: "clinepass", Accounts: []string{"Cline"}, Windows: []planusage.Window{
		{Name: "5h", Status: "ok", Percent: 10, PeriodStart: &start5h, ResetsAt: &end5h},
		{Name: "weekly", Status: "ok", Percent: 50, PeriodStart: &startW, ResetsAt: &endW},
		{Name: "monthly", Status: "ok", Percent: 5, PeriodStart: &startM, ResetsAt: &endM},
	}}
	got := applyQuotaClinePassEstimate(context.Background(), snap, 34)
	// The weekly molecule is 20000+30000 = 50000 at 50 % → L = 100000, and the
	// monthly window is 2L whatever its own 5 % would have reversed to.
	want := map[string]int64{"5h": 0, "weekly": 100_000, "monthly": 200_000}
	for _, w := range got.Windows {
		if w.LimitTokensEstimate != want[w.Name] {
			t.Fatalf("%s estimate = %d, want %d", w.Name, w.LimitTokensEstimate, want[w.Name])
		}
	}

	// The service-side path over the same fixture must produce identical
	// numbers: both run planusage.ApplyClinePassEstimates.
	st, err := metapiusage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	direct := planusage.ApplyClinePassEstimates(context.Background(), snap, func(c context.Context, from, to int64) (int64, error) {
		return st.SumClinePassTokensByAccount(c, 34, from, to)
	}, now)
	for i := range got.Windows {
		if got.Windows[i].LimitTokensEstimate != direct.Windows[i].LimitTokensEstimate {
			t.Fatalf("CLI %s = %d, service path = %d",
				got.Windows[i].Name, got.Windows[i].LimitTokensEstimate, direct.Windows[i].LimitTokensEstimate)
		}
	}

	// The CLI renders the totals: the derived pool is written like any other
	// total (no "~" marker, no 估算池 title segment), and the 5-hour row
	// carries the countdown instead of a pair.
	cards := planusage.RenderCards([]planusage.Snapshot{got}, now, planusage.CardOptions{NoColor: true})
	for _, wantText := range []string{
		"50.0K/100.0K",
		"10.0K/200.0K",
		"2h 后重置",
	} {
		if !strings.Contains(cards, wantText) {
			t.Fatalf("cards missing %q:\n%s", wantText, cards)
		}
	}
}

// TestApplyQuotaClinePassEstimateMissingDB pins the degradation: a missing
// metapi database leaves every window without a 总额 and does not touch the
// snapshot (no error, no state change).
func TestApplyQuotaClinePassEstimateMissingDB(t *testing.T) {
	prev := metapiUsageDBPath
	metapiUsageDBPath = filepath.Join(t.TempDir(), "missing", "hub.db")
	t.Cleanup(func() { metapiUsageDBPath = prev })

	start := time.Now().Add(-time.Hour)
	end := start.Add(5 * time.Hour)
	snap := planusage.Snapshot{Provider: "clinepass", Windows: []planusage.Window{
		{Name: "5h", Status: "ok", Percent: 7, PeriodStart: &start, ResetsAt: &end},
	}}
	// A missing metapi database leaves every window without a 总额 and does
	// not touch the snapshot (no error, no state change). The account id is
	// the real one: the accountID <= 0 path is a DIFFERENT degrade and is
	// pinned by TestApplyQuotaClinePassEstimatePerAccount.
	got := applyQuotaClinePassEstimate(context.Background(), snap, 34)
	if got.Windows[0].LimitTokensEstimate != 0 || got.Err != "" {
		t.Fatalf("missing db must leave the snapshot estimate empty: %+v", got)
	}
}

// TestApplyQuotaClinePassEstimateCreatedAtDrift pins the M11 degrade end
// to end on the CLI path: metapi switching created_at away from the
// expected UTC `YYYY-MM-DD HH:MM:SS` text makes the sums fail, so the CLI
// leaves the windows without a 总额 and never touches the snapshot. The
// fixture timestamps are literals (RFC3339), deliberately not written
// through the production layout; they sit on dates where a MISSING shape
// guard would have text-matched the old-format window bounds and produced
// a wrong number instead of an error.
func TestApplyQuotaClinePassEstimateCreatedAtDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE proxy_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER,
		model_requested TEXT,
		prompt_tokens INTEGER,
		completion_tokens INTEGER,
		total_tokens INTEGER,
		created_at TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, at := range []time.Time{now.Add(-20 * 24 * time.Hour), now.Add(-10 * 24 * time.Hour)} {
		if _, err := db.Exec(
			`INSERT INTO proxy_logs (account_id, model_requested, prompt_tokens, total_tokens, created_at) VALUES (34, 'cline-pass/x', 100, 100, ?)`,
			at.UTC().Format("2006-01-02T15:04:05Z"),
		); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	prev := metapiUsageDBPath
	metapiUsageDBPath = path
	t.Cleanup(func() { metapiUsageDBPath = prev })

	start := now.AddDate(0, -1, 0)
	end := now.Add(10 * 24 * time.Hour)
	snap := planusage.Snapshot{Provider: "clinepass", Windows: []planusage.Window{
		{Name: "monthly", Status: "ok", Percent: 10, PeriodStart: &start, ResetsAt: &end},
	}}
	got := applyQuotaClinePassEstimate(context.Background(), snap, 34)
	if got.Windows[0].LimitTokensEstimate != 0 {
		t.Fatalf("drifted created_at must not produce a 总额: %+v", got.Windows[0])
	}
	if got.Err != "" {
		t.Fatalf("drift must not touch Snapshot.Err: %+v", got)
	}
	if got.Windows[0].Percent != 10 || got.Windows[0].Status != "ok" {
		t.Fatalf("drift must not touch the snapshot window: %+v", got.Windows[0])
	}
}

// TestApplyQuotaClinePassEstimateBrokenDB pins the table-missing degrade
// (the database file exists but has no proxy_logs): the estimate stays
// empty; the quota fetch itself is unaffected.
func TestApplyQuotaClinePassEstimateBrokenDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	prev := metapiUsageDBPath
	metapiUsageDBPath = path
	t.Cleanup(func() { metapiUsageDBPath = prev })

	start := time.Now().Add(-time.Hour)
	end := start.Add(5 * time.Hour)
	snap := planusage.Snapshot{Provider: "clinepass", Windows: []planusage.Window{
		{Name: "5h", Status: "ok", Percent: 7, PeriodStart: &start, ResetsAt: &end},
	}}
	got := applyQuotaClinePassEstimate(context.Background(), snap, 34)
	if got.Windows[0].LimitTokensEstimate != 0 || got.Err != "" {
		t.Fatalf("broken db must leave the snapshot estimate empty: %+v", got)
	}
}
