package usage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPiEventMapsUsageAndError(t *testing.T) {
	var line piMessageLine
	raw := `{"type":"session","timestamp":"2026-01-01T00:00:00Z"}
{"type":"message","message":{"role":"assistant","provider":"openai","model":"gpt-test","timestamp":1700000123456,"stopReason":"error","usage":{"input":10,"output":3,"totalTokens":25,"cacheRead":4,"cacheWrite":2,"cost":{"total":0.25}}}}`
	parts := strings.Split(raw, "\n")
	if err := unmarshalPi([]byte(parts[0]), &line); err != nil {
		t.Fatal(err)
	}
	if _, ok := piEvent(line, "header"); ok {
		t.Fatal("header must not be an event")
	}
	if err := unmarshalPi([]byte(parts[1]), &line); err != nil {
		t.Fatal(err)
	}
	ev, ok := piEvent(line, "event")
	if !ok {
		t.Fatal("assistant usage line was skipped")
	}
	if ev.Ts.Unix() != 1700000123 || ev.Provider != "openai" || ev.Model != "gpt-test" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.PromptTokens != 10 || ev.CompletionTokens != 3 || ev.TotalTokens != 25 || ev.CachedTokens != 4 || ev.CacheWriteTokens != 2 || ev.Cost == nil || *ev.Cost != .25 || ev.Success {
		t.Fatalf("mapping = %+v", ev)
	}
}

func TestImportPiSessionsRangeAndFiles(t *testing.T) {
	s := openTestStore(t)
	dir := t.TempDir()
	for _, name := range []string{"a/x.jsonl", "b/y.jsonl"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0755); err != nil {
			t.Fatal(err)
		}
		body := `{"type":"message","message":{"role":"assistant","provider":"p","model":"m","timestamp":1700000000000,"usage":{"input":1,"output":2,"totalTokens":3}}}`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\nnot json\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(dir, name), time.Unix(1700000000, 0), time.Unix(1700000000, 0)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := ImportPiSessions(context.Background(), s, dir, 1700000000, 1700000000)
	if err != nil || n != 2 {
		t.Fatalf("import = %d, %v", n, err)
	}
	ov, err := s.Overview(context.Background(), SummaryQuery{From: 1700000000, To: 1700000000, KeyID: PiSessionKeyID})
	if err != nil {
		t.Fatal(err)
	}
	if ov.Requests != 2 || ov.TotalTokens != 6 {
		t.Fatalf("overview = %+v", ov)
	}
}

func TestPiEventRejectsMissingProviderOrModel(t *testing.T) {
	for _, raw := range []string{
		`{"type":"message","message":{"role":"assistant","model":"m","timestamp":1700000000000,"usage":{"input":1,"output":2}}}`,
		`{"type":"message","message":{"role":"assistant","provider":"p","timestamp":1700000000000,"usage":{"input":1,"output":2}}}`,
	} {
		var line piMessageLine
		if err := unmarshalPi([]byte(raw), &line); err != nil {
			t.Fatal(err)
		}
		if _, ok := piEvent(line, "missing"); ok {
			t.Fatal("event with missing provider or model must be skipped")
		}
	}
}

func TestPiEventTotalTokensFallbackIncludesCache(t *testing.T) {
	var line piMessageLine
	raw := `{"type":"message","message":{"role":"assistant","provider":"p","model":"m","timestamp":1700000000000,"usage":{"input":1,"output":2,"cacheRead":4,"cacheWrite":8}}}`
	if err := unmarshalPi([]byte(raw), &line); err != nil {
		t.Fatal(err)
	}
	ev, ok := piEvent(line, "fallback")
	if !ok || ev.TotalTokens != 15 {
		t.Fatalf("event = %+v, ok=%v", ev, ok)
	}
}

func TestPiEventTotalTokensPreferred(t *testing.T) {
	var line piMessageLine
	raw := `{"type":"message","message":{"role":"assistant","provider":"p","model":"m","timestamp":1700000000000,"usage":{"input":1,"output":2,"cacheRead":4,"cacheWrite":8,"totalTokens":99}}}`
	if err := unmarshalPi([]byte(raw), &line); err != nil {
		t.Fatal(err)
	}
	ev, ok := piEvent(line, "preferred")
	if !ok || ev.TotalTokens != 99 {
		t.Fatalf("event = %+v, ok=%v", ev, ok)
	}
}

// Keep JSON decoding in the test explicit while allowing malformed records to
// be tested without exposing another production helper.
func unmarshalPi(data []byte, line *piMessageLine) error {
	return json.Unmarshal(data, line)
}
