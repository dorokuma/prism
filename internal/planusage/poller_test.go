package planusage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
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
// weekly window anchors the pool (1000 tokens ÷ 8 % = 12500), the monthly
// window is exactly twice it, and the 5-hour window is reversed on its OWN
// (1000 tokens ÷ 7 % = 14285) — it never joins the weekly-anchored pool.
func TestPollerClinePassEstimateApplied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, clinepassUsageBody)
	}))
	defer srv.Close()
	c := NewCache()
	p := NewPoller([]Fetcher{ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}}, c, 30*time.Second, time.Second)
	p.SetAccounts([]AccountView{fakeAcc{
		name: "ClinePass", provider: "clinepass", base: "https://api.cline.bot/api/v1",
		key: "k", client: srv.Client(), accountID: 7,
	}})
	p.SetClinePassEstimate(func(accountID int64) GrokTokenSum {
		if accountID != 7 {
			t.Errorf("clinepass sum factory got account id %d, want the account's own 7", accountID)
		}
		return func(context.Context, int64, int64) (int64, error) { return 1000, nil }
	})
	p.Refresh()
	snaps := c.List()
	if len(snaps) != 1 || len(snaps[0].Windows) != 3 {
		t.Fatalf("snapshots = %+v, want 1 snapshot with 3 windows", snaps)
	}
	// clinepassUsageBody percentUsed 7 / 8 / 4 (integral → the midpoint of each
	// interval: 7 % → 0.075, 8 % → 0.085). The weekly percent anchors the pool;
	// monthly = 2 × weekly; the 5-hour window is reversed on its own percent.
	want := map[string]int64{"5h": 13333, "weekly": 11765, "monthly": 23530}
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
	p.SetClinePassEstimate(func(accountID int64) GrokTokenSum {
		return func(context.Context, int64, int64) (int64, error) { return 1000, nil }
	})
	got := collect(t, p)
	// The POOL windows are estimated (weekly anchors the pool, monthly is
	// 2 × weekly); the 5-hour window is reversed on its OWN percent (7 %), so
	// it is 1000/0.075 and not 0 and not the pool.
	pool := map[string]int64{"weekly": 11765, "monthly": 23530, "5h": 13333}
	if len(got["clinepass"]) != 3 {
		t.Fatalf("clinepass windows = %+v, want 3", got["clinepass"])
	}
	for _, w := range got["clinepass"] {
		if w.LimitTokensEstimate != pool[w.Name] {
			t.Fatalf("clinepass %s estimate = %d, want %d", w.Name, w.LimitTokensEstimate, pool[w.Name])
		}
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

// TestPollerClinePassPerAccountEstimate pins O10-1, the service-side
// per-account wiring: the poller must hand EACH account's own metapi
// account_id to the sum factory (AccountIDFrom(g.Accounts[0])) and write that
// account's consumption into that account's snapshot. Before this case the
// factory in every poller test ignored its accountID and fakeAcc.accountID was
// never assigned, so passing 0 — or picking the wrong account of a group —
// kept the whole suite green.
func TestPollerClinePassPerAccountEstimate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, clinepassUsageBody)
	}))
	defer srv.Close()

	const tokA, tokB = "cline-token-alpha", "cline-token-bravo"
	c := NewCache()
	p := NewPoller([]Fetcher{ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}}, c, 30*time.Second, time.Second)
	p.SetAccounts([]AccountView{
		fakeAcc{name: "Cline", provider: "clinepass", base: "https://api.cline.bot/api/v1", key: tokA, client: srv.Client(), accountID: 7},
		fakeAcc{name: "Cline", provider: "clinepass", base: "https://api.cline.bot/api/v1", key: tokB, client: srv.Client(), accountID: 9},
	})

	// Each account has its own distinct consumption, keyed by its own id.
	consumed := map[int64]int64{7: 1000, 9: 3000}
	var mu sync.Mutex
	var asked []int64
	p.SetClinePassEstimate(func(accountID int64) GrokTokenSum {
		mu.Lock()
		asked = append(asked, accountID)
		mu.Unlock()
		n, ok := consumed[accountID]
		if !ok {
			t.Errorf("sum factory got account id %d, want one of the accounts' own ids (7, 9)", accountID)
		}
		return func(context.Context, int64, int64) (int64, error) { return n, nil }
	})

	p.Refresh()

	mu.Lock()
	got := append([]int64(nil), asked...)
	mu.Unlock()
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 2 || got[0] != 7 || got[1] != 9 {
		t.Fatalf("factory account ids = %v, want [7 9] (one per account, its own id)", got)
	}

	// Each snapshot counts only its own account's consumption:
	// clinepassUsageBody is 7 % / 8 % / 4 %, so the WEEKLY window (the pool
	// anchor) takes 1000 → 11765 and 3000 → 35294, while the 5-hour window is
	// reversed on its own 7 % (1000 → 13333, 3000 → 40000).
	snaps := c.List()
	byFP := map[string]int64{}
	for _, s := range snaps {
		fps := s.AccountFPs()
		if len(fps) != 1 || len(s.Accounts) != 1 {
			t.Fatalf("one snapshot per account key expected: %+v", s)
		}
		var weekly int64 = -1
		for _, w := range s.Windows {
			if w.Name == "weekly" {
				weekly = w.LimitTokensEstimate
			}
			if w.Name == "5h" && w.LimitTokensEstimate <= 0 {
				t.Errorf("the 5h window must be reversed on its own percent: %+v", w)
			}
		}
		if weekly < 0 {
			t.Fatalf("weekly window missing: %+v", s)
		}
		byFP[fps[0]] = weekly
	}
	if len(byFP) != 2 {
		t.Fatalf("snapshots = %d, want 2 (one per account)", len(byFP))
	}
	if got := byFP[KeyFingerprint(tokA)]; got != 11765 {
		t.Fatalf("account 7 (1000 tokens ÷ 8%%) weekly estimate = %d, want 11765", got)
	}
	if got := byFP[KeyFingerprint(tokB)]; got != 35294 {
		t.Fatalf("account 9 (3000 tokens ÷ 8%%) weekly estimate = %d, want 35294", got)
	}
}

