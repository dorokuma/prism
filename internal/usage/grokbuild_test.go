package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectGrokSegmentsSplitsOnTurnReset(t *testing.T) {
	raw := `
{"method":"_x.ai/session/update","timestamp":1787388600,"params":{"sessionId":"s1","update":{"prompt_id":"a","usage":{"numTurns":2,"inputTokens":100,"outputTokens":10,"totalTokens":110,"cachedReadTokens":40,"costUsdTicks":1100000000,"modelUsage":{"grok-4.6-build":{"inputTokens":100,"outputTokens":10,"totalTokens":110,"cachedReadTokens":40,"costUsdTicks":1100000000}}}}}}
{"method":"_x.ai/session/update","timestamp":1787388700,"params":{"sessionId":"s1","update":{"prompt_id":"a","usage":{"numTurns":5,"inputTokens":500,"outputTokens":20,"totalTokens":520,"cachedReadTokens":200,"costUsdTicks":2200000000,"modelUsage":{"grok-4.6-build":{"inputTokens":500,"outputTokens":20,"totalTokens":520,"cachedReadTokens":200,"costUsdTicks":2200000000}}}}}}
{"method":"_x.ai/session/update","timestamp":1787388800,"params":{"sessionId":"s1","update":{"prompt_id":"b","usage":{"numTurns":1,"inputTokens":50,"outputTokens":5,"totalTokens":55,"cachedReadTokens":10,"costUsdTicks":300000000,"modelUsage":{"grok-4.6-build":{"inputTokens":50,"outputTokens":5,"totalTokens":55,"cachedReadTokens":10,"costUsdTicks":300000000}}}}}}
`
	segs := collectGrokSegments(strings.NewReader(raw))
	if len(segs) != 2 {
		t.Fatalf("segments = %d, want 2", len(segs))
	}
	if segs[0].Usage.TotalTokens != 520 || segs[1].Usage.TotalTokens != 55 {
		t.Fatalf("segment totals = %d %d", segs[0].Usage.TotalTokens, segs[1].Usage.TotalTokens)
	}
}

func TestImportGrokBuildReplacesWeekRows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	sess := filepath.Join(dir, "proj", "sid")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"method":"_x.ai/session/update","timestamp":1787388600,"params":{"sessionId":"sid","update":{"prompt_id":"p1","usage":{"numTurns":1,"inputTokens":1000,"outputTokens":100,"totalTokens":1100,"cachedReadTokens":400,"costUsdTicks":2000000000,"modelUsage":{"grok-4.6-build":{"inputTokens":1000,"outputTokens":100,"totalTokens":1100,"cachedReadTokens":400,"costUsdTicks":2000000000}}}}}}`
	if err := os.WriteFile(filepath.Join(sess, "updates.jsonl"), []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := ImportGrokBuild(ctx, s, dir, 1787388500, 1787389000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("imported %d, want 1", n)
	}
	n, err = ImportGrokBuild(ctx, s, dir, 1787388500, 1787389000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reimport %d, want 1", n)
	}
	got, err := s.SumGrokTokens(ctx, 1787388500, 1787389000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1100 {
		t.Fatalf("SumGrokTokens = %d, want 1100", got)
	}
	ov, err := s.Overview(ctx, SummaryQuery{From: 1787388500, To: 1787389000, Account: GrokBuildAccount})
	if err != nil {
		t.Fatal(err)
	}
	if ov.CachedTokens != 400 {
		t.Fatalf("cached = %d", ov.CachedTokens)
	}
	if ov.TotalCost == nil || *ov.TotalCost < 1.9 || *ov.TotalCost > 2.1 {
		t.Fatalf("cost = %v, want ~2 from ticks", ov.TotalCost)
	}
}

func TestStripBuildSuffix(t *testing.T) {
	if stripBuildSuffix("grok-4.6-build") != "grok-4.6" {
		t.Fatal(stripBuildSuffix("grok-4.6-build"))
	}
	if stripBuildSuffix("grok-4.5") != "grok-4.5" {
		t.Fatal(stripBuildSuffix("grok-4.5"))
	}
}
