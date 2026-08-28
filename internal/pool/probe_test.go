package pool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
)

func TestProbeExhausted_EmptyPool(t *testing.T) {
	// No accounts → nothing to probe, no panic
	pool := NewPool(nil)
	ProbeExhausted(pool)
}

func TestProbeExhausted_NoExhaustedAccounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not have been called — no exhausted accounts")
		w.WriteHeader(500)
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "healthy1", Key: "k1", BaseURL: upstream.URL},
	})

	// All accounts start healthy, so ExhaustedAccounts() returns empty
	ProbeExhausted(pool)

	// Account should still be healthy
	accs := pool.AllAccounts()
	if !accs[0].IsHealthy() {
		t.Error("account should still be healthy")
	}
}

func TestProbeExhausted_200Recovery(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify it's a GET to /v1/models
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("expected /v1/models, got %s", r.URL.Path)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
	})

	// Mark account as exhausted
	accs := pool.AllAccounts()
	accs[0].MarkExhausted()
	if accs[0].IsHealthy() {
		t.Fatal("account should start as exhausted")
	}

	ProbeExhausted(pool)

	if !accs[0].IsHealthy() {
		t.Error("account should be marked healthy after 200 response")
	}
}

func TestProbeExhausted_429StaysExhausted(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
	})

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	ProbeExhausted(pool)

	// Should NOT be marked healthy
	if accs[0].IsHealthy() {
		t.Error("account should NOT be marked healthy after 429")
	}

	// 429 should not retry — only 1 call
	if callCount != 1 {
		t.Errorf("429 should not retry, got %d calls", callCount)
	}
}

func TestProbeExhausted_503Retries(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"message":"service unavailable"}}`))
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
	})

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()
	probeRetryDelay = time.Millisecond

	start := time.Now()
	ProbeExhausted(pool)

	// Should have retried maxProbeAttempts times (3)
	if callCount != maxProbeAttempts {
		t.Errorf("expected %d attempts for 503, got %d", maxProbeAttempts, callCount)
	}
	// Guard that retry delay is actually applied (2 sleeps between 3 attempts)
	if since := time.Since(start); since < 2*probeRetryDelay-time.Millisecond {
		t.Errorf("retry delay not applied: elapsed %v, expected at least ~%v", since, 2*probeRetryDelay)
	}

	// Should still be exhausted (503 doesn't trigger recovery)
	if accs[0].IsHealthy() {
		t.Error("account should NOT be marked healthy after 503")
	}
}

func TestProbeExhausted_401Stops(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
	})

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	ProbeExhausted(pool)

	// Permanent credential: stop this round, do not retry.
	if callCount != 1 {
		t.Errorf("401 should not retry, got %d calls", callCount)
	}

	if accs[0].IsHealthy() {
		t.Error("account should NOT be marked healthy after 401")
	}
	if accs[0].LastExhaustClass() != ExhaustPermanentCredential {
		t.Errorf("lastExhaustClass = %d, want credential", accs[0].LastExhaustClass())
	}
}

func TestProbeExhausted_403Stops(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"message":"forbidden"}}`))
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
	})

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	ProbeExhausted(pool)

	// Bare 403 is temporary (Classify) but not 5xx: stop, do not retry.
	if callCount != 1 {
		t.Errorf("bare 403 should not retry, got %d calls", callCount)
	}

	if accs[0].IsHealthy() {
		t.Error("account should NOT be marked healthy after 403")
	}
}

func TestProbeExhausted_ConnectionRefused(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	upstreamURL := upstream.URL
	upstream.Close() // close immediately → connection refused

	pool := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstreamURL},
	})

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()
	probeRetryDelay = time.Millisecond

	// Should not panic; will retry and fail
	ProbeExhausted(pool)

	// Account should still be exhausted
	if accs[0].IsHealthy() {
		t.Error("account should NOT be marked healthy after connection failure")
	}
}

