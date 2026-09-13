package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/usage"
)

func TestRunUsageMergesAgyGemini(t *testing.T) {
	base := time.Date(2026, 9, 12, 15, 0, 0, 0, time.Local)
	dbPath := seedUsageDB(t, base)

	convDir := t.TempDir()
	idxPath := filepath.Join(t.TempDir(), "agy-usage.db")
	writeCLIConv(t, convDir, "agy-cli", base.Unix()-3600, "gemini-3.7-flash", "req-cli", 100, 400, 20, 50)

	prevDir, prevIdx, prevEst := agyConvDir, agyIndexPath, geminiEstimatePath
	agyConvDir, agyIndexPath, geminiEstimatePath = convDir, idxPath, filepath.Join(t.TempDir(), "no-est.json")
	t.Cleanup(func() {
		agyConvDir, agyIndexPath, geminiEstimatePath = prevDir, prevIdx, prevEst
	})

	var buf bytes.Buffer
	if err := runUsageWith([]string{"--db", dbPath, "--json"}, &buf, base); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Overview *usage.Overview    `json:"overview"`
		Rows     []usage.SummaryRow `json:"rows"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Overview == nil {
		t.Fatal("nil overview")
	}
	// seedUsageDB: 5 requests / 750 tokens; agy: 1 request / 570 tokens.
	if doc.Overview.Requests != 6 {
		t.Fatalf("overview.requests = %d, want 6 (5 usage + 1 agy)", doc.Overview.Requests)
	}
	if doc.Overview.TotalTokens != 1320 {
		t.Fatalf("overview.total = %d, want 1320 (750+570)", doc.Overview.TotalTokens)
	}
	if doc.Overview.CachedTokens != 400 {
		t.Fatalf("overview.cached = %d, want 400", doc.Overview.CachedTokens)
	}
	if doc.Overview.ReasoningTokens != 50 {
		t.Fatalf("overview.reasoning = %d, want 50", doc.Overview.ReasoningTokens)
	}
	var gem *usage.SummaryRow
	for i := range doc.Rows {
		if m, _ := doc.Rows[i].Groups["model"].(string); m == "gemini-3.7-flash" {
			gem = &doc.Rows[i]
		}
	}
	if gem == nil {
		t.Fatalf("missing gemini row: %+v", doc.Rows)
	}
	if gem.HitRateInputTokens != 500 || gem.CachedTokens != 400 || gem.PromptTokens != 100 {
		t.Fatalf("gemini row = %+v, want prompt=100 cached=400 denom=500", gem)
	}

	buf.Reset()
	if err := runUsageWith([]string{"--db", dbPath}, &buf, base); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "gemini-3.7-flash") {
		t.Errorf("table missing gemini row:\n%s", out)
	}
	if !strings.Contains(out, "80.0%") {
		t.Errorf("table hit rate must not be 0.0%%:\n%s", out)
	}
}

func writeCLIConv(t *testing.T, dir, id string, ts int64, model, req string, prompt, cached, completion, reasoning uint64) {
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
	usageBag := encodeCLIVarintField(nil, 1, prompt)
	usageBag = encodeCLIVarintField(usageBag, 2, cached)
	usageBag = encodeCLIVarintField(usageBag, 3, completion)
	usageBag = encodeCLIVarintField(usageBag, 5, reasoning)
	gm := encodeCLIBytesField(nil, 4, usageBag)
	gm = encodeCLIBytesField(gm, 19, []byte(model))
	kv := encodeCLIBytesField(nil, 1, []byte("request_id"))
	kv = encodeCLIBytesField(kv, 2, []byte(req))
	gm = encodeCLIBytesField(gm, 20, kv)
	blob := encodeCLIBytesField(nil, 1, gm)
	if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data, size) VALUES(1, ?, ?)`, blob, len(blob)); err != nil {
		t.Fatal(err)
	}
	inner := encodeCLIVarintField(nil, 1, uint64(ts))
	meta := encodeCLIBytesField(nil, 1, inner)
	if _, err := db.Exec(`INSERT INTO steps(idx, metadata) VALUES(1, ?)`, meta); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0o600)
}

func encodeCLIVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func encodeCLIVarintField(b []byte, num int, v uint64) []byte {
	b = encodeCLIVarint(b, uint64(num<<3))
	return encodeCLIVarint(b, v)
}

func encodeCLIBytesField(b []byte, num int, payload []byte) []byte {
	b = encodeCLIVarint(b, uint64(num<<3|2))
	b = encodeCLIVarint(b, uint64(len(payload)))
	return append(b, payload...)
}
