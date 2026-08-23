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

func TestXAIFetcherMatch(t *testing.T) {
	var f XAIFetcher
	if !f.Match("xai", "") {
		t.Fatal("provider xai")
	}
	if !f.Match("XAI", "https://example.com") {
		t.Fatal("provider XAI")
	}
	if !f.Match("other", "https://api.x.ai/v1") {
		t.Fatal("api.x.ai host")
	}
	if f.Match("opencode-go", "https://opencode.ai/zen/go/v1") {
		t.Fatal("opencode-go must not match")
	}
}

func TestXAIFetcherOK(t *testing.T) {
	var sawAuth, sawCLI bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "format=credits" && !strings.Contains(r.URL.String(), "format=credits") {
			// allow exact BillingURL including query
		}
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") && auth != "Bearer " {
			sawAuth = true
		}
		if r.Header.Get("x-xai-token-auth") == xaiTokenAuth {
			sawCLI = true
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-22T08:48:26.339554+00:00","end":"2026-08-29T08:48:26.339554+00:00"},"creditUsagePercent":53.7}}`)
	}))
	defer srv.Close()

	f := XAIFetcher{BillingURL: srv.URL + "?format=credits", Timeout: time.Second}
	snap, err := f.Fetch(context.Background(), fakeAcc{
		name: "supergrok", provider: "xai", base: "https://api.x.ai/v1", key: "tok",
		client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawAuth || !sawCLI {
		t.Fatalf("headers auth=%v cli=%v", sawAuth, sawCLI)
	}
	if snap.Provider != "xai" || len(snap.Windows) != 1 {
		t.Fatalf("snap=%+v", snap)
	}
	w := snap.Windows[0]
	if w.Name != "weekly" || w.Percent != 53 || w.Status != "ok" || w.ResetsAt == nil || w.PeriodStart == nil {
		t.Fatalf("window=%+v", w)
	}
}

func TestXAIFetcherEmptyKey(t *testing.T) {
	f := XAIFetcher{BillingURL: "http://127.0.0.1:1/nope"}
	_, err := f.Fetch(context.Background(), fakeAcc{name: "a", provider: "xai"})
	if err != ErrUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestXAIFetcherUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	f := XAIFetcher{BillingURL: srv.URL, Timeout: time.Second}
	_, err := f.Fetch(context.Background(), fakeAcc{name: "a", provider: "xai", key: "tok", client: srv.Client()})
	if err != ErrUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestXAIFetcherQuotaExhaustedReturnsFullWindow(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
	}{
		{"payment required", `{"error":{"message":"credits exhausted"}}`, http.StatusPaymentRequired},
		{"structured 429", `{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`, http.StatusTooManyRequests},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			snap, err := (XAIFetcher{BillingURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
				name: "a", provider: "xai", key: "tok", client: srv.Client(),
			})
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if len(snap.Windows) != 1 || snap.Windows[0].Percent != 100 || snap.Windows[0].Status != "rate-limited" {
				t.Fatalf("windows = %+v, want one rate-limited 100%% window", snap.Windows)
			}
		})
	}
}

func TestXAIFetcherRealUpstreamErrorStillFails(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"temporary failure"}}`)
			}))
			defer srv.Close()
			_, err := (XAIFetcher{BillingURL: srv.URL, Timeout: time.Second}).Fetch(context.Background(), fakeAcc{
				name: "a", provider: "xai", key: "tok", client: srv.Client(),
			})
			if err == nil || !errors.Is(err, ErrUnexpectedStatus) {
				t.Fatalf("err = %v, want ErrUnexpectedStatus", err)
			}
		})
	}
}
func TestXAIFetcherFullMarksRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"config":{"creditUsagePercent":100,"billingPeriodEnd":"2026-08-29T00:00:00Z"}}`)
	}))
	defer srv.Close()
	f := XAIFetcher{BillingURL: srv.URL, Timeout: time.Second}
	snap, err := f.Fetch(context.Background(), fakeAcc{name: "a", provider: "xai", key: "tok", client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Windows[0].Status != "rate-limited" || snap.Windows[0].Percent != 100 {
		t.Fatalf("window=%+v", snap.Windows[0])
	}
}
