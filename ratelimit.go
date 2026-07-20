package main

import (
	"context"
	"encoding/json"
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

const rateLimitIdleTTL = 10 * time.Minute

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

// getClientIP extracts the client IP from the request.
// It checks X-Forwarded-For (first non-private IP), then X-Real-IP, then RemoteAddr.
// WARNING: If X-Forwarded-For can be spoofed by clients, consider only trusting
// the rightmost IP added by your trusted reverse proxy instead of the leftmost.
func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for _, part := range parts {
			ip := net.ParseIP(strings.TrimSpace(part))
			if ip != nil && !ip.IsPrivate() {
				return ip.String()
			}
		}
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		if ip := net.ParseIP(strings.TrimSpace(xri)); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimitMiddleware returns an HTTP middleware that rate-limits per client IP.
func rateLimitMiddleware(next http.Handler, rl *rateLimiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rl != nil {
			ip := getClientIP(r)
			if !rl.Allow(ip) {
				recordRateLimited()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(429)
				json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"message": "Rate limit exceeded", "code": "rate_limited"},
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
