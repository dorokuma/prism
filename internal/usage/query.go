package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// groupByColumns is the whitelist mapping user-supplied group_by and filter
// names to fixed SQL column expressions. User input is never interpolated
// into SQL: unknown names are rejected before any SQL is built, and every
// value is bound through a placeholder.
var groupByColumns = map[string]string{
	"model":    "model",
	"provider": "provider",
	"account":  "account",
	"key_id":   "key_id",
	"stream":   "stream",
	"success":  "success",
	"hour":     "(ts_unix/3600)*3600",
	"day":      "(ts_unix/86400)*86400",
}

// ValidGroupBy reports whether name is an allowed group_by key (the same
// whitelist Summary enforces). The CLI uses it to reject a bad --by value
// before opening the database; Summary applies the identical check at query
// time, so the two can never disagree.
func ValidGroupBy(name string) bool {
	_, ok := groupByColumns[name]
	return ok
}

// SummaryQuery describes an aggregation over usage_events. GroupBy and filter
// names must come from the whitelist above. From/To are unix seconds,
// inclusive; 0 means unbounded on that side. Overview accepts the same
// struct but ignores GroupBy and Limit: it aggregates the whole filtered
// range, so its totals are never truncated.
type SummaryQuery struct {
	From     int64
	To       int64
	GroupBy  []string
	Model    string
	Provider string
	Account  string
	KeyID    string
	Stream   *bool
	Success  *bool
	Limit    int
}

// SummaryRow is one aggregated group (or the single overall row when no
// group_by is requested).
type SummaryRow struct {
	Groups              map[string]any `json:"groups"`
	Requests            int64          `json:"requests"`
	PromptTokens        int64          `json:"prompt_tokens"`
	CompletionTokens    int64          `json:"completion_tokens"`
	TotalTokens         int64          `json:"total_tokens"`
	CachedTokens        int64          `json:"cached_tokens"`
	ReasoningTokens     int64          `json:"reasoning_tokens"`
	CacheWriteTokens    int64          `json:"cache_write_tokens"`
	CostUSD             *float64       `json:"cost_usd,omitempty"`
	CostMissingRequests int64          `json:"cost_missing_requests"`
}

// QueryError marks a client-side validation error (bad group_by name, bad
// parameter value). Handlers map it to 400; any other error is a database
// problem and maps to 503.
type QueryError struct {
	Msg string
}

func (e *QueryError) Error() string { return "usage: " + e.Msg }

const (
	summaryDefaultLimit = 100
	summaryMaxLimit     = 1000
	// totalTokensSumExpr is the report "词元" aggregate. Prefer the stored
	// total; a row whose total_tokens is 0 (legacy OpenAI parses that
	// omitted the field) still contributes prompt+completion so the
	// header cannot read 0 while 输入/输出词元 are non-zero. Shared by
	// Summary and Overview so the two cannot drift.
	totalTokensSumExpr = `SUM(CASE WHEN total_tokens > 0 THEN total_tokens ELSE prompt_tokens + completion_tokens END)`
)

// buildSummaryWhere renders the shared WHERE clause for q's filter fields
// and returns it together with the placeholder-bound arguments. It is the
// single implementation used by both Summary and Overview so the two queries
// can never drift apart: every value is bound through a "?" placeholder and
// column names are hard-coded in the function body, never interpolated from
// user input.
func buildSummaryWhere(q SummaryQuery) (string, []any) {
	var sb strings.Builder
	sb.WriteString(" WHERE 1=1")
	args := make([]any, 0, 8)
	if q.From > 0 {
		sb.WriteString(" AND ts_unix >= ?")
		args = append(args, q.From)
	}
	if q.To > 0 {
		sb.WriteString(" AND ts_unix <= ?")
		args = append(args, q.To)
	}
	addFilter := func(col, val string) {
		if val == "" {
			return
		}
		sb.WriteString(" AND " + col + " = ?")
		args = append(args, val)
	}
	addFilter("model", q.Model)
	addFilter("provider", q.Provider)
	addFilter("account", q.Account)
	addFilter("key_id", q.KeyID)
	if q.Stream != nil {
		sb.WriteString(" AND stream = ?")
		args = append(args, boolInt(*q.Stream))
	}
	if q.Success != nil {
		sb.WriteString(" AND success = ?")
		args = append(args, boolInt(*q.Success))
	}
	return sb.String(), args
}

