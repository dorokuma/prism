package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	GrokBuildKeyID   = "grok-build"
	GrokBuildAccount = "grok-build"
	grokBuildPath    = "/grok-build"
	costTicksPerUSD  = 1e9
)

type grokUsage struct {
	InputTokens         int64                `json:"inputTokens"`
	OutputTokens        int64                `json:"outputTokens"`
	TotalTokens         int64                `json:"totalTokens"`
	CachedReadTokens    int64                `json:"cachedReadTokens"`
	CacheCreationTokens int64                `json:"cacheCreationTokens"`
	ReasoningTokens     int64                `json:"reasoningTokens"`
	CostUsdTicks        int64                `json:"costUsdTicks"`
	APIDurationMs       float64              `json:"apiDurationMs"`
	NumTurns            int                  `json:"numTurns"`
	ModelUsage          map[string]grokUsage `json:"modelUsage"`
}

type grokUpdateLine struct {
	Method    string          `json:"method"`
	Timestamp json.RawMessage `json:"timestamp"`
	Params    struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			PromptID string     `json:"prompt_id"`
			Usage    *grokUsage `json:"usage"`
		} `json:"update"`
	} `json:"params"`
}

type grokSegment struct {
	Ts        time.Time
	SessionID string
	PromptID  string
	Usage     grokUsage
}

// DefaultGrokSessionsDir is the Grok Build CLI session tree.
func DefaultGrokSessionsDir() string {
	if h := strings.TrimSpace(os.Getenv("GROK_HOME")); h != "" {
		return filepath.Join(h, "sessions")
	}
	return "/root/.grok/sessions"
}

func stripBuildSuffix(model string) string {
	return strings.TrimSuffix(strings.TrimSpace(model), "-build")
}

