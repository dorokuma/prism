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

// TestPollerClinePassEstimateApplied pins the clinepass poller path: the
// three windows each get their own live reversal from their own percent.
func TestPollerClinePassEstimateApplied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, clinepassUsageBody)
	}))
	defer srv.Close()
	c := NewCache()
	p := NewPoller([]Fetcher{ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}}, c, 30*time.Second, time.Second)
	p.SetAccounts([]AccountView{fakeAcc{
		name: "ClinePass", provider: "clinepass", base: "https://api.cline.bot/api/v1",
		key: "k", client: srv.Client(),
	}})
	p.SetClinePassEstimate(func(context.Context, int64, int64) (int64, error) { return 1000, nil })
	p.Refresh()
	snaps := c.List()
	if len(snaps) != 1 || len(snaps[0].Windows) != 3 {
		t.Fatalf("snapshots = %+v, want 1 snapshot with 3 windows", snaps)
	}
	// clinepassUsageBody percentUsed 7 / 8 / 4 (integral → Percent/100).
	want := map[string]int64{"5h": 14286, "weekly": 12500, "monthly": 25000}
	for _, w := range snaps[0].Windows {
		if w.LimitTokensEstimate != want[w.Name] {
			t.Fatalf("%s estimate = %d, want %d", w.Name, w.LimitTokensEstimate, want[w.Name])
		}
	}
}

// TestPollerEstimateDispatchPerProvider pins that the estimate sums never
// cross providers: the clinepass sum must not touch a gemini snapshot and
// the gemini sum must not touch a clinepass snapshot.
func TestPollerEstimateDispatchPerProvider(t *testing.T) {
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, clinepassUsageBody)
	}))
	defer cpSrv.Close()
	gemSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, geminiSummaryBody)
	}))
	defer gemSrv.Close()

	fetchers := []Fetcher{
		ClinePassFetcher{QuotaURL: cpSrv.URL, Timeout: time.Second},
		GeminiFetcher{QuotaURL: gemSrv.URL, Timeout: time.Second},
	}
	accounts := []AccountView{
		fakeAcc{name: "ClinePass", provider: "clinepass", base: "https://api.cline.bot/api/v1", key: "k1", client: cpSrv.Client()},
		fakeAcc{name: "Gemini", provider: "gemini", base: "https://cloudcode-pa.googleapis.com", key: "k2", client: gemSrv.Client()},
	}

	collect := func(t *testing.T, p *Poller) map[string][]Window {
		t.Helper()
		p.Refresh()
		out := map[string][]Window{}
		for _, s := range p.Cache().List() {
			out[s.Provider] = s.Windows
		}
		return out
	}

	// Only the clinepass sum is wired.
	p := NewPoller(fetchers, NewCache(), 30*time.Second, time.Second)
	p.SetAccounts(accounts)
	p.SetClinePassEstimate(func(context.Context, int64, int64) (int64, error) { return 1000, nil })
	got := collect(t, p)
	if len(got["clinepass"]) != 3 || got["clinepass"][0].LimitTokensEstimate == 0 {
		t.Fatalf("clinepass windows must be estimated: %+v", got["clinepass"])
	}
	for _, w := range got["gemini"] {
		if w.LimitTokensEstimate != 0 {
			t.Fatalf("clinepass sum must not leak into gemini: %+v", w)
		}
	}

	// Only the gemini sum is wired.
	p2 := NewPoller(fetchers, NewCache(), 30*time.Second, time.Second)
	p2.SetAccounts(accounts)
	p2.SetGeminiEstimate(func(context.Context, int64, int64) (int64, error) { return 500, nil }, filepath.Join(t.TempDir(), "gem.json"))
	got2 := collect(t, p2)
	gemEstimated := false
	for _, w := range got2["gemini"] {
		if w.LimitTokensEstimate > 0 {
			gemEstimated = true
		}
	}
	if !gemEstimated {
		t.Fatalf("gemini weekly must be estimated by its own sum: %+v", got2["gemini"])
	}
	for _, w := range got2["clinepass"] {
		if w.LimitTokensEstimate != 0 {
			t.Fatalf("gemini sum must not leak into clinepass: %+v", w)
		}
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
	// geminiSummaryBody: remainingFraction 0.0558405 → used 0.9441595,
	// not the floored 94% (1000*100/94 = 1063).
	want := reversePool(1000, 1-0.0558405)
	if weekly.LimitTokensEstimate != want {
		t.Fatalf("gemini weekly estimate = %d, want %d", weekly.LimitTokensEstimate, want)
	}
}
