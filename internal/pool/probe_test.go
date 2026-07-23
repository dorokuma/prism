package pool

import (
	"net/http"
	"net/http/httptest"
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

func TestProbeExhausted_401Retries(t *testing.T) {
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
	probeRetryDelay = time.Millisecond

	ProbeExhausted(pool)

	// 401 currently falls through to the retry path in ProbeExhausted
	if callCount != maxProbeAttempts {
		t.Errorf("expected %d attempts for 401 (retry logic), got %d", maxProbeAttempts, callCount)
	}

	// Should still be exhausted
	if accs[0].IsHealthy() {
		t.Error("account should NOT be marked healthy after 401")
	}
}

func TestProbeExhausted_403Retries(t *testing.T) {
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
	probeRetryDelay = time.Millisecond

	ProbeExhausted(pool)

	// 403 currently falls through to the retry path in ProbeExhausted
	if callCount != maxProbeAttempts {
		t.Errorf("expected %d attempts for 403 (retry logic), got %d", maxProbeAttempts, callCount)
	}

	// Should still be exhausted
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
