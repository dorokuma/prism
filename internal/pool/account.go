package pool

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dorokuma/prism/internal/config"
)

// AccountStatus represents the health status of an upstream account.
type AccountStatus int

const (
	StatusHealthy AccountStatus = iota
	StatusExhausted
)

// Account represents an upstream API account with its key, base URL, HTTP client,
// and health/cooldown state for pool selection.
type Account struct {
	cfg           config.AccountConfig
	status        AccountStatus
	client        *http.Client
	mu            sync.Mutex
	inFlight      atomic.Int32
	totalRequests atomic.Int64
	cooldownCount atomic.Int64
	exhaustCount  atomic.Int64
	cooldownUntil time.Time
}

func (a *Account) Name() string               { return a.cfg.Name }
func (a *Account) Key() string                { return a.cfg.Key }
func (a *Account) Headers() map[string]string { return a.cfg.Headers }
func (a *Account) AuthHeader() string         { return a.cfg.AuthHeader }
func (a *Account) ProbePath() string          { return a.cfg.ProbePath }
func (a *Account) SkipPISync() bool           { return a.cfg.SkipPISync }

// Provider returns the provider name this account belongs to.
// Empty string means the account belongs to no specific provider (backward compat).
func (a *Account) Provider() string {
	return a.cfg.Provider
}

func (a *Account) BaseURL() string      { return a.cfg.BaseURL }
func (a *Account) Client() *http.Client { return a.client }

func (a *Account) IsHealthy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status == StatusHealthy
}

func (a *Account) MarkExhausted() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status == StatusHealthy {
		a.status = StatusExhausted
		a.exhaustCount.Add(1)
		slog.Warn("account marked exhausted", "account", a.Name(), "in_flight", a.inFlight.Load())
	}
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			DisableCompression:    true,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

func (a *Account) MarkHealthy() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status == StatusExhausted {
		a.status = StatusHealthy
		a.cooldownUntil = time.Time{}
		slog.Info("account marked healthy", "account", a.Name())
	}
}

func (a *Account) Status() AccountStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

func (a *Account) TryAcquire(max int) bool {
	for {
		cur := a.inFlight.Load()
		if cur >= int32(max) {
			return false
		}
		if a.inFlight.CompareAndSwap(cur, cur+1) {
			a.totalRequests.Add(1)
			return true
		}
	}
}

func (a *Account) Release() {
	for {
		cur := a.inFlight.Load()
		if cur <= 0 {
			slog.Warn("Release on zero inFlight", "account", a.Name())
			return
		}
		if a.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

func (a *Account) InFlightCount() int {
	return int(a.inFlight.Load())
}

func (a *Account) TotalRequests() int64 {
	return a.totalRequests.Load()
}

func (a *Account) CooldownCount() int64 {
	return a.cooldownCount.Load()
}

func (a *Account) ExhaustCount() int64 {
	return a.exhaustCount.Load()
}

func (a *Account) SetCooldown(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	newUntil := time.Now().Add(d)
	if newUntil.After(a.cooldownUntil) {
		a.cooldownUntil = newUntil
	}
	a.cooldownCount.Add(1)
	slog.Warn("account cooldown", "account", a.Name(), "duration", d.String(), "until", a.cooldownUntil.Format(time.RFC3339), "in_flight", a.inFlight.Load())
}

func (a *Account) IsInCooldown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Before(a.cooldownUntil)
}

// ApplyAuthHeader writes the account credential header onto dst following the
// account-level auth_header rule (the single source of truth for both upstream
// forwards and probe requests):
//   - auth_header empty/omitted, or its canonical form equals "Authorization":
//     write "Authorization: Bearer <key>" (identical to current behavior,
//     backward compatible)
//   - auth_header any other value (e.g. "x-api-key"):
//     write "<auth_header>: <key>" (raw key, NO Bearer prefix) and do NOT
//     write an Authorization header at all
//
// The credential always comes from acc.Key(); callers must never place keys
// in account headers (see ApplyAccountHeaders).
func ApplyAuthHeader(dst http.Header, acc *Account) {
	authHeader := strings.TrimSpace(acc.AuthHeader())
	if authHeader == "" || http.CanonicalHeaderKey(authHeader) == "Authorization" {
		dst.Set("Authorization", "Bearer "+acc.Key())
		return
	}
	dst.Set(authHeader, acc.Key())
}

// ApplyAccountHeaders applies account-level custom headers to dst using
// Header.Set (account headers override same-named client headers).
// Two header names are always ignored with a warning because the credential
// may only come from acc.Key() via ApplyAuthHeader:
//   - "Authorization" (prism-managed key must not be overridden)
//   - any header whose canonical name equals the account's auth_header
//     (the custom credential header, e.g. "x-api-key")
//
// Nil/empty headers are a no-op (backward compat).
func ApplyAccountHeaders(dst http.Header, acc *Account) {
	authHeader := http.CanonicalHeaderKey(strings.TrimSpace(acc.AuthHeader()))
	for k, v := range acc.Headers() {
		if k == "" {
			continue
		}
		ck := http.CanonicalHeaderKey(k)
		if ck == "Authorization" {
			slog.Warn("account header Authorization ignored; use account key", "account", acc.Name())
			continue
		}
		if authHeader != "" && authHeader != "Authorization" && ck == authHeader {
			slog.Warn("account header ignored; credential comes from account key", "account", acc.Name(), "header", k)
			continue
		}
		dst.Set(k, v)
	}
}

// waiter represents a goroutine waiting for an available account in the pool.
type waiter struct {
	ch     chan struct{}
	active bool
}