func parseUnix(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return int64(n)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.Unix()
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

func collectGrokSegments(r io.Reader) []grokSegment {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var out []grokSegment
	var cur []grokSegment
	prevTurns := -1
	flush := func() {
		if len(cur) == 0 {
			return
		}
		out = append(out, cur[len(cur)-1])
		cur = nil
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var o grokUpdateLine
		if err := json.Unmarshal([]byte(line), &o); err != nil {
			continue
		}
		if o.Params.Update.Usage == nil {
			continue
		}
		u := *o.Params.Update.Usage
		nt := u.NumTurns
		if prevTurns >= 0 && nt < prevTurns {
			flush()
		}
		ts := parseUnix(o.Timestamp)
		cur = append(cur, grokSegment{
			Ts:        time.Unix(ts, 0).UTC(),
			SessionID: o.Params.SessionID,
			PromptID:  o.Params.Update.PromptID,
			Usage:     u,
		})
		prevTurns = nt
	}
	flush()
	return out
}

func eventsFromSegment(seg grokSegment, priceFor func(model string, contextTokens int64) *Price) []Event {
	models := seg.Usage.ModelUsage
	if len(models) == 0 {
		models = map[string]grokUsage{"grok-4.6": seg.Usage}
	}
	out := make([]Event, 0, len(models))
	i := 0
	for rawModel, u := range models {
		model := stripBuildSuffix(rawModel)
		if model == "" {
			model = "grok-4.6"
		}
		if u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 {
			continue
		}
		var cost *float64
		status := ""
		if u.CostUsdTicks > 0 {
			v := float64(u.CostUsdTicks) / costTicksPerUSD
			cost = &v
			status = CostStatusOK
		} else if priceFor != nil {
			cost, status = ComputeCost(u.InputTokens, u.OutputTokens, u.CachedReadTokens, u.CacheCreationTokens, SourceOpenAI, priceFor(model, u.InputTokens))
		}
		total := u.TotalTokens
		if total == 0 {
			total = u.InputTokens + u.OutputTokens
		}
		rid := fmt.Sprintf("grok-build:%s:%s:%d", seg.SessionID, seg.PromptID, i)
		out = append(out, Event{
			Ts:               seg.Ts,
			RequestID:        rid,
			Path:             grokBuildPath,
			Model:            model,
			Provider:         "xai",
			Account:          GrokBuildAccount,
			KeyID:            GrokBuildKeyID,
			Success:          true,
			Status:           200,
			PromptTokens:     u.InputTokens,
			CompletionTokens: u.OutputTokens,
			TotalTokens:      total,
			CachedTokens:     u.CachedReadTokens,
			ReasoningTokens:  u.ReasoningTokens,
			CacheWriteTokens: u.CacheCreationTokens,
			DurationMS:       u.APIDurationMs,
			Source:           SourceOpenAI,
			Cost:             cost,
			CostStatus:       status,
		})
		i++
	}
	return out
}

// probeWalkableDir is the shared sessions-dir importability probe used by
// ImportGrokBuild and ImportPiSessions. Three steps, all-or-nothing:
//
//  1. os.Stat — missing path or !IsDir (regular file, FIFO, symlink→file)
//     skips. Stat follows the last component, so a FIFO/file at the
//     sessionsDir ROOT is gated here and never opened (no O_NONBLOCK
//     needed). In-tree non-regular files are skipped later by walkFn.
//  2. filepath.EvalSymlinks — Walk uses Lstat, so a symlink→dir passes
//     Stat+IsDir but Walk treats the root as a file and harvests nothing,
//     then DELETE+insert-0 wipes previously imported rows. Resolving to
//     the real path makes probe semantics match Walk consumption: a legal
//     symlink→dir imports normally; a broken link (or EvalSymlinks race)
//     errors and skips. The caller MUST Walk the returned realPath.
//  3. os.ReadDir(realPath) — --x (execute, no read) fails open and is
//     blocked here. r-- (read, no execute) PASSES this probe
//     (open+getdents only need r) but Walk's per-entry lstat needs x;
//     those permission errors abort the import in walkFn (no DELETE)
//     rather than harvesting zero then wiping old rows. Any ReadDir
//     error (EACCES, vanished, …) skips. Probe pass ⇒ Walk can at
//     least list the root level; deeper permission problems are walkFn's
//     job.
//
// Every skip logs one slog.Warn tagged with source ("grok-build" /
// "pi-sessions") so the two call sites stay distinguishable. Skip includes
// the window DELETE — the "先删后插零" no-op. One warn per xai group per
// poller round (~120s); no sync.Once, so a recovery-then-re-failure still
// surfaces. Runtime permission check, not a hard-coded user: `prism quota`
// as root still lists the directory and imports normally.
func probeWalkableDir(path, source string) (realPath string, ok bool) {
	info, err := os.Stat(path)
	if err != nil {
		slog.Warn("usage: "+source+" import skipped: sessions dir stat failed", "error", err, "path", path)
		return "", false
	}
	if !info.IsDir() {
		slog.Warn("usage: "+source+" import skipped: sessions dir is not a directory", "path", path, "mode", info.Mode().String())
		return "", false
	}
	realPath, err = filepath.EvalSymlinks(path)
	if err != nil {
		slog.Warn("usage: "+source+" import skipped: sessions dir evalsymlinks failed", "error", err, "path", path)
		return "", false
	}
	if _, err := os.ReadDir(realPath); err != nil {
		slog.Warn("usage: "+source+" import skipped: sessions dir unreadable", "error", err, "path", realPath)
		return "", false
	}
	return realPath, true
}

// shouldAbortWalk reports whether a Walk-supplied error should abort the
// whole import (no DELETE). Any non-nil error aborts: swallowing
// IsPermission as SkipDir/return-nil used to harvest zero then
// DELETE+insert-0. Conservative — a single-file lstat EACCES also
// aborts — because uncertain harvest completeness must not wipe old rows.
func shouldAbortWalk(err error) bool {
	return err != nil
}

// walkErrForTest is a test-only hook. When non-nil, walkFn treats it as
// a Walk-supplied error (as if lstat failed). Production stays nil. Root
// DAC override makes a real in-tree EACCES unreproducible, so tests inject
// the error to pin "abort ⇒ no DELETE".
var walkErrForTest error

// ImportGrokBuild replaces grok-build usage rows in [fromUnix, toUnix] with
// session snapshots from the Grok Build CLI tree. Segments split when
// numTurns drops (new conversation / rewind). Each segment contributes its
// last usage snapshot so totals are not prefix-summed.
func ImportGrokBuild(ctx context.Context, store *SQLiteStore, sessionsDir string, fromUnix, toUnix int64, priceFor func(model string, contextTokens int64) *Price) (int, error) {
	if store == nil || strings.TrimSpace(sessionsDir) == "" || fromUnix <= 0 {
		return 0, nil
	}
	if toUnix <= 0 {
		toUnix = time.Now().Unix()
	}
	realPath, ok := probeWalkableDir(sessionsDir, "grok-build")
	if !ok {
		return 0, nil
	}
	var events []Event
	var aborted bool
	err := filepath.Walk(realPath, func(path string, info os.FileInfo, err error) error {
		if err == nil && walkErrForTest != nil {
			err = walkErrForTest
		}
		if shouldAbortWalk(err) {
			slog.Warn("usage: grok-build import aborted: walk error", "path", path, "error", err)
			aborted = true
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Name() != "updates.jsonl" {
			return nil
		}
		if !info.Mode().IsRegular() {
			slog.Warn("usage: grok-build import skipped: not a regular file", "path", path, "mode", info.Mode().String())
			return nil
		}
		if info.ModTime().Unix() < fromUnix {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			if os.IsPermission(err) {
				slog.Warn("usage: grok-build import skipped: open permission denied", "path", path, "error", err)
				return nil
			}
			return nil
		}
		defer f.Close()
		for _, seg := range collectGrokSegments(f) {
			ts := seg.Ts.Unix()
			if ts < fromUnix || ts > toUnix {
				continue
			}
			events = append(events, eventsFromSegment(seg, priceFor)...)
		}
		return nil
	})
	if aborted {
		slog.Warn("usage: grok-build import aborted: skip window delete", "path", realPath)
		return 0, nil
	}
	if err != nil && !os.IsNotExist(err) && !os.IsPermission(err) {
		return 0, err
	}
	if _, err := store.DeleteKeyIDRange(ctx, GrokBuildKeyID, fromUnix, toUnix); err != nil {
		return 0, err
	}
	if err := store.InsertBatch(ctx, events); err != nil {
		return 0, err
	}
	return len(events), nil
}
