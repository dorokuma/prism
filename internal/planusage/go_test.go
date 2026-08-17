package planusage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeAcc struct {
	name, provider, base, key, authHeader string
	client                                *http.Client
}

func (a fakeAcc) Name() string         { return a.name }
func (a fakeAcc) Provider() string     { return a.provider }
func (a fakeAcc) BaseURL() string      { return a.base }
func (a fakeAcc) Key() string          { return a.key }
func (a fakeAcc) AuthHeader() string   { return a.authHeader }
func (a fakeAcc) Client() *http.Client { return a.client }

func TestUsageURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://opencode.ai/zen/go/v1", "https://opencode.ai/zen/go/v1/usage"},
		{"https://opencode.ai/zen/go/v1/", "https://opencode.ai/zen/go/v1/usage"},
		{"https://opencode.ai/zen/go", "https://opencode.ai/zen/go/v1/usage"},
		{"https://opencode.ai/zen/go/v1/usage", "https://opencode.ai/zen/go/v1/usage"},
	}
	for _, tc := range cases {
		got := UsageURL(tc.in)
		if got != tc.want {
			t.Errorf("UsageURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(got, "/v1/v1/") {
			t.Errorf("UsageURL(%q) produced double v1: %q", tc.in, got)
		}
	}
}

func TestGoFetcherMatch(t *testing.T) {
	var f GoFetcher
	if !f.Match("opencode-go", "https://example.com") {
		t.Fatal("provider name should match")
	}
	if !f.Match("custom", "https://opencode.ai/zen/go/v1") {
		t.Fatal("base_url host+path should match")
	}
	if f.Match("ollama-cloud", "https://ollama.com/v1") {
		t.Fatal("ollama must not match")
	}
}

func TestGoFetcherOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Errorf("path = %s, want /v1/usage", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth = %q", got)
		}
		io.WriteString(w, `{
			"usage": {
				"rolling": {"status":"ok","percent":12,"resetsAt":"2026-08-13T12:00:00Z"},
				"weekly":  {"status":"ok","percent":8,"resetsAt":"2026-08-17T00:00:00Z"},
				"monthly": {"status":"rate-limited","percent":100,"resetsAt":"2026-09-01T00:00:00Z"}
			}
		}`)
	}))
	defer srv.Close()

	f := GoFetcher{Timeout: time.Second}
	snap, err := f.Fetch(context.Background(), fakeAcc{
		name: "a1", provider: "opencode-go", base: srv.URL + "/v1", key: "sk-test",
		client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(snap.Windows))
	}
	if snap.Windows[0].Percent != 12 || snap.Windows[0].LimitUSDEstimate != 12 || snap.Windows[0].USDStatus != "estimated" {
		t.Errorf("rolling = %+v", snap.Windows[0])
	}
	if snap.Windows[0].ResetsAt == nil {
		t.Fatal("rolling resets_at missing")
	}
	if snap.Windows[2].Status != "rate-limited" {
		t.Errorf("monthly status = %q", snap.Windows[2].Status)
	}
}

func TestGoFetcherCustomAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "raw-secret" {
			t.Errorf("X-Api-Key = %q, want raw-secret", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		io.WriteString(w, `{"usage":{"rolling":{"status":"ok","percent":1}}}`)
	}))
	defer srv.Close()

	f := GoFetcher{Timeout: time.Second}
	if _, err := f.Fetch(context.Background(), fakeAcc{
		name: "a", provider: "opencode-go", base: srv.URL + "/v1", key: "raw-secret",
		authHeader: "x-api-key",
		client:     srv.Client(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGoFetcherErrors(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{401, ErrUnauthorized},
		{403, ErrNoSubscription},
		{500, ErrUnexpectedStatus},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			io.WriteString(w, `{"type":"error"}`)
		}))
		f := GoFetcher{Timeout: time.Second}
		_, err := f.Fetch(context.Background(), fakeAcc{
			name: "a", provider: "opencode-go", base: srv.URL + "/v1", key: "k",
			client: srv.Client(),
		})
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestParseMissingWindows(t *testing.T) {
	if _, err := parseGoWindows([]byte(`{"usage":{}}`)); err == nil {
		t.Fatal("expected error for empty usage")
	}
}

func TestParseFloatPercentFloors(t *testing.T) {
	wins, err := parseGoWindows([]byte(`{
		"usage": {
			"rolling": {"status":"ok","percent":12.9,"resetsAt":"2026-08-13T12:00:00Z"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(wins) != 1 || wins[0].Percent != 12 {
		t.Fatalf("percent = %+v, want floor 12", wins)
	}
}

func TestWithDeadlineKeepsParentWhenNoFetcherTimeout(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx, stop := (GoFetcher{}).withDeadline(parent)
	defer stop()
	got, ok := ctx.Deadline()
	want, _ := parent.Deadline()
	if !ok || !got.Equal(want) {
		t.Fatalf("deadline = %v, want parent %v", got, want)
	}
}

func TestWithDeadlineDefaultWhenNoParentDeadline(t *testing.T) {
	ctx, stop := (GoFetcher{}).withDeadline(context.Background())
	defer stop()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected default deadline")
	}
	left := time.Until(dl)
	if left > 6*time.Second || left < 4*time.Second {
		t.Fatalf("default leftover %v, want ~5s", left)
	}
}

func TestWindowJSONOmitsZeroResetsAtAndMarksEstimate(t *testing.T) {
	w := Window{Name: "rolling", Status: "ok", Percent: 10, LimitUSDEstimate: 12, USDStatus: "estimated"}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "0001-01-01") || strings.Contains(s, `"resets_at"`) {
		t.Fatalf("zero resets_at leaked: %s", s)
	}
	if !strings.Contains(s, `"limit_usd_estimate":12`) || !strings.Contains(s, `"usd_status":"estimated"`) {
		t.Fatalf("estimate fields missing: %s", s)
	}
	if strings.Contains(s, `"limit_usd"`) && !strings.Contains(s, `"limit_usd_estimate"`) {
		t.Fatalf("old limit_usd field: %s", s)
	}
}

// TestGoFetcherNilClientRefusesCrossHostRedirect pins the CLI path
// (Client() == nil): a custom auth_header must not follow a 3xx to another
// host. Production accounts use the pool client; this is the fallback.
func TestGoFetcherNilClientRefusesCrossHostRedirect(t *testing.T) {
	var foreignHits int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&foreignHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer foreign.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "raw-secret" {
			t.Errorf("X-Api-Key = %q, want raw-secret on the first hop", got)
		}
		foreignHost := strings.Replace(foreign.URL, "127.0.0.1", "localhost", 1)
		http.Redirect(w, r, foreignHost+"/steal", http.StatusFound)
	})
	local := httptest.NewServer(mux)
	defer local.Close()

	f := GoFetcher{Timeout: time.Second}
	_, err := f.Fetch(context.Background(), fakeAcc{
		name: "a", provider: "opencode-go", base: local.URL + "/v1",
		key: "raw-secret", authHeader: "x-api-key",
	})
	if err == nil {
		t.Fatal("cross-host redirect must fail (custom auth_header leak prevention)")
	}
	if n := atomic.LoadInt32(&foreignHits); n != 0 {
		t.Errorf("foreign host hits = %d, want 0", n)
	}
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		t.Fatalf("error = %v, want *url.Error", err)
	}
	if !strings.Contains(uerr.Error(), "cross-host redirect") {
		t.Errorf("error = %q, want cross-host redirect", uerr.Error())
	}
}

func TestGoFetcherNilClientFollowsSameHostRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/usage/current", http.StatusFound)
	})
	mux.HandleFunc("/v1/usage/current", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"rolling":{"status":"ok","percent":3}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := GoFetcher{Timeout: time.Second}
	snap, err := f.Fetch(context.Background(), fakeAcc{
		name: "a", provider: "opencode-go", base: srv.URL + "/v1", key: "k",
	})
	if err != nil {
		t.Fatalf("same-host redirect must be followed: %v", err)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Percent != 3 {
		t.Fatalf("windows = %+v, want rolling percent 3", snap.Windows)
	}
}
