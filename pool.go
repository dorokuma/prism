package main

import (
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type AccountStatus int

const (
	StatusHealthy AccountStatus = iota
	StatusExhausted
)

const upstreamTimeout = 10 * time.Minute

type Account struct {
	cfg           AccountConfig
	status        AccountStatus
	client        *http.Client
	mu            sync.Mutex
	borrowed      atomic.Bool
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
		log.Printf("account %s: marked exhausted (removed from pool)", a.Name())
	}
}

func newHTTPClient() *http.Client {
	return &http.Client{
		// Timeout must be 0: per-request context owns the full lifecycle
		// (headers + streaming body). Client.Timeout aborts body reads
		// independently and poisons shared keep-alive connections.
		Timeout: 0,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

func (a *Account) MarkHealthy() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status == StatusExhausted {
		a.status = StatusHealthy
		a.cooldownUntil = time.Time{} // 清除冷却
		log.Printf("account %s: marked healthy (returned to pool)", a.Name())
	}
}

func (a *Account) Status() AccountStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// TryBorrow 尝试借用该账号。如果已被借用返回 false。
func (a *Account) TryBorrow() bool {
	return a.borrowed.CompareAndSwap(false, true)
}

// Release 释放借用，请求完成后必须调用。
func (a *Account) Release() {
	a.borrowed.Store(false)
}

// SetCooldown 设置冷却时间（用于 429 临时限流场景）
func (a *Account) SetCooldown(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cooldownUntil = time.Now().Add(d)
}

// IsInCooldown 检查是否在冷却期内
func (a *Account) IsInCooldown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Before(a.cooldownUntil)
}

type Pool struct {
	accounts []*Account
	nextIdx  uint64
	mu       sync.Mutex
}

func NewPool(cfgs []AccountConfig) *Pool {
	accs := make([]*Account, len(cfgs))
	for i, cfg := range cfgs {
		accs[i] = &Account{
			cfg:    cfg,
			status: StatusHealthy,
			client: newHTTPClient(),
		}
	}
	return &Pool{accounts: accs}
}

// Select returns a healthy account via round-robin. Returns nil if none healthy.
func (p *Pool) Select() *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accounts) == 0 {
		return nil
	}
	startIdx := int(p.nextIdx % uint64(len(p.accounts)))
	for i := 0; i < len(p.accounts); i++ {
		idx := (startIdx + i) % len(p.accounts)
		p.nextIdx++
		acc := p.accounts[idx]
		if acc.IsInCooldown() {
			continue
		}
		if acc.IsHealthy() && acc.TryBorrow() {
			return acc
		}
	}
	return nil
}

// AllAccounts returns a copy of all accounts (healthy + exhausted).
func (p *Pool) AllAccounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]*Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

// ExhaustedAccounts returns all accounts currently in exhausted state (for probing).
func (p *Pool) ExhaustedAccounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*Account
	for _, a := range p.accounts {
		if a.Status() == StatusExhausted {
			out = append(out, a)
		}
	}
	return out
}
