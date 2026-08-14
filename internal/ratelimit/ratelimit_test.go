package ratelimit_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/ratelimit"
)

// TestRateLimitMiddleware_HealthExempt pins item 7: /health and /ready
// bypass the business-wide rate limiter (liveness and readiness endpoints
// must stay reachable under load) while every other path stays limited —
// the exemption is not widened.
func TestRateLimitMiddleware_HealthExempt(t *testing.T) {
	// rate 0 / burst 0: no tokens are ever granted — every non-exempt
	// request is limited, so the test is deterministic.
	rl := ratelimit.NewRateLimiter(0, 0)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := ratelimit.RateLimitMiddleware(next, rl, nil, nil)

	// /health and /ready always pass, even from an IP whose bucket is
	// exhausted.
	for _, path := range []string{"/health", "/ready"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (must bypass the rate limiter)", path, rec.Code)
		}
	}

	// Every other path stays limited (the exemption must not widen).
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/models", "/metrics", "/admin/usage/summary", "/admin/quota"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("%s: status = %d, want 429 (only /health and /ready are exempt)", path, rec.Code)
		}
	}
}

// TestRateLimitMiddleware_NilLimiterPassesThrough guards the nil-limiter
// path: without a limiter every request passes, including /health.
func TestRateLimitMiddleware_NilLimiterPassesThrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := ratelimit.RateLimitMiddleware(next, nil, nil, nil)
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

	// All XFF hops trusted and no untrusted IP: the chain carries no
	// client — fall back to RemoteAddr, and NEVER to X-Real-IP (X-Real-IP
	// is an independent client-spoofable claim that must not be consulted
	// while XFF is present).
	r4 := httptest.NewRequest("GET", "/", nil)
	r4.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	r4.Header.Set("X-Real-IP", "203.0.113.9")
	r4.RemoteAddr = "10.0.0.3:34567"
	if ip := ratelimit.GetClientIP(r4, trusted); ip != "10.0.0.3" {
		t.Errorf("all-trusted XFF: got %q, want 10.0.0.3 (RemoteAddr fallback; X-Real-IP must be ignored while XFF is present)", ip)
	}

	// Untrusted RemoteAddr: XFF ignored entirely (spoofing guard).
	r5 := httptest.NewRequest("GET", "/", nil)
	r5.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	r5.RemoteAddr = "100.64.0.1:34567"
	if ip := ratelimit.GetClientIP(r5, trusted); ip != "100.64.0.1" {
		t.Errorf("untrusted remote: got %q, want 100.64.0.1 (XFF ignored)", ip)
	}
}

// TestGetClientIP_XRealIPRules pins the X-Real-IP acceptance rules: it is
// consulted ONLY when XFF is empty, and ONLY when it parses as a valid IP
// that is NOT itself a trusted proxy address; every other case falls back
// to RemoteAddr.
func TestGetClientIP_XRealIPRules(t *testing.T) {
	_, proxyCIDR, _ := net.ParseCIDR("10.0.0.0/8")
	trusted := []*net.IPNet{proxyCIDR}

	// (a) XFF empty + valid untrusted X-Real-IP → accepted.
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Real-IP", "203.0.113.6")
	r.RemoteAddr = "10.0.0.3:34567"
	if ip := ratelimit.GetClientIP(r, trusted); ip != "203.0.113.6" {
		t.Errorf("empty XFF + untrusted X-Real-IP: got %q, want 203.0.113.6", ip)
	}

	// (b) XFF empty + X-Real-IP is a TRUSTED proxy address → it is the
	// proxy's own IP, not a client: fall back to RemoteAddr.
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("X-Real-IP", "10.0.0.9")
	r2.RemoteAddr = "10.0.0.3:34567"
	if ip := ratelimit.GetClientIP(r2, trusted); ip != "10.0.0.3" {
		t.Errorf("X-Real-IP inside trusted CIDR: got %q, want 10.0.0.3 (trusted X-Real-IP is the proxy itself, not a client)", ip)
	}

	// (c) XFF empty + invalid X-Real-IP → fall back to RemoteAddr.
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.Header.Set("X-Real-IP", "not-an-ip")
	r3.RemoteAddr = "10.0.0.3:34567"
	if ip := ratelimit.GetClientIP(r3, trusted); ip != "10.0.0.3" {
		t.Errorf("invalid X-Real-IP: got %q, want 10.0.0.3", ip)
	}

	// (d) XFF PRESENT (even all-trusted) + untrusted X-Real-IP: X-Real-IP
	// must be ignored — XFF is the authoritative chain while present.
	r4 := httptest.NewRequest("GET", "/", nil)
	r4.Header.Set("X-Forwarded-For", "10.0.0.1")
	r4.Header.Set("X-Real-IP", "203.0.113.9")
	r4.RemoteAddr = "10.0.0.3:34567"
	if ip := ratelimit.GetClientIP(r4, trusted); ip != "10.0.0.3" {
		t.Errorf("XFF present + X-Real-IP: got %q, want 10.0.0.3 (X-Real-IP must never be consulted while XFF is present)", ip)
	}

	// (e) Untrusted RemoteAddr: X-Real-IP ignored entirely.
	r5 := httptest.NewRequest("GET", "/", nil)
	r5.Header.Set("X-Real-IP", "203.0.113.6")
	r5.RemoteAddr = "100.64.0.1:34567"
	if ip := ratelimit.GetClientIP(r5, trusted); ip != "100.64.0.1" {
		t.Errorf("untrusted remote + X-Real-IP: got %q, want 100.64.0.1", ip)
	}

	// (f) No trusted proxies configured: X-Real-IP ignored entirely.
	r6 := httptest.NewRequest("GET", "/", nil)
	r6.Header.Set("X-Real-IP", "203.0.113.6")
	r6.RemoteAddr = "198.51.100.1:34567"
	if ip := ratelimit.GetClientIP(r6, nil); ip != "198.51.100.1" {
		t.Errorf("no trusted proxies + X-Real-IP: got %q, want 198.51.100.1", ip)
	}
}

