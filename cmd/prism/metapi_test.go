package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/metapiusage"
	"github.com/dorokuma/prism/internal/planusage"
	_ "modernc.org/sqlite"
)

// metapiRow is one proxy_logs row in the CLI test fixture. total is the
// stored total_tokens; invalid = SQL NULL (exercises the
// prompt+completion fallback).
type metapiRow struct {
	model      string
	at         time.Time
	prompt     int64
	completion int64
	total      sql.NullInt64
}

// seedMetapiDB writes a minimal metapi hub database (the proxy_logs subset
// the estimate reads) and returns its path.
func seedMetapiDB(t *testing.T, rows []metapiRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO proxy_logs (model_requested, prompt_tokens, completion_tokens, total_tokens, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			r.model, r.prompt, r.completion, r.total,
			r.at.UTC().Format("2006-01-02 15:04:05"),
		); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func nullableInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// TestApplyQuotaClinePassEstimate pins the CLI path of `prism quota`: the
// three ClinePass windows get their 总额 from metapi's cline-pass
// consumption, and the result is identical to the shared
// planusage.ApplyClinePassEstimates the service poller uses (CLI and
// /admin/quota cannot drift).
func TestApplyQuotaClinePassEstimate(t *testing.T) {
	now := time.Now()
	end5h := now.Add(2 * time.Hour)
	start5h := end5h.Add(-5 * time.Hour)
	endW := now.Add(72 * time.Hour)
	startW := endW.Add(-7 * 24 * time.Hour)
	endM := now.Add(10 * 24 * time.Hour)
	startM := endM.AddDate(0, -1, 0)

	dbPath := seedMetapiDB(t, []metapiRow{
		// 5h window: 20000 tokens at 10 % → 200000 (NULL total exercises
		// the prompt+completion fallback).
		{model: "cline-pass/deepseek-v4.1-flash", at: now.Add(-time.Hour), prompt: 12000, completion: 8000, total: sql.NullInt64{}},
		// weekly-only: 30000 more at 50 % → (20000+30000)/0.5 = 100000.
		{model: "cline-pass/kimi-k3", at: now.Add(-48 * time.Hour), prompt: 18000, completion: 12000, total: nullableInt(30000)},
		// monthly-only: 40000 more at 5 % → (50000+40000)/0.05 = 1.8M.
		{model: "cline-pass/kimi-k3", at: now.Add(-10 * 24 * time.Hour), prompt: 25000, completion: 15000, total: nullableInt(40000)},
		// ignored: another provider, and a cline-pass row older than the
		// monthly window.
		{model: "gpt-5", at: now.Add(-time.Hour), total: nullableInt(9999999)},
		{model: "cline-pass/deepseek-v4.1-flash", at: now.Add(-40 * 24 * time.Hour), total: nullableInt(9999999)},
	})

	prev := metapiUsageDBPath
	metapiUsageDBPath = dbPath
	t.Cleanup(func() { metapiUsageDBPath = prev })

	snap := planusage.Snapshot{Provider: "clinepass", Accounts: []string{"ClinePass"}, Windows: []planusage.Window{
		{Name: "5h", Status: "ok", Percent: 10, PeriodStart: &start5h, ResetsAt: &end5h},
		{Name: "weekly", Status: "ok", Percent: 50, PeriodStart: &startW, ResetsAt: &endW},
		{Name: "monthly", Status: "ok", Percent: 5, PeriodStart: &startM, ResetsAt: &endM},
	}}
	got := applyQuotaClinePassEstimate(context.Background(), snap)
	want := map[string]int64{"5h": 200_000, "weekly": 100_000, "monthly": 1_800_000}
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
	direct := planusage.ApplyClinePassEstimates(context.Background(), snap, st.SumClinePassTokens, now)
	for i := range got.Windows {
		if got.Windows[i].LimitTokensEstimate != direct.Windows[i].LimitTokensEstimate {
			t.Fatalf("CLI %s = %d, service path = %d",
				got.Windows[i].Name, got.Windows[i].LimitTokensEstimate, direct.Windows[i].LimitTokensEstimate)
		}
	}

	// The CLI renders the totals; the card text is the shared renderer.
	cards := planusage.RenderCards([]planusage.Snapshot{got}, now, planusage.CardOptions{NoColor: true})
	for _, wantText := range []string{
		"已用 10% / 总额 200k 词元",
		"已用 50% / 总额 100k 词元",
		"已用 5% / 总额 1.8M 词元",
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
	got := applyQuotaClinePassEstimate(context.Background(), snap)
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
			`INSERT INTO proxy_logs (model_requested, total_tokens, created_at) VALUES ('cline-pass/x', 100, ?)`,
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
	got := applyQuotaClinePassEstimate(context.Background(), snap)
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
	got := applyQuotaClinePassEstimate(context.Background(), snap)
	if got.Windows[0].LimitTokensEstimate != 0 || got.Err != "" {
		t.Fatalf("broken db must leave the snapshot estimate empty: %+v", got)
	}
}
