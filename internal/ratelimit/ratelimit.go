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
// cap on the number of tracked IP buckets. When the cap is reached, the
// bucket with the oldest lastCheck is evicted to make room for a new IP, so
// a flood of distinct IPs cannot grow the map without bound. Tests inject a
// small cap; production uses config.RateLimitMaxBuckets (100000).
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
			evictOldestBucket(rl.buckets)
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

// evictOldestBucket removes the bucket with the oldest lastCheck (ties are
// broken arbitrarily). Must be called with rl.mu held; the evicted entry is
// always one of the oldest-lastCheck buckets, never a recently active IP.
func evictOldestBucket(buckets map[string]*tokenBucket) {
	var oldestIP string
	var oldestTime time.Time
	for ip, b := range buckets {
		if oldestIP == "" || b.lastCheck.Before(oldestTime) {
			oldestIP = ip
			oldestTime = b.lastCheck
		}
	}
	if oldestIP != "" {
		delete(buckets, oldestIP)
	}
}

// GetClientIP extracts the client IP from the request using trusted proxy
// awareness. If trustedProxies is empty, X-Forwarded-For and X-Real-IP are
// ignored entirely (only RemoteAddr is used) to prevent IP spoofing. If
// trustedProxies is non-empty and RemoteAddr is within a trusted CIDR, the
// X-Forwarded-For chain is walked right-to-left skipping every trusted proxy
// hop, and the first valid IP that is NOT trusted is returned (the original
// client). This makes multi-hop chains (client → trusted proxy 1 → trusted
// proxy 2 → prism) resolve to the client, not to the innermost proxy. If no
// untrusted valid IP is found, X-Real-IP is tried, then RemoteAddr. If
// RemoteAddr is not trusted, XFF/X-Real-IP are ignored.
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
	// trusted hops, and return the first untrusted valid IP.
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
	}
	// No XFF, or every XFF hop is trusted: fall back to X-Real-IP, then host.
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		if ip := net.ParseIP(strings.TrimSpace(xri)); ip != nil {
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
func RateLimitMiddleware(next http.Handler, rl *RateLimiter, trustedProxies []*net.IPNet) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rl != nil {
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