// TestRateLimiterBucketCapOverflowShared pins the bucket-cap behavior: with
// a cap of 2, a third distinct IP is NOT rejected outright — it is served
// from a single shared overflow bucket. The map never grows beyond the cap,
// existing buckets keep their tokens untouched, and a flood of new IPs
// drains the shared burst and is then rate-limited (never hard-rejected for
// the whole idle TTL).
func TestRateLimiterBucketCapOverflowShared(t *testing.T) {
	rl := ratelimit.NewRateLimiterWithMaxBuckets(0, 5, 2)

	if !rl.Allow("10.0.0.1") {
		t.Fatal("first IP must be allowed")
	}
	if !rl.Allow("10.0.0.2") {
		t.Fatal("second IP must be allowed")
	}
	// Cap reached: new IPs share one overflow bucket with the full burst.
	// .3 through .7 are five distinct new IPs consuming the 5 shared tokens.
	for i := 3; i <= 7; i++ {
		if !rl.Allow(fmt.Sprintf("10.0.0.%d", i)) {
			t.Fatalf("new IP 10.0.0.%d must be served from the shared overflow bucket (not hard-rejected)", i)
		}
	}
	// The overflow bucket is drained: the 6th new IP is rate-limited
	// (shared-token exhaustion), not permanently rejected.
	if rl.Allow("10.0.0.8") {
		t.Error("overflow bucket must be drained after 5 new IPs shared its burst")
	}
	// The rejection must not consume anything from the existing buckets:
	// 10.0.0.1 keeps its remaining 4 tokens (1 consumed earlier).
	if !rl.Allow("10.0.0.1") {
		t.Error("10.0.0.1 must keep its bucket across the overflow traffic")
	}
	// After 4 more Allow calls the 10.0.0.1 bucket is exhausted.
	for i := 0; i < 4; i++ {
		rl.Allow("10.0.0.1")
	}
	if rl.Allow("10.0.0.1") {
		t.Error("10.0.0.1 must be limited after its burst is exhausted")
	}
}

