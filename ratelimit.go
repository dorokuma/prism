package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateLimiter implements a simple per-IP token bucket rate limiter.
type rateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*tokenBucket
	rate      int // tokens per second
	burst     int // max burst
}

type tokenBucket struct {
	tokens    float64
	lastCheck time.Time
}

func newRateLimiter(rate, burst int) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    rate,
		burst:   burst,
	}
}

func (rl *rateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	b, ok := rl.buckets[ip]
	if !ok {
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

// startCleanupLoop starts a background goroutine that periodically cleans up stale buckets.
func (rl *rateLimiter) startCleanupLoop(ctx context.Context) {
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
					if now.Sub(b.lastCheck) > rateLimitIdleTTL {
						delete(rl.buckets, ip)
					}
				}
				rl.mu.Unlock()
			}
		}
	}()
}

// getClientIP extracts the client IP from the request using trusted proxy awareness.
// If trustedProxies is empty, X-Forwarded-For and X-Real-IP are ignored entirely
// (only RemoteAddr is used) to prevent IP spoofing.
// If trustedProxies is non-empty and RemoteAddr is within a trusted CIDR,
// the rightmost IP from X-Forwarded-For is trusted (or X-Real-IP as fallback).
// If RemoteAddr is not trusted, XFF/X-Real-IP are ignored.
func getClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	if len(trustedProxies) == 0 {
		return host
	}

	// Check if RemoteAddr is from a trusted proxy
	remoteIP := net.ParseIP(host)
	trusted := false
	if remoteIP != nil {
		for _, cidr := range trustedProxies {
			if cidr.Contains(remoteIP) {
				trusted = true
				break
			}
		}
	}

	if !trusted {
		return host
	}

	// RemoteAddr is trusted — use X-Forwarded-For (rightmost) or X-Real-IP
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(parts[i]))
			if ip != nil {
				return ip.String()
			}
		}
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		if ip := net.ParseIP(strings.TrimSpace(xri)); ip != nil {
			return ip.String()
		}
	}
	return host
}

// rateLimitMiddleware returns an HTTP middleware that rate-limits per client IP.
func rateLimitMiddleware(next http.Handler, rl *rateLimiter, trustedProxies []*net.IPNet) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rl != nil {
			ip := getClientIP(r, trustedProxies)
			if !rl.Allow(ip) {
				recordRateLimited()
				writeJSON(w, http.StatusTooManyRequests, map[string]any{
					"error": map[string]any{"message": "Rate limit exceeded", "code": "rate_limited"},
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
