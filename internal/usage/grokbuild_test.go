package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// TestImportGrokBuildSessionsDirIsFile pins the Stat precheck: sessionsDir
// pointing at a regular file (e.g. a FIFO would block an O_RDONLY open, a
// plain file is a config mistake) must skip the whole import INCLUDING the
// window DELETE — the seeded grok-build row survives, no "先删后插零".
func TestImportGrokBuildSessionsDirIsFile(t *testing.T) {
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

	// A regular file at the sessions-dir path: IsDir precheck skips it
	// (Stat succeeds, !IsDir) without ever opening the path.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("not a session tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := ImportGrokBuild(ctx, s, file, now-3600, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("file as sessions dir: imported %d, want 0", n)
	}
	if got, err := s.SumGrokTokens(ctx, now-3600, now); err != nil {
		t.Fatal(err)
	} else if got != 20 {
		t.Fatalf("file as sessions dir: SumGrokTokens = %d, want 20 (seeded row must survive)", got)
	}
}

const grokBuildTestJSONL = `{"method":"_x.ai/session/update","timestamp":1787388600,"params":{"sessionId":"sid","update":{"prompt_id":"p1","usage":{"numTurns":1,"inputTokens":1000,"outputTokens":100,"totalTokens":1100,"cachedReadTokens":400,"costUsdTicks":2000000000,"modelUsage":{"grok-4.6-build":{"inputTokens":1000,"outputTokens":100,"totalTokens":1100,"cachedReadTokens":400,"costUsdTicks":2000000000}}}}}}`

func seedGrokBuildEvent(t *testing.T, s *SQLiteStore, now int64) {
	t.Helper()
	price := &Price{Input: 1, Output: 1}
	c, _ := costOf(10, 10, 0, 0, "", price)
	seed := Event{
		Ts: time.Unix(now, 0), RequestID: "seed", Path: grokBuildPath, Model: "grok-4.6",
		Provider: "xai", Account: GrokBuildAccount, KeyID: GrokBuildKeyID,
		Success: true, Status: 200,
		PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20,
		Cost: c, CostStatus: CostStatusOK,
	}
	if err := s.InsertBatch(context.Background(), []Event{seed}); err != nil {
		t.Fatal(err)
	}
}

// TestImportGrokBuildSymlinkToDirImports pins EvalSymlinks: sessionsDir as a
// symlink to a real tree with updates.jsonl must import the real data.
// filepath.Walk uses Lstat, so walking the symlink itself would harvest
// nothing; the probe resolves the real path and Walk consumes that.
func TestImportGrokBuildSymlinkToDirImports(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	real := t.TempDir()
	sess := filepath.Join(real, "proj", "sid")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess, "updates.jsonl"), []byte(grokBuildTestJSONL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "sessions-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	n, err := ImportGrokBuild(ctx, s, link, 1787388500, 1787389000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n <= 0 {
		t.Fatalf("symlink\u2192dir: imported %d, want >0", n)
	}
	got, err := s.SumGrokTokens(ctx, 1787388500, 1787389000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1100 {
		t.Fatalf("symlink\u2192dir: SumGrokTokens = %d, want 1100 (real data must land)", got)
	}
}

// TestImportGrokBuildSymlinkToFileKeepsRows: symlink\u2192regular file is not a
// directory (Stat follows). Skip including DELETE; seeded row survives.
func TestImportGrokBuildSymlinkToFileKeepsRows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	seedGrokBuildEvent(t, s, now)

	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("not a session tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "file-link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	n, err := ImportGrokBuild(ctx, s, link, now-3600, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("symlink\u2192file: imported %d, want 0", n)
	}
	if got, err := s.SumGrokTokens(ctx, now-3600, now); err != nil {
		t.Fatal(err)
	} else if got != 20 {
		t.Fatalf("symlink\u2192file: SumGrokTokens = %d, want 20 (seeded row must survive)", got)
	}
}