// TestRateLimiterCleanupResetsOverflowOnly pins the cleanup interaction
// with the overflow bucket: while the per-IP buckets stay ACTIVE (their
// lastCheck keeps refreshing during the whole wait), the map stays at the
// cap and ONLY the idle overflow bucket is reset — so a NEW IP is served
// from a refreshed shared allowance instead of inheriting a permanently
// drained one. This test directly guards the `rl.overflow = nil` reset in
// StartCleanupLoop: delete that line and the first post-cleanup Allow
// fails, because at rate=0 nothing ever refills the drained overflow
// bucket. (To confirm: remove the overflow reset from StartCleanupLoop and
// run `go test -run TestRateLimiterCleanupResetsOverflowOnly ./internal/ratelimit/`
// — the test fails on the 10.0.0.4 assertion.) The OLD test let the
// per-IP buckets go idle too, so its fresh-allowance assertions could not
// tell whether the map-slot freeing or the overflow reset had produced
// the result; here the map MUST stay full (the 10.0.0.5 and 10.0.0.1
// assertions pin it), so a pass can only come from the overflow reset.
func TestRateLimiterCleanupResetsOverflowOnly(t *testing.T) {
	rl := ratelimit.NewRateLimiterWithMaxBuckets(0, 5, 2)
	rl.SetCleanupForTest(50*time.Millisecond, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rl.StartCleanupLoop(ctx)

	// Fill the map to the cap (2 buckets) and push a third IP into the
	// shared overflow bucket.
	if !rl.Allow("10.0.0.1") || !rl.Allow("10.0.0.2") || !rl.Allow("10.0.0.3") {
		t.Fatal("setup allows must succeed")
	}
	// Drain the overflow bucket (rate=0 → no refill): without a reset a
	// later new IP would be rate-limited forever.
	for i := 0; i < 4; i++ {
		rl.Allow("10.0.0.3")
	}
	if rl.Allow("10.0.0.3") {
		t.Fatal("overflow bucket must be drained before the cleanup wait")
	}

	// Keep the per-IP buckets ACTIVE across the whole idle TTL (touch them
	// every few ms): they must NOT be cleaned up, so the map stays full
	// and the cleanup can only reset the overflow bucket.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		rl.Allow("10.0.0.1")
		rl.Allow("10.0.0.2")
		time.Sleep(5 * time.Millisecond)
	}
	// Final touch keeps both buckets fresh past any remaining cleanup tick.
	rl.Allow("10.0.0.1")
	rl.Allow("10.0.0.2")

	// The idle overflow bucket was reset: a new IP gets the full shared
	// burst (the map is still full, so it lands in the overflow bucket, not
	// a fresh per-IP bucket). This is the assertion that fails when the
	// `rl.overflow = nil` reset is deleted.
	if !rl.Allow("10.0.0.4") {
		t.Error("after cleanup, a new IP must be served (the idle overflow bucket was reset)")
	}
	for i := 0; i < 4; i++ {
		rl.Allow("10.0.0.4")
	}
	if rl.Allow("10.0.0.4") {
		t.Error("10.0.0.4 must drain the refreshed overflow burst")
	}

	// The map is STILL at the cap: another new IP cannot get its own
	// bucket — it shares the (now drained) overflow bucket and is
	// rate-limited. This pins that the active per-IP buckets survived
	// cleanup: if they had been freed, 10.0.0.5 would get a fresh bucket
	// and this Allow would succeed.
	if rl.Allow("10.0.0.5") {
		t.Error("map must stay full: 10.0.0.5 must share the drained overflow bucket")
	}

	// The ACTIVE buckets were neither deleted nor recreated: 10.0.0.1 is
	// still the drained bucket from the touch loop (rate=0 → no refill), so
	// it is still limited. A deleted-and-recreated bucket would carry a
	// fresh 5-token burst and this Allow would succeed.
	if rl.Allow("10.0.0.1") {
		t.Error("active bucket must survive cleanup untouched (still drained, not recreated)")
	}
}

// TestRateLimiterBucketCapSparseAllow guards the under-cap path: while the
// map is below the cap new IPs are still admitted normally.
func TestRateLimiterBucketCapSparseAllow(t *testing.T) {
	rl := ratelimit.NewRateLimiterWithMaxBuckets(10, 5, 100)
	for i := 0; i < 3; i++ {
		if !rl.Allow(fmt.Sprintf("10.0.0.%d", i+1)) {
			t.Errorf("IP %d must be allowed while under the cap", i+1)
		}
	}
}

func TestRateLimitMiddleware_AuthenticatedKeysSeparateBuckets(t *testing.T) {
	keys := []config.APIKey{
		{Name: "alice", Token: "tok-alice"},
		{Name: "bob", Token: "tok-bob"},
	}
	holder := config.NewConfigHolder(&config.Config{APIKeys: keys})
	rl := ratelimit.NewRateLimiter(0, 1) // burst 1, no refill
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	bucketKey := func(r *http.Request) string {
		cfg := holder.Load()
		if cfg != nil && len(cfg.APIKeys) > 0 {
			if name, ok := middleware.Authenticate(r, cfg.APIKeys); ok {
				return "key:" + name
			}
		}
		return ratelimit.GetClientIP(r, nil)
	}
	h := ratelimit.RateLimitMiddleware(next, rl, nil, bucketKey)

	req := func(token string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		return r
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req("tok-alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice first: status = %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req("tok-alice"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("alice second: status = %d, want 429 (same key bucket)", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req("tok-bob"))
	if rec.Code != http.StatusOK {
		t.Fatalf("bob first: status = %d, want 200 (different key must not share alice's bucket)", rec.Code)
	}
}

func TestRateLimitMiddleware_BadTokenUsesIPBucket(t *testing.T) {
	keys := []config.APIKey{{Name: "alice", Token: "tok-alice"}}
	rl := ratelimit.NewRateLimiter(0, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	bucketKey := func(r *http.Request) string {
		if name, ok := middleware.Authenticate(r, keys); ok && len(keys) > 0 {
			return "key:" + name
		}
		return ratelimit.GetClientIP(r, nil)
	}
	h := ratelimit.RateLimitMiddleware(next, rl, nil, bucketKey)

	req := func(token string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		return r
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req(""))
	if rec.Code != http.StatusOK {
		t.Fatalf("no token first: status = %d, want 200 (IP bucket)", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req("bad-token"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("bad token: status = %d, want 429 (shares the IP bucket)", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req("tok-alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid key: status = %d, want 200 (key bucket is not the IP bucket)", rec.Code)
	}
}
