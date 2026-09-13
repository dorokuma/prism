package planusage

import (
	"context"
	"net/http"
	"time"
)

// Window is one upstream plan window (rolling / weekly / monthly).
type Window struct {
	Name             string     `json:"name"`
	Status           string     `json:"status"`
	Percent          int        `json:"percent"`
	ResetsAt         *time.Time `json:"resets_at,omitempty"`
	PeriodStart      *time.Time `json:"period_start,omitempty"`
	LimitUSDEstimate int        `json:"limit_usd_estimate,omitempty"`
	USDStatus        string     `json:"usd_status,omitempty"`
	// UsedFraction is the unfloored used share of this window (0..1),
	// when the upstream reports it. Gemini week reversal uses this
	// instead of Percent so a 0.4% week is not lost to int floor=0,
	// and a 12.7% week is not reversed as 12%.
	UsedFraction float64 `json:"used_fraction,omitempty"`
	// LimitTokensEstimate is the weekly pool size in tokens, inferred
	// from consumed tokens ÷ used fraction. Zero means not yet
	// available (no usage, or used fraction unknown).
	LimitTokensEstimate int64 `json:"limit_tokens_estimate,omitempty"`
}

// Snapshot is one upstream plan fetch for a unique API key.
type Snapshot struct {
	Provider  string    `json:"provider"`
	Accounts  []string  `json:"accounts"`
	FetchedAt time.Time `json:"fetched_at"`
	Windows   []Window  `json:"windows"`
	Err       string    `json:"error,omitempty"`
	Stale     bool      `json:"stale"`
}

// AccountView is the read-only account surface the fetchers need.
// pool.Account already implements it.
type AccountView interface {
	Name() string
	Provider() string
	BaseURL() string
	Key() string
	AuthHeader() string
	Client() *http.Client
}

// Fetcher pulls plan usage for one upstream family.
type Fetcher interface {
	Match(provider, baseURL string) bool
	Fetch(ctx context.Context, acc AccountView) (Snapshot, error)
}

// Response is the JSON body of GET /admin/quota.
type Response struct {
	FetchedAt time.Time  `json:"fetched_at"`
	Providers []Snapshot `json:"providers"`
}