func TestProbeExhausted_RecoveryAfterRetry(t *testing.T) {
	// First two attempts return 503, third returns 200
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount < 3 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(200)
		}
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
	})

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()
	probeRetryDelay = time.Millisecond

	start := time.Now()
	ProbeExhausted(pool)

	if callCount != 3 {
		t.Errorf("expected 3 attempts, got %d", callCount)
	}
	// Guard that retry delay is actually applied (2 sleeps between 3 attempts)
	if since := time.Since(start); since < 2*probeRetryDelay-time.Millisecond {
		t.Errorf("retry delay not applied: elapsed %v, expected at least ~%v", since, 2*probeRetryDelay)
	}

	// Should be healed after 200 on third attempt
	if !accs[0].IsHealthy() {
		t.Error("account should be marked healthy after 200 on retry")
	}
}

func TestProbeExhausted_MultipleAccounts(t *testing.T) {
	// Create separate servers for each account to test fan-out logic
	srv200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv200.Close()

	srv429 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}))
	defer srv429.Close()

	srv503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv503.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "acc200", Key: "k1", BaseURL: srv200.URL},
		{Name: "acc429", Key: "k2", BaseURL: srv429.URL},
		{Name: "acc503", Key: "k3", BaseURL: srv503.URL},
	})

	accs := pool.AllAccounts()
	// Mark all as exhausted
	for _, a := range accs {
		a.MarkExhausted()
	}
	probeRetryDelay = time.Millisecond

	ProbeExhausted(pool)

	// acc200 (200 response) should be healthy
	if !accs[0].IsHealthy() {
		t.Error("acc200 should be marked healthy after 200")
	}
	// acc429 (429 response) should still be exhausted
	if accs[1].IsHealthy() {
		t.Error("acc429 should NOT be marked healthy after 429")
	}
	// acc503 (503 response) should still be exhausted (retries happened but no recovery)
	if accs[2].IsHealthy() {
		t.Error("acc503 should NOT be marked healthy after 503")
	}
}

func TestAccountHeadersAccessor(t *testing.T) {
	cfg := config.AccountConfig{
		Name:       "acc1",
		Key:        "k1",
		BaseURL:    "https://x.com/v1",
		Headers:    map[string]string{"User-Agent": "ua", "x-app": "cli"},
		AuthHeader: "x-api-key",
		ProbePath:  "/custom",
		SkipPISync: true,
	}
	acc := &Account{cfg: cfg}
	if len(acc.Headers()) != 2 || acc.Headers()["User-Agent"] != "ua" {
		t.Errorf("Headers() = %v", acc.Headers())
	}
	if acc.AuthHeader() != "x-api-key" {
		t.Errorf("AuthHeader() = %q", acc.AuthHeader())
	}
	if acc.ProbePath() != "/custom" {
		t.Errorf("ProbePath() = %q", acc.ProbePath())
	}
	if !acc.SkipPISync() {
		t.Error("SkipPISync() = false, want true")
	}
}

// TestProbeExhausted_CustomProbePath verifies an account with probe_path=/custom
// is probed at that exact path (not /v1/models) and recovers on 200.
func TestProbeExhausted_CustomProbePath(t *testing.T) {
	gotPath := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/custom" {
			t.Errorf("expected probe at /custom, got %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL, ProbePath: "/custom"},
	})

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	ProbeExhausted(pool)

	if !accs[0].IsHealthy() {
		t.Error("account should be marked healthy after 200 on custom path")
	}
	if gotPath != "/custom" {
		t.Errorf("probe path = %q, want /custom", gotPath)
	}
}

// TestProbeExhausted_DisabledKeepsState pins probe_path: disabled semantics:
// ZERO HTTP requests are sent, and the account state is NOT touched — an
// exhausted account must NOT be optimistically revived ("probing disabled"
// does not mean "the credential recovered"; the exhausted flag is only set
// for permanent upstream rejection).
func TestProbeExhausted_DisabledKeepsState(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		t.Error("no HTTP request should be sent when probe is disabled")
		w.WriteHeader(500)
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL, ProbePath: "disabled"},
	})

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	ProbeExhausted(pool)

	if accs[0].IsHealthy() {
		t.Error("account must stay exhausted when probe is disabled (no optimistic revival)")
	}
	if hits != 0 {
		t.Errorf("expected 0 HTTP requests, got %d", hits)
	}
}

