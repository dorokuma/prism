package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/usage"
)

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
	// bare `prism usage` → today, grouped by model
	_, q, _, err := parseUsageArgs(nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.GroupBy) != 1 || q.GroupBy[0] != "model" {
		t.Errorf("default group_by = %v, want [model]", q.GroupBy)
	}
	if q.From != time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix() {
		t.Errorf("default from must be today midnight, got %d", q.From)
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
		_, q, _, err := parseUsageArgs([]string{p.preset}, now)
		if err != nil {
			t.Fatalf("preset %q: %v", p.preset, err)
		}
		if strings.Join(q.GroupBy, ",") != strings.Join(p.want, ",") {
			t.Errorf("preset %q group_by = %v, want %v", p.preset, q.GroupBy, p.want)
		}
	}
	// errors preset forces success=false
	_, q, _, _ = parseUsageArgs([]string{"errors"}, now)
	if q.Success == nil || *q.Success {
		t.Errorf("errors preset must filter success=false, got %v", q.Success)
	}
	// default preset does not filter success
	_, q, _, _ = parseUsageArgs(nil, now)
	if q.Success != nil {
		t.Errorf("default must not filter success, got %v", *q.Success)
	}
	// --by overrides the preset group_by; --failed forces success=false
	_, q, _, err = parseUsageArgs([]string{"keys", "--by", "day,hour", "--failed", "--limit", "50"}, now)
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
	if _, _, _, err := parseUsageArgs([]string{"--since", "7d", "models"}, now); err == nil {
		t.Error("preset after flags must be rejected with a hint")
	}
}

func TestParseUsageArgsValidation(t *testing.T) {
	now := time.Now()
	bad := [][]string{
		{"bogus"},                          // unknown preset
		{"models", "--by", "model,nope"}, // unknown group_by
		{"models", "--by", ","},         // empty --by
		{"models", "--limit", "0"},      // limit below 1
		{"models", "--json", "--watch", "5s"}, // json + watch conflict
		{"models", "--watch", "abc"},    // bad watch interval
		{"models", "--since", "6d", "--until", "7d"}, // since after until
		{"models", "--since", "banana"}, // bad time
		{"models", "--until", "2026-13-40"}, // bad date
	}
	for _, args := range bad {
		if _, _, _, err := parseUsageArgs(args, now); err == nil {
			t.Errorf("parseUsageArgs(%v) must fail", args)
		}
	}
	// limit above the package cap is clamped to 1000, like the HTTP path
	_, q, _, err := parseUsageArgs([]string{"models", "--limit", "2000"}, now)
	if err != nil || q.Limit != 1000 {
		t.Errorf("limit 2000 must clamp to 1000, got %d, %v", q.Limit, err)
	}
	// filters map to the query
	_, q, _, err = parseUsageArgs([]string{"models", "--model", "m1", "--key", "k1", "--account", "a1", "--provider", "p1", "--since", "2026-08-01", "--until", "08-10"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if q.Model != "m1" || q.KeyID != "k1" || q.Account != "a1" || q.Provider != "p1" {
		t.Errorf("filters not mapped: %+v", q)
	}
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
		Period   string              `json:"period"`
		From     int64               `json:"from"`
		To       int64               `json:"to"`
		GroupBy  []string            `json:"group_by"`
		Overview *usage.Overview     `json:"overview"`
		Rows     []usage.SummaryRow  `json:"rows"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Period != "今天" {
		t.Errorf("period = %q, want 今天", doc.Period)
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
	if !strings.Contains(out, "今天  ·  5 请求  ·  750 token  ·  $0.600") {
		t.Errorf("summary line missing/wrong:\n%s", out)
	}
	// the failed + missing-price hints
	if !strings.Contains(out, "失败 1 (20.0%)") {
		t.Errorf("failure line missing:\n%s", out)
	}
	if !strings.Contains(out, "⚠ 有 1 个请求未算出金额") {
		t.Errorf("missing-price warning missing:\n%s", out)
	}
	// table: header + the three models, nil cost as dash
	for _, want := range []string{"model", "请求", "alpha", "beta", "gamma", "  -"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
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
		Overview *usage.Overview `json:"overview"`
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
	err := runUsageWith([]string{"--db", filepath.Join(t.TempDir(), "nope", "usage.db")}, &buf, time.Now())
	if err == nil {
		t.Fatal("missing DB must fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "usage.enabled") || !strings.Contains(msg, "重启") {
		t.Errorf("friendly hint missing in %q", msg)
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
	if !strings.Contains(string(out), "usage.enabled") {
		t.Errorf("friendly hint missing in output:\n%s", out)
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

	deadline := time.Now().Add(2 * time.Second)
	queries := 0
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
