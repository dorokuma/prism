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
//
// Round-robin state: each provider has its own account slice
// (providerAccounts) and its own cursor (providerNextIdx) that rotates
// strictly within that slice, plus a single fallback cursor (nextIdx) for
// the full-pool Select path. A cursor only advances past the account that
// was actually selected, so selections rotate uniformly and traffic on one
// provider never pollutes another provider's rotation.
type Pool struct {
	accounts         []*Account
	providerAccounts map[string][]*Account
	providerNextIdx  map[string]uint64
	nextIdx          uint64
	mu               sync.Mutex
	waiters          *list.List
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
	// Per-provider account slices preserve the flattened config (YAML) order
	// within a provider. Providers themselves may appear in any order because
	// config flattening iterates a Go map; that randomness is confined to
	// provider order and does not affect in-provider rotation.
	providerAccounts := make(map[string][]*Account)
	for _, acc := range accs {
		prov := acc.Provider()
		providerAccounts[prov] = append(providerAccounts[prov], acc)
	}
	return &Pool{
		accounts:         accs,
		providerAccounts: providerAccounts,
		providerNextIdx:  make(map[string]uint64),
		waiters:          list.New(),
	}
}

func (p *Pool) Release(a *Account) {
	a.Release()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Wake the first waiter that can actually use the freed capacity:
	// provider-matched AND within its own max right now. Waking the front
	// waiter without the capacity check would let a front waiter with a
	// too-small max consume the wakeup and re-park at the back while a
	// later usable waiter stays parked — the mixed-max lost wakeup.
	p.wakeNextUsable(a.Provider())
}

func (p *Pool) MarkHealthy(a *Account) {
	a.MarkHealthy()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Same capacity-aware scan as Release: the freshly-healthy account
	// may only serve waiters whose provider/max fit, and waking an
	// unusable front waiter would strand the usable waiter behind it.
	p.wakeNextUsable(a.Provider())
}

func (p *Pool) removeWaiterAndTransfer(elem *list.Element) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := elem.Value.(*waiter)
	woken := !w.active
	if w.active {
		p.waiters.Remove(elem)
		w.active = false
	}
	// The waiter is bailing (ctx cancel / select timeout) after a wake was
	// already consumed on it: a concurrent Release/MarkHealthy (or a
	// previous transfer) freed capacity and woke THIS waiter, but the
	// waiter's select picked the error case and exits without taking the
	// slot. Without a handover the freed capacity would sit idle while the
	// next waiter that could use it stays parked — the classic
	// cancel-vs-release lost wakeup. Transfer the wakeup to the next
	// waiter that can actually use currently-available capacity. When the
	// waiter was never woken (woken == false) its removal cannot consume a
	// wakeup: the releasing caller's own wake scan runs after this removal
	// and reaches the next waiter directly, so transferring here would
	// double-wake.
	//
	// The transfer is NOT gated on the bailing waiter's own
	// provider/max: with mixed maxConcurrent values the freed capacity may
	// be unusable for THIS waiter yet usable for the next one (a max=1
	// waiter bailing while inFlight is in [1, maxOfNext) leaves the next
	// max=N waiter servable). Gating on the bailing waiter's max would
	// strand that waiter until its fallback timer — the mixed-max lost
	// wakeup. wakeNextUsable applies each candidate waiter's OWN
	// provider/max check, so the transfer can never wake a waiter that
	// would immediately re-park.
	if woken {
		p.wakeNextUsable(w.provider)
	}
}

// capacityAvailableFor reports whether a waiter with the given provider
// ("" = any account) and maxConcurrent could currently acquire a slot: some
// matching account is healthy, out of cooldown, and below its concurrency
// cap. Must be called with p.mu held.
func (p *Pool) capacityAvailableFor(provider string, max int) bool {
	for _, acc := range p.accounts {
		if provider != "" && acc.Provider() != provider {
			continue
		}
		if acc.IsInCooldown() || !acc.IsHealthy() {
			continue
		}
		if acc.InFlightCount() < max {
			return true
		}
	}
	return false
}

