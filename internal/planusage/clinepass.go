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
	clinepassProviderName = "clinepass"
	// clinepassDomain is the account base_url domain (config.isClineHost
	// semantics: the domain itself or a subdomain).
	clinepassDomain = "cline.bot"
	// clinepassUsageURL is the Cline Pass plan usage-limits endpoint
	// (GET, Bearer auth). The response carries the three subscription
	// windows (five_hour / weekly / monthly).
	clinepassUsageURL = "https://api.cline.bot/api/v1/users/me/plan/usage-limits"
)

// ClinePassFetcher implements Fetcher for Cline Pass subscription accounts.
// It polls the plan usage-limits endpoint and maps the three upstream
// windows (five_hour / weekly / monthly) onto the shared Snapshot model.
type ClinePassFetcher struct {
	Timeout  time.Duration
	QuotaURL string
}

func (f ClinePassFetcher) withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if f.Timeout > 0 {
		return context.WithTimeout(ctx, f.Timeout)
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, 5*time.Second)
}

func (f ClinePassFetcher) quotaURL() string {
	u := strings.TrimSpace(f.QuotaURL)
	if u == "" {
		return clinepassUsageURL
	}
	return u
}

// Match reports whether this fetcher owns the account: provider
// "clinepass" (case-insensitive), or any cline.bot base_url host. The
// host check mirrors config.isClineHost (the domain itself or a
// subdomain, never a lookalike suffix); the quota endpoint itself is
// always api.cline.bot, base_url only identifies the account here.
func (f ClinePassFetcher) Match(provider, baseURL string) bool {
	if strings.EqualFold(strings.TrimSpace(provider), clinepassProviderName) {
		return true
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == clinepassDomain || strings.HasSuffix(host, "."+clinepassDomain)
}

// clinepassLimit is one data.limits entry. percentUsed is the CONSUMED
// share in percent (a JSON number, integer in practice — prism renders
// consumed, not remaining, so no reversal); resetsAt is RFC3339Nano.
type clinepassLimit struct {
	Type        string  `json:"type"`
	PercentUsed float64 `json:"percentUsed"`
	ResetsAt    string  `json:"resetsAt"`
}

type clinepassUsageLimits struct {
	Data struct {
		Limits []clinepassLimit `json:"limits"`
	} `json:"data"`
}

func (f ClinePassFetcher) Fetch(ctx context.Context, acc AccountView) (Snapshot, error) {
	snap := Snapshot{
		Provider:  clinepassProviderName,
		Accounts:  []string{acc.Name()},
		FetchedAt: time.Now().UTC(),
	}
	if strings.TrimSpace(acc.Key()) == "" {
		return snap, ErrUnauthorized
	}
	ctx, cancel := f.withDeadline(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.quotaURL(), nil)
	if err != nil {
		return snap, err
	}
	req.Header.Set("Authorization", "Bearer "+acc.Key())
	req.Header.Set("Accept", "application/json")

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
		windows, perr := parseClinePassLimits(body)
		if perr != nil {
			return snap, perr
		}
		snap.Windows = windows
		return snap, nil
	case http.StatusUnauthorized:
		return snap, ErrUnauthorized
	case http.StatusForbidden:
		return snap, ErrNoSubscription
	default:
		return snap, fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}
}

func parseClinePassLimits(body []byte) ([]Window, error) {
	var parsed clinepassUsageLimits
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	byName := map[string]Window{}
	for _, l := range parsed.Data.Limits {
		name := clinepassWindowName(l.Type)
		if name == "" {
			continue
		}
		byName[name] = clinepassLimitWindow(name, l)
	}
	if len(byName) == 0 {
		return nil, errors.New("missing clinepass usage limits")
	}
	// Fixed order, short → long, like the fetcher's window set.
	var out []Window
	for _, name := range []string{"5h", "weekly", "monthly"} {
		if w, ok := byName[name]; ok {
			out = append(out, w)
		}
	}
	return out, nil
}

// clinepassWindowName maps the upstream window type onto the shared
// window names: five_hour → 5h (the Gemini precedent — windowTitle knows
// it as 5小时限额), weekly/monthly pass through. An unknown type is
// dropped, never guessed onto a known window.
func clinepassWindowName(typ string) string {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "five_hour", "five-hour", "5h":
		return "5h"
	case "weekly":
		return "weekly"
	case "monthly":
		return "monthly"
	default:
		return ""
	}
}

// clinepassLimitWindow maps one upstream limit entry onto a Window.
// percentUsed is clamped to 0..100 and carried as the floored int.
// Fractional upstream values additionally keep their unfloored share in
// UsedFraction, so a sub-percent usage is not floored away
// (displayPercent ceils it). Integral percents deliberately skip
// UsedFraction: n/100 in float64 does not always multiply back to
// exactly n (0.07*100 = 7.000000000000001), so the ceil in displayPercent
// would turn an exact 7 % into 8 %. ResetsAt is the upstream reset
// instant, when reported.
func clinepassLimitWindow(name string, l clinepassLimit) Window {
	pct := l.PercentUsed
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	w := Window{
		Name:    name,
		Status:  "ok",
		Percent: int(math.Floor(pct + 1e-9)),
	}
	if pct > 0 && pct != math.Trunc(pct) {
		w.UsedFraction = pct / 100
	}
	if w.Percent >= 100 {
		w.Percent = 100
		w.Status = "rate-limited"
	}
	w.ResetsAt = parseXAITime(l.ResetsAt)
	return w
}
