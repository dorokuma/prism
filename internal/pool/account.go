package pool

import (
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dorokuma/prism/internal/config"
)

// AccountStatus represents the health status of an upstream account.
type AccountStatus int

const (
	StatusHealthy   AccountStatus = iota
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

func (a *Account) Name() string         { return a.cfg.Name }
func (a *Account) Key() string          { return a.cfg.Key }
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

// waiter represents a goroutine waiting for an available account in the pool.
type waiter struct {
	ch     chan struct{}
	active bool
}