// TestProbeExhausted_DisabledHealthyStaysHealthy: a HEALTHY account with
// probe_path disabled is untouched too (no request, no state change).
func TestProbeExhausted_DisabledHealthyStaysHealthy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no HTTP request should be sent when probe is disabled")
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "healthy1", Key: "k1", BaseURL: upstream.URL, ProbePath: "disabled"},
	})

	accs := pool.AllAccounts()
	if !accs[0].IsHealthy() {
		t.Fatal("account must start healthy")
	}

	ProbeExhausted(pool)

	if !accs[0].IsHealthy() {
		t.Error("healthy account must stay healthy")
	}
}

// TestProbeExhausted_DefaultPathStillV1Models is a regression test: accounts
// without probe_path keep probing GET /v1/models.
func TestProbeExhausted_DefaultPathStillV1Models(t *testing.T) {
	gotPath := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
	})

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	ProbeExhausted(pool)

	if !accs[0].IsHealthy() {
		t.Error("account should be marked healthy after 200")
	}
	if gotPath != "/v1/models" {
		t.Errorf("default probe path = %q, want /v1/models", gotPath)
	}
}

// TestProbeSendsAccountHeaders verifies probe requests carry account-level
// headers and the account credential header (Bearer by default).
func TestProbeSendsAccountHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "claude-cli/1.0.0 (external, cli)" {
			t.Errorf("probe User-Agent = %q", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("anthropic-beta") != "claude-code-20250219" {
			t.Errorf("probe anthropic-beta = %q", r.Header.Get("anthropic-beta"))
		}
		if r.Header.Get("Authorization") != "Bearer k1" {
			t.Errorf("probe Authorization = %q, want Bearer k1", r.Header.Get("Authorization"))
		}
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	pool := NewPool([]config.AccountConfig{
		{
			Name:    "exhausted1",
			Key:     "k1",
			BaseURL: upstream.URL,
			Headers: map[string]string{
				"User-Agent":     "claude-cli/1.0.0 (external, cli)",
				"anthropic-beta": "claude-code-20250219",
			},
		},
	})

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	ProbeExhausted(pool)

	if !accs[0].IsHealthy() {
		t.Error("account should be marked healthy after 200")
	}
}

func TestProbeExhausted_402Stops(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(402)
		w.Write([]byte(`{"error":{"message":"payment required"}}`))
	}))
	defer upstream.Close()

	p := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
	})
	accs := p.AllAccounts()
	accs[0].MarkExhausted()

	ProbeExhausted(p)

	if callCount != 1 {
		t.Errorf("402 should not retry, got %d calls", callCount)
	}
	if accs[0].IsHealthy() {
		t.Error("account should NOT be marked healthy after 402")
	}
	if accs[0].LastExhaustClass() != ExhaustPermanentQuota {
		t.Errorf("lastExhaustClass = %d, want quota", accs[0].LastExhaustClass())
	}
}

