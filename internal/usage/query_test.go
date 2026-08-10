package usage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestSummaryRejectsInvalidGroupBy is the acceptance test for the whitelist:
// user input must never be interpolated into SQL. Anything that is not an
// exact whitelist name is rejected with a QueryError before any SQL runs.
func TestSummaryRejectsInvalidGroupBy(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.InsertBatch(ctx, []Event{testEvent(time.Now(), "model-a", nil)}); err != nil {
		t.Fatal(err)
	}

	attacks := [][]string{
		{"model; DROP TABLE usage_events"},
		{"model) --"},
		{"(ts_unix/3600)*3600"}, // direct expression injection attempt
		{"model, provider"},
		{"DAY"},
		{"unknown"},
		{"", "model"},
	}
	for _, gb := range attacks {
		_, err := s.Summary(ctx, SummaryQuery{GroupBy: gb})
		var qe *QueryError
		if !errors.As(err, &qe) {
			t.Errorf("group_by %q: want QueryError, got %v", gb, err)
		}
	}

	// the table and its data must be untouched
	rows, err := s.Summary(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatalf("data damaged after injection attempts: %+v", rows)
	}
	// valid whitelist names still work
	rows, err = s.Summary(ctx, SummaryQuery{GroupBy: []string{"model", "day"}})
	if err != nil {
		t.Fatalf("valid group_by rejected: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("valid group_by rows = %d, want 1", len(rows))
	}
}

func TestSummaryFiltersAndLimit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	price := &Price{Input: 1, Output: 1}
	c, _ := costOf(1, 1, 0, 0, "", price)
	now := time.Now()
	const total = 1300
	evs := make([]Event, 0, total)
	for i := 0; i < total; i++ {
		evs = append(evs, Event{
			Ts: now, RequestID: fmt.Sprintf("req-%d", i), Model: fmt.Sprintf("m%04d", i),
			Provider: "prov", Account: "acc", KeyID: "k",
			Stream: i%2 == 0, Success: true, Status: 200,
			PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2, Cost: c, CostStatus: CostStatusOK,
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

	// default limit is 100
	rows, err := s.Summary(ctx, SummaryQuery{GroupBy: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 100 {
		t.Fatalf("default limit: %d rows, want 100", len(rows))
	}

	// limit is clamped at 1000
	rows, err = s.Summary(ctx, SummaryQuery{GroupBy: []string{"model"}, Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1000 {
		t.Fatalf("clamped limit: %d rows, want 1000", len(rows))
	}

	// filter by model (placeholder-bound value)
	rows, err = s.Summary(ctx, SummaryQuery{Model: "m0042"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatalf("model filter: %+v", rows)
	}

	// filter value with SQL metacharacters must be treated as data
	rows, err = s.Summary(ctx, SummaryQuery{Model: "m' OR '1'='1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 0 {
		t.Fatalf("metacharacter filter matched %+v, want 0 rows", rows)
	}

	// filter by stream bool
	stream := true
	rows, err = s.Summary(ctx, SummaryQuery{Stream: &stream})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Requests != total/2 {
		t.Fatalf("stream filter: %d, want %d", rows[0].Requests, total/2)
	}

	// filter by key_id and provider
	rows, err = s.Summary(ctx, SummaryQuery{KeyID: "k", Provider: "prov"})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Requests != total {
		t.Fatalf("key_id/provider filter: %d, want %d", rows[0].Requests, total)
	}

	// unknown filter values simply match nothing
	rows, err = s.Summary(ctx, SummaryQuery{Account: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Requests != 0 {
		t.Fatalf("account filter: %d, want 0", rows[0].Requests)
	}
}

func TestSummaryTimeWindowAndBuckets(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2024, 1, 1, 0, 30, 0, 0, time.UTC)
	e1 := testEvent(base, "a", nil)
	e2 := testEvent(base.Add(time.Hour), "b", nil)
	e3 := testEvent(time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC), "c", nil)
	if err := s.InsertBatch(ctx, []Event{e1, e2, e3}); err != nil {
		t.Fatal(err)
	}

	// time window [from, to]: only the two 2024-01-01 events
	from := base.Unix()
	to := time.Date(2024, 1, 1, 23, 59, 59, 0, time.UTC).Unix()
	rows, err := s.Summary(ctx, SummaryQuery{From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Requests != 2 {
		t.Fatalf("window: %d, want 2", rows[0].Requests)
	}

	// day buckets: 2024-01-01 and 2024-01-02, ordered ascending
	rows, err = s.Summary(ctx, SummaryQuery{GroupBy: []string{"day"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("day buckets: %d, want 2", len(rows))
	}
	day1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	day2 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC).Unix()
	if rows[0].Groups["day"] != day1 || rows[1].Groups["day"] != day2 {
		t.Fatalf("day buckets = %v / %v, want %d / %d", rows[0].Groups["day"], rows[1].Groups["day"], day1, day2)
	}
	if rows[0].Requests != 2 || rows[1].Requests != 1 {
		t.Fatalf("day counts = %d / %d, want 2 / 1", rows[0].Requests, rows[1].Requests)
	}

	// hour buckets within the first day: 00:00 and 01:00
	rows, err = s.Summary(ctx, SummaryQuery{GroupBy: []string{"hour"}, From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("hour buckets: %d, want 2", len(rows))
	}
	hour1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	hour2 := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC).Unix()
	if rows[0].Groups["hour"] != hour1 || rows[1].Groups["hour"] != hour2 {
		t.Fatalf("hour buckets = %v / %v, want %d / %d", rows[0].Groups["hour"], rows[1].Groups["hour"], hour1, hour2)
	}

	// multi group_by: model + day → three rows
	rows, err = s.Summary(ctx, SummaryQuery{GroupBy: []string{"model", "day"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("model+day rows: %d, want 3", len(rows))
	}
	for _, row := range rows {
		if _, ok := row.Groups["model"]; !ok {
			t.Errorf("row missing model group: %+v", row.Groups)
		}
		if _, ok := row.Groups["day"]; !ok {
			t.Errorf("row missing day group: %+v", row.Groups)
		}
	}
}

func TestCostMissingInSummary(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	price := &Price{Input: 2, Output: 2}
	c, _ := costOf(100, 0, 0, 0, "", price)
	now := time.Now()
	if err := s.InsertBatch(ctx, []Event{
		{Ts: now, RequestID: "r1", Model: "a", PromptTokens: 100, TotalTokens: 100, Cost: c, CostStatus: CostStatusOK},
		{Ts: now, RequestID: "r2", Model: "a", PromptTokens: 100, TotalTokens: 100, Cost: nil, CostStatus: CostStatusMissingPrice},
		{Ts: now, RequestID: "r3", Model: "b", PromptTokens: 100, TotalTokens: 100, Cost: c, CostStatus: CostStatusOK},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Summary(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Requests != 3 || rows[0].CostMissingRequests != 1 {
		t.Fatalf("ungrouped: requests=%d missing=%d, want 3/1", rows[0].Requests, rows[0].CostMissingRequests)
	}

	byModel, err := s.Summary(ctx, SummaryQuery{GroupBy: []string{"model"}})
	if err != nil {
		t.Fatal(err)
	}
	var aRow, bRow *SummaryRow
	for i := range byModel {
		switch byModel[i].Groups["model"] {
		case "a":
			aRow = &byModel[i]
		case "b":
			bRow = &byModel[i]
		}
	}
	if aRow == nil || bRow == nil {
		t.Fatalf("missing rows: %+v", byModel)
	}
	if aRow.Requests != 2 || aRow.CostMissingRequests != 1 {
		t.Fatalf("model a: requests=%d missing=%d, want 2/1", aRow.Requests, aRow.CostMissingRequests)
	}
	if bRow.Requests != 1 || bRow.CostMissingRequests != 0 {
		t.Fatalf("model b: requests=%d missing=%d, want 1/0", bRow.Requests, bRow.CostMissingRequests)
	}
	if aRow.CostUSD == nil {
		t.Fatalf("model a cost should be the sum of priced events only")
	}
	wantA := 2 * 100.0 / 1e6
	if *aRow.CostUSD != wantA {
		t.Fatalf("model a cost = %v, want %v", *aRow.CostUSD, wantA)
	}
	if bRow.CostUSD == nil || *bRow.CostUSD != wantA {
		t.Fatalf("model b cost = %v, want %v", bRow.CostUSD, wantA)
	}
}

func TestSummaryStreamSuccessGroups(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	c, _ := costOf(0, 0, 0, 0, "", &Price{Input: 1, Output: 1})
	if err := s.InsertBatch(ctx, []Event{
		{Ts: now, RequestID: "r1", Model: "a", Stream: true, Success: true, Cost: c, CostStatus: CostStatusOK},
		{Ts: now, RequestID: "r2", Model: "a", Stream: false, Success: true, Cost: c, CostStatus: CostStatusOK},
		{Ts: now, RequestID: "r3", Model: "a", Stream: true, Success: false, Cost: c, CostStatus: CostStatusOK},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Summary(ctx, SummaryQuery{GroupBy: []string{"stream", "success"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("stream+success rows: %d, want 3", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		seen[fmt.Sprintf("%v/%v", row.Groups["stream"], row.Groups["success"])] = true
	}
	for _, combo := range []string{"1/1", "0/1", "1/0"} {
		if !seen[combo] {
			t.Errorf("missing group %s (have %v)", combo, seen)
		}
	}
}
