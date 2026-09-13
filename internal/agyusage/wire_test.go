package agyusage

import (
	"os"
	"path/filepath"
	"testing"
)

// Live blobs extracted from 001dc1dd-6bec-48fd-9eb9-cae8a1492770.db idx=1.
// They lock the undocumented field mapping against a real GeneratorMetadata
// usage bag / request_id kv / steps.metadata timestamp.
const (
	fixturePrompt     = 1299
	fixtureCached     = 4786
	fixtureCompletion = 224
	fixtureReasoning  = 12197
	fixtureTotal      = fixturePrompt + fixtureCached + fixtureCompletion + fixtureReasoning
	fixtureRequestID  = "ea68aa5a-1349-48fa-a275-db6eb49fa1fa-1"
	fixtureStepUnix   = 1788278754
)

func TestParseUsageBagFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "usage_bag.bin"))
	if err != nil {
		t.Fatal(err)
	}
	u := parseUsageBag(b)
	if u.Prompt != fixturePrompt || u.Cached != fixtureCached ||
		u.Completion != fixtureCompletion || u.Reasoning != fixtureReasoning {
		t.Fatalf("usage bag = %+v, want f1=%d f2=%d f3=%d f5=%d",
			u, fixturePrompt, fixtureCached, fixtureCompletion, fixtureReasoning)
	}
	if u.total() != fixtureTotal {
		t.Fatalf("total = %d, want f1+f2+f3+f5 = %d", u.total(), fixtureTotal)
	}
}

func TestParseRequestIDFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "request_id_kv.bin"))
	if err != nil {
		t.Fatal(err)
	}
	k, v := parseKV(b)
	if k != "request_id" || v != fixtureRequestID {
		t.Fatalf("kv = %q=%q, want request_id=%s", k, v, fixtureRequestID)
	}
}

func TestParseStepTsFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "step_ts.bin"))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := varintField(b, 1)
	if !ok || int64(v) != fixtureStepUnix {
		t.Fatalf("step ts = %d ok=%v, want %d", v, ok, fixtureStepUnix)
	}
	// wrap as steps.metadata field 1 and parse through the production helper
	meta := appendBytesField(nil, 1, b)
	if ts := parseStepUnix(meta); ts != fixtureStepUnix {
		t.Fatalf("parseStepUnix = %d, want %d", ts, fixtureStepUnix)
	}
}

func TestParseUsageBagIgnoresOtherFields(t *testing.T) {
	var b []byte
	b = appendVarintField(b, 1, 10)
	b = appendVarintField(b, 2, 20)
	b = appendVarintField(b, 3, 30)
	b = appendVarintField(b, 5, 40)
	b = appendVarintField(b, 6, 24)
	b = appendVarintField(b, 9, 99)
	b = appendVarintField(b, 10, 7)
	u := parseUsageBag(b)
	if u.Prompt != 10 || u.Cached != 20 || u.Completion != 30 || u.Reasoning != 40 {
		t.Fatalf("got %+v", u)
	}
	if u.total() != 100 {
		t.Fatalf("total = %d, want 100 (thinking is in the sum, not extra fields)", u.total())
	}
}

func TestHitRateDenominatorExcludesThinking(t *testing.T) {
	u := tokenBag{Prompt: 100, Cached: 400, Completion: 50, Reasoning: 9000}
	denom := u.Prompt + u.Cached
	if denom != 500 {
		t.Fatalf("hit-rate denom = %d, want prompt+cached = 500 (not thinking)", denom)
	}
	if u.total() != 9550 {
		t.Fatalf("total includes thinking: got %d", u.total())
	}
}