// TestPollerFailedRoundKeepsAccountFingerprints pins O7 end to end: after a
// FAILED fetch round the cached snapshot must still carry the account names
// and their aligned, non-empty fingerprints, exactly like a successful round.
// The failure branch used to call StoreFailed with the names only, which
// rebuilt the snapshot without accountFPs: a merged ClinePass row then degraded
// to the bare ·, an uncoloured account name and a position-keyed row.
func TestPollerFailedRoundKeepsAccountFingerprints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	const tokA, tokB = "cline-token-alpha", "cline-token-bravo"
	c := NewCache()
	p := NewPoller([]Fetcher{ClinePassFetcher{QuotaURL: srv.URL, Timeout: time.Second}}, c, 30*time.Second, time.Second)
	p.SetAccounts([]AccountView{
		fakeAcc{name: "Cline", provider: "clinepass", base: "https://api.cline.bot/api/v1", key: tokA, client: srv.Client(), accountID: 7},
		fakeAcc{name: "Cline", provider: "clinepass", base: "https://api.cline.bot/api/v1", key: tokB, client: srv.Client(), accountID: 9},
	})
	p.Refresh()

	snaps := c.List()
	if len(snaps) != 2 {
		t.Fatalf("failed round snapshots = %d, want 2 (one per account key)", len(snaps))
	}
	seen := map[string]bool{}
	for _, s := range snaps {
		if s.Err != "unexpected_status" {
			t.Fatalf("failed round error = %q, want unexpected_status: %+v", s.Err, s)
		}
		fps := s.AccountFPs()
		if len(s.Accounts) != 1 || len(fps) != 1 {
			t.Fatalf("failed round must keep names AND fingerprints aligned: %+v (fps=%v)", s, fps)
		}
		if fps[0] == "" {
			t.Fatalf("failed round dropped the account fingerprint: %+v", s)
		}
		if fps[0] != KeyFingerprint(tokA) && fps[0] != KeyFingerprint(tokB) {
			t.Fatalf("failed round fingerprint %q is not an account key fingerprint", fps[0])
		}
		seen[fps[0]] = true
	}
	if len(seen) != 2 {
		t.Fatalf("failed round fingerprints = %v, want 2 distinct (one per account)", seen)
	}
}
