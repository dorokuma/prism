package agyusage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConv(t *testing.T, dir, id string, gens map[int64][]byte, steps map[int64][]byte) string {
	t.Helper()
	path := filepath.Join(dir, id+".db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx integer, data blob, size integer NOT NULL DEFAULT 0, PRIMARY KEY (idx))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE steps (idx integer, metadata blob, PRIMARY KEY (idx))`); err != nil {
		t.Fatal(err)
	}
	for idx, data := range gens {
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES(?, ?, ?)`, idx, data, len(data)); err != nil {
			t.Fatal(err)
		}
	}
	for idx, meta := range steps {
		if _, err := db.Exec(`INSERT INTO steps(idx, metadata) VALUES(?, ?)`, idx, meta); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func openIndex(t *testing.T, convDir string) *Index {
	t.Helper()
	idx, err := Open(filepath.Join(t.TempDir(), "agy-usage.db"), convDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestParseIdxAlignsTimestamp(t *testing.T) {
	dir := t.TempDir()
	usage := encodeUsageBag(11, 22, 33, 44)
	blob := encodeGenMeta("gemini-3.7-flash", "req-align", usage)
	writeConv(t, dir, "conv-align",
		map[int64][]byte{7: blob},
		map[int64][]byte{
			0: encodeStepMeta(111),
			7: encodeStepMeta(12345),
		},
	)
	gens, err := parseConversationFile(filepath.Join(dir, "conv-align.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 1 {
		t.Fatalf("gens = %d, want 1", len(gens))
	}
	if gens[0].TsUnix != 12345 {
		t.Fatalf("ts = %d, want 12345 (steps.idx must align with gen_metadata.idx)", gens[0].TsUnix)
	}
	if gens[0].PromptTokens != 11 || gens[0].CachedTokens != 22 ||
		gens[0].CompletionTokens != 33 || gens[0].ReasoningTokens != 44 ||
		gens[0].TotalTokens != 110 {
		t.Fatalf("tokens = %+v", gens[0])
	}
}

func TestParseRequestIDDedup(t *testing.T) {
	dir := t.TempDir()
	usage := encodeUsageBag(1, 2, 3, 4)
	a := encodeGenMeta("gemini-3.7-flash", "same-req", usage)
	b := encodeGenMeta("gemini-3.7-flash", "same-req", encodeUsageBag(9, 9, 9, 9))
	writeConv(t, dir, "conv-dup",
		map[int64][]byte{0: a, 1: b},
		map[int64][]byte{0: encodeStepMeta(1000), 1: encodeStepMeta(1001)},
	)
	gens, err := parseConversationFile(filepath.Join(dir, "conv-dup.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 1 {
		t.Fatalf("gens = %d, want 1 (request_id dedup)", len(gens))
	}
	if gens[0].RequestID != "same-req" {
		t.Fatalf("id = %q", gens[0].RequestID)
	}
}

func TestParseMissingRequestIDFallsBackToConvIdx(t *testing.T) {
	dir := t.TempDir()
	blob := encodeGenMeta("gemini-3.8-flash", "", encodeUsageBag(5, 0, 1, 0))
	writeConv(t, dir, "no-req",
		map[int64][]byte{3: blob},
		map[int64][]byte{3: encodeStepMeta(50)},
	)
	gens, err := parseConversationFile(filepath.Join(dir, "no-req.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 1 || gens[0].RequestID != "no-req-3" {
		t.Fatalf("fallback id = %+v, want no-req-3", gens)
	}
}

func TestRefreshMtimeSkipAndDeletedSourceKept(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().Unix()
	usage, err := os.ReadFile(filepath.Join("testdata", "usage_bag.bin"))
	if err != nil {
		t.Fatal(err)
	}
	reqKV, err := os.ReadFile(filepath.Join("testdata", "request_id_kv.bin"))
	if err != nil {
		t.Fatal(err)
	}
	var gm []byte
	gm = appendBytesField(gm, 4, usage)
	gm = appendStringField(gm, 19, "gemini-3.7-flash")
	gm = appendBytesField(gm, 20, reqKV)
	blob := appendBytesField(nil, 1, gm)
	path := writeConv(t, dir, "keep-me",
		map[int64][]byte{1: blob},
		map[int64][]byte{1: encodeStepMeta(ts)},
	)

	var reads int
	orig := readConversation
	readConversation = func(p string) ([]Generation, error) {
		reads++
		return orig(p)
	}
	t.Cleanup(func() { readConversation = orig })

	idx := openIndex(t, dir)
	ctx := context.Background()
	if err := idx.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("first refresh reads = %d, want 1", reads)
	}
	n, err := idx.SumTokens(ctx, ts-10, ts+10)
	if err != nil {
		t.Fatal(err)
	}
	if n != fixtureTotal {
		t.Fatalf("sum = %d, want fixture total %d", n, fixtureTotal)
	}
	if err := idx.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("unchanged mtime must skip parse, reads = %d", reads)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := idx.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	n, err = idx.SumTokens(ctx, ts-10, ts+10)
	if err != nil {
		t.Fatal(err)
	}
	if n != fixtureTotal {
		t.Fatalf("deleted source must keep indexed rows, sum = %d", n)
	}
}

func TestQueryWeekWindowAndHitRate(t *testing.T) {
	dir := t.TempDir()
	weekStart := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC).Unix()
	inWeek := weekStart + 3600
	outWeek := weekStart - 3600
	writeConv(t, dir, "win",
		map[int64][]byte{
			1: encodeGenMeta("gemini-3.7-flash", "in", encodeUsageBag(100, 400, 20, 50)),
			2: encodeGenMeta("gemini-3.8-flash", "out", encodeUsageBag(9, 9, 9, 9)),
		},
		map[int64][]byte{
			1: encodeStepMeta(inWeek),
			2: encodeStepMeta(outWeek),
		},
	)
	idx := openIndex(t, dir)
	ctx := context.Background()
	if err := idx.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	sum, err := idx.SumTokens(ctx, weekStart, weekStart+7*24*3600)
	if err != nil {
		t.Fatal(err)
	}
	if sum != 570 {
		t.Fatalf("week sum = %d, want 570 (out-of-window excluded)", sum)
	}
	if z, err := idx.SumTokens(ctx, 0, weekStart+10); err != nil || z != 0 {
		t.Fatalf("fromUnix<=0 must return 0, got %d err=%v", z, err)
	}

	rows, err := idx.Query(ctx, Query{From: weekStart, To: weekStart + 7*24*3600, GroupBy: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1 in-week model", rows)
	}
	r := rows[0]
	if r.Groups["model"] != "gemini-3.7-flash" {
		t.Fatalf("model = %v", r.Groups["model"])
	}
	if r.PromptTokens != 100 || r.CachedTokens != 400 || r.CompletionTokens != 20 ||
		r.ReasoningTokens != 50 || r.TotalTokens != 570 || r.Requests != 1 {
		t.Fatalf("row = %+v", r)
	}
	if r.HitRateInputTokens != 500 {
		t.Fatalf("HitRateInputTokens = %d, want prompt+cached = 500 (Anthropic denom; thinking excluded)", r.HitRateInputTokens)
	}

	// provider/account filters
	empty, err := idx.Query(ctx, Query{From: weekStart, To: weekStart + 10, Provider: "xai"})
	if err != nil || len(empty) != 0 {
		t.Fatalf("provider=xai must skip agy, got %+v err=%v", empty, err)
	}
	ok, err := idx.Query(ctx, Query{From: weekStart, To: weekStart + 7*24*3600, Provider: Provider, Account: Account})
	if err != nil || len(ok) != 1 {
		t.Fatalf("provider=gemini must include, got %+v err=%v", ok, err)
	}
}

func TestOpenFailure(t *testing.T) {
	_, err := Open("/proc/1/not-writable/agy.db", t.TempDir())
	if err == nil {
		t.Fatal("Open on an unwritable path must fail (caller degrades)")
	}
}

func TestFixtureRoundTripThroughIndex(t *testing.T) {
	dir := t.TempDir()
	usage, err := os.ReadFile(filepath.Join("testdata", "usage_bag.bin"))
	if err != nil {
		t.Fatal(err)
	}
	reqKV, err := os.ReadFile(filepath.Join("testdata", "request_id_kv.bin"))
	if err != nil {
		t.Fatal(err)
	}
	var gm []byte
	gm = appendBytesField(gm, 4, usage)
	gm = appendStringField(gm, 19, "gemini-3.7-flash")
	gm = appendBytesField(gm, 20, reqKV)
	blob := appendBytesField(nil, 1, gm)
	writeConv(t, dir, "001dc1dd-6bec-48fd-9eb9-cae8a1492770",
		map[int64][]byte{1: blob},
		// Use now so the 14-day prune cannot drop the fixture; the live
		// step_ts.bin is still parsed in wire_test.go.
		map[int64][]byte{1: encodeStepMeta(time.Now().Unix())},
	)
	idx := openIndex(t, dir)
	ctx := context.Background()
	if err := idx.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := idx.Query(ctx, Query{GroupBy: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.PromptTokens != fixturePrompt || r.CachedTokens != fixtureCached ||
		r.CompletionTokens != fixtureCompletion || r.ReasoningTokens != fixtureReasoning ||
		r.TotalTokens != fixtureTotal || r.HitRateInputTokens != fixturePrompt+fixtureCached {
		t.Fatalf("fixture round-trip = %+v", r)
	}
	if r.Requests != 1 {
		t.Fatalf("requests = %d", r.Requests)
	}
}

func TestSumTokensSkipsNonGemini(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().Unix()
	writeConv(t, dir, "mix",
		map[int64][]byte{
			1: encodeGenMeta("gemini-3.7-flash", "g", encodeUsageBag(10, 20, 30, 40)),
			2: encodeGenMeta("claude-sonnet-4", "c", encodeUsageBag(1000, 0, 0, 0)),
			3: encodeGenMeta("", "blank", encodeUsageBag(1, 2, 3, 4)),
		},
		map[int64][]byte{
			1: encodeStepMeta(ts),
			2: encodeStepMeta(ts),
			3: encodeStepMeta(ts),
		},
	)
	idx := openIndex(t, dir)
	ctx := context.Background()
	if err := idx.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	sum, err := idx.SumTokens(ctx, ts-10, ts+10)
	if err != nil {
		t.Fatal(err)
	}
	// gemini 100 + blank 10; claude 1000 must not inflate the week pool.
	if sum != 110 {
		t.Fatalf("sum = %d, want 110", sum)
	}
	rows, err := idx.Query(ctx, Query{GroupBy: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("query still lists all models, got %d rows", len(rows))
	}
}
