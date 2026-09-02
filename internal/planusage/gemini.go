package planusage

import (
	"bytes"
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
	geminiProviderName = "gemini"
	geminiQuotaURL     = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	geminiUserAgent    = "antigravity"
)

// GeminiFetcher implements Fetcher for Antigravity / Gemini OAuth accounts.
// It polls retrieveUserQuotaSummary and keeps only the Gemini Models group
// (5h + weekly). Claude / GPT buckets are dropped.
type GeminiFetcher struct {
	Timeout  time.Duration
	QuotaURL string
}

func (f GeminiFetcher) withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if f.Timeout > 0 {
		return context.WithTimeout(ctx, f.Timeout)
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, 5*time.Second)
}

func (f GeminiFetcher) quotaURL() string {
	u := strings.TrimSpace(f.QuotaURL)
	if u == "" {
		return geminiQuotaURL
	}
	return u
}

// Match reports whether this fetcher owns the account.
func (f GeminiFetcher) Match(provider, baseURL string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case geminiProviderName, "google", "antigravity":
		return true
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "cloudcode-pa.googleapis.com" || host == "daily-cloudcode-pa.sandbox.googleapis.com"
}

type geminiBucket struct {
	BucketID          string   `json:"bucketId"`
	Window            string   `json:"window"`
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
}

type geminiGroup struct {
	DisplayName string         `json:"displayName"`
	Buckets     []geminiBucket `json:"buckets"`
}

type geminiSummary struct {
	Groups []geminiGroup `json:"groups"`
}

func (f GeminiFetcher) Fetch(ctx context.Context, acc AccountView) (Snapshot, error) {
	snap := Snapshot{
		Provider:  geminiProviderName,
		Accounts:  []string{acc.Name()},
		FetchedAt: time.Now().UTC(),
	}
	if strings.TrimSpace(acc.Key()) == "" {
		return snap, ErrUnauthorized
	}
	ctx, cancel := f.withDeadline(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.quotaURL(), bytes.NewReader([]byte("{}")))
	if err != nil {
		return snap, err
	}
	req.Header.Set("Authorization", "Bearer "+acc.Key())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", geminiUserAgent)

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
		windows, perr := parseGeminiSummary(body)
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

func parseGeminiSummary(body []byte) ([]Window, error) {
	var parsed geminiSummary
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	byName := map[string]Window{}
	for _, g := range parsed.Groups {
		if !isGeminiQuotaGroup(g.DisplayName) {
			continue
		}
		for _, b := range g.Buckets {
			name := geminiWindowName(b)
			if name == "" || b.RemainingFraction == nil {
				continue
			}
			byName[name] = geminiBucketWindow(name, b)
		}
	}
	if len(byName) == 0 {
		return nil, errors.New("missing gemini quota buckets")
	}
	var out []Window
	for _, name := range []string{"5h", "weekly"} {
		if w, ok := byName[name]; ok {
			out = append(out, w)
		}
	}
	return out, nil
}

func isGeminiQuotaGroup(displayName string) bool {
	n := strings.ToLower(displayName)
	if !strings.Contains(n, "gemini") {
		return false
	}
	if strings.Contains(n, "claude") || strings.Contains(n, "gpt") {
		return false
	}
	return true
}

func geminiWindowName(b geminiBucket) string {
	switch strings.ToLower(strings.TrimSpace(b.Window)) {
	case "5h", "five-hour", "fivehour":
		return "5h"
	case "weekly":
		return "weekly"
	}
	id := strings.ToLower(b.BucketID)
	switch {
	case strings.Contains(id, "5h"):
		return "5h"
	case strings.Contains(id, "weekly"):
		return "weekly"
	default:
		return ""
	}
}

func geminiBucketWindow(name string, b geminiBucket) Window {
	remaining := *b.RemainingFraction
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	used := (1 - remaining) * 100
	w := Window{
		Name:    name,
		Status:  "ok",
		Percent: int(math.Floor(used + 1e-9)),
	}
	if w.Percent < 0 {
		w.Percent = 0
	}
	if w.Percent > 100 {
		w.Percent = 100
	}
	if w.Percent >= 100 {
		w.Status = "rate-limited"
	}
	w.ResetsAt = parseXAITime(b.ResetTime)
	// The weekly window is a rolling 7-day span; Google reports only the
	// reset time, so the period start is inferred. Needed by the week
	// estimate (usage range for the consumed-tokens sum).
	if name == "weekly" && w.ResetsAt != nil {
		start := w.ResetsAt.Add(-7 * 24 * time.Hour)
		w.PeriodStart = &start
	}
	return w
}
