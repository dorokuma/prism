package main

import (
	"context"
	"os"
	"time"

	"github.com/dorokuma/prism/internal/agyusage"
	"github.com/dorokuma/prism/internal/planusage"
	"github.com/dorokuma/prism/internal/usage"
)

// agyIndexPath / agyConvDir / geminiEstimatePath are the production defaults
// redirected by CLI tests so a live ~/.gemini tree cannot leak into fixtures.
var (
	agyIndexPath       = agyusage.DefaultIndexPath
	agyConvDir         = agyusage.DefaultConversationsDir()
	geminiEstimatePath = planusage.DefaultGeminiEstimatePath
)

func openAgyIndex() *agyusage.Index {
	dir := agyConvDir
	if dir == "" {
		dir = agyusage.DefaultConversationsDir()
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil
	}
	path := agyIndexPath
	if path == "" {
		path = agyusage.DefaultIndexPath
	}
	idx, err := agyusage.Open(path, dir)
	if err != nil {
		return nil
	}
	return idx
}

func toAgyQuery(q usage.SummaryQuery) agyusage.Query {
	return agyusage.Query{
		From:     q.From,
		To:       q.To,
		GroupBy:  q.GroupBy,
		Model:    q.Model,
		Provider: q.Provider,
		Account:  q.Account,
		KeyID:    q.KeyID,
		Stream:   q.Stream,
		Success:  q.Success,
	}
}

func agyToSummary(rows []agyusage.Row) []usage.SummaryRow {
	out := make([]usage.SummaryRow, len(rows))
	for i, r := range rows {
		out[i] = usage.SummaryRow{
			Groups:             r.Groups,
			Requests:           r.Requests,
			PromptTokens:       r.PromptTokens,
			CachedTokens:       r.CachedTokens,
			CompletionTokens:   r.CompletionTokens,
			ReasoningTokens:    r.ReasoningTokens,
			TotalTokens:        r.TotalTokens,
			HitRateInputTokens: r.HitRateInputTokens,
		}
	}
	return out
}

func loadAgyRows(ctx context.Context, q usage.SummaryQuery, weekDefault bool, now time.Time) []usage.SummaryRow {
	idx := openAgyIndex()
	if idx == nil {
		return nil
	}
	defer idx.Close()
	_ = idx.Refresh(ctx)
	aq := q
	if weekDefault {
		if v := planusage.GeminiWeekStartUnix(nil, geminiEstimatePath, now); v > 0 {
			aq.From = v
		}
	}
	rows, err := idx.Query(ctx, toAgyQuery(aq))
	if err != nil {
		return nil
	}
	return agyToSummary(rows)
}

func agyQueryFunc(idx *agyusage.Index) func(context.Context, usage.SummaryQuery) ([]usage.SummaryRow, error) {
	if idx == nil {
		return nil
	}
	return func(ctx context.Context, q usage.SummaryQuery) ([]usage.SummaryRow, error) {
		rows, err := idx.Query(ctx, toAgyQuery(q))
		if err != nil {
			return nil, err
		}
		return agyToSummary(rows), nil
	}
}

func agySumFunc(idx *agyusage.Index) planusage.GrokTokenSum {
	if idx == nil {
		return nil
	}
	return idx.SumTokens
}
