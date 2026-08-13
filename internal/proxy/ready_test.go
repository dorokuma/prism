package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

// newReadyHandler builds a proxy handler whose pool contains one account per
// provider in cfgs. The /ready answer is derived from the pool state only.
func newReadyHandler(t *testing.T, cfgs []config.AccountConfig) http.Handler {
	t.Helper()
	p := pool.NewPool(cfgs)
	return NewProxyHandler(p, config.WireAPIBoth, config.NewConfigHolder(&config.Config{Accounts: cfgs}), nil)
}

// TestReady_HealthyAccounts200: with at least one healthy, non-cooldown
// account the readiness endpoint answers 200 ok.
func TestReady_HealthyAccounts200(t *testing.T) {
	h := newReadyHandler(t, []config.AccountConfig{{Name: "a1", Key: "k", BaseURL: "http://localhost:1", Provider: "p"}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/ready with a healthy account: status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("/ready body = %q, want %q", rec.Body.String(), "ok")
	}
}

// TestReady_AllExhausted503: every account exhausted → 503 not ready
// (the process is up — liveness /health stays 200 — but nothing can serve).
func TestReady_AllExhausted503(t *testing.T) {
	cfgs := []config.AccountConfig{{Name: "a1", Key: "k", BaseURL: "http://localhost:1", Provider: "p"}}
	p := pool.NewPool(cfgs)
	p.AllAccounts()[0].MarkExhausted()
	h := NewProxyHandler(p, config.WireAPIBoth, config.NewConfigHolder(&config.Config{Accounts: cfgs}), nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ready with all accounts exhausted: status = %d, want 503", rec.Code)
	}
	if rec.Body.String() != "not ready" {
		t.Errorf("/ready body = %q, want %q", rec.Body.String(), "not ready")
	}

	// Liveness stays 200 regardless of pool state.
	recHealth := httptest.NewRecorder()
	h.ServeHTTP(recHealth, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recHealth.Code != http.StatusOK {
		t.Errorf("/health with all accounts exhausted: status = %d, want 200 (liveness is process-level)", recHealth.Code)
	}
}

// TestReady_CooldownCountsAsNotReady: a healthy account in cooldown cannot
// serve — readiness must be false until the cooldown expires.
func TestReady_CooldownCountsAsNotReady(t *testing.T) {
	cfgs := []config.AccountConfig{{Name: "a1", Key: "k", BaseURL: "http://localhost:1", Provider: "p"}}
	p := pool.NewPool(cfgs)
	p.AllAccounts()[0].SetCooldown(10 * time.Minute)
	h := NewProxyHandler(p, config.WireAPIBoth, config.NewConfigHolder(&config.Config{Accounts: cfgs}), nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ready with the only account in cooldown: status = %d, want 503", rec.Code)
	}
}

// TestReady_OneOfManyHealthy200: readiness is true when at least ONE
// account is healthy and out of cooldown, even if others are exhausted.
func TestReady_OneOfManyHealthy200(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "a1", Key: "k1", BaseURL: "http://localhost:1", Provider: "p"},
		{Name: "a2", Key: "k2", BaseURL: "http://localhost:2", Provider: "p"},
	}
	p := pool.NewPool(cfgs)
	accs := p.AllAccounts()
	accs[0].MarkExhausted()
	accs[1].SetCooldown(10 * time.Minute)
	h := NewProxyHandler(p, config.WireAPIBoth, config.NewConfigHolder(&config.Config{Accounts: cfgs}), nil)

	// a1 exhausted, a2 in cooldown → not ready.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("all accounts busy: status = %d, want 503", rec.Code)
	}

	// a1 recovered (healthy, no cooldown) → ready (at least one usable
	// account) even though a2 is still in cooldown.
	accs[0].MarkHealthy()
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("one healthy account: status = %d, want 200", rec2.Code)
	}
}

// TestCopyClientHeaders_DropsHostContentLengthExpect pins the explicit
// non-forwarded headers: Host, Content-Length and Expect must never reach
// the upstream (the upstream request is built by prism — its Host, its own
// body length, and no client 100-continue negotiation), while normal
// business headers still pass through.
func TestCopyClientHeaders_DropsHostContentLengthExpect(t *testing.T) {
	src := http.Header{}
	src.Set("Host", "client.example.com")
	src.Set("Content-Length", "999999")
	src.Set("Expect", "100-continue")
	src.Set("X-Custom-Business", "keep-me")
	src.Set("Connection", "keep-alive") // hop-by-hop, dropped by the existing rule
	dst := http.Header{}
	copyClientHeaders(dst, src)

	for _, h := range []string{"Host", "Content-Length", "Expect", "Connection"} {
		if dst.Get(h) != "" {
			t.Errorf("copyClientHeaders forwarded %s = %q, want dropped", h, dst.Get(h))
		}
	}
	if dst.Get("X-Custom-Business") != "keep-me" {
		t.Errorf("copyClientHeaders dropped a normal business header: %q", dst.Get("X-Custom-Business"))
	}
}
