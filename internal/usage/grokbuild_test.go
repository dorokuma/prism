package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestImportGrokBuildUnreadableDirKeepsRows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	price := &Price{Input: 1, Output: 1}
	c, _ := costOf(10, 10, 0, 0, "", price)
	now := time.Now().Unix()
	seed := Event{
		Ts: time.Unix(now, 0), RequestID: "seed", Path: grokBuildPath, Model: "grok-4.6",
		Provider: "xai", Account: GrokBuildAccount, KeyID: GrokBuildKeyID,
		Success: true, Status: 200,
		PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20,
		Cost: c, CostStatus: CostStatusOK,
	}
	if err := s.InsertBatch(ctx, []Event{seed}); err != nil {
		t.Fatal(err)
	}

	// Missing dir: the import must skip the whole harvest INCLUDING the
	// window DELETE — the seeded grok-build row in the import window
	// survives (no "先删后插零" on an empty harvest).
	missing := filepath.Join(t.TempDir(), "no-such-sessions")
	n, err := ImportGrokBuild(ctx, s, missing, now-3600, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("missing dir: imported %d, want 0", n)
	}
	if got, err := s.SumGrokTokens(ctx, now-3600, now); err != nil {
		t.Fatal(err)
	} else if got != 20 {
		t.Fatalf("missing dir: SumGrokTokens = %d, want 20 (seeded row must survive)", got)
	}

	// Dir present but unreadable (no read permission): same skip. The
	// probe is a runtime permission check — when the test runs as root
	// the permission bit is bypassed and the import legitimately
	// proceeds, so that branch is skipped rather than asserted.
	perm := filepath.Join(t.TempDir(), "no-perm-sessions")
	if err := os.Mkdir(perm, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Open(perm); err != nil {
		n, err = ImportGrokBuild(ctx, s, perm, now-3600, now, nil)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("unreadable dir: imported %d, want 0", n)
		}
		if got, err := s.SumGrokTokens(ctx, now-3600, now); err != nil {
			t.Fatal(err)
		} else if got != 20 {
			t.Fatalf("unreadable dir: SumGrokTokens = %d, want 20 (seeded row must survive)", got)
		}
	}
	if err := os.Chmod(perm, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestEventsFromSegmentTierSelectionUsesInputTokens(t *testing.T) {
	short := &Price{Input: 1}
	long := &Price{Input: 2}
	var gotContext int64
	seg := grokSegment{SessionID: "s", PromptID: "p", Usage: grokUsage{
		InputTokens: 200000, OutputTokens: 1, TotalTokens: 200001,
		CachedReadTokens: 50000, ModelUsage: map[string]grokUsage{"grok-4.6-build": {
			InputTokens: 200000, OutputTokens: 1, TotalTokens: 200001, CachedReadTokens: 50000,
		}},
	}}
	events := eventsFromSegment(seg, func(model string, contextTokens int64) *Price {
		gotContext = contextTokens
		if contextTokens >= 200000 {
			return long
		}
		return short
	})
	if len(events) != 1 || gotContext != 200000 {
		t.Fatalf("events=%d context=%d, want one event and InputTokens context", len(events), gotContext)
	}
	if events[0].Cost == nil || *events[0].Cost <= 0 {
		t.Fatal("expected estimated cost")
	}
}

func TestEventsFromSegmentCostTicksTakePriority(t *testing.T) {
	called := false
	seg := grokSegment{SessionID: "s", PromptID: "p", Usage: grokUsage{
		InputTokens: 100, OutputTokens: 10, TotalTokens: 110, CostUsdTicks: 2500000000,
	}}
	events := eventsFromSegment(seg, func(model string, contextTokens int64) *Price {
		called = true
		return &Price{Input: 999}
	})
	if len(events) != 1 || called {
		t.Fatalf("estimated pricing called=%v, want false", called)
	}
	if events[0].Cost == nil || *events[0].Cost != 2.5 {
		t.Fatalf("cost=%v, want 2.5 from CostUsdTicks", events[0].Cost)
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