// wakeNextUsable wakes the front-most queued waiter that can use a slot of
// the given provider ("" = any provider matches) AND has capacity available
// for its own provider/max right now — the wakeup must not be spent on a
// waiter that would immediately re-park. FIFO is preserved within the usable
// set because the scan starts at the queue front. It is the single wake
// path for every capacity-freeing event: Release/MarkHealthy (initial
// wakeup after a slot frees or an account recovers) and the cancel-vs-
// release transfer (removeWaiterAndTransfer). Must be called with p.mu
// held.
func (p *Pool) wakeNextUsable(provider string) {
	for elem := p.waiters.Front(); elem != nil; elem = elem.Next() {
		w := elem.Value.(*waiter)
		if provider != "" && w.provider != "" && w.provider != provider {
			continue
		}
		if !p.capacityAvailableFor(w.provider, w.max) {
			continue
		}
		p.waiters.Remove(elem)
		w.active = false
		close(w.ch)
		return
	}
}

func (p *Pool) trySelectLocked(maxConcurrent int) *Account {
	if len(p.accounts) == 0 {
		return nil
	}
	startIdx := int(p.nextIdx % uint64(len(p.accounts)))
	for i := 0; i < len(p.accounts); i++ {
		idx := (startIdx + i) % len(p.accounts)
		acc := p.accounts[idx]
		if acc.IsInCooldown() {
			continue
		}
		if acc.IsHealthy() && acc.TryAcquire(maxConcurrent) {
			// Advance exactly one position per successful selection so the
			// full-pool rotation stays uniform.
			p.nextIdx = uint64(idx) + 1
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
			ch:       make(chan struct{}),
			active:   true,
			provider: "",
			max:      maxConcurrent,
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

// WaitingCount returns the number of waiters currently parked in the wait
// queue (across all providers). It is a read-only observability view of the
// queue, safe for concurrent use. It also lets tests deterministically
// observe wait-queue entry (poll until a waiter is provably parked — it is
// registered under the pool lock) without a global test hook.
func (p *Pool) WaitingCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waiters.Len()
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
	Total       int
	Healthy     int
	Exhausted   int
	InCooldown  int
	InFlightSum int
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

// SelectByProvider is like Select but only considers accounts belonging to the given provider.
// When provider is empty, it falls back to Select (all accounts).
func (p *Pool) SelectByProvider(ctx context.Context, maxConcurrent int, provider string) (*Account, error) {
	if provider == "" {
		return p.Select(ctx, maxConcurrent)
	}
	timer := time.NewTimer(2 * config.AccountSelectTimeout)
	defer timer.Stop()

	for {
		hasHealthy := false
		allHealthyInCooldown := true
		var minCooldown time.Duration
		now := time.Now()

		p.mu.Lock()
		for _, acc := range p.accounts {
			if acc.Provider() != provider {
				continue
			}
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

		if acc := p.trySelectLockedByProvider(maxConcurrent, provider); acc != nil {
			p.mu.Unlock()
			return acc, nil
		}

		w := &waiter{
			ch:       make(chan struct{}),
			active:   true,
			provider: provider,
			max:      maxConcurrent,
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

// trySelectLockedByProvider is like trySelectLocked but only considers the
// accounts of the given provider, rotating through that provider's own
// account subset with its own cursor so one provider's selections never
// advance or pollute another provider's rotation.
func (p *Pool) trySelectLockedByProvider(maxConcurrent int, provider string) *Account {
	if provider == "" {
		return p.trySelectLocked(maxConcurrent)
	}
	accs := p.providerAccounts[provider]
	if len(accs) == 0 {
		return nil
	}
	startIdx := int(p.providerNextIdx[provider] % uint64(len(accs)))
	for i := 0; i < len(accs); i++ {
		idx := (startIdx + i) % len(accs)
		acc := accs[idx]
		if acc.IsInCooldown() {
			continue
		}
		if acc.IsHealthy() && acc.TryAcquire(maxConcurrent) {
			// Land the cursor right after the selected account so the next
			// selection continues the rotation; skipped (cooldown/busy)
			// accounts fall behind the cursor instead of being re-picked.
			p.providerNextIdx[provider] = uint64(idx) + 1
			return acc
		}
	}
	return nil
}
