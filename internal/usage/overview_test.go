package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

// approxEqual compares two USD amounts with a relative tolerance: sums are
// built by float64 addition (2e-6 * 300 is not bit-exact), so exact equality
// would be flaky; the tolerance is far below any meaningful money amount.
func approxEqual(a, b float64) bool {
	if a == b {
		return true
	}
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return math.Abs(a-b) <= 1e-9*scale
}

// checkOverviewVsSummaryRow asserts that Overview and an ungrouped SummaryRow
// agree on every field the two queries share. This is the anti-drift guard
// for the shared where-clause builder.
func checkOverviewVsSummaryRow(t *testing.T, o *Overview, r SummaryRow) {
	t.Helper()
	if o.Requests != r.Requests {
		t.Errorf("requests: overview=%d summary=%d", o.Requests, r.Requests)
	}
	if o.PromptTokens != r.PromptTokens {
		t.Errorf("prompt_tokens: overview=%d summary=%d", o.PromptTokens, r.PromptTokens)
	}
	if o.CompletionTokens != r.CompletionTokens {
		t.Errorf("completion_tokens: overview=%d summary=%d", o.CompletionTokens, r.CompletionTokens)
	}
	if o.TotalTokens != r.TotalTokens {
		t.Errorf("total_tokens: overview=%d summary=%d", o.TotalTokens, r.TotalTokens)
	}
	if o.CachedTokens != r.CachedTokens {
		t.Errorf("cached_tokens: overview=%d summary=%d", o.CachedTokens, r.CachedTokens)
	}
	if o.ReasoningTokens != r.ReasoningTokens {
		t.Errorf("reasoning_tokens: overview=%d summary=%d", o.ReasoningTokens, r.ReasoningTokens)
	}
	if o.CacheWriteTokens != r.CacheWriteTokens {
		t.Errorf("cache_write_tokens: overview=%d summary=%d", o.CacheWriteTokens, r.CacheWriteTokens)
	}
	if (o.TotalCost == nil) != (r.CostUSD == nil) {
		t.Errorf("cost nil-ness: overview=%v summary=%v", o.TotalCost, r.CostUSD)
	} else if o.TotalCost != nil && !approxEqual(*o.TotalCost, *r.CostUSD) {
		t.Errorf("cost: overview=%v summary=%v", *o.TotalCost, *r.CostUSD)
	}
	if o.CostMissingRequests != r.CostMissingRequests {
		t.Errorf("cost_missing_requests: overview=%d summary=%d", o.CostMissingRequests, r.CostMissingRequests)
	}
}

// sumSummaryRows folds grouped Summary rows back into a single row. Used to
// prove Overview equals the sum of the un-truncated per-group breakdown.
func sumSummaryRows(rows []SummaryRow) SummaryRow {
	var out SummaryRow
	var costSum float64
	hasCost := false
	for _, r := range rows {
		out.Requests += r.Requests
		out.PromptTokens += r.PromptTokens
		out.CompletionTokens += r.CompletionTokens
		out.TotalTokens += r.TotalTokens
		out.CachedTokens += r.CachedTokens
		out.ReasoningTokens += r.ReasoningTokens
		out.CacheWriteTokens += r.CacheWriteTokens
		out.CostMissingRequests += r.CostMissingRequests
		if r.CostUSD != nil {
			hasCost = true
			costSum += *r.CostUSD
		}
	}
	if hasCost {
		out.CostUSD = &costSum
	}
	return out
}

