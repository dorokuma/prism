package planusage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// clinepassUsageBody mirrors a real GET users/me/plan/usage-limits
// response (three windows, percentUsed + resetsAt, RFC3339Nano).
const clinepassUsageBody = `{
  "data": {
    "limits": [
      {"type": "five_hour", "percentUsed": 7, "resetsAt": "2026-09-24T14:06:04.104052829Z"},
      {"type": "weekly", "percentUsed": 8, "resetsAt": "2026-10-01T04:05:56.106063373Z"},
      {"type": "monthly", "percentUsed": 4, "resetsAt": "2026-10-24T04:05:56.108134482Z"}
    ]
  },
  "success": true
}`

func TestClinePassFetcherMatch(t *testing.T) {
	var f ClinePassFetcher
	if !f.Match("clinepass", "") {
		t.Fatal("provider clinepass")
	}
	if !f.Match("ClinePass", "https://example.com") {
		t.Fatal("provider ClinePass")
	}
	if !f.Match("other", "https://api.cline.bot/api/v1") {
		t.Fatal("api.cline.bot host")
	}
	if !f.Match("other", "https://cline.bot/api/v1") {
		t.Fatal("bare cline.bot host")
	}
	if f.Match("other", "https://notcline.bot/v1") || f.Match("other", "https://evilcline.bot/v1") {
		t.Fatal("lookalike hosts must not match")
	}
	if f.Match("xai", "https://api.x.ai/v1") {
		t.Fatal("xai must not match")
	}
	if f.Match("gemini", "https://cloudcode-pa.googleapis.com") {
		t.Fatal("gemini must not match")
	}
}

func TestClinePassFetcherOK(t *testing.T) {
	var sawMethod, sawAuth, sawAccept bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMethod = r.Method == http.MethodGet
		sawAuth = strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && r.Header.Get("Authorization") != "Bearer "
		sawAccept = r.Header.Get("Accept") == "application/json"
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, clinepassUsageBody)
	}))
	defer srv.Close()

	f := ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}
	snap, err := f.Fetch(context.Background(), fakeAcc{
		name: "ClinePass", provider: "clinepass", base: "https://api.cline.bot/api/v1",
		key: "tok", client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawMethod || !sawAuth || !sawAccept {
		t.Fatalf("request method=%v auth=%v accept=%v", sawMethod, sawAuth, sawAccept)
	}
	if snap.Provider != "clinepass" || len(snap.Windows) != 3 {
		t.Fatalf("snap=%+v", snap)
	}
	if snap.Accounts[0] != "ClinePass" {
		t.Fatalf("accounts=%v", snap.Accounts)
	}
	wantNames := []string{"5h", "weekly", "monthly"}
	wantPcts := []int{7, 8, 4}
	for i, w := range snap.Windows {
		if w.Name != wantNames[i] || w.Percent != wantPcts[i] || w.Status != "ok" || w.ResetsAt == nil {
			t.Fatalf("window[%d]=%+v", i, w)
		}
	}
	if snap.Windows[0].UsedFraction != 0 {
		t.Fatalf("5h used_fraction=%v, want 0 (integral percent stays exact)", snap.Windows[0].UsedFraction)
	}
	if got := displayPercent(snap.Windows[0]); got != 7 {
		t.Fatalf("5h display percent=%d, want 7", got)
	}
	if got := snap.Windows[0].ResetsAt.Format(time.RFC3339Nano); got != "2026-09-24T14:06:04.104052829Z" {
		t.Fatalf("5h resets_at=%q", got)
	}
}

func TestClinePassFetcherSubPercentKeepsFraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"limits":[{"type":"five_hour","percentUsed":0.4,"resetsAt":"2026-09-24T14:06:04Z"}]}}`)
	}))
	defer srv.Close()
	snap, err := (ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
		name: "ClinePass", provider: "clinepass", key: "tok", client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 1 {
		t.Fatalf("windows=%+v", snap.Windows)
	}
	w := snap.Windows[0]
	if w.Percent != 0 || w.UsedFraction < 0.0039 || w.UsedFraction > 0.0041 {
		t.Fatalf("sub-percent window=%+v, want Percent 0 and UsedFraction ~0.004", w)
	}
	// The fraction keeps the sub-percent usage visible: displayPercent
	// ceils it to 1 % instead of flooring it away.
	if got := displayPercent(w); got != 1 {
		t.Fatalf("sub-percent display percent=%d, want 1", got)
	}
}

func TestClinePassFetcherFractionalPercentCeils(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"limits":[{"type":"weekly","percentUsed":7.4}]}}`)
	}))
	defer srv.Close()
	snap, err := (ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
		name: "ClinePass", provider: "clinepass", key: "tok", client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 1 {
		t.Fatalf("windows=%+v", snap.Windows)
	}
	w := snap.Windows[0]
	if w.Percent != 7 || w.UsedFraction < 0.0739 || w.UsedFraction > 0.0741 {
		t.Fatalf("fractional window=%+v, want Percent 7 and UsedFraction ~0.074", w)
	}
	if got := displayPercent(w); got != 8 {
		t.Fatalf("fractional display percent=%d, want 8 (ceil)", got)
	}
}

func TestClinePassFetcherEmptyKey(t *testing.T) {
	_, err := (ClinePassFetcher{QuotaURL: "http://127.0.0.1:1/nope"}).Fetch(context.Background(), fakeAcc{name: "a", provider: "clinepass"})
	if err != ErrUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestClinePassFetcherUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := (ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
		name: "a", provider: "clinepass", key: "tok", client: srv.Client(),
	})
	if err != ErrUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestClinePassFetcherForbiddenIsNoSubscription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := (ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
		name: "a", provider: "clinepass", key: "tok", client: srv.Client(),
	})
	if err != ErrNoSubscription {
		t.Fatalf("err=%v", err)
	}
}

func TestClinePassFetcherUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := (ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
		name: "a", provider: "clinepass", key: "tok", client: srv.Client(),
	})
	if err == nil || !errors.Is(err, ErrUnexpectedStatus) {
		t.Fatalf("err=%v", err)
	}
}

func TestClinePassFetcherMissingLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"limits":[]},"success":true}`)
	}))
	defer srv.Close()
	_, err := (ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
		name: "a", provider: "clinepass", key: "tok", client: srv.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "missing clinepass usage limits") {
		t.Fatalf("err=%v", err)
	}
}

func TestClinePassFetcherFullMarksRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"limits":[{"type":"five_hour","percentUsed":100,"resetsAt":"2026-09-24T14:06:04Z"}]},"success":true}`)
	}))
	defer srv.Close()
	snap, err := (ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
		name: "a", provider: "clinepass", key: "tok", client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Status != "rate-limited" || snap.Windows[0].Percent != 100 {
		t.Fatalf("windows=%+v", snap.Windows)
	}
}

func TestParseClinePassLimitsSkipsUnknownType(t *testing.T) {
	windows, err := parseClinePassLimits([]byte(`{"data":{"limits":[
		{"type":"monthly","percentUsed":4,"resetsAt":"2026-10-24T04:05:56Z"},
		{"type":"mystery","percentUsed":50,"resetsAt":"2026-10-24T04:05:56Z"},
		{"type":"weekly","percentUsed":8}
	]},"success":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 || windows[0].Name != "weekly" || windows[1].Name != "monthly" {
		t.Fatalf("windows=%+v", windows)
	}
	if windows[1].ResetsAt == nil || windows[0].ResetsAt != nil {
		t.Fatalf("resets_at mapping wrong: %+v", windows)
	}
}
