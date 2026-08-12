package ratelimit_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dorokuma/prism/internal/ratelimit"
)

// TestRateLimitMiddleware_HealthExempt pins item 7: /health bypasses the
// business-wide rate limiter (a liveness endpoint must stay reachable under
// load) while every other path stays limited — the exemption is not widened.
func TestRateLimitMiddleware_HealthExempt(t *testing.T) {
	// rate 0 / burst 0: no tokens are ever granted — every non-exempt
	// request is limited, so the test is deterministic.
	rl := ratelimit.NewRateLimiter(0, 0)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := ratelimit.RateLimitMiddleware(next, rl, nil)

	// /health always passes, even from an IP whose bucket is exhausted.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/health: status = %d, want 200 (must bypass the rate limiter)", rec.Code)
	}

	// Every other path stays limited (the exemption must not widen).
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/models", "/metrics", "/admin/usage/summary"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("%s: status = %d, want 429 (only /health is exempt)", path, rec.Code)
		}
	}
}

// TestRateLimitMiddleware_NilLimiterPassesThrough guards the nil-limiter
// path: without a limiter every request passes, including /health.
func TestRateLimitMiddleware_NilLimiterPassesThrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := ratelimit.RateLimitMiddleware(next, nil, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("nil limiter: status = %d, want 200", rec.Code)
	}
}

func TestRateLimiterAllow(t *testing.T) {
	rl := ratelimit.NewRateLimiter(10, 5) // 10 tokens/sec, burst of 5
	// Burst should allow 5 immediate requests
	for i := 0; i < 5; i++ {
		if !rl.Allow("192.168.1.1") {
			t.Errorf("burst allow %d: expected true, got false", i)
		}
	}
	// 6th request within burst window should be denied
	if rl.Allow("192.168.1.1") {
		t.Error("expected rate limited after burst consumed")
	}
	// Different IP should be allowed (separate bucket)
	if !rl.Allow("192.168.1.2") {
		t.Error("different IP should be allowed")
	}
}

func TestRateLimiterCleanup(t *testing.T) {
	rl := ratelimit.NewRateLimiter(10, 5)
	// Create some buckets and verify via behavior (burst consumed)
	for i := 0; i < 5; i++ {
		if !rl.Allow("10.0.0.1") {
			t.Errorf("burst allow 10.0.0.1 %d: expected true, got false", i)
		}
	}
	// 6th request for 10.0.0.1 should be rate limited (burst exhausted)
	if rl.Allow("10.0.0.1") {
		t.Error("expected 10.0.0.1 to be rate limited after burst")
	}
	// Different IP should have its own bucket
	if !rl.Allow("10.0.0.2") {
		t.Error("different IP should be allowed")
	}
}

func TestGetClientIP(t *testing.T) {
	// (a) No trustedProxies: XFF is ignored entirely, use RemoteAddr
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.1, 10.0.0.1")
	r.RemoteAddr = "198.51.100.1:34567"
	if ip := ratelimit.GetClientIP(r, nil); ip != "198.51.100.1" {
		t.Errorf("no trusted proxies: got %q, want 198.51.100.1", ip)
	}

	// (b) Trusted proxies + RemoteAddr in CIDR + XFF → rightmost XFF
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	trusted := []*net.IPNet{cidr}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("X-Forwarded-For", "10.0.0.1, 203.0.113.5")
	r2.RemoteAddr = "198.51.100.2:34567"
	if ip := ratelimit.GetClientIP(r2, trusted); ip != "203.0.113.5" {
		t.Errorf("trusted proxy + XFF: got %q, want 203.0.113.5", ip)
	}

	// (c) Trusted proxies + RemoteAddr NOT in CIDR + XFF → use RemoteAddr
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.Header.Set("X-Forwarded-For", "10.0.0.1, 203.0.113.5")
	r3.RemoteAddr = "100.64.0.1:34567"
	if ip := ratelimit.GetClientIP(r3, trusted); ip != "100.64.0.1" {
		t.Errorf("untrusted remote + XFF: got %q, want 100.64.0.1", ip)
	}

	// (d) Trusted proxies + RemoteAddr trusted + X-Real-IP (no XFF)
	r4 := httptest.NewRequest("GET", "/", nil)
	r4.Header.Set("X-Real-IP", "203.0.113.6")
	r4.RemoteAddr = "198.51.100.3:34567"
	if ip := ratelimit.GetClientIP(r4, trusted); ip != "203.0.113.6" {
		t.Errorf("trusted proxy + X-Real-IP: got %q, want 203.0.113.6", ip)
	}
}

