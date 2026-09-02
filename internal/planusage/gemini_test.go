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

const geminiSummaryBody = `{
  "groups": [
    {
      "buckets": [
        {
          "bucketId": "gemini-weekly",
          "displayName": "Weekly Limit Remaining",
          "window": "weekly",
          "resetTime": "2026-09-02T01:23:09Z",
          "remainingFraction": 0.0558405
        },
        {
          "bucketId": "gemini-5h",
          "displayName": "Five Hour Limit Remaining",
          "window": "5h",
          "resetTime": "2026-09-02T05:34:01Z",
          "remainingFraction": 1
        }
      ],
      "displayName": "Gemini Models"
    },
    {
      "buckets": [
        {
          "bucketId": "3p-weekly",
          "window": "weekly",
          "resetTime": "2026-09-09T00:34:01Z",
          "remainingFraction": 1
        },
        {
          "bucketId": "3p-5h",
          "window": "5h",
          "resetTime": "2026-09-02T05:34:01Z",
          "remainingFraction": 1
        }
      ],
      "displayName": "Claude and GPT models"
    }
  ]
}`

func TestGeminiFetcherMatch(t *testing.T) {
	var f GeminiFetcher
	if !f.Match("gemini", "") {
		t.Fatal("provider gemini")
	}
	if !f.Match("Google", "https://example.com") {
		t.Fatal("provider Google")
	}
	if !f.Match("antigravity", "") {
		t.Fatal("provider antigravity")
	}
	if !f.Match("other", "https://cloudcode-pa.googleapis.com/v1") {
		t.Fatal("cloudcode host")
	}
	if f.Match("xai", "https://api.x.ai/v1") {
		t.Fatal("xai must not match")
	}
}

func TestGeminiFetcherOKDropsClaude(t *testing.T) {
	var sawUA, sawAuth, sawJSON bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		sawUA = r.Header.Get("User-Agent") == geminiUserAgent
		sawAuth = strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
		body, _ := io.ReadAll(r.Body)
		sawJSON = strings.TrimSpace(string(body)) == "{}"
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, geminiSummaryBody)
	}))
	defer srv.Close()

	f := GeminiFetcher{QuotaURL: srv.URL, Timeout: time.Second}
	snap, err := f.Fetch(context.Background(), fakeAcc{
		name: "Gemini", provider: "gemini", base: "https://cloudcode-pa.googleapis.com",
		key: "tok", client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawUA || !sawAuth || !sawJSON {
		t.Fatalf("headers ua=%v auth=%v json=%v", sawUA, sawAuth, sawJSON)
	}
	if snap.Provider != "gemini" || len(snap.Windows) != 2 {
		t.Fatalf("snap=%+v", snap)
	}
	if snap.Windows[0].Name != "5h" || snap.Windows[0].Percent != 0 || snap.Windows[0].Status != "ok" || snap.Windows[0].ResetsAt == nil {
		t.Fatalf("5h=%+v", snap.Windows[0])
	}
	if snap.Windows[1].Name != "weekly" || snap.Windows[1].Percent != 94 || snap.Windows[1].Status != "ok" || snap.Windows[1].ResetsAt == nil {
		t.Fatalf("weekly=%+v", snap.Windows[1])
	}
	for _, w := range snap.Windows {
		if strings.Contains(strings.ToLower(w.Name), "claude") || w.Name == "3p-weekly" || w.Name == "3p-5h" {
			t.Fatalf("claude window leaked: %+v", w)
		}
	}
}

func TestGeminiFetcherEmptyKey(t *testing.T) {
	_, err := (GeminiFetcher{QuotaURL: "http://127.0.0.1:1/nope"}).Fetch(context.Background(), fakeAcc{name: "a", provider: "gemini"})
	if err != ErrUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestGeminiFetcherUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := (GeminiFetcher{QuotaURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
		name: "a", provider: "gemini", key: "tok", client: srv.Client(),
	})
	if err != ErrUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestGeminiFetcherMissingBuckets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"groups":[{"displayName":"Claude and GPT models","buckets":[{"window":"weekly","remainingFraction":1}]}]}`)
	}))
	defer srv.Close()
	_, err := (GeminiFetcher{QuotaURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
		name: "a", provider: "gemini", key: "tok", client: srv.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "missing gemini quota") {
		t.Fatalf("err=%v", err)
	}
}

func TestGeminiFetcherFullMarksRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"groups":[{"displayName":"Gemini Models","buckets":[
			{"bucketId":"gemini-5h","window":"5h","remainingFraction":0,"resetTime":"2026-09-02T05:34:01Z"},
			{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0}
		]}]}`)
	}))
	defer srv.Close()
	snap, err := (GeminiFetcher{QuotaURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
		name: "a", provider: "gemini", key: "tok", client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows=%+v", snap.Windows)
	}
	for _, w := range snap.Windows {
		if w.Status != "rate-limited" || w.Percent != 100 {
			t.Fatalf("window=%+v", w)
		}
	}
}

func TestGeminiFetcherUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := (GeminiFetcher{QuotaURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
		name: "a", provider: "gemini", key: "tok", client: srv.Client(),
	})
	if err == nil || !errors.Is(err, ErrUnexpectedStatus) {
		t.Fatalf("err=%v", err)
	}
}

func TestParseGeminiSummarySkipsUnknownWindows(t *testing.T) {
	windows, err := parseGeminiSummary([]byte(`{"groups":[{"displayName":"Gemini Models","buckets":[
		{"bucketId":"gemini-mystery","window":"monthly","remainingFraction":0.5},
		{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.5}
	]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 1 || windows[0].Name != "5h" || windows[0].Percent != 50 {
		t.Fatalf("windows=%+v", windows)
	}
}