// TestImportGrokBuildBrokenSymlinkKeepsRows: dangling symlink fails Stat
// (and would fail EvalSymlinks). Skip including DELETE; seeded row survives.
func TestImportGrokBuildBrokenSymlinkKeepsRows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	seedGrokBuildEvent(t, s, now)

	link := filepath.Join(t.TempDir(), "broken-link")
	if err := os.Symlink(filepath.Join(t.TempDir(), "no-such-target"), link); err != nil {
		t.Fatal(err)
	}
	n, err := ImportGrokBuild(ctx, s, link, now-3600, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("broken symlink: imported %d, want 0", n)
	}
	if got, err := s.SumGrokTokens(ctx, now-3600, now); err != nil {
		t.Fatal(err)
	} else if got != 20 {
		t.Fatalf("broken symlink: SumGrokTokens = %d, want 20 (seeded row must survive)", got)
	}
}

// TestImportGrokBuildNoExecDirKeepsRows pins r-- without x (chmod 0644).
// Root ignores mode bits so ReadDir still succeeds here; the keep-rows
// assertion is environment-adaptive like TestImportGrokBuildUnreadableDirKeepsRows
// (only asserted when ReadDir itself fails). Non-root subprocess construction
// is environment-limited and not attempted.
func TestImportGrokBuildNoExecDirKeepsRows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	seedGrokBuildEvent(t, s, now)

	perm := filepath.Join(t.TempDir(), "no-exec-sessions")
	if err := os.Mkdir(perm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(perm, 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(perm, 0o700) }()
	if _, err := os.ReadDir(perm); err != nil {
		n, err := ImportGrokBuild(ctx, s, perm, now-3600, now, nil)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("r-- no-x dir: imported %d, want 0", n)
		}
		if got, err := s.SumGrokTokens(ctx, now-3600, now); err != nil {
			t.Fatal(err)
		} else if got != 20 {
			t.Fatalf("r-- no-x dir: SumGrokTokens = %d, want 20 (seeded row must survive)", got)
		}
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

func TestShouldAbortWalk(t *testing.T) {
	if shouldAbortWalk(nil) {
		t.Fatal("nil must not abort")
	}
	for _, err := range []error{os.ErrPermission, os.ErrNotExist, os.ErrInvalid} {
		if !shouldAbortWalk(err) {
			t.Fatalf("shouldAbortWalk(%v) = false, want true", err)
		}
	}
}

// TestImportGrokBuildWalkAbortKeepsRows pins the abort-without-DELETE
// contract. Root DAC override makes a real in-tree lstat EACCES
// unreproducible, so walkErrForTest injects the Walk-supplied error.
func TestImportGrokBuildWalkAbortKeepsRows(t *testing.T) {
	walkErrForTest = os.ErrPermission
	t.Cleanup(func() { walkErrForTest = nil })

	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Unix()
	seedGrokBuildEvent(t, s, now)

	dir := t.TempDir()
	n, err := ImportGrokBuild(ctx, s, dir, now-3600, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("aborted import: imported %d, want 0", n)
	}
	if got, err := s.SumGrokTokens(ctx, now-3600, now); err != nil {
		t.Fatal(err)
	} else if got != 20 {
		t.Fatalf("aborted import: SumGrokTokens = %d, want 20 (seeded row must survive)", got)
	}
}

// TestImportGrokBuildInTreeFIFOSkipped: a regular updates.jsonl imports;
// a sibling-tree FIFO named updates.jsonl is skipped without Open (which
// would block the poller). Timeout guards against a hang.
func TestImportGrokBuildInTreeFIFOSkipped(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	sess := filepath.Join(dir, "proj", "sid")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess, "updates.jsonl"), []byte(grokBuildTestJSONL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fifoDir := filepath.Join(dir, "proj", "fifo-sid")
	if err := os.MkdirAll(fifoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fifoPath := filepath.Join(fifoDir, "updates.jsonl")
	if err := syscall.Mkfifo(fifoPath, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(fifoPath) })

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := ImportGrokBuild(ctx, s, dir, 1787388500, 1787389000, nil)
		done <- result{n, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.n != 1 {
			t.Fatalf("imported %d, want 1 (FIFO skipped, regular file imported)", got.n)
		}
		sum, err := s.SumGrokTokens(ctx, 1787388500, 1787389000)
		if err != nil {
			t.Fatal(err)
		}
		if sum != 1100 {
			t.Fatalf("SumGrokTokens = %d, want 1100", sum)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ImportGrokBuild hung on in-tree FIFO (timeout)")
	}
}