// Summary runs the aggregated query on the read pool. All values are bound
// as parameters; group_by and filter column names come exclusively from the
// whitelist.
func (s *SQLiteStore) Summary(ctx context.Context, q SummaryQuery) ([]SummaryRow, error) {
	db := s.readPool()
	if db == nil {
		return nil, errors.New("usage: store not open")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = summaryDefaultLimit
	}
	if limit > summaryMaxLimit {
		limit = summaryMaxLimit
	}

	var groupExprs []string
	var groupNames []string
	seen := make(map[string]bool, len(q.GroupBy))
	for _, g := range q.GroupBy {
		expr, ok := groupByColumns[g]
		if !ok {
			return nil, &QueryError{Msg: fmt.Sprintf("invalid group_by %q", g)}
		}
		if !seen[g] {
			seen[g] = true
			groupExprs = append(groupExprs, expr)
			groupNames = append(groupNames, g)
		}
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	if len(groupExprs) > 0 {
		sb.WriteString(strings.Join(groupExprs, ", "))
		sb.WriteString(", ")
	}
	sb.WriteString(`COUNT(*) AS requests,
		SUM(prompt_tokens), SUM(completion_tokens), ` + totalTokensSumExpr + `,
		SUM(cached_tokens), SUM(reasoning_tokens), SUM(cache_write_tokens),
		SUM(cost_usd),
		SUM(CASE WHEN cost_usd IS NULL THEN 1 ELSE 0 END) AS cost_missing_requests
	FROM usage_events`)

	where, args := buildSummaryWhere(q)
	sb.WriteString(where)
	if len(groupExprs) > 0 {
		sb.WriteString(" GROUP BY " + strings.Join(groupExprs, ", "))
	}
	// Deterministic ordering: time buckets ascending, everything else by
	// request count descending.
	if len(groupNames) > 0 && (groupNames[0] == "hour" || groupNames[0] == "day") {
		sb.WriteString(" ORDER BY " + groupExprs[0] + " ASC")
	} else {
		sb.WriteString(" ORDER BY requests DESC")
	}
	sb.WriteString(" LIMIT ?")
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SummaryRow, 0, 16)
	for rows.Next() {
		row := SummaryRow{Groups: make(map[string]any, len(groupNames))}
		dest := make([]any, 0, len(groupNames)+9)
		strVals := make([]*string, len(groupNames))
		intVals := make([]*int64, len(groupNames))
		for i, name := range groupNames {
			switch name {
			case "hour", "day", "stream", "success":
				intVals[i] = new(int64)
				dest = append(dest, intVals[i])
			default:
				strVals[i] = new(string)
				dest = append(dest, strVals[i])
			}
		}
		var requests, pt, ct, tt, cached, rt, cwt, missing sql.NullInt64
		var costSum sql.NullFloat64
		dest = append(dest, &requests, &pt, &ct, &tt, &cached, &rt, &cwt, &costSum, &missing)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		for i, name := range groupNames {
			if intVals[i] != nil {
				row.Groups[name] = *intVals[i]
			} else {
				row.Groups[name] = *strVals[i]
			}
		}
		row.Requests = requests.Int64
		row.PromptTokens = pt.Int64
		row.CompletionTokens = ct.Int64
		row.TotalTokens = tt.Int64
		row.CachedTokens = cached.Int64
		row.ReasoningTokens = rt.Int64
		row.CacheWriteTokens = cwt.Int64
		if costSum.Valid {
			v := costSum.Float64
			row.CostUSD = &v
		}
		row.CostMissingRequests = missing.Int64
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Overview is a single global aggregation over the whole filtered range — no
// group_by, no limit. It backs the CLI header lines (totals, failure and
// streaming counts). Cost semantics: TotalCost is nil when no row in range
// had a price (SUM over an all-NULL set is NULL — "cannot be computed", not
// "zero dollars"); when only some rows were priced, TotalCost is the sum of
// the priced ones and CostMissingRequests reports how many rows could not be
// priced so callers can flag the number as partial.
//
// The OpenAI*/Anthropic* fields split the cache-relevant counters by
// usage_source so the renderer can compute two independent cache-hit ratios
// with their own denominators. The OpenAI bucket takes everything that is
// NOT priced with the Anthropic formula — usage_source = 'openai', plus the
// two legacy/unknown forms: NULL (rows written before the v2 migration) and
// the empty string (events built without a parsed usage payload). This
// mirrors ComputeCost's branch (source == SourceAnthropic vs everything
// else) exactly, so the cache segments and the cost accounting can never
// disagree about which rows use which semantics. Anthropic-form rows carry
// cache_read/cache_creation as separate top-level counters excluded from
// input_tokens. Each split sums over the same WHERE clause as the totals;
// the two prompt splits sum to PromptTokens, the two cached splits sum to
// CachedTokens, and the two request splits sum to Requests exactly (every
// row falls into exactly one bucket).
type Overview struct {
	Requests            int64    `json:"requests"`
	PromptTokens        int64    `json:"prompt_tokens"`
	CompletionTokens    int64    `json:"completion_tokens"`
	TotalTokens         int64    `json:"total_tokens"`
	CachedTokens        int64    `json:"cached_tokens"`
	ReasoningTokens     int64    `json:"reasoning_tokens"`
	CacheWriteTokens    int64    `json:"cache_write_tokens"`
	TotalCost           *float64 `json:"total_cost,omitempty"`
	CostMissingRequests int64    `json:"cost_missing_requests"`
	FailedRequests      int64    `json:"failed_requests"`
	StreamingRequests   int64    `json:"streaming_requests"`
	// OpenAI-form bucket: everything not priced with the Anthropic formula
	// (usage_source = 'openai', legacy NULL rows, and empty-string rows
	// from events without a parsed usage payload) — the same partition
	// ComputeCost applies.
	OpenAIRequests     int64 `json:"openai_requests"`
	OpenAIPromptTokens int64 `json:"openai_prompt_tokens"`
	OpenAICachedTokens int64 `json:"openai_cached_tokens"`
	// Anthropic-form bucket (usage_source = 'anthropic'). The cache-hit
	// denominator for this bucket is assembled by the renderer as
	// AnthropicPromptTokens + AnthropicCachedTokens + AnthropicCacheWriteTokens,
	// because Anthropic input_tokens excludes the cache counters.
	AnthropicRequests         int64 `json:"anthropic_requests"`
	AnthropicPromptTokens     int64 `json:"anthropic_prompt_tokens"`
	AnthropicCachedTokens     int64 `json:"anthropic_cached_tokens"`
	AnthropicCacheWriteTokens int64 `json:"anthropic_cache_write_tokens"`
}

// Overview runs the global aggregation on the read pool. GroupBy and Limit in
// q are ignored: the query covers the whole filtered range so callers never
// get a truncated total. Filters share buildSummaryWhere with Summary.
// An aggregate SELECT without GROUP BY always returns exactly one row (with
// COUNT(*)=0 and NULL SUMs on an empty range), so Scan never sees
// sql.ErrNoRows; every SUM is scanned into a NullInt64/NullFloat64 to absorb
// that NULL.
func (s *SQLiteStore) Overview(ctx context.Context, q SummaryQuery) (*Overview, error) {
	db := s.readPool()
	if db == nil {
		return nil, errors.New("usage: store not open")
	}
	where, args := buildSummaryWhere(q)
	query := `SELECT COUNT(*),
		SUM(prompt_tokens), SUM(completion_tokens), ` + totalTokensSumExpr + `,
		SUM(cached_tokens), SUM(reasoning_tokens), SUM(cache_write_tokens),
		SUM(cost_usd),
		SUM(CASE WHEN cost_usd IS NULL THEN 1 ELSE 0 END),
		SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END),
		SUM(CASE WHEN stream = 1 THEN 1 ELSE 0 END),
		SUM(CASE WHEN usage_source = 'openai' OR usage_source = '' OR usage_source IS NULL THEN 1 ELSE 0 END),
		SUM(CASE WHEN usage_source = 'openai' OR usage_source = '' OR usage_source IS NULL THEN prompt_tokens ELSE 0 END),
		SUM(CASE WHEN usage_source = 'openai' OR usage_source = '' OR usage_source IS NULL THEN cached_tokens ELSE 0 END),
		SUM(CASE WHEN usage_source = 'anthropic' THEN 1 ELSE 0 END),
		SUM(CASE WHEN usage_source = 'anthropic' THEN prompt_tokens ELSE 0 END),
		SUM(CASE WHEN usage_source = 'anthropic' THEN cached_tokens ELSE 0 END),
		SUM(CASE WHEN usage_source = 'anthropic' THEN cache_write_tokens ELSE 0 END)
	FROM usage_events` + where

	var o Overview
	var requests int64
	var pt, ct, tt, cached, rt, cwt, missing, failed, streamed sql.NullInt64
	var oaiReq, oaiPt, oaiCached, antReq, antPt, antCached, antCwt sql.NullInt64
	var costSum sql.NullFloat64
	if err := db.QueryRowContext(ctx, query, args...).Scan(
		&requests, &pt, &ct, &tt, &cached, &rt, &cwt, &costSum, &missing, &failed, &streamed,
		&oaiReq, &oaiPt, &oaiCached, &antReq, &antPt, &antCached, &antCwt,
	); err != nil {
		return nil, err
	}
	o.Requests = requests
	o.PromptTokens = pt.Int64
	o.CompletionTokens = ct.Int64
	o.TotalTokens = tt.Int64
	o.CachedTokens = cached.Int64
	o.ReasoningTokens = rt.Int64
	o.CacheWriteTokens = cwt.Int64
	if costSum.Valid {
		v := costSum.Float64
		o.TotalCost = &v
	}
	o.CostMissingRequests = missing.Int64
	o.FailedRequests = failed.Int64
	o.StreamingRequests = streamed.Int64
	o.OpenAIRequests = oaiReq.Int64
	o.OpenAIPromptTokens = oaiPt.Int64
	o.OpenAICachedTokens = oaiCached.Int64
	o.AnthropicRequests = antReq.Int64
	o.AnthropicPromptTokens = antPt.Int64
	o.AnthropicCachedTokens = antCached.Int64
	o.AnthropicCacheWriteTokens = antCwt.Int64
	return &o, nil
}
