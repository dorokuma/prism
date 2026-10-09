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
	// MeasuredTokens is the actually measured token consumption for
	// this window from the upstream / the local usage source. It is used as the total
	// when LimitTokensEstimate is unavailable (e.g. a 100 % exhausted
	// window) so the card can show X/X instead of "-".
	MeasuredTokens int64 `json:"measured_tokens,omitempty"`
}

// Snapshot is one upstream plan fetch for a unique API key.
type Snapshot struct {
	Provider  string    `json:"provider"`
	Accounts  []string  `json:"accounts"`
	FetchedAt time.Time `json:"fetched_at"`
	Windows   []Window  `json:"windows"`
	Err       string    `json:"error,omitempty"`
	Stale     bool      `json:"stale"`
	// accountFPs holds the per-account key fingerprints (SHA-256 first 8
	// bytes hex) for color assignment. It is NOT serialized to JSON.
	accountFPs []string
}

// AccountFPs returns the per-account fingerprints, aligned with Accounts.
// When nil or shorter than Accounts, missing entries are "".
func (s Snapshot) AccountFPs() []string {
	if s.accountFPs == nil {
		return make([]string, len(s.Accounts))
	}
	out := make([]string, len(s.Accounts))
	copy(out, s.accountFPs)
	return out
}

// SetAccountFPs sets the per-account fingerprints. The slice must have
// the same length as Accounts, or be shorter (missing entries become "").
// Callers that have the accounts at hand should prefer AssignAccountViews,
// which fills Accounts and the fingerprints in ONE step.
func (s *Snapshot) SetAccountFPs(fps []string) {
	if s == nil {
		return
	}
	s.accountFPs = fps
}

// AssignAccountViews stamps s with the names of accounts and their
// per-account key fingerprints, in one step so the two slices can never
// drift out of alignment: AccountFPs()[i] belongs to Accounts[i].
//
// The fingerprint is KeyFingerprint(AccountView.Key()) — the same 口径 the
// service poller uses — and it is what a merged ClinePass row's identity
// (clineRowID) and colour (accountColor) are made of. A snapshot that
// carries the names but no fingerprints renders the degraded bare ·, an
// uncoloured name and position-keyed rows, so every snapshot built from a
// key group must go through here: the service poller and the CLI both do.
func AssignAccountViews(s *Snapshot, accounts []AccountView) {
	if s == nil {
		return
	}
	names := make([]string, 0, len(accounts))
	fps := make([]string, 0, len(accounts))
	for _, a := range accounts {
		names = append(names, a.Name())
		fps = append(fps, KeyFingerprint(a.Key()))
	}
	s.Accounts = names
	s.SetAccountFPs(fps)
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
