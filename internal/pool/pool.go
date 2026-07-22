package pool

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dorokuma/prism/internal/config"
)

// Pool manages a set of upstream accounts with round-robin selection and
// a FIFO wait queue for contention when all accounts are busy.
type Pool struct {
	accounts []*Account
	nextIdx  uint64
	mu       sync.Mutex
	waiters  *list.List
}

func NewPool(cfgs []config.AccountConfig) *Pool {
	accs := make([]*Account, len(cfgs))
	for i, cfg := range cfgs {
		accs[i] = &Account{
			cfg:    cfg,
			status: StatusHealthy,
			client: newHTTPClient(),
		}
	}
	return &Pool{
		accounts: accs,
		waiters:  list.New(),
	}
}

func (p *Pool) Release(a *Account) {
	a.Release()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiters.Len() > 0 {
		elem := p.waiters.Front()
		p.waiters.Remove(elem)
		w := elem.Value.(*waiter)
		w.active = false
		close(w.ch)
	}
}

func (p *Pool) MarkHealthy(a *Account) {
	a.MarkHealthy()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiters.Len() > 0 {
		elem := p.waiters.Front()
		p.waiters.Remove(elem)
		w := elem.Value.(*waiter)
		w.active = false
		close(w.ch)
	}
}

func (p *Pool) removeWaiterAndTransfer(elem *list.Element) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := elem.Value.(*waiter)
	if w.active {
		p.waiters.Remove(elem)
		w.active = false
	}
}

func (p *Pool) trySelectLocked(maxConcurrent int) *Account {
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
		if acc.IsHealthy() && acc.TryAcquire(maxConcurrent) {
			return acc
		}
	}
	return nil
}

// AccountCount returns the number of accounts in the pool.
func (p *Pool) AccountCount() int {
	return len(p.accounts)
}

// ErrNoHealthyAccounts is returned when no healthy upstream accounts are available in the pool.
var ErrNoHealthyAccounts = errors.New("no healthy accounts available")

// ErrSelectTimeout is returned as a safety net when waiting for an available account exceeds 2×accountSelectTimeout.
// Under normal operation the caller's context (accountSelectTimeout) expires first, so this acts as a fallback.
var ErrSelectTimeout = errors.New("select account timeout")

func (p *Pool) Select(ctx context.Context, maxConcurrent int) (*Account, error) {
	timer := time.NewTimer(2 * config.AccountSelectTimeout)
	defer timer.Stop()

	for {
		hasHealthy := false
		allHealthyInCooldown := true
		var minCooldown time.Duration
		now := time.Now()

		p.mu.Lock()
		for _, acc := range p.accounts {
			acc.mu.Lock()
			isHealthy := acc.status == StatusHealthy
			cooldownUntil := acc.cooldownUntil
			acc.mu.Unlock()

			if isHealthy {
				hasHealthy = true
				if cooldownUntil.After(now) {
					remaining := cooldownUntil.Sub(now)
					if minCooldown == 0 || remaining < minCooldown {
						minCooldown = remaining
					}
				} else {
					allHealthyInCooldown = false
				}
			}
		}

		if !hasHealthy {
			p.mu.Unlock()
			return nil, ErrNoHealthyAccounts
		}

		if acc := p.trySelectLocked(maxConcurrent); acc != nil {
			p.mu.Unlock()
			return acc, nil
		}

		w := &waiter{
			ch:     make(chan struct{}),
			active: true,
		}
		elem := p.waiters.PushBack(w)
		p.mu.Unlock()

		var cooldownChan <-chan time.Time
		var cooldownTimer *time.Timer
		if allHealthyInCooldown && minCooldown > 0 {
			cooldownTimer = time.NewTimer(minCooldown)
			cooldownChan = cooldownTimer.C
		}

		var selectErr error
		var isClosed bool
		select {
		case <-ctx.Done():
			selectErr = ctx.Err()
		case <-timer.C:
			selectErr = ErrSelectTimeout
		case <-w.ch:
			isClosed = true
		case <-cooldownChan:
		}

		if selectErr != nil {
			p.removeWaiterAndTransfer(elem)
			if cooldownTimer != nil {
				cooldownTimer.Stop()
			}
			return nil, selectErr
		}

		if !isClosed {
			select {
			case <-w.ch:
				isClosed = true
			default:
			}
		}

		if cooldownTimer != nil {
			cooldownTimer.Stop()
		}

		if !isClosed {
			p.removeWaiterAndTransfer(elem)
		}
	}
}

func (p *Pool) AllAccounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]*Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

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

// PoolSnapshot holds a point-in-time summary of pool state for metrics/observability.
type PoolSnapshot struct {
	Total        int
	Healthy      int
	Exhausted    int
	InCooldown   int
	InFlightSum  int
}

func (p *Pool) SnapshotStats() PoolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	var s PoolSnapshot
	s.Total = len(p.accounts)
	for _, a := range p.accounts {
		a.mu.Lock()
		if a.status == StatusHealthy {
			s.Healthy++
		} else {
			s.Exhausted++
		}
		if time.Now().Before(a.cooldownUntil) {
			s.InCooldown++
		}
		a.mu.Unlock()
		s.InFlightSum += int(a.inFlight.Load())
	}
	return s
}