func TestProbeExhausted_403PreConsumeQuotaStops(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"message":"pre-consume quota failed, user quota: ＄0.031792, need quota: ＄0.545150 (request id: x)","type":"new_api_error"},"type":"error"}`))
	}))
	defer upstream.Close()

	p := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
	})
	accs := p.AllAccounts()
	accs[0].MarkExhausted()

	ProbeExhausted(p)

	if callCount != 1 {
		t.Errorf("pre-consume 403 should not retry, got %d calls", callCount)
	}
	if accs[0].IsHealthy() {
		t.Error("account should stay exhausted")
	}
	if accs[0].LastExhaustClass() != ExhaustPermanentQuota {
		t.Errorf("lastExhaustClass = %d, want quota", accs[0].LastExhaustClass())
	}
}

func TestProbeExhausted_403QuotaBodyStops(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"code":"insufficient_quota","message":"quota"}}`))
	}))
	defer upstream.Close()

	p := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
	})
	accs := p.AllAccounts()
	accs[0].MarkExhausted()

	ProbeExhausted(p)

	if callCount != 1 {
		t.Errorf("permanent quota 403 should not retry, got %d calls", callCount)
	}
	if accs[0].IsHealthy() {
		t.Error("account should stay exhausted")
	}
	if accs[0].LastExhaustClass() != ExhaustPermanentQuota {
		t.Errorf("lastExhaustClass = %d, want quota", accs[0].LastExhaustClass())
	}
}

func TestProbeExhausted_200DoesNotReviveQuota(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(200)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	p := NewPool([]config.AccountConfig{
		{Name: "quota1", Key: "k1", BaseURL: upstream.URL},
	})
	accs := p.AllAccounts()
	accs[0].MarkExhaustedWithClass(ExhaustPermanentQuota)

	ProbeExhausted(p)

	if callCount != 1 {
		t.Errorf("expected 1 probe, got %d", callCount)
	}
	if accs[0].IsHealthy() {
		t.Error("quota-exhausted account must not revive on /v1/models 200")
	}
}

func TestProbeExhausted_200RevivesQuotaAfterWindow(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	p := NewPool([]config.AccountConfig{
		{Name: "quota1", Key: "k1", BaseURL: upstream.URL},
	})
	accs := p.AllAccounts()
	accs[0].MarkExhaustedWithClass(ExhaustPermanentQuota)
	accs[0].mu.Lock()
	accs[0].exhaustedAt = time.Now().Add(-config.QuotaReviveAfter - time.Second)
	accs[0].mu.Unlock()

	ProbeExhausted(p)

	if !accs[0].IsHealthy() {
		t.Error("quota-exhausted account must revive on 200 after QuotaReviveAfter")
	}
}

func TestProbeExhausted_200RevivesQuotaWhenWindowZero(t *testing.T) {
	old := config.QuotaReviveAfter
	config.QuotaReviveAfter = 0
	defer func() { config.QuotaReviveAfter = old }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	p := NewPool([]config.AccountConfig{
		{Name: "quota0", Key: "k1", BaseURL: upstream.URL},
	})
	accs := p.AllAccounts()
	accs[0].MarkExhaustedWithClass(ExhaustPermanentQuota)

	ProbeExhausted(p)

	if !accs[0].IsHealthy() {
		t.Error("QuotaReviveAfter=0 must revive a quota-exhausted account on 200 immediately")
	}
}

func TestProbeExhausted_200RevivesCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	p := NewPool([]config.AccountConfig{
		{Name: "cred1", Key: "k1", BaseURL: upstream.URL},
	})
	accs := p.AllAccounts()
	accs[0].MarkExhaustedWithClass(ExhaustPermanentCredential)

	ProbeExhausted(p)

	if !accs[0].IsHealthy() {
		t.Error("credential-exhausted account should revive on 200")
	}
}

func TestProbeExhausted_SkipsRecentlyProbed(t *testing.T) {
	callCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(429)
	}))
	defer upstream.Close()

	p := NewPool([]config.AccountConfig{
		{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
	})
	accs := p.AllAccounts()
	accs[0].MarkExhausted()

	ProbeExhausted(p)
	ProbeExhausted(p)

	if callCount != 1 {
		t.Errorf("second ProbeExhausted should skip recently probed account, got %d calls", callCount)
	}
}