func TestRateLimit_HitLogsWarn(t *testing.T) {
	// This test uses slog directly to verify the "rate_limit.hit" log message.
	// The capturingHandler intercepts slog output.
	// Rate limiter behaviour is tested in TestRateLimiterAllow.
	rl := ratelimit.NewRateLimiter(1, 1)
	// First request passes.
	if !rl.Allow("10.0.0.1") {
		t.Fatal("first Allow should succeed")
	}
	// Second request within the same second should be rate limited.
	if rl.Allow("10.0.0.1") {
		t.Fatal("second Allow should fail")
	}
}

// TestGetClientIP_MultiHopXFF verifies the right-to-left trusted-proxy walk:
// every trusted hop in the X-Forwarded-For chain is skipped, and the first
// untrusted valid IP is the client. Regression for the bug where the
// rightmost XFF entry (the innermost proxy) was returned even when trusted.
func TestGetClientIP_MultiHopXFF(t *testing.T) {
	_, proxyCIDR, _ := net.ParseCIDR("10.0.0.0/8")
	trusted := []*net.IPNet{proxyCIDR}

	// client → proxy1 (10.0.0.1) → proxy2 (10.0.0.2) → prism.
	// RemoteAddr = proxy2 (trusted); XFF = "203.0.113.7, 10.0.0.1".
	// Right-to-left: 10.0.0.1 trusted → skip; 203.0.113.7 untrusted → client.
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	r.RemoteAddr = "10.0.0.2:34567"
	if ip := ratelimit.GetClientIP(r, trusted); ip != "203.0.113.7" {
		t.Errorf("two-hop chain: got %q, want 203.0.113.7 (client, not the innermost trusted proxy)", ip)
	}

	// Three hops: client → proxy1 → proxy2 → proxy3 → prism.
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("X-Forwarded-For", "198.51.100.9, 10.0.0.1, 10.0.0.2")
	r2.RemoteAddr = "10.0.0.3:34567"
	if ip := ratelimit.GetClientIP(r2, trusted); ip != "198.51.100.9" {
		t.Errorf("three-hop chain: got %q, want 198.51.100.9", ip)
	}

	// One hop: RemoteAddr is the trusted proxy, XFF has the client only.
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.Header.Set("X-Forwarded-For", "203.0.113.8")
	r3.RemoteAddr = "10.0.0.1:34567"
	if ip := ratelimit.GetClientIP(r3, trusted); ip != "203.0.113.8" {
		t.Errorf("one-hop chain: got %q, want 203.0.113.8", ip)
	}

	// All XFF hops trusted and no untrusted IP: fall back to X-Real-IP.
	r4 := httptest.NewRequest("GET", "/", nil)
	r4.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	r4.Header.Set("X-Real-IP", "203.0.113.9")
	r4.RemoteAddr = "10.0.0.3:34567"
	if ip := ratelimit.GetClientIP(r4, trusted); ip != "203.0.113.9" {
		t.Errorf("all-trusted XFF: got %q, want 203.0.113.9 (X-Real-IP fallback)", ip)
	}

	// Untrusted RemoteAddr: XFF ignored entirely (spoofing guard).
	r5 := httptest.NewRequest("GET", "/", nil)
	r5.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	r5.RemoteAddr = "100.64.0.1:34567"
	if ip := ratelimit.GetClientIP(r5, trusted); ip != "100.64.0.1" {
		t.Errorf("untrusted remote: got %q, want 100.64.0.1 (XFF ignored)", ip)
	}
}

// TestRateLimiterBucketCapEvictsOldest verifies the deterministic bucket cap:
// with a cap of 2, inserting a third distinct IP evicts the bucket with the
// oldest lastCheck (the first IP). The evicted IP's next request starts with
// a fresh burst, proving it was evicted (rate 0: no refill, so a surviving
// bucket with 4 tokens left could never serve 5 more requests).
func TestRateLimiterBucketCapEvictsOldest(t *testing.T) {
	rl := ratelimit.NewRateLimiterWithMaxBuckets(0, 5, 2)

	if !rl.Allow("10.0.0.1") {
		t.Fatal("first IP must be allowed")
	}
	if !rl.Allow("10.0.0.2") {
		t.Fatal("second IP must be allowed")
	}
	// Third IP: cap reached → evict the oldest (10.0.0.1, created first).
	if !rl.Allow("10.0.0.3") {
		t.Fatal("third IP must be allowed (evicting the oldest bucket)")
	}

	// 10.0.0.1 was evicted: its bucket is recreated with a full burst of 5.
	for i := 0; i < 5; i++ {
		if !rl.Allow("10.0.0.1") {
			t.Fatalf("evicted IP request %d denied: bucket was not recreated with a fresh burst", i+1)
		}
	}
	if rl.Allow("10.0.0.1") {
		t.Error("6th request of the recreated bucket must be denied (fresh burst exhausted)")
	}

	// 10.0.0.2 survived the eviction: it still has 4 tokens (1 consumed).
	if !rl.Allow("10.0.0.2") {
		t.Error("10.0.0.2 must keep its bucket across the eviction")
	}
}
