package planusage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	xaiProviderName = "xai"
	xaiAPIHost      = "api.x.ai"
	xaiTokenAuth    = "xai-grok-cli"
	// cli-chat-proxy billing is what Grok Build / SuperGrok OAuth actually
	// answers. It is not on docs.x.ai; api.x.ai has no equivalent /billing
	// for this token type.
	xaiDefaultBillingURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
)

// XAIFetcher implements Fetcher for SuperGrok / xAI OAuth accounts.
type XAIFetcher struct {
	Timeout    time.Duration
	BillingURL string
}

func (f XAIFetcher) withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if f.Timeout > 0 {
		return context.WithTimeout(ctx, f.Timeout)
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, 5*time.Second)
}

func (f XAIFetcher) billingURL() string {
	u := strings.TrimSpace(f.BillingURL)
	if u == "" {
		return xaiDefaultBillingURL
	}
	return u
}

// Match reports whether this fetcher owns the account.
func (f XAIFetcher) Match(provider, baseURL string) bool {
	if strings.EqualFold(strings.TrimSpace(provider), xaiProviderName) {
		return true
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), xaiAPIHost)
}

type xaiPeriod struct {
	Type  string `json:"type"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type xaiCreditsConfig struct {
	CurrentPeriod      *xaiPeriod `json:"currentPeriod"`
	CreditUsagePercent float64    `json:"creditUsagePercent"`
	BillingPeriodEnd   string     `json:"billingPeriodEnd"`
}

type xaiBillingBody struct {
	Config *xaiCreditsConfig `json:"config"`
}

func (f XAIFetcher) Fetch(ctx context.Context, acc AccountView) (Snapshot, error) {
	snap := Snapshot{
		Provider:  xaiProviderName,
		Accounts:  []string{acc.Name()},
		FetchedAt: time.Now().UTC(),
	}
	if strings.TrimSpace(acc.Key()) == "" {
		return snap, ErrUnauthorized
	}
	ctx, cancel := f.withDeadline(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.billingURL(), nil)
	if err != nil {
		return snap, err
	}
	req.Header.Set("Authorization", "Bearer "+acc.Key())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-xai-token-auth", xaiTokenAuth)

	client := acc.Client()
	if client == nil {
		client = fetchHTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return snap, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return snap, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		windows, perr := parseXAICredits(body)
		if perr != nil {
			return snap, perr
		}
		snap.Windows = windows
		return snap, nil
	case http.StatusUnauthorized:
		return snap, ErrUnauthorized
	case http.StatusForbidden:
		return snap, ErrNoSubscription
	case http.StatusPaymentRequired:
		// The CLI billing proxy answers 402 when the included credits are
		// exhausted. This is a valid, fully known quota state, not a fetch
		// failure: keep the billing window visible at 100%.
		if xaiQuotaExhausted(resp.StatusCode, body) {
			snap.Windows = []Window{{Name: "weekly", Status: "rate-limited", Percent: 100}}
			return snap, nil
		}
		return snap, fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	case http.StatusTooManyRequests:
		// Some proxy versions use 429 for an exhausted billing allowance.
		// Only accept a body that explicitly identifies exhaustion; a generic
		// rate limit remains a real upstream error.
		if xaiQuotaExhausted(resp.StatusCode, body) {
			snap.Windows = []Window{{Name: "weekly", Status: "rate-limited", Percent: 100}}
			return snap, nil
		}
		return snap, fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	default:
		return snap, fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}
}

func xaiQuotaExhausted(status int, body []byte) bool {
	if status == http.StatusPaymentRequired {
		return true
	}
	text := strings.ToLower(string(body))
	// Do not classify every 429 as exhaustion: transient request throttling
	// must continue to surface as ErrUnexpectedStatus. These are the stable
	// quota markers used by xAI/OpenAI-compatible billing envelopes.
	for _, marker := range []string{
		`"code":"insufficient_quota"`,
		`"code":"quota_exceeded"`,
		`"code":"credit_exhausted"`,
		`"type":"insufficient_quota"`,
		`"type":"quota_exceeded"`,
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return strings.Contains(text, "quota exhausted") ||
		strings.Contains(text, "credits exhausted") ||
		strings.Contains(text, "credit exhausted")
}

func parseXAICredits(body []byte) ([]Window, error) {
	var parsed xaiBillingBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if parsed.Config == nil {
		return nil, errors.New("missing billing config")
	}
	pct := parsed.Config.CreditUsagePercent
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	w := Window{
		Name:    "weekly",
		Status:  "ok",
		Percent: int(math.Floor(pct)),
	}
	if w.Percent >= 100 {
		w.Status = "rate-limited"
	}
	if parsed.Config.CurrentPeriod != nil {
		w.PeriodStart = parseXAITime(parsed.Config.CurrentPeriod.Start)
		w.ResetsAt = parseXAITime(parsed.Config.CurrentPeriod.End)
	}
	if w.ResetsAt == nil {
		w.ResetsAt = parseXAITime(parsed.Config.BillingPeriodEnd)
	}
	return []Window{w}, nil
}

func parseXAITime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return &t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t
	}
	return nil
}