func TestProbeExhausted_ConcurrencyCap(t *testing.T) {
	var inflight, maxSeen int32
	block := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inflight, 1)
		for {
			old := atomic.LoadInt32(&maxSeen)
			if n <= old || atomic.CompareAndSwapInt32(&maxSeen, old, n) {
				break
			}
		}
		<-block
		atomic.AddInt32(&inflight, -1)
		w.WriteHeader(429)
	}))
	defer upstream.Close()

	const nAcc = 25
	cfgs := make([]config.AccountConfig, nAcc)
	for i := 0; i < nAcc; i++ {
		cfgs[i] = config.AccountConfig{
			Name:    "acc-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Key:     "k",
			BaseURL: upstream.URL,
		}
	}
	p := NewPool(cfgs)
	for _, a := range p.AllAccounts() {
		a.MarkExhausted()
	}

	done := make(chan struct{})
	go func() {
		ProbeExhausted(p)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&maxSeen) < ProbeConcurrency && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	seen := atomic.LoadInt32(&maxSeen)
	if seen > ProbeConcurrency {
		close(block)
		<-done
		t.Fatalf("in-flight probes %d exceeded ProbeConcurrency %d", seen, ProbeConcurrency)
	}
	if seen != ProbeConcurrency {
		close(block)
		<-done
		t.Fatalf("in-flight probes %d, want cap %d", seen, ProbeConcurrency)
	}
	close(block)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ProbeExhausted did not finish after releasing blockers")
	}
}

// stubOAuthTokens mimics the surface of oauth.Source that the probe
// terminal-skip check uses (structural assertion in
// Account.OAuthTerminalInvalid).
type stubOAuthTokens struct {
	tok      string
	err      error
	terminal bool
}

func (s stubOAuthTokens) Token(context.Context) (string, error) { return s.tok, s.err }
func (s stubOAuthTokens) OAuthTerminalInvalid() bool            { return s.terminal }

// Audit item 6d: an exhausted xai account in the TERMINAL OAuth state
// (dead refresh token, re-login required) must be skipped by the probe
// loop — probing would call acc.Key() → Token() and either hammer the
// invalid refresh token or log a terminal error every probe cycle. A
// NON-terminal exhausted OAuth account must still be probed normally.
func TestProbeExhausted_SkipsTerminalOAuthAccount(t *testing.T) {
	termURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("terminal OAuth account must not be probed (no HTTP request, no refresh attempt)")
		w.WriteHeader(500)
	}))
	defer termURL.Close()
	liveURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer liveURL.Close()

	p := NewPool([]config.AccountConfig{
		{Name: "xai-terminal", OAuth: "xai", BaseURL: termURL.URL},
		{Name: "xai-live", OAuth: "xai", BaseURL: liveURL.URL},
	})
	accs := p.AllAccounts()
	accs[0].SetTokenSource(stubOAuthTokens{tok: "dead", terminal: true})
	accs[1].SetTokenSource(stubOAuthTokens{tok: "live"})
	accs[0].MarkExhaustedWithClass(ExhaustPermanentCredential)
	accs[1].MarkExhaustedWithClass(ExhaustPermanentCredential)

	ProbeExhausted(p)

	// The terminal account stays exhausted, untouched.
	if accs[0].IsHealthy() {
		t.Error("terminal account must not be revived by a probe")
	}
	if accs[0].LastExhaustClass() != ExhaustPermanentCredential {
		t.Errorf("terminal account exhaust class = %v, want unchanged", accs[0].LastExhaustClass())
	}
	// The non-terminal OAuth account is still probed (200 → healthy).
	if !accs[1].IsHealthy() {
		t.Error("non-terminal exhausted OAuth account should be probed and revived")
	}
}

// A static-key exhausted account (no OAuth) is unaffected by the
// terminal-skip guard and keeps being probed.
func TestProbeExhausted_StaticAccountStillProbed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	p := NewPool([]config.AccountConfig{
		{Name: "static1", Key: "k1", BaseURL: upstream.URL},
	})
	acc := p.AllAccounts()[0]
	acc.MarkExhausted()

	ProbeExhausted(p)

	if !acc.IsHealthy() {
		t.Error("static-key exhausted account should be probed and revived")
	}
}