// TestOverviewMatchesSummaryTotals is the anti-drift acceptance test: after
// inserting known data, Overview and the un-truncated Summary (ungrouped AND
// the sum of the per-model rows) must report identical requests, token sums
// and cost fields. If the two where clauses ever diverge, this fails.
func TestOverviewMatchesSummaryTotals(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	price := &Price{Input: 1, Output: 2}
	c, _ := costOf(100, 50, 0, 0, "", price) // 0.0002
	if err := s.InsertBatch(ctx, []Event{
		{Ts: now, RequestID: "r1", Model: "a", Provider: "p", Account: "ac", KeyID: "k",
			Stream: true, Success: true, Status: 200,
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
			Cost: c, CostStatus: CostStatusOK},
		{Ts: now, RequestID: "r2", Model: "a", Provider: "p", Account: "ac", KeyID: "k",
			Stream: false, Success: true, Status: 200,
			PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300,
			Cost: nil, CostStatus: CostStatusMissingPrice},
		{Ts: now, RequestID: "r3", Model: "b", Provider: "p", Account: "ac", KeyID: "k",
			Stream: true, Success: false, Status: 500,
			PromptTokens: 300, CompletionTokens: 150, TotalTokens: 450,
			Cost: c, CostStatus: CostStatusOK},
	}); err != nil {
		t.Fatal(err)
	}

	o, err := s.Overview(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if o.Requests != 3 || o.PromptTokens != 600 || o.CompletionTokens != 300 || o.TotalTokens != 900 {
		t.Fatalf("overview totals = %+v", o)
	}
	if o.TotalCost == nil || !approxEqual(*o.TotalCost, 0.0004) {
		t.Fatalf("overview cost = %v, want 0.0004", o.TotalCost)
	}
	if o.CostMissingRequests != 1 {
		t.Fatalf("overview cost_missing = %d, want 1", o.CostMissingRequests)
	}
	if o.FailedRequests != 1 || o.StreamingRequests != 2 {
		t.Fatalf("overview failed=%d streamed=%d, want 1/2", o.FailedRequests, o.StreamingRequests)
	}
	// All three events carry an empty Source → stored as NULL → the whole
	// range belongs to the OpenAI bucket (legacy-row fallback).
	if o.OpenAIRequests != 3 || o.OpenAIPromptTokens != 600 || o.OpenAICachedTokens != 0 {
		t.Fatalf("openai split = %d/%d/%d, want 3 requests / 600 prompt / 0 cached", o.OpenAIRequests, o.OpenAIPromptTokens, o.OpenAICachedTokens)
	}
	if o.AnthropicRequests != 0 || o.AnthropicPromptTokens != 0 || o.AnthropicCachedTokens != 0 || o.AnthropicCacheWriteTokens != 0 {
		t.Fatalf("anthropic split must be zero, got %d/%d/%d/%d", o.AnthropicRequests, o.AnthropicPromptTokens, o.AnthropicCachedTokens, o.AnthropicCacheWriteTokens)
	}

	// ungrouped Summary must agree field-for-field
	rows, err := s.Summary(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("ungrouped summary: %d rows, want 1", len(rows))
	}
	checkOverviewVsSummaryRow(t, o, rows[0])

	// the sum of the full per-model breakdown must agree too
	byModel, err := s.Summary(ctx, SummaryQuery{GroupBy: []string{"model"}, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	checkOverviewVsSummaryRow(t, o, sumSummaryRows(byModel))
}

// TestOverviewZeroTotalFallsBackToPromptPlusCompletion: a stored row with
// total_tokens=0 (legacy OpenAI parse that omitted the field) still
// contributes prompt+completion to the report "词元" figure. Summary and
// Overview share the same expression.
func TestOverviewZeroTotalFallsBackToPromptPlusCompletion(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertBatch(ctx, []Event{
		{Ts: now, RequestID: "r1", Model: "a",
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 0,
			Source: SourceOpenAI},
	}); err != nil {
		t.Fatal(err)
	}
	o, err := s.Overview(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if o.PromptTokens != 100 || o.CompletionTokens != 50 {
		t.Fatalf("prompt/completion = %d/%d, want 100/50", o.PromptTokens, o.CompletionTokens)
	}
	if o.TotalTokens != 150 {
		t.Fatalf("total_tokens = %d, want 150 (zero total falls back to prompt+completion)", o.TotalTokens)
	}
	rows, err := s.Summary(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TotalTokens != 150 {
		t.Fatalf("summary total_tokens = %+v, want 150", rows)
	}
}

// TestOverviewIgnoresLimitAndGroupBy proves the reason Overview exists: with
// more groups than the default Summary limit, the truncated per-group rows
// sum to less than the true total, while Overview still returns the full
// numbers. GroupBy and Limit passed in the query must be ignored.
func TestOverviewIgnoresLimitAndGroupBy(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	price := &Price{Input: 1, Output: 1}
	c, _ := costOf(1, 1, 0, 0, "", price) // 2e-6
	const total = 300
	evs := make([]Event, 0, total)
	for i := 0; i < total; i++ {
		evs = append(evs, Event{
			Ts: now, RequestID: fmt.Sprintf("req-%d", i), Model: fmt.Sprintf("m%04d", i),
			Provider: "p", Account: "a", KeyID: "k",
			Stream: i%2 == 0, Success: i%3 != 0, Status: 200,
			PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2,
			Cost: c, CostStatus: CostStatusOK,
		})
	}
	for i := 0; i < len(evs); i += 100 {
		end := i + 100
		if end > len(evs) {
			end = len(evs)
		}
		if err := s.InsertBatch(ctx, evs[i:end]); err != nil {
			t.Fatal(err)
		}
	}

	// GroupBy and Limit are set on purpose: Overview must ignore both.
	o, err := s.Overview(ctx, SummaryQuery{GroupBy: []string{"model"}, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if o.Requests != total || o.PromptTokens != total || o.CompletionTokens != total || o.TotalTokens != 2*total {
		t.Fatalf("overview totals = %+v, want %d requests / %d tokens", o, total, 2*total)
	}
	if o.StreamingRequests != total/2 {
		t.Fatalf("streamed = %d, want %d", o.StreamingRequests, total/2)
	}
	if o.FailedRequests != 100 {
		t.Fatalf("failed = %d, want 100 (every third request)", o.FailedRequests)
	}
	if o.CostMissingRequests != 0 || o.TotalCost == nil || !approxEqual(*o.TotalCost, 300*2e-6) {
		t.Fatalf("cost = %v missing=%d, want ~6e-4 / 0", o.TotalCost, o.CostMissingRequests)
	}

	// full (un-truncated) grouped Summary sums to exactly the Overview
	full, err := s.Summary(ctx, SummaryQuery{GroupBy: []string{"model"}, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != total {
		t.Fatalf("full grouped summary: %d rows, want %d", len(full), total)
	}
	checkOverviewVsSummaryRow(t, o, sumSummaryRows(full))

	// default-limit grouped Summary is truncated: 100 rows, sum < total.
	// This is the exact failure mode Overview exists to avoid.
	trunc, err := s.Summary(ctx, SummaryQuery{GroupBy: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(trunc) != 100 {
		t.Fatalf("default-limit summary: %d rows, want 100", len(trunc))
	}
	if summed := sumSummaryRows(trunc); summed.Requests >= total {
		t.Fatalf("truncated summary summed to %d, want < %d (limit truncation must not reach Overview)", summed.Requests, total)
	}
}

// TestOverviewNoData: an empty range must return an all-zero Overview with
// nil TotalCost — never an error, never sql.ErrNoRows. COUNT(*) is 0 but
// every SUM is NULL on zero rows, so all SUM columns must scan as nulls.
func TestOverviewNoData(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for name, q := range map[string]SummaryQuery{
		"empty-table":       {},
		"empty-time-window": {From: 1, To: 2},
		"no-model-match":    {Model: "nope"},
	} {
		o, err := s.Overview(ctx, q)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if o.Requests != 0 || o.PromptTokens != 0 || o.CompletionTokens != 0 ||
			o.TotalTokens != 0 || o.CachedTokens != 0 || o.ReasoningTokens != 0 ||
			o.CacheWriteTokens != 0 || o.CostMissingRequests != 0 ||
			o.FailedRequests != 0 || o.StreamingRequests != 0 {
			t.Errorf("%s: nonzero fields: %+v", name, o)
		}
		if o.OpenAIRequests != 0 || o.OpenAIPromptTokens != 0 || o.OpenAICachedTokens != 0 ||
			o.AnthropicRequests != 0 || o.AnthropicPromptTokens != 0 ||
			o.AnthropicCachedTokens != 0 || o.AnthropicCacheWriteTokens != 0 {
			t.Errorf("%s: nonzero source-split fields: %+v", name, o)
		}
		if o.TotalCost != nil {
			t.Errorf("%s: TotalCost = %v, want nil (no rows priced)", name, *o.TotalCost)
		}
	}
}

// TestOverviewAllCostsMissing: when every row in range has a NULL cost_usd,
// TotalCost must be nil (\"cannot be computed\"), never 0 (\"spent
// nothing\"). SQL SUM over an all-NULL set is NULL; it must map to nil, not
// be COALESCEd to zero.
func TestOverviewAllCostsMissing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertBatch(ctx, []Event{
		{Ts: now, RequestID: "r1", Model: "a", PromptTokens: 10, TotalTokens: 10, Cost: nil, CostStatus: CostStatusMissingPrice},
		{Ts: now, RequestID: "r2", Model: "b", PromptTokens: 20, TotalTokens: 20, Cost: nil, CostStatus: CostStatusMissingPrice},
	}); err != nil {
		t.Fatal(err)
	}
	o, err := s.Overview(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if o.Requests != 2 || o.PromptTokens != 30 {
		t.Fatalf("overview = %+v, want 2 requests / 30 prompt tokens", o)
	}
	if o.TotalCost != nil {
		t.Fatalf("TotalCost = %v, want nil when every cost_usd is NULL", *o.TotalCost)
	}
	if o.CostMissingRequests != 2 {
		t.Fatalf("cost_missing = %d, want 2", o.CostMissingRequests)
	}
}

// TestOverviewPartialCostMissing: with a mix of priced and unpriced rows,
// TotalCost must be the sum of the priced rows only, and CostMissingRequests
// must report the unpriced count so callers can flag the total as partial.
func TestOverviewPartialCostMissing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	price := &Price{Input: 2, Output: 2}
	c, _ := costOf(100, 0, 0, 0, "", price) // 0.0002
	if err := s.InsertBatch(ctx, []Event{
		{Ts: now, RequestID: "r1", Model: "a", PromptTokens: 100, TotalTokens: 100, Cost: c, CostStatus: CostStatusOK},
		{Ts: now, RequestID: "r2", Model: "a", PromptTokens: 100, TotalTokens: 100, Cost: c, CostStatus: CostStatusOK},
		{Ts: now, RequestID: "r3", Model: "b", PromptTokens: 100, TotalTokens: 100, Cost: c, CostStatus: CostStatusOK},
		{Ts: now, RequestID: "r4", Model: "c", PromptTokens: 100, TotalTokens: 100, Cost: nil, CostStatus: CostStatusMissingPrice},
	}); err != nil {
		t.Fatal(err)
	}
	o, err := s.Overview(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if o.Requests != 4 || o.CostMissingRequests != 1 {
		t.Fatalf("requests=%d missing=%d, want 4/1", o.Requests, o.CostMissingRequests)
	}
	if o.TotalCost == nil {
		t.Fatalf("TotalCost is nil, want sum of priced rows")
	}
	if !approxEqual(*o.TotalCost, 0.0006) {
		t.Fatalf("TotalCost = %v, want 0.0006 (3 priced rows, one NULL ignored)", *o.TotalCost)
	}
}

// TestOverviewFiltersMatchSummary drives the same filter values through
// Overview and the ungrouped Summary and requires identical results: the
// shared where-clause builder must behave the same for time windows, string
// filters and bool filters (including combinations and empty results).
func TestOverviewFiltersMatchSummary(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	price := &Price{Input: 1, Output: 2}
	c, _ := costOf(100, 50, 0, 0, "", price) // 0.0002
	if err := s.InsertBatch(ctx, []Event{
		{Ts: base, RequestID: "e1", Model: "m1", Provider: "p1", Account: "a1", KeyID: "k1",
			Stream: true, Success: true, Status: 200,
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, Cost: c, CostStatus: CostStatusOK},
		{Ts: base.Add(time.Hour), RequestID: "e2", Model: "m2", Provider: "p1", Account: "a1", KeyID: "k2",
			Stream: false, Success: true, Status: 200,
			PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300, Cost: nil, CostStatus: CostStatusMissingPrice},
		{Ts: base.Add(2 * time.Hour), RequestID: "e3", Model: "m1", Provider: "p2", Account: "a2", KeyID: "k1",
			Stream: true, Success: false, Status: 500,
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, Cost: c, CostStatus: CostStatusOK},
		{Ts: base.Add(3 * time.Hour), RequestID: "e4", Model: "m2", Provider: "p2", Account: "a2", KeyID: "k2",
			Stream: false, Success: false, Status: 500,
			PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300, Cost: nil, CostStatus: CostStatusMissingPrice},
	}); err != nil {
		t.Fatal(err)
	}

	streamTrue, streamFalse := true, false
	successTrue, successFalse := true, false
	cases := []struct {
		name string
		q    SummaryQuery
		want int64
	}{
		{"all", SummaryQuery{}, 4},
		{"time-window", SummaryQuery{From: base.Unix(), To: base.Add(90 * time.Minute).Unix()}, 2},
		{"model", SummaryQuery{Model: "m1"}, 2},
		{"provider", SummaryQuery{Provider: "p2"}, 2},
		{"account", SummaryQuery{Account: "a2"}, 2},
		{"keyid", SummaryQuery{KeyID: "k2"}, 2},
		{"stream-true", SummaryQuery{Stream: &streamTrue}, 2},
		{"stream-false", SummaryQuery{Stream: &streamFalse}, 2},
		{"success-true", SummaryQuery{Success: &successTrue}, 2},
		{"success-false", SummaryQuery{Success: &successFalse}, 2},
		{"model+stream", SummaryQuery{Model: "m1", Stream: &streamTrue}, 2},
		{"window+success-empty", SummaryQuery{From: base.Unix(), To: base.Add(90 * time.Minute).Unix(), Success: &successFalse}, 0},
		{"provider+success", SummaryQuery{Provider: "p1", Success: &successTrue}, 2},
	}
	for _, tc := range cases {
		o, err := s.Overview(ctx, tc.q)
		if err != nil {
			t.Fatalf("%s: overview error: %v", tc.name, err)
		}
		rows, err := s.Summary(ctx, tc.q)
		if err != nil {
			t.Fatalf("%s: summary error: %v", tc.name, err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s: summary rows = %d, want 1", tc.name, len(rows))
		}
		if o.Requests != tc.want {
			t.Errorf("%s: overview requests = %d, want %d", tc.name, o.Requests, tc.want)
		}
		checkOverviewVsSummaryRow(t, o, rows[0])
	}
}

// TestOverviewPlaceholderBinding: filter values containing SQL metacharacters
// must be treated as data, never spliced into the statement. The query must
// match the literal value when present and match nothing for a forged one,
// and the table must stay intact afterwards.
func TestOverviewPlaceholderBinding(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	c, _ := costOf(1, 1, 0, 0, "", &Price{Input: 1, Output: 1})
	if err := s.InsertBatch(ctx, []Event{
		{Ts: now, RequestID: "r1", Model: `weird"model`, Provider: "p'1", Account: "a--", KeyID: "k;x",
			Stream: true, Success: true, Status: 200,
			PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2, Cost: c, CostStatus: CostStatusOK},
	}); err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		name string
		q    SummaryQuery
		want int64
	}{
		{"literal-model", SummaryQuery{Model: `weird"model`}, 1},
		{"literal-provider", SummaryQuery{Provider: "p'1"}, 1},
		{"literal-account", SummaryQuery{Account: "a--"}, 1},
		{"literal-keyid", SummaryQuery{KeyID: "k;x"}, 1},
		{"forged-model", SummaryQuery{Model: "x' OR '1'='1"}, 0},
		{"forged-provider", SummaryQuery{Provider: "p' OR '1'='1"}, 0},
		{"forged-keyid", SummaryQuery{KeyID: "k; DROP TABLE usage_events"}, 0},
	}
	for _, tc := range checks {
		o, err := s.Overview(ctx, tc.q)
		if err != nil {
			t.Fatalf("%s: overview error: %v", tc.name, err)
		}
		if o.Requests != tc.want {
			t.Errorf("%s: requests = %d, want %d", tc.name, o.Requests, tc.want)
		}
		// Summary must agree (same shared builder)
		rows, err := s.Summary(ctx, tc.q)
		if err != nil {
			t.Fatalf("%s: summary error: %v", tc.name, err)
		}
		if rows[0].Requests != tc.want {
			t.Errorf("%s: summary requests = %d, want %d", tc.name, rows[0].Requests, tc.want)
		}
	}

	// nothing was injected or dropped: the single row is still there
	o, err := s.Overview(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if o.Requests != 1 {
		t.Fatalf("data damaged by injection attempts: %d rows, want 1", o.Requests)
	}
}

// TestOverviewSourceSplit is the acceptance test for the per-source cache
// statistics behind the two summary segments: usage_source='openai', NULL
// (pre-v2 legacy) and ” (no parsed usage payload) rows all land in the
// OpenAI bucket — the same partition ComputeCost prices with the OpenAI
// formula — while usage_source='anthropic' rows land in the Anthropic
// bucket, each with its own prompt/cached/cache_write sums. The Anthropic
// denominator is NOT assembled here — the renderer adds
// prompt + cached + cache_write — so the three sums must be reported raw.
func TestOverviewSourceSplit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertBatch(ctx, []Event{
		{Ts: now, RequestID: "o1", Model: "gpt", Source: SourceOpenAI,
			PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100, CachedTokens: 900},
		{Ts: now, RequestID: "a1", Model: "claude", Source: SourceAnthropic,
			PromptTokens: 1, CompletionTokens: 50, TotalTokens: 51, CachedTokens: 500, CacheWriteTokens: 0},
		// Post-v2 row built without a parsed usage payload: Source stays ""
		// and is stored as the empty string, not NULL.
		{Ts: now, RequestID: "legacy-empty", Model: "old", Source: "",
			PromptTokens: 200, CompletionTokens: 20, TotalTokens: 220, CachedTokens: 150},
		{Ts: now, RequestID: "a2", Model: "claude", Source: SourceAnthropic,
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CachedTokens: 50, CacheWriteTokens: 40},
	}); err != nil {
		t.Fatal(err)
	}
	// Pre-v2 legacy row: InsertBatch always writes a string, so write a real
	// NULL usage_source directly, exactly as a v1-era row looks after the v2
	// migration added the column.
	db, err := sql.Open("sqlite", dsn(s.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO usage_events
		(ts_unix, request_id, model, prompt_tokens, completion_tokens, total_tokens, cached_tokens, usage_source)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`,
		now.Unix(), "legacy-null", "old", 100, 10, 110, 80); err != nil {
		t.Fatal(err)
	}

	o, err := s.Overview(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	// OpenAI bucket: the explicit openai row + the NULL legacy row + the
	// empty-string legacy row.
	if o.OpenAIRequests != 3 {
		t.Errorf("openai_requests = %d, want 3 (openai + NULL + empty-string)", o.OpenAIRequests)
	}
	if o.OpenAIPromptTokens != 1300 {
		t.Errorf("openai_prompt_tokens = %d, want 1300 (1000 + 100 + 200)", o.OpenAIPromptTokens)
	}
	if o.OpenAICachedTokens != 1130 {
		t.Errorf("openai_cached_tokens = %d, want 1130 (900 + 80 + 150)", o.OpenAICachedTokens)
	}
	// Anthropic bucket: both anthropic rows.
	if o.AnthropicRequests != 2 {
		t.Errorf("anthropic_requests = %d, want 2", o.AnthropicRequests)
	}
	if o.AnthropicPromptTokens != 101 {
		t.Errorf("anthropic_prompt_tokens = %d, want 101 (1 + 100)", o.AnthropicPromptTokens)
	}
	if o.AnthropicCachedTokens != 550 {
		t.Errorf("anthropic_cached_tokens = %d, want 550 (500 + 50)", o.AnthropicCachedTokens)
	}
	if o.AnthropicCacheWriteTokens != 40 {
		t.Errorf("anthropic_cache_write_tokens = %d, want 40", o.AnthropicCacheWriteTokens)
	}
	// Cross-checks against the global totals: the buckets partition the rows.
	if o.OpenAIRequests+o.AnthropicRequests != o.Requests {
		t.Errorf("request splits %d+%d != requests %d", o.OpenAIRequests, o.AnthropicRequests, o.Requests)
	}
	if o.OpenAIPromptTokens+o.AnthropicPromptTokens != o.PromptTokens {
		t.Errorf("prompt splits %d+%d != prompt %d", o.OpenAIPromptTokens, o.AnthropicPromptTokens, o.PromptTokens)
	}
	if o.OpenAICachedTokens+o.AnthropicCachedTokens != o.CachedTokens {
		t.Errorf("cached splits %d+%d != cached %d", o.OpenAICachedTokens, o.AnthropicCachedTokens, o.CachedTokens)
	}

	// A model filter narrows the splits to the matching rows only: both
	// claude rows are anthropic, so the openai bucket must be empty.
	o, err = s.Overview(ctx, SummaryQuery{Model: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if o.OpenAIRequests != 0 || o.OpenAIPromptTokens != 0 || o.OpenAICachedTokens != 0 {
		t.Errorf("filtered openai bucket = %+v, want all-zero", o)
	}
	if o.AnthropicRequests != 2 || o.AnthropicPromptTokens != 101 || o.AnthropicCachedTokens != 550 || o.AnthropicCacheWriteTokens != 40 {
		t.Errorf("filtered anthropic bucket = %+v, want 2/101/550/40", o)
	}
}

// TestOverviewJSONFields guards the JSON contract: the per-source fields are
// ADDITIVE — every pre-existing key must keep its name and type, and the new
// split keys must be present. Consumers (jq pipelines, the CLI --json path)
// rely on the old keys never changing meaning.
func TestOverviewJSONFields(t *testing.T) {
	ov := &Overview{
		Requests: 3, PromptTokens: 1101, CompletionTokens: 210, TotalTokens: 1311,
		CachedTokens: 1480, ReasoningTokens: 0, CacheWriteTokens: 40,
		TotalCost: ptr64(0.5), CostMissingRequests: 0,
		FailedRequests: 1, StreamingRequests: 2,
		OpenAIRequests: 2, OpenAIPromptTokens: 1100, OpenAICachedTokens: 980,
		AnthropicRequests: 1, AnthropicPromptTokens: 1, AnthropicCachedTokens: 500, AnthropicCacheWriteTokens: 40,
	}
	b, err := json.Marshal(ov)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	// Every key must exist and be a JSON number (int64 marshals as number).
	for _, k := range []string{
		"requests", "prompt_tokens", "completion_tokens", "total_tokens",
		"cached_tokens", "reasoning_tokens", "cache_write_tokens",
		"total_cost", "cost_missing_requests", "failed_requests", "streaming_requests",
		"openai_requests", "openai_prompt_tokens", "openai_cached_tokens",
		"anthropic_requests", "anthropic_prompt_tokens", "anthropic_cached_tokens",
		"anthropic_cache_write_tokens",
	} {
		v, ok := m[k]
		if !ok {
			t.Errorf("json key %q missing: %s", k, b)
			continue
		}
		if _, isNum := v.(float64); !isNum {
			t.Errorf("json key %q has type %T, want number", k, v)
		}
	}
	if m["total_cost"].(float64) != 0.5 {
		t.Errorf("total_cost = %v, want 0.5", m["total_cost"])
	}
	if m["openai_prompt_tokens"].(float64) != 1100 || m["anthropic_cached_tokens"].(float64) != 500 {
		t.Errorf("split values wrong: %s", b)
	}
}
