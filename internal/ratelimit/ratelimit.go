package ratelimit

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/util"
)

// RateLimiter implements a simple per-IP token bucket rate limiter.
type RateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*tokenBucket
	rate       int // tokens per second
	burst      int // max burst
	maxBuckets int // hard cap on distinct IP buckets
}

type tokenBucket struct {
	tokens    float64
	lastCheck time.Time
}

// NewRateLimiter creates a new RateLimiter with the given rate and burst and
// the production default bucket cap (config.RateLimitMaxBuckets).
func NewRateLimiter(rate, burst int) *RateLimiter {
	return NewRateLimiterWithMaxBuckets(rate, burst, config.RateLimitMaxBuckets)
}

// NewRateLimiterWithMaxBuckets creates a RateLimiter with a deterministic
// cap on the number of tracked IP buckets. When the cap is reached, a NEW
// IP is rejected outright (Allow returns false) — the map can never grow
// without bound and the hot path never scans the whole map under the lock
// (the old oldest-bucket eviction was an O(n) scan per new IP). Existing
// buckets are untouched and keep their tokens; the background cleanup loop
// frees space by removing idle buckets. Tests inject a small cap;
// production uses config.RateLimitMaxBuckets (100000).
func NewRateLimiterWithMaxBuckets(rate, burst, maxBuckets int) *RateLimiter {
	if maxBuckets <= 0 {
		maxBuckets = config.RateLimitMaxBuckets
	}
	return &RateLimiter{
		buckets:    make(map[string]*tokenBucket),
		rate:       rate,
		burst:      burst,
		maxBuckets: maxBuckets,
	}
}

// Allow checks if the given IP is allowed to proceed. If allowed, one token
// is consumed from the IP's bucket.
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	b, ok := rl.buckets[ip]
	if !ok {
		if len(rl.buckets) >= rl.maxBuckets {
			// Bucket cap reached: reject the new IP outright. No eviction
			// scan (O(n) under the lock), no stealing from existing buckets
			// — a flood of distinct IPs cannot grow the map, cannot slow
			// the hot path, and cannot degrade the buckets that are already
			// being served. The cleanup loop eventually frees a slot.
			return false
		}
		b = &tokenBucket{
			tokens:    float64(rl.burst),
			lastCheck: now,
		}
		rl.buckets[ip] = b
	}

	elapsed := now.Sub(b.lastCheck).Seconds()
	b.tokens += elapsed * float64(rl.rate)
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.lastCheck = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// StartCleanupLoop starts a background goroutine that periodically cleans up
// stale buckets.
func (rl *RateLimiter) StartCleanupLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rl.mu.Lock()
				now := time.Now()
				for ip, b := range rl.buckets {
					if now.Sub(b.lastCheck) > config.RateLimitIdleTTL {
						delete(rl.buckets, ip)
					}
				}
				rl.mu.Unlock()
			}
		}
	}()
}

// GetClientIP extracts the client IP from the request using trusted proxy
// awareness. If trustedProxies is empty, X-Forwarded-For and X-Real-IP are
// ignored entirely (only RemoteAddr is used) to prevent IP spoofing. If
// trustedProxies is non-empty and RemoteAddr is within a trusted CIDR:
//
//   - when X-Forwarded-For is non-empty, the chain is walked right-to-left
//     skipping every trusted proxy hop, and the first valid IP that is NOT
//     trusted is returned (the original client). This makes multi-hop
//     chains (client → trusted proxy 1 → trusted proxy 2 → prism) resolve
//     to the client, not to the innermost proxy. X-Real-IP is NEVER
//     consulted while XFF is present — a proxy that appends its own hop to
//     XFF can still be relied on, but X-Real-IP would be a second,
//     independently client-controlled claim. When every XFF hop is trusted
//     (or all are invalid), the chain carries no client: fall back to
//     RemoteAddr.
//   - only when X-Forwarded-For is EMPTY is X-Real-IP accepted, and only
//     if it parses as a valid IP that is NOT inside trustedProxies (a
//     trusted X-Real-IP is a proxy's own address, not a client).
//   - everything else falls back to RemoteAddr.
//
// If RemoteAddr is not trusted, XFF/X-Real-IP are ignored entirely (they
// are client-spoofable).
func GetClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	if len(trustedProxies) == 0 {
		return host
	}

	// Check if RemoteAddr is from a trusted proxy
	remoteIP := net.ParseIP(host)
	if !ipTrusted(remoteIP, trustedProxies) {
		return host
	}

	// RemoteAddr is trusted — walk X-Forwarded-For right-to-left, skipping
	// trusted hops, and return the first untrusted valid IP. X-Real-IP is
	// deliberately NOT consulted while XFF is present: XFF is the
	// hop-by-hop chain the trusted proxy maintains, X-Real-IP is an
	// independent claim the same proxy could have been fed by the client.
	// An all-trusted (or all-invalid) XFF therefore falls back to
	// RemoteAddr, never to X-Real-IP.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(parts[i]))
			if ip == nil {
				continue
			}
			if ipTrusted(ip, trustedProxies) {
				continue
			}
			return ip.String()
		}
		return host
	}
	// XFF is empty: X-Real-IP is accepted ONLY when it is a valid IP that is
	// NOT itself a trusted proxy address (a trusted value is the proxy's own
	// IP, not a client). Otherwise fall back to RemoteAddr.
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		if ip := net.ParseIP(strings.TrimSpace(xri)); ip != nil && !ipTrusted(ip, trustedProxies) {
			return ip.String()
		}
	}
	return host
}

// ipTrusted reports whether ip falls inside any of the trusted CIDRs.
func ipTrusted(ip net.IP, trustedProxies []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, cidr := range trustedProxies {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// RateLimitMiddleware returns an HTTP middleware that rate-limits per client IP.
// /health and /ready are exempt: /health is the liveness endpoint used by
// load balancers and deploy checks, and /ready is the readiness endpoint
// used by deploy.sh — both must stay reachable when business traffic is
// being limited. No other path is exempted — /v1/*, /metrics and /admin/*
// remain limited.
func RateLimitMiddleware(next http.Handler, rl *RateLimiter, trustedProxies []*net.IPNet) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rl != nil {
			if r.URL.Path == "/health" || r.URL.Path == "/ready" {
				next.ServeHTTP(w, r)
				return
			}
			ip := GetClientIP(r, trustedProxies)
			if !rl.Allow(ip) {
				util.RecordRateLimited()
				slog.Warn("rate_limit.hit", "ip", ip, "path", r.URL.Path, "req", util.RequestIDFromCtx(r.Context()))
				util.WriteJSON(w, http.StatusTooManyRequests, map[string]any{
					"error": map[string]any{"message": "Rate limit exceeded", "code": "rate_limited"},
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
