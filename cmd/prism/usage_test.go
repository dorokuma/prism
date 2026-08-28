package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/usage"
)

func TestMain(m *testing.M) {
	// Keep CLI tests off the production estimate file so a live SuperGrok
	// period_start cannot empty March-dated fixtures or shift default From.
	grokEstimatePath = filepath.Join(os.TempDir(), "prism-cli-usage-test-no-est.json")
	os.Exit(m.Run())
}

// seedUsageDB creates a usage database with deterministic events around
// base: "alpha" (priced, 2 requests), "beta" (missing price, 1 request) and
// "gamma" (priced, 2 requests), one of which failed. Returns the path.
func seedUsageDB(t *testing.T, base time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	s := usage.NewSQLiteStore(path)
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	price := &usage.Price{Input: 1000, Output: 1000}
	ev := func(id, model string, ts time.Time, success bool, total int64) usage.Event {
		cost, status := usage.ComputeCost(100, 50, 0, 0, "", price)
		if model == "beta" {
			cost, status = nil, usage.CostStatusMissingPrice
		}
		return usage.Event{
			Ts: ts, RequestID: id, Model: model, Provider: "p", Account: "a", KeyID: "k",
			Stream: true, Success: success, Status: 200,
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: total,
			Cost: cost, CostStatus: status,
		}
	}
	events := []usage.Event{
		ev("r1", "alpha", base.Add(-2*time.Hour), true, 150),
		ev("r2", "alpha", base.Add(-3*time.Hour), true, 150),
		ev("r3", "beta", base.Add(-4*time.Hour), true, 150),
		ev("r4", "gamma", base.Add(-5*time.Hour), true, 150),
		ev("r5", "gamma", base.Add(-6*time.Hour), false, 150),
	}
	if err := s.InsertBatch(ctx, events); err != nil {
		t.Fatal(err)
	}
	s.Close()
	return path
}

