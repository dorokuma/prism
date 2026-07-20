package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeExhausted_EmptyPool(t *testing.T) {
	// No accounts → nothing to probe, no panic
	pool := NewPool(nil)
	probeExhausted(pool, "test-model")
}

func TestProbeExhausted_NoExhaustedAccounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not have been called — no exhausted accounts")
		w.WriteHeader(500)
	}))
	defer upstream.Close()

	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "healthy1", Key: "k1", BaseURL: upstream.URL},
		},
	}
	pool := NewPool(cfg.Accounts)

	// All accounts start healthy, so ExhaustedAccounts() returns empty
	probeExhausted(pool, "test-model")

	// Account should still be healthy
	accs := pool.AllAccounts()
	if !accs[0].IsHealthy() {
		t.Error("account should still be healthy")
	}
}

func TestProbeExhausted_200Recovery(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify it's a POST to /chat/completions
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("expected /chat/completions, got %s", r.URL.Path)
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
		},
	}
	pool := NewPool(cfg.Accounts)

	// Mark account as exhausted
	accs := pool.AllAccounts()
	accs[0].MarkExhausted()
	if accs[0].IsHealthy() {
		t.Fatal("account should start as exhausted")
	}

	probeExhausted(pool, "test-model")

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

	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
		},
	}
	pool := NewPool(cfg.Accounts)

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	probeExhausted(pool, "test-model")

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

	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
		},
	}
	pool := NewPool(cfg.Accounts)

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	start := time.Now()
	probeExhausted(pool, "test-model")
	elapsed := time.Since(start)

	// Should have retried maxProbeAttempts times (3)
	if callCount != maxProbeAttempts {
		t.Errorf("expected %d attempts for 503, got %d", maxProbeAttempts, callCount)
	}

	// Should still be exhausted (503 doesn't trigger recovery)
	if accs[0].IsHealthy() {
		t.Error("account should NOT be marked healthy after 503")
	}

	// Should have waited between retries (at least 2 * probeRetryDelay)
	minWait := 2 * probeRetryDelay
	if elapsed < minWait {
		t.Errorf("expected at least %v elapsed for retries, got %v", minWait, elapsed)
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

	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
		},
	}
	pool := NewPool(cfg.Accounts)

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	probeExhausted(pool, "test-model")

	// 401 currently falls through to the retry path in probeExhausted
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

	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
		},
	}
	pool := NewPool(cfg.Accounts)

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	probeExhausted(pool, "test-model")

	if callCount != maxProbeAttempts {
		t.Errorf("expected %d attempts for 403 (retry logic), got %d", maxProbeAttempts, callCount)
	}

	if accs[0].IsHealthy() {
		t.Error("account should NOT be marked healthy after 403")
	}
}

func TestProbeExhausted_MultipleAccounts(t *testing.T) {
	// Create separate servers for each account
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

	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "acc200", Key: "k1", BaseURL: srv200.URL},
			{Name: "acc429", Key: "k2", BaseURL: srv429.URL},
			{Name: "acc503", Key: "k3", BaseURL: srv503.URL},
		},
	}
	pool := NewPool(cfg.Accounts)

	accs := pool.AllAccounts()
	// Mark all as exhausted
	for _, a := range accs {
		a.MarkExhausted()
	}

	probeExhausted(pool, "test-model")

	// acc200 should be healthy
	if !accs[0].IsHealthy() {
		t.Error("acc200 should be marked healthy after 200")
	}
	// acc429 should still be exhausted
	if accs[1].IsHealthy() {
		t.Error("acc429 should NOT be marked healthy after 429")
	}
	// acc503 should still be exhausted (retries happened but no recovery)
	if accs[2].IsHealthy() {
		t.Error("acc503 should NOT be marked healthy after 503")
	}
}

func TestProbeExhausted_ConnectionRefused(t *testing.T) {
	// Start and immediately close a server to simulate connection refused
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	upstreamURL := upstream.URL
	upstream.Close() // close immediately → connection refused

	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "exhausted1", Key: "k1", BaseURL: upstreamURL},
		},
	}
	pool := NewPool(cfg.Accounts)

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	// Should not panic; will retry and fail
	probeExhausted(pool, "test-model")

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

	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "exhausted1", Key: "k1", BaseURL: upstream.URL},
		},
	}
	pool := NewPool(cfg.Accounts)

	accs := pool.AllAccounts()
	accs[0].MarkExhausted()

	start := time.Now()
	probeExhausted(pool, "test-model")
	elapsed := time.Since(start)

	if callCount != 3 {
		t.Errorf("expected 3 attempts, got %d", callCount)
	}

	// Should be healed after 200 on third attempt
	if !accs[0].IsHealthy() {
		t.Error("account should be marked healthy after 200 on retry")
	}

	// Should have waited at least 2 * probeRetryDelay (two retry delays)
	if elapsed < 2*probeRetryDelay {
		t.Errorf("expected at least %v elapsed, got %v", 2*probeRetryDelay, elapsed)
	}
}
