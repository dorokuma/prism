package planusage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollerStopIdempotent(t *testing.T) {
	p := NewPoller(DefaultFetchers(), NewCache(), 30*time.Second, time.Second)
	p.Start()
	p.Stop()
	p.Stop()
}

func TestPollerStopBeforeStart(t *testing.T) {
	p := NewPoller(DefaultFetchers(), NewCache(), 30*time.Second, time.Second)
	p.Stop()
	p.Stop()
}

func TestPollerDisabledSkipsFetch(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"usage":{"rolling":{"status":"ok","percent":1}}}`)
	}))
	defer srv.Close()

	p := NewPoller([]Fetcher{GoFetcher{}}, NewCache(), 30*time.Second, time.Second)
	p.SetAccounts([]AccountView{fakeAcc{
		name: "a", provider: "opencode-go", base: srv.URL + "/v1", key: "k",
		client: srv.Client(),
	}})
	p.SetOptions(false, 30*time.Second, time.Second)
	p.Refresh()
	if hits.Load() != 0 {
		t.Fatalf("disabled poller hit upstream %d times", hits.Load())
	}
}

func TestPollerStopCancelsInFlight(t *testing.T) {
	started := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := NewPoller([]Fetcher{GoFetcher{}}, NewCache(), 30*time.Second, 5*time.Second)
	p.SetAccounts([]AccountView{
		fakeAcc{name: "a", provider: "opencode-go", base: srv.URL + "/v1", key: "k1", client: srv.Client()},
		fakeAcc{name: "b", provider: "opencode-go", base: srv.URL + "/v1", key: "k2", client: srv.Client()},
	})
	done := make(chan struct{})
	go func() {
		p.Refresh()
		close(done)
	}()
	<-started
	<-started
	begin := time.Now()
	p.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Refresh did not abort after Stop")
	}
	if time.Since(begin) > 2*time.Second {
		t.Fatalf("Stop waited %v, want well under 2s", time.Since(begin))
	}
}

func TestPollerUnauthorizedClearsWindows(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status.Load() != http.StatusOK {
			w.WriteHeader(int(status.Load()))
			return
		}
		io.WriteString(w, `{"usage":{"rolling":{"status":"ok","percent":7}}}`)
	}))
	defer srv.Close()

	p := NewPoller([]Fetcher{GoFetcher{}}, NewCache(), 30*time.Second, time.Second)
	p.SetAccounts([]AccountView{fakeAcc{
		name: "a", provider: "opencode-go", base: srv.URL + "/v1", key: "k",
		client: srv.Client(),
	}})
	p.Refresh()
	got := p.Cache().List()
	if len(got) != 1 || len(got[0].Windows) != 1 || got[0].Windows[0].Percent != 7 {
		t.Fatalf("first fetch: %+v", got)
	}

	status.Store(http.StatusUnauthorized)
	p.Refresh()
	got = p.Cache().List()
	if len(got) != 1 {
		t.Fatalf("after 401: list = %d", len(got))
	}
	if got[0].Stale || len(got[0].Windows) != 0 || got[0].Err != "unauthorized" {
		t.Fatalf("after 401: %+v", got[0])
	}

	status.Store(http.StatusOK)
	p.Refresh()
	status.Store(http.StatusForbidden)
	p.Refresh()
	got = p.Cache().List()
	if len(got) != 1 || got[0].Stale || len(got[0].Windows) != 0 || got[0].Err != "no_subscription" {
		t.Fatalf("after 403: %+v", got)
	}
}

func TestErrorCode(t *testing.T) {
	if ErrorCode(ErrUnauthorized) != "unauthorized" {
		t.Fatal(ErrorCode(ErrUnauthorized))
	}
	if ErrorCode(io.EOF) != "fetch_failed" {
		t.Fatal(ErrorCode(io.EOF))
	}
}

// TestPollerGeminiEstimateApplied pins the per-provider estimate split:
// a gemini snapshot gets the gemini sum applied to its weekly window
// (period inferred from ResetsAt-7d), without touching the grok side.
func TestPollerGeminiEstimateApplied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, geminiSummaryBody)
	}))
	defer srv.Close()
	c := NewCache()
	p := NewPoller([]Fetcher{GeminiFetcher{QuotaURL: srv.URL, Timeout: time.Second}}, c, 30*time.Second, time.Second)
	p.SetAccounts([]AccountView{fakeAcc{
		name: "Gemini", provider: "gemini", base: "https://cloudcode-pa.googleapis.com",
		key: "k", client: srv.Client(),
	}})
	p.SetGeminiEstimate(func(context.Context, int64, int64) (int64, error) {
		return 1000, nil
	}, filepath.Join(t.TempDir(), "gem.json"))
	p.Refresh()
	snaps := c.List()
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snaps))
	}
	var weekly *Window
	for i := range snaps[0].Windows {
		if snaps[0].Windows[i].Name == "weekly" {
			weekly = &snaps[0].Windows[i]
		}
	}
	if weekly == nil {
		t.Fatalf("weekly window missing: %+v", snaps[0].Windows)
	}
	// geminiSummaryBody: weekly remainingFraction 0.0558405 → 94% used.
	want := int64(1000 * 100 / 94)
	if weekly.LimitTokensEstimate != want {
		t.Fatalf("gemini weekly estimate = %d, want %d", weekly.LimitTokensEstimate, want)
	}
}
