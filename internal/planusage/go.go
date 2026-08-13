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
	goProviderName = "opencode-go"
	goHost         = "opencode.ai"
	goPathMark     = "/zen/go"

	// Documented OpenCode Go dollar windows. Display-only estimates.
	LimitRollingUSD = 12
	LimitWeeklyUSD  = 30
	LimitMonthlyUSD = 60
)

// Typed fetch failures. The poller maps these onto Snapshot.Err and does
// not treat them as "keep last success + stale" the way a 5xx/timeout is.
var (
	ErrUnauthorized     = errors.New("unauthorized")
	ErrNoSubscription   = errors.New("no_subscription")
	ErrUnexpectedStatus = errors.New("unexpected_status")
)

// GoFetcher implements Fetcher for OpenCode Go.
type GoFetcher struct {
	// Timeout, when > 0, is applied even if ctx already has a deadline
	// (the shorter of the two wins). Zero means: keep the parent deadline
	// if any, otherwise apply a 5s default. This must not silently cap a
	// longer caller deadline (quota.request_timeout).
	Timeout time.Duration
}

// applyAccountAuth writes the credential using the same rules as
// pool.ApplyAuthHeader: empty or Authorization → Bearer token; any other
// auth_header name → raw key and no Authorization header. The key is never
// logged here.
func applyAccountAuth(dst http.Header, acc AccountView) {
	authHeader := strings.TrimSpace(acc.AuthHeader())
	if authHeader == "" || http.CanonicalHeaderKey(authHeader) == "Authorization" {
		dst.Set("Authorization", "Bearer "+acc.Key())
		return
	}
	dst.Set(authHeader, acc.Key())
}

func (f GoFetcher) withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if f.Timeout > 0 {
		return context.WithTimeout(ctx, f.Timeout)
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, 5*time.Second)
}

// Match reports whether this fetcher owns the provider.
func (f GoFetcher) Match(provider, baseURL string) bool {
	if provider == goProviderName {
		return true
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == goHost && strings.Contains(u.Path, goPathMark)
}

// UsageURL joins baseURL with /usage. It never produces /v1/v1/usage.
func UsageURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return "https://opencode.ai/zen/go/v1/usage"
	}
	if strings.HasSuffix(base, "/usage") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/usage"
	}
	if strings.Contains(base, goPathMark) {
		return base + "/v1/usage"
	}
	return base + "/usage"
}

type goWindow struct {
	Status   string          `json:"status"`
	Percent  json.RawMessage `json:"percent"`
	ResetsAt string          `json:"resetsAt"`
}

type goUsageBody struct {
	Usage struct {
		Rolling *goWindow `json:"rolling"`
		Weekly  *goWindow `json:"weekly"`
		Monthly *goWindow `json:"monthly"`
	} `json:"usage"`
	Type  string `json:"type"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (f GoFetcher) Fetch(ctx context.Context, acc AccountView) (Snapshot, error) {
	snap := Snapshot{
		Provider:  goProviderName,
		Accounts:  []string{acc.Name()},
		FetchedAt: time.Now().UTC(),
	}
	ctx, cancel := f.withDeadline(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UsageURL(acc.BaseURL()), nil)
	if err != nil {
		return snap, err
	}
	applyAccountAuth(req.Header, acc)
	req.Header.Set("Accept", "application/json")

	client := acc.Client()
	if client == nil {
		client = http.DefaultClient
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
		windows, perr := parseGoWindows(body)
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

func parseGoWindows(body []byte) ([]Window, error) {
	var parsed goUsageBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if parsed.Usage.Rolling == nil && parsed.Usage.Weekly == nil && parsed.Usage.Monthly == nil {
		return nil, errors.New("missing usage windows")
	}
	out := make([]Window, 0, 3)
	out = appendWindow(out, "rolling", LimitRollingUSD, parsed.Usage.Rolling)
	out = appendWindow(out, "weekly", LimitWeeklyUSD, parsed.Usage.Weekly)
	out = appendWindow(out, "monthly", LimitMonthlyUSD, parsed.Usage.Monthly)
	if len(out) == 0 {
		return nil, errors.New("missing usage windows")
	}
	return out, nil
}

func appendWindow(dst []Window, name string, limit int, src *goWindow) []Window {
	if src == nil {
		return dst
	}
	pct, err := parsePercent(src.Percent)
	if err != nil {
		return dst
	}
	w := Window{
		Name:             name,
		Status:           src.Status,
		Percent:          pct,
		LimitUSDEstimate: limit,
		USDStatus:        "estimated",
	}
	if src.ResetsAt != "" {
		if t, err := time.Parse(time.RFC3339, src.ResetsAt); err == nil {
			w.ResetsAt = &t
		} else if t, err := time.Parse(time.RFC3339Nano, src.ResetsAt); err == nil {
			w.ResetsAt = &t
		}
	}
	if w.Status == "" {
		w.Status = "ok"
	}
	return append(dst, w)
}

func parsePercent(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	if n < 0 {
		n = 0
	}
	if n > 100 {
		n = 100
	}
	return int(math.Floor(n)), nil
}