func TestParseTimeArg(t *testing.T) {
	loc := time.Local
	at := func(y int, m time.Month, d, h, mi, s int) time.Time {
		return time.Date(y, m, d, h, mi, s, 0, loc)
	}
	now := at(2026, 3, 3, 15, 4, 5)
	cases := []struct {
		in   string
		want time.Time
	}{
		// relative forms, cross-month and cross-year (AddDate calendar math)
		{"7d", at(2026, 2, 24, 15, 4, 5)}, // 3月3日 - 7d = 2月24日
		{"1d", at(2026, 3, 2, 15, 4, 5)},
		{"30d", at(2026, 2, 1, 15, 4, 5)},
		{"24h", at(2026, 3, 2, 15, 4, 5)},
		{"30m", at(2026, 3, 3, 14, 34, 5)},
		{"90s", at(2026, 3, 3, 15, 2, 35)},
		// month-day: future date in the current year falls back to last year
		{"08-01", at(2025, 8, 1, 0, 0, 0)},
		// full date
		{"2026-08-01", at(2026, 8, 1, 0, 0, 0)},
	}
	for _, c := range cases {
		got, err := parseTimeArg(c.in, now)
		if err != nil {
			t.Errorf("parseTimeArg(%q): %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("parseTimeArg(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	// 30d in January crosses into December of the previous year
	jan := at(2026, 1, 15, 10, 0, 0)
	if got, _ := parseTimeArg("30d", jan); !got.Equal(at(2025, 12, 16, 10, 0, 0)) {
		t.Errorf("30d from January = %v, want 2025-12-16", got)
	}
	// leap year: 2028-03-03 minus 7 days = 2028-02-25 (February has 29 days)
	leap := at(2028, 3, 3, 0, 0, 0)
	if got, _ := parseTimeArg("7d", leap); !got.Equal(at(2028, 2, 25, 0, 0, 0)) {
		t.Errorf("7d from 2028-03-03 = %v, want 2028-02-25", got)
	}
	// month-day already past in the current year stays in the current year
	aug := at(2026, 8, 10, 12, 0, 0)
	if got, _ := parseTimeArg("08-01", aug); !got.Equal(at(2026, 8, 1, 0, 0, 0)) {
		t.Errorf("08-01 from August = %v, want 2026-08-01", got)
	}

	for _, bad := range []string{"banana", "7", "08/01", "-7d", "0x10h", "08-1", "2026-8-1", ""} {
		if _, err := parseTimeArg(bad, now); err == nil {
			t.Errorf("parseTimeArg(%q) must fail", bad)
		}
	}
}

func TestParseUsageArgsPresets(t *testing.T) {
	now := time.Now()
	// bare `prism usage` → SuperGrok week (7d fallback with no estimate file)
	o, q, _, _, err := parseUsageArgs(nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.GroupBy) != 1 || q.GroupBy[0] != "model" {
		t.Errorf("default group_by = %v, want [model]", q.GroupBy)
	}
	wantFrom := now.Add(-7 * 24 * time.Hour).Unix()
	if q.From != wantFrom {
		t.Errorf("default from must be week start (7d fallback), got %d want %d", q.From, wantFrom)
	}
	if !o.weekDefault {
		t.Error("bare usage must mark weekDefault")
	}
	if q.To != now.Unix() {
		t.Errorf("default to must be now, got %d", q.To)
	}

	presets := []struct {
		preset string
		want   []string
	}{
		{"models", []string{"model"}},
		{"keys", []string{"key_id"}},
		{"accounts", []string{"account"}},
		{"providers", []string{"provider"}},
		{"days", []string{"day"}},
		{"hours", []string{"hour"}},
		{"errors", []string{"model"}},
	}
	for _, p := range presets {
		_, q, _, _, err := parseUsageArgs([]string{p.preset}, now)
		if err != nil {
			t.Fatalf("preset %q: %v", p.preset, err)
		}
		if strings.Join(q.GroupBy, ",") != strings.Join(p.want, ",") {
			t.Errorf("preset %q group_by = %v, want %v", p.preset, q.GroupBy, p.want)
		}
	}
	// errors preset forces success=false
	_, q, _, _, _ = parseUsageArgs([]string{"errors"}, now)
	if q.Success == nil || *q.Success {
		t.Errorf("errors preset must filter success=false, got %v", q.Success)
	}
	// default preset does not filter success
	_, q, _, _, _ = parseUsageArgs(nil, now)
	if q.Success != nil {
		t.Errorf("default must not filter success, got %v", *q.Success)
	}
	// --by overrides the preset group_by; --failed forces success=false
	_, q, _, _, err = parseUsageArgs([]string{"keys", "--by", "day,hour", "--failed", "--limit", "50"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(q.GroupBy, ",") != "day,hour" {
		t.Errorf("--by group_by = %v, want [day hour]", q.GroupBy)
	}
	if q.Success == nil || *q.Success {
		t.Errorf("--failed must force success=false")
	}
	if q.Limit != 50 {
		t.Errorf("limit = %d, want 50", q.Limit)
	}
	// flags may appear before the preset? No: preset must come first
	if _, _, _, _, err := parseUsageArgs([]string{"--since", "7d", "models"}, now); err == nil {
		t.Error("preset after flags must be rejected with a hint")
	}
}

func TestParseUsageArgsWeekFromEstimate(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "est.json")
	data := fmt.Sprintf(`{"period_start":%q,"live_tokens":1,"live_percent":1,"live_estimate":1,"display_estimate":1}`, start.UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := grokEstimatePath
	grokEstimatePath = path
	t.Cleanup(func() { grokEstimatePath = prev })

	o, q, _, _, err := parseUsageArgs(nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if q.From != start.Unix() {
		t.Errorf("from = %d, want estimate period_start %d", q.From, start.Unix())
	}
	if !o.weekDefault {
		t.Error("want weekDefault")
	}
	_, q, _, _, err = parseUsageArgs([]string{"--since", "24h"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if q.From != now.Add(-24*time.Hour).Unix() {
		t.Errorf("--since must override week default, got %d", q.From)
	}
}

func TestParseUsageArgsValidation(t *testing.T) {
	now := time.Now()
	bad := [][]string{
		{"bogus"},                                    // unknown preset
		{"models", "--by", "model,nope"},             // unknown group_by
		{"models", "--by", ","},                      // empty --by
		{"models", "--limit", "0"},                   // limit below 1
		{"models", "--json", "--watch", "5s"},        // json + watch conflict
		{"models", "--watch", "abc"},                 // bad watch interval
		{"models", "--since", "6d", "--until", "7d"}, // since after until
		{"models", "--since", "banana"},              // bad time
		{"models", "--until", "2026-13-40"},          // bad date
	}
	for _, args := range bad {
		if _, _, _, _, err := parseUsageArgs(args, now); err == nil {
			t.Errorf("parseUsageArgs(%v) must fail", args)
		}
	}
	// limit above the package cap is clamped to 1000, like the HTTP path
	_, q, _, _, err := parseUsageArgs([]string{"models", "--limit", "2000"}, now)
	if err != nil || q.Limit != 1000 {
		t.Errorf("limit 2000 must clamp to 1000, got %d, %v", q.Limit, err)
	}
	// filters map to the query
	_, q, _, _, err = parseUsageArgs([]string{"models", "--model", "m1", "--key", "k1", "--account", "a1", "--provider", "p1", "--since", "2026-08-01", "--until", "08-10"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if q.Model != "m1" || q.KeyID != "k1" || q.Account != "a1" || q.Provider != "p1" {
		t.Errorf("filters not mapped: %+v", q)
	}
}

// writeUsageTestConfig writes a minimal valid prism config (one account with
// a key, so LoadConfig passes validation) into dir/config.yaml with the given
// usage db_path.
func writeUsageTestConfig(t *testing.T, dir, dbPath string) {
	t.Helper()
	cfg := fmt.Sprintf("accounts:\n  - name: test\n    base_url: https://api.example.com/v1\n    provider: p\n    key: sk-test\nusage:\n  enabled: true\n  db_path: %s\n", dbPath)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestUsageDBPathResolution covers the db_path lookup priority of
// `prism usage`: --db > cwd config.yaml > fixed fallback config
// (/var/lib/prism/config.yaml) > code default. The lookup locations are
// injected via usageConfigDir / usageConfigFallbackPath, so no test touches
// the real /var/lib/prism.
func TestUsageDBPathResolution(t *testing.T) {
	now := time.Now()
	// withLookup redirects the cwd lookup and the fallback config path for
	// the duration of the current subtest, and restores them afterwards.
	withLookup := func(t *testing.T, cwd, fallbackCfg string) {
		t.Helper()
		oldDir, oldFallback := usageConfigDir, usageConfigFallbackPath
		usageConfigDir = func() string { return cwd }
		usageConfigFallbackPath = fallbackCfg
		t.Cleanup(func() {
			usageConfigDir, usageConfigFallbackPath = oldDir, oldFallback
		})
	}

	t.Run("cwd has no config, fallback config exists", func(t *testing.T) {
		cwd := t.TempDir() // deliberately no config.yaml here
		fallbackDir := t.TempDir()
		fallbackDB := filepath.Join(fallbackDir, "fallback.db")
		fallbackCfg := filepath.Join(fallbackDir, "config.yaml")
		writeUsageTestConfig(t, fallbackDir, fallbackDB)
		withLookup(t, cwd, fallbackCfg)

		_, _, dbPath, source, err := parseUsageArgs(nil, now)
		if err != nil {
			t.Fatal(err)
		}
		if dbPath != fallbackDB {
			t.Errorf("dbPath = %q, want fallback config db_path %q", dbPath, fallbackDB)
		}
		if !strings.Contains(source, "回退配置") || !strings.Contains(source, fallbackCfg) {
			t.Errorf("source = %q, want it to name the fallback config %q", source, fallbackCfg)
		}
	})

	t.Run("cwd config wins over fallback config", func(t *testing.T) {
		cwd := t.TempDir()
		fallbackDir := t.TempDir()
		cwdDB := filepath.Join(cwd, "cwd.db")
		cwdCfg := filepath.Join(cwd, "config.yaml")
		writeUsageTestConfig(t, cwd, cwdDB)
		writeUsageTestConfig(t, fallbackDir, filepath.Join(fallbackDir, "fallback.db"))
		withLookup(t, cwd, filepath.Join(fallbackDir, "config.yaml"))

		_, _, dbPath, source, err := parseUsageArgs(nil, now)
		if err != nil {
			t.Fatal(err)
		}
		if dbPath != cwdDB {
			t.Errorf("dbPath = %q, want cwd config db_path %q (old behavior must not change)", dbPath, cwdDB)
		}
		if !strings.Contains(source, "当前目录配置") || !strings.Contains(source, cwdCfg) {
			t.Errorf("source = %q, want it to name the cwd config %q", source, cwdCfg)
		}
	})

	t.Run("neither config exists → code default", func(t *testing.T) {
		withLookup(t, t.TempDir(), filepath.Join(t.TempDir(), "config.yaml"))

		_, _, dbPath, source, err := parseUsageArgs(nil, now)
		if err != nil {
			t.Fatal(err)
		}
		if dbPath != defaultUsageDBPath {
			t.Errorf("dbPath = %q, want code default %q", dbPath, defaultUsageDBPath)
		}
		if !strings.Contains(source, "代码默认值") {
			t.Errorf("source = %q, want it to name the code default", source)
		}
	})

	t.Run("--db wins over both configs", func(t *testing.T) {
		cwd := t.TempDir()
		fallbackDir := t.TempDir()
		writeUsageTestConfig(t, cwd, filepath.Join(cwd, "cwd.db"))
		writeUsageTestConfig(t, fallbackDir, filepath.Join(fallbackDir, "fallback.db"))
		withLookup(t, cwd, filepath.Join(fallbackDir, "config.yaml"))
		explicit := filepath.Join(t.TempDir(), "explicit.db")

		_, _, dbPath, source, err := parseUsageArgs([]string{"--db", explicit}, now)
		if err != nil {
			t.Fatal(err)
		}
		if dbPath != explicit {
			t.Errorf("dbPath = %q, want --db path %q", dbPath, explicit)
		}
		if !strings.Contains(source, "--db") {
			t.Errorf("source = %q, want it to name the --db flag", source)
		}
	})

	t.Run("cwd config with no accounts falls through to fallback config", func(t *testing.T) {
		cwd := t.TempDir()
		fallbackDir := t.TempDir()
		fallbackDB := filepath.Join(fallbackDir, "fallback.db")
		// cwd config.yaml exists but LoadConfig rejects it (no accounts →
		// "no accounts configured"). The CLI only needs db_path, so it
		// must not die on a broken/unrelated same-named file in cwd: it
		// continues to the fallback config.
		if err := os.WriteFile(filepath.Join(cwd, "config.yaml"), []byte("usage:\n  enabled: true\n  db_path: /tmp/should-not-be-used.db\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeUsageTestConfig(t, fallbackDir, fallbackDB)
		withLookup(t, cwd, filepath.Join(fallbackDir, "config.yaml"))

		_, _, dbPath, source, err := parseUsageArgs(nil, now)
		if err != nil {
			t.Fatal(err)
		}
		if dbPath != fallbackDB {
			t.Errorf("dbPath = %q, want fallback db_path %q after cwd config failed to load", dbPath, fallbackDB)
		}
		if !strings.Contains(source, "回退配置") {
			t.Errorf("source = %q, want it to name the fallback config", source)
		}
	})

	t.Run("invalid yaml in cwd config falls through to fallback config", func(t *testing.T) {
		cwd := t.TempDir()
		fallbackDir := t.TempDir()
		fallbackDB := filepath.Join(fallbackDir, "fallback.db")
		if err := os.WriteFile(filepath.Join(cwd, "config.yaml"), []byte("accounts: [\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeUsageTestConfig(t, fallbackDir, fallbackDB)
		withLookup(t, cwd, filepath.Join(fallbackDir, "config.yaml"))

		_, _, dbPath, source, err := parseUsageArgs(nil, now)
		if err != nil {
			t.Fatal(err)
		}
		if dbPath != fallbackDB {
			t.Errorf("dbPath = %q, want fallback db_path %q after cwd config failed to parse", dbPath, fallbackDB)
		}
		if !strings.Contains(source, "回退配置") {
			t.Errorf("source = %q, want it to name the fallback config", source)
		}
	})
}

func TestRunUsageJSON(t *testing.T) {
	base := time.Date(2026, 3, 10, 15, 4, 5, 0, time.Local)
	path := seedUsageDB(t, base)
	var buf bytes.Buffer
	// default range: today → the seeded events (base-2h..base-6h) are in range
	err := runUsageWith([]string{"--db", path, "--json"}, &buf, base)
	if err != nil {
		t.Fatal(err)
	}
	// the output must be pure JSON: no table characters, no summary lines
	if !json.Valid(buf.Bytes()) {
		t.Fatalf("--json output is not valid JSON:\n%s", buf.String())
	}
	var doc struct {
		Period   string             `json:"period"`
		From     int64              `json:"from"`
		To       int64              `json:"to"`
		GroupBy  []string           `json:"group_by"`
		Overview *usage.Overview    `json:"overview"`
		Rows     []usage.SummaryRow `json:"rows"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Period != "本周" {
		t.Errorf("period = %q, want 本周", doc.Period)
	}
	// overview covers ALL 5 events (alpha 2 + beta 1 + gamma 2), including
	// the failed one
	if doc.Overview.Requests != 5 {
		t.Errorf("overview.requests = %d, want 5", doc.Overview.Requests)
	}
	if doc.Overview.FailedRequests != 1 || doc.Overview.CostMissingRequests != 1 {
		t.Errorf("overview failed/missing = %d/%d, want 1/1", doc.Overview.FailedRequests, doc.Overview.CostMissingRequests)
	}
	// rows grouped by model: alpha, beta, gamma
	if len(doc.Rows) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(doc.Rows), doc.Rows)
	}
	// the failed-only filter: errors preset → only the failed gamma event
	buf.Reset()
	err = runUsageWith([]string{"errors", "--db", path, "--json"}, &buf, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Overview.Requests != 1 || len(doc.Rows) != 1 || doc.Rows[0].Groups["model"] != "gamma" {
		t.Errorf("errors preset: %+v / %+v, want only the 1 failed gamma event", doc.Overview, doc.Rows)
	}
}

func TestRunUsageTable(t *testing.T) {
	base := time.Date(2026, 3, 10, 15, 4, 5, 0, time.Local)
	path := seedUsageDB(t, base)
	var buf bytes.Buffer
	// default range (today): events at base-2h..base-6h are within today
	err := runUsageWith([]string{"--db", path}, &buf, base)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// summary from Overview: all 5 requests, 750 total tokens, 4 priced
	// events × $0.15 (price 1000/1000 on 150 tokens each)
	if !strings.Contains(out, "本周  ·  5 请求  ·  750 词元  ·  $0.600") {
		t.Errorf("summary line missing/wrong:\n%s", out)
	}
	// the missing-price hint
	if !strings.Contains(out, "⚠ 有 1 个请求未算出金额") {
		t.Errorf("missing-price warning missing:\n%s", out)
	}
	// compact table: one header line carrying both token labels, with the
	// short 请求/缓存 headers (never the long 请求数/缓存命中)
	h := usageTableHeader(out)
	if h == "" {
		t.Fatalf("default output must be the compact single-line table:\n%s", out)
	}
	for _, want := range []string{"模型", "请求", "输入词元", "缓存", "命中率", "输出词元", "花费", "未计价"} {
		if !strings.Contains(h, want) {
			t.Errorf("table header missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "请求数") {
		t.Errorf("the long 请求数 header must be gone:\n%s", out)
	}
	// one row per model, each row carrying all its values; nil cost as dash
	rows := map[string][]string{
		"alpha": {"2", "200", "0.0%", "100", "$0.30"},
		"beta":  {"1", "100", "0.0%", "50", "-"},
		"gamma": {"2", "200", "0.0%", "100", "$0.30"},
	}
	for m, vals := range rows {
		lines := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, m) {
				lines++
				for _, v := range vals {
					if !strings.Contains(line, v) {
						t.Errorf("%s row missing %q: %q", m, v, line)
					}
				}
			}
		}
		if lines != 1 {
			t.Errorf("%s must appear exactly once, got %d lines", m, lines)
		}
	}
	// non-TTY output (a buffer) must never contain ANSI escapes
	if strings.Contains(out, "\x1b[") {
		t.Errorf("piped output must not be colored:\n%q", out)
	}
}

func TestRunUsageLimitAndFilters(t *testing.T) {
	base := time.Date(2026, 3, 10, 15, 4, 5, 0, time.Local)
	path := seedUsageDB(t, base)
	// --limit 2 → only the top-2 models in the table; --limit 2000 clamps
	for _, args := range [][]string{
		{"--db", path, "--limit", "2"},
		{"--db", path, "--limit", "2000"},
	} {
		var buf bytes.Buffer
		if err := runUsageWith(args, &buf, base); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	// model filter
	var buf bytes.Buffer
	if err := runUsageWith([]string{"--db", path, "--model", "alpha", "--json"}, &buf, base); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Overview *usage.Overview    `json:"overview"`
		Rows     []usage.SummaryRow `json:"rows"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Overview.Requests != 2 || len(doc.Rows) != 1 {
		t.Errorf("model filter: overview=%+v rows=%+v, want 2 requests / 1 row", doc.Overview, doc.Rows)
	}
}

func TestRunUsageMissingDBFriendlyError(t *testing.T) {
	var buf bytes.Buffer
	missing := filepath.Join(t.TempDir(), "nope", "usage.db")
	err := runUsageWith([]string{"--db", missing}, &buf, time.Now())
	if err == nil {
		t.Fatal("missing DB must fail")
	}
	msg := err.Error()
	// the message must name the actual path it tried and where the path
	// came from (here: the --db flag), so the user can tell whether the
	// CLI looked in the wrong place
	if !strings.Contains(msg, missing) {
		t.Errorf("message must contain the attempted db path %q: %q", missing, msg)
	}
	if !strings.Contains(msg, "--db") || !strings.Contains(msg, "显式指定") {
		t.Errorf("message must contain the --db escape hint: %q", msg)
	}
	// "not enabled" may appear only as one possible cause, never as an
	// assertion — the CLI only checks os.IsNotExist and cannot know the
	// feature state
	if strings.Contains(msg, "该功能尚未开启") {
		t.Errorf("message must not assert the feature was never enabled: %q", msg)
	}
	if strings.Contains(msg, "runtime") || strings.Contains(msg, ".go:") {
		t.Errorf("raw Go error leaked into %q", msg)
	}
}

// TestUsageMainMissingDBExitCode re-executes main() in a subprocess and
// asserts the hardcoded os.Args[1] dispatch exits non-zero with a friendly
// message when the usage database does not exist.
func TestUsageMainMissingDBExitCode(t *testing.T) {
	if os.Getenv("PRISM_TEST_USAGE_EXIT") == "1" {
		os.Args = []string{"prism", "usage", "--db", "/nonexistent/prism/usage.db"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestUsageMainMissingDBExitCode")
	cmd.Env = append(os.Environ(), "PRISM_TEST_USAGE_EXIT=1")
	out, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("want non-zero exit, got err=%v output=%s", err, out)
	}
	if ee.ExitCode() != 1 {
		t.Fatalf("exit code = %d, want 1; output=%s", ee.ExitCode(), out)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "/nonexistent/prism/usage.db") {
		t.Errorf("output must contain the attempted db path:\n%s", outStr)
	}
	if !strings.Contains(outStr, "--db") {
		t.Errorf("output must contain the --db hint:\n%s", outStr)
	}
	if strings.Contains(outStr, "该功能尚未开启") {
		t.Errorf("output must not assert the feature was never enabled:\n%s", outStr)
	}
}

func TestRunUsageConcurrentWithLiveRecorder(t *testing.T) {
	// The acceptance scenario for the CLI read path: the prism service (a
	// real Recorder) is writing while `prism usage` queries the same file.
	// The subcommand must never fail with "database is locked" and must not
	// disturb the writer.
	path := filepath.Join(t.TempDir(), "usage.db")
	store := usage.NewSQLiteStore(path)
	rec := usage.NewRecorder(usage.Config{Enabled: true, DBPath: path, BatchSize: 8, BatchFlushMS: 5}, store)
	rec.Start()

	var recorded atomic.Int64
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			rec.Record(usage.Event{Ts: time.Now(), RequestID: "r", Model: "m", PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2})
			recorded.Add(1)
			i++
			time.Sleep(time.Millisecond)
		}
	}()

	queries := 0
	// The 30-query floor is a load/smoke threshold, not a correctness
	// assertion (correctness is the per-query error check below plus the
	// final persist count). Under -race or on loaded CI a single 2s window
	// can fall short purely due to machine speed, so grant one extra window
	// before failing instead of flaking.
	for window := 0; window < 2 && queries < 30; window++ {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			var buf bytes.Buffer
			err := runUsageWith([]string{"--db", path, "--json", "--since", "7d"}, &buf, time.Now())
			if err != nil {
				t.Fatalf("prism usage during live writes failed: %v", err)
			}
			var doc struct {
				Overview *usage.Overview `json:"overview"`
			}
			if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
				t.Fatalf("json output: %v\n%s", err, buf.String())
			}
			if doc.Overview == nil {
				t.Fatal("missing overview in JSON")
			}
			queries++
			time.Sleep(2 * time.Millisecond)
		}
	}
	close(stop)
	<-writerDone
	rec.Close()

	if queries < 30 {
		t.Fatalf("only %d CLI queries ran, want at least 30", queries)
	}
	// the writer was not disturbed: every recorded event is persisted
	check := usage.NewSQLiteStore(path)
	if err := check.Open(); err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	rows, err := check.Summary(context.Background(), usage.SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != recorded.Load() {
		t.Fatalf("persisted %+v, want %d — the CLI reader disturbed the writer", rows, recorded.Load())
	}
}

func TestWatchLoopRedraws(t *testing.T) {
	var buf bytes.Buffer
	count := 0
	render := func() error {
		count++
		_, err := buf.WriteString("report\n")
		return err
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- watchLoop(render, &buf, 5*time.Millisecond, stop) }()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n := strings.Count(out, "report\n"); n < 3 {
		t.Fatalf("renders = %d, want >= 3", n)
	}
	clears := strings.Count(out, "\x1b[2J\x1b[H")
	if clears < 2 {
		t.Fatalf("clears = %d, want >= 2 (one per redraw)", clears)
	}
	if !strings.HasPrefix(out, "report\n") {
		t.Error("first paint must not clear the screen")
	}
	// 清屏后重画: every clear sequence must be immediately followed by a render
	if n := strings.Count(out, "\x1b[2J\x1b[Hreport\n"); n != clears {
		t.Errorf("clear/render pairs = %d, clears = %d — redraw must follow each clear", n, clears)
	}
	if count != strings.Count(out, "report\n") {
		t.Errorf("render invocation count %d != rendered lines %d", count, strings.Count(out, "report\n"))
	}
}

func TestRunUsageHelp(t *testing.T) {
	var buf bytes.Buffer
	// -h prints the usage and exits successfully (no error surfaced)
	if err := runUsageWith([]string{"-h"}, &buf, time.Now()); err != nil {
		t.Fatalf("-h must not be an error, got %v", err)
	}
	// unknown flag is a real error
	if err := runUsageWith([]string{"--nope"}, &buf, time.Now()); err == nil {
		t.Fatal("unknown flag must fail")
	}
}

func TestWantColor(t *testing.T) {
	if wantColor(&bytes.Buffer{}, false) {
		t.Error("a plain writer must not be colored")
	}
	if wantColor(&bytes.Buffer{}, true) {
		t.Error("--no-color must win over any writer")
	}
	if wantColor(os.Stdout, true) {
		t.Error("--no-color must force color off even on a TTY")
	}
	// a pipe is not a character device → no color
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if wantColor(w, false) {
		t.Error("a pipe must not be colored (ModeCharDevice check)")
	}
}

// TestRunUsageSplitCacheSegments drives the two-segment cache summary
// through the CLI: the table shows the openai and anthropic segments side by
// side with their own denominators (never above 100%), the legacy
// empty-source row joins the openai bucket (the same partition ComputeCost
// applies), and the --json overview keeps every pre-existing field while
// adding the split ones.
func TestRunUsageSplitCacheSegments(t *testing.T) {
	base := time.Date(2026, 3, 10, 15, 4, 5, 0, time.Local)
	path := filepath.Join(t.TempDir(), "usage.db")
	s := usage.NewSQLiteStore(path)
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertBatch(ctx, []usage.Event{
		{Ts: base, RequestID: "o1", Model: "gpt", Source: usage.SourceOpenAI,
			PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100, CachedTokens: 900},
		{Ts: base, RequestID: "a1", Model: "claude", Source: usage.SourceAnthropic,
			PromptTokens: 1, CompletionTokens: 50, TotalTokens: 51, CachedTokens: 500},
		{Ts: base, RequestID: "legacy", Model: "old", Source: "",
			PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, CachedTokens: 80},
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	var buf bytes.Buffer
	if err := runUsageWith([]string{"--db", path}, &buf, base); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// openai bucket = openai row + NULL legacy row: (900+80)/(1000+100) =
	// 980/1100 = 89.1%; anthropic bucket: 500/(1+500+0) = 99.8%.
	if !strings.Contains(out, "命中(OpenAI) 980 (89.1%)   命中(Anthropic) 500 (99.8%)") {
		t.Errorf("split cache segments missing or wrong:\n%s", out)
	}
	// The old single-segment format must be gone.
	if strings.Contains(out, "缓存命中 980 (") || strings.Contains(out, "缓存命中 500 (") {
		t.Errorf("old single-segment format still present:\n%s", out)
	}
	if strings.Contains(out, "50000") {
		t.Errorf("claude table row still uses cached/prompt:\n%s", out)
	}

	// --json: the overview keeps every pre-existing field and adds the
	// per-source split without renaming or retyping anything.
	buf.Reset()
	if err := runUsageWith([]string{"--db", path, "--json"}, &buf, base); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Overview *usage.Overview `json:"overview"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	ov := doc.Overview
	if ov == nil {
		t.Fatal("missing overview in --json output")
	}
	// pre-existing fields untouched
	if ov.Requests != 3 || ov.PromptTokens != 1101 || ov.CachedTokens != 1480 {
		t.Errorf("overview totals broken: %+v", ov)
	}
	// new split fields
	if ov.OpenAIRequests != 2 || ov.OpenAIPromptTokens != 1100 || ov.OpenAICachedTokens != 980 {
		t.Errorf("openai split = %+v, want 2/1100/980", ov)
	}
	if ov.AnthropicRequests != 1 || ov.AnthropicPromptTokens != 1 || ov.AnthropicCachedTokens != 500 || ov.AnthropicCacheWriteTokens != 0 {
		t.Errorf("anthropic split = %+v, want 1/1/500/0", ov)
	}
}

// usageTableHeader returns the header line of the CLI usage report (the
// compact single-line table is the only layout), or "" when the report has
// no single header line carrying both token labels.
func usageTableHeader(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "输入词元") && strings.Contains(line, "输出词元") {
			return line
		}
	}
	return ""
}

// TestRunUsageDefaultCompactNonTTY pins the π-first default: a non-TTY
// bytes.Buffer receives the compact single-line table regardless of COLUMNS
// (the env var is never consulted), every model appears exactly once on a
// row carrying all its values, and the output is uncolored.
func TestRunUsageDefaultCompactNonTTY(t *testing.T) {
	base := time.Date(2026, 3, 10, 15, 4, 5, 0, time.Local)
	path := seedUsageDB(t, base)
	render := func(columns string) string {
		t.Setenv("COLUMNS", columns)
		var buf bytes.Buffer
		if err := runUsageWith([]string{"--db", path}, &buf, base); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	narrow, wide := render("40"), render("200")
	if narrow != wide {
		t.Errorf("COLUMNS must not affect the default output:\n--- COLUMNS=40 ---\n%s\n--- COLUMNS=200 ---\n%s", narrow, wide)
	}
	out := narrow
	if h := usageTableHeader(out); h == "" {
		t.Fatalf("default non-TTY output must be the compact single-line table:\n%s", out)
	}
	// The piped output is also uncolored, as before.
	if strings.Contains(out, "\x1b[") {
		t.Errorf("piped output must not be colored:\n%q", out)
	}
}
