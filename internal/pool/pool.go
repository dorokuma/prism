package pool

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"sync/atomic"
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
	quotaReviveAfter time.Duration
}

// NewPool builds a pool without an account-wide aggregate concurrency
// bound (totalCap = 0). Production wiring uses NewPoolWithTotalCap with
// config.ResolveAccountTotalCap; the bare constructor exists for tests and
// callers that manage the bound themselves.
func NewPool(cfgs []config.AccountConfig) *Pool {
	return newPool(cfgs, 0)
}

// NewPoolWithTotalCap builds a pool whose every account is bounded by the
// given account-wide aggregate concurrency cap (across ALL concurrency
// keys; see Account.totalCap). A non-positive cap means no aggregate bound.
func NewPoolWithTotalCap(cfgs []config.AccountConfig, totalCap int) *Pool {
	return newPool(cfgs, totalCap)
}

func newPool(cfgs []config.AccountConfig, totalCap int) *Pool {
	accs := make([]*Account, len(cfgs))
	for i, cfg := range cfgs {
		accs[i] = &Account{
			cfg:           cfg,
			status:        StatusHealthy,
			client:        newHTTPClient(),
			totalCap:      totalCap,
			inFlightByKey: make(map[string]*atomic.Int32),
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
		quotaReviveAfter: config.QuotaReviveAfter,
	}
}

// SetQuotaReviveAfter sets the duration a quota-exhausted account remains
// exhausted before it may be revived.
func (p *Pool) SetQuotaReviveAfter(d time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.quotaReviveAfter = d
}

// QuotaReviveAfter returns the duration a quota-exhausted account remains exhausted.
func (p *Pool) QuotaReviveAfter() time.Duration {
	if p == nil {
		return config.QuotaReviveAfter
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.quotaReviveAfter
}

// Release frees the concurrency slot of the given lease and wakes the
// first queued waiter that can actually use currently-free capacity:
// key-matched AND within its own max and the account total right now.
// Waking the front waiter without the capacity check would let a front
// waiter with a too-small max or a different key consume the wakeup and
// re-park at the back while a later usable waiter stays parked — the
// mixed-key lost wakeup.
//
// The wake scan runs ONLY when the slot was actually released (Account.
// Release returned true — a unit was returned to the account-wide total,
// on the normal path or on the abnormal zeroed-key path that returns its
// total unit): capacity was freed, so the first usable waiter is woken. A
// nil slot, a duplicate release (the slot's one-shot gate was already
// consumed) or a release with nothing to return (even the account total is
// already 0 — whether the per-key was zeroed or the total was reset under
// a live slot) frees no capacity, so no waiter can be woken — a duplicate
// or empty release must never trigger a false waiter wakeup (the woken
// waiter would re-park on capacity that was never freed).
//
// s must be the *Slot returned by the acquisition (TryAcquire via Select /
// SelectByProvider); the slot carries the exact account, key and max, so a
// mismatched release is impossible.
func (p *Pool) Release(s *Slot) {
	if s == nil {
		return
	}
	if !s.acc.Release(s) {
		// Nothing was actually released: no capacity was freed, so no
		// waiter can be woken.
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// No priority pass: a release must wake the front-most waiter whose OWN
	// capacity is available, whatever provider it asked for (see
	// wakeNextUsable).
	p.wakeNextUsable(nil)
}

func (p *Pool) MarkHealthy(a *Account) {
	a.MarkHealthy()
	p.mu.Lock()
	defer p.mu.Unlock()
	// A recovery event runs a two-pass scan (wakeNextUsable): the freshly-
	// healthy account may only serve waiters whose provider/key/max fit, so
	// the priority pass serves THOSE waiters first — waking an unusable
	// front waiter would strand the usable waiter behind it. The priority
	// pass is bound to THIS account, not to its provider: it serves the
	// first waiter the recovered account ITSELF can serve right now (same
	// provider or a global waiter, healthy, out of cooldown, below its own
	// per-key and total caps), so a front waiter the recovered account
	// cannot serve — even one a sibling account of the same provider could
	// serve — does not swallow the recovery. Only when the recovered account
	// can serve none of the queued waiters does the fallback pass wake the
	// front-most waiter whose OWN capacity is available, so a recovery event
	// is never spent on a provider that did not recover while a waiter with
	// free capacity of its own stays parked.
	p.wakeNextUsable(a)
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
	// provider/key/max: with mixed maxConcurrent values the freed capacity
	// may be unusable for THIS waiter yet usable for the next one (a max=1
	// waiter bailing while inFlight is in [1, maxOfNext) leaves the next
	// max=N waiter servable). Gating on the bailing waiter's max would
	// strand that waiter until its fallback timer — the mixed-max lost
	// wakeup. wakeNextUsable applies each candidate waiter's OWN
	// provider/key/max check, so the transfer can never wake a waiter that
	// would immediately re-park.
	if woken {
		// No priority pass: the transfer is a plain capacity-freeing wakeup
		// (same semantics as Release), not a recovery event.
		p.wakeNextUsable(nil)
	}
}

// capacityAvailableFor reports whether a waiter with the given provider
// ("" = any account), key and maxConcurrent could currently acquire a slot:
// some matching account is healthy, out of cooldown, below ITS OWN per-key
// cap (InFlightForKey(key) < max) AND below the account total cap — exactly
// the two gates TryAcquire applies, so a woken waiter never immediately
// re-parks. Must be called with p.mu held.
func (p *Pool) capacityAvailableFor(provider, key string, max int) bool {
	for _, acc := range p.accounts {
		if provider != "" && acc.Provider() != provider {
			continue
		}
		if acc.IsInCooldown() || !acc.IsHealthy() {
			continue
		}
		if acc.canAcquire(key, max) {
			return true
		}
	}
	return false
}

// accountCanServe reports whether THIS single account could serve the given
// waiter right now: it is healthy, out of cooldown, and below both its
// per-key cap (InFlightForKey(key) < max) and its account total cap —
// exactly the gates trySelectLocked applies before TryAcquire. Unlike
// capacityAvailableFor, which answers the provider-level existence question
// ("some account of the waiter's provider could serve it"), this asks about
// the ONE account — which is what MarkHealthy's priority pass needs: the
// priority pass serves the waiter the RECOVERED ACCOUNT can serve, not one
// that merely a sibling account of the same provider could. Must be called
// with p.mu held, like every other wake-path capacity probe, so the queue
// traversal and the account health/cooldown reads stay consistent with the
// acquisition path.
func accountCanServe(a *Account, w *waiter) bool {
	return a.IsHealthy() && !a.IsInCooldown() && a.canAcquire(w.key, w.max)
}

// wakeNextUsable wakes the front-most queued waiter that can actually use
// currently-free capacity — the wakeup must not be spent on a waiter that
// would immediately re-park. FIFO is preserved within the usable set
// because every scan starts at the queue front. It is the single wake path
// for every capacity-freeing event: Release/MarkHealthy (initial wakeup
// after a slot frees or an account recovers) and the cancel-vs-release
// transfer (removeWaiterAndTransfer). Must be called with p.mu held.
//
// prefer is nil for a plain capacity-freeing wakeup (Release and the
// cancel-vs-release transfer), which runs only the unfiltered pass below.
// For MarkHealthy it is the recovered account and the scan runs in two
// passes. The PRIORITY pass serves the first waiter that the RECOVERED
// ACCOUNT ITSELF can serve right now — w.provider == "" (a global waiter)
// or w.provider == prefer.Provider(), AND accountCanServe(prefer, w): that
// is the pre-existing semantics of a recovery event (serve the recovered
// provider's waiters first), narrowed from "the recovered provider" to "the
// one account that actually recovered". Only when that pass finds nobody
// does the FALLBACK pass run the plain unfiltered scan, so a recovery event
// can never leave a waiter stranded whose own capacity is free — binding
// the whole wakeup to the event's provider is what causes the mixed-
// provider lost wakeup.
//
// The priority pass deliberately asks about the ONE recovered account
// rather than about "some account of its provider": capacityAvailableFor
// (the fallback predicate) answers a provider-level existence question, so
// a front waiter that the recovered account cannot serve — but a sibling
// account of the same provider can — would swallow the recovery event and
// leave the waiter the recovered account CAN serve parked until its 60s
// 2*AccountSelectTimeout fallback. accountCanServe adds trySelectLocked's
// own cooldown/unhealthy skip on top of canAcquire's per-key and total
// gates, so a woken waiter never immediately re-parks on a stale wakeup.
//
// Residual limitation (pre-existing, NOT fixed here): the priority pass only
// decides WHICH waiter to wake — it does not hand the recovered account's
// capacity over to it. The woken waiter re-runs its OWN
// trySelectLocked(provider, key, max), which starts at the providerNextIdx /
// nextIdx cursor and may land on a DIFFERENT account of the same provider,
// so the capacity the recovery freed is not guaranteed to be claimed by
// this wakeup.
//
// The FALLBACK scan (and Release's single scan) is deliberately NOT filtered
// by the provider of the account whose capacity was just freed: the wakeup
// is a queue re-check signal, not a handover of that specific slot. A woken
// waiter re-runs its OWN
// trySelectLocked(provider, key, max) and may land on a DIFFERENT account
// (another account of its provider, or the full-pool cursor), so binding
// the wakeup to the releasing account's provider strands a servable waiter
// whose own provider has had free capacity all along — the mixed-provider
// lost wakeup: Release(x1) wakes the front global waiter, Release(y1) then
// finds only an X-only waiter and (with the provider filter) wakes nobody,
// the global waiter claims y1, and x1 stays free with the X waiter parked
// until the 60s fallback. That sequence strands the X waiter only while the
// woken global waiter sits in the wake->acquire window — already dequeued
// by the first scan but not yet holding its slot; once it takes x1 no waiter
// is left behind. The waiter's OWN capacity check is the exact predicate
// that matters: it mirrors TryAcquire's per-key and total gates, so a woken
// waiter never immediately re-parks on a stale wakeup.
func (p *Pool) wakeNextUsable(prefer *Account) {
	if prefer != nil {
		provider := prefer.Provider()
		for elem := p.waiters.Front(); elem != nil; elem = elem.Next() {
			w := elem.Value.(*waiter)
			if w.provider != "" && w.provider != provider {
				continue
			}
			if !accountCanServe(prefer, w) {
				continue
			}
			p.wakeWaiter(elem, w)
			return
		}
	}
	for elem := p.waiters.Front(); elem != nil; elem = elem.Next() {
		w := elem.Value.(*waiter)
		if !p.capacityAvailableFor(w.provider, w.key, w.max) {
			continue
		}
		p.wakeWaiter(elem, w)
		return
	}
}

// wakeWaiter removes the given waiter from the queue, marks it inactive and
// closes its channel — the single wake primitive shared by both passes of
// wakeNextUsable, so the two passes can never diverge in wake mechanics.
// Must be called with p.mu held.
func (p *Pool) wakeWaiter(elem *list.Element, w *waiter) {
	p.waiters.Remove(elem)
	w.active = false
	close(w.ch)
}

// trySelectLocked returns the account+lease for one acquisition on an
// account of the given provider ("" = any account, using the full-pool
// cursor), or nil when every candidate account is at capacity. Each
// provider rotates through its own account slice with its own cursor so
// one provider's selections never advance or pollute another provider's
// rotation; the full-pool path keeps its own cursor. A cursor only
// advances past the account that was actually selected, so skipped
// (cooldown/busy) accounts fall behind the cursor instead of being
// re-picked.
func (p *Pool) trySelectLocked(provider, key string, maxConcurrent int) (*Account, *Slot) {
	if provider == "" {
		if len(p.accounts) == 0 {
			return nil, nil
		}
		startIdx := int(p.nextIdx % uint64(len(p.accounts)))
		for i := 0; i < len(p.accounts); i++ {
			idx := (startIdx + i) % len(p.accounts)
			acc := p.accounts[idx]
			if acc.IsInCooldown() {
				continue
			}
			if acc.IsHealthy() {
				if s := acc.TryAcquire(key, maxConcurrent); s != nil {
					// Advance exactly one position per successful selection so
					// the full-pool rotation stays uniform.
					p.nextIdx = uint64(idx) + 1
					return acc, s
				}
			}
		}
		return nil, nil
	}
	accs := p.providerAccounts[provider]
	if len(accs) == 0 {
		return nil, nil
	}
	startIdx := int(p.providerNextIdx[provider] % uint64(len(accs)))
	for i := 0; i < len(accs); i++ {
		idx := (startIdx + i) % len(accs)
		acc := accs[idx]
		if acc.IsInCooldown() {
			continue
		}
		if acc.IsHealthy() {
			if s := acc.TryAcquire(key, maxConcurrent); s != nil {
				// Land the cursor right after the selected account so the
				// next selection continues the rotation.
				p.providerNextIdx[provider] = uint64(idx) + 1
				return acc, s
			}
		}
	}
	return nil, nil
}

// Ready reports whether the pool can serve at least one request right now:
// some account is healthy AND out of cooldown. It is the readiness probe
// behind /ready (deploy.sh), deliberately distinct from liveness: the
// process may be up (liveness) while every account is exhausted or cooling
// down — ready=false tells the load balancer / deploy script to hold
// traffic until at least one account can actually serve it.
//
// The cooldown boundary matches Select/IsInCooldown exactly: an account is
// selectable when cooldownUntil is NOT after now (!now.Before(cooldownUntil),
// i.e. now == cooldownUntil counts as expired), so /ready can never report
// ready=false for an account that Select would pick right now.
func (p *Pool) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, a := range p.accounts {
		a.mu.Lock()
		ok := a.status == StatusHealthy && !now.Before(a.cooldownUntil)
		a.mu.Unlock()
		if ok {
			return true
		}
	}
	return false
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

// Select acquires a concurrency slot for the given key (the model name the
// caller resolved the max from; "" for model-less fetches) on any account,
// waiting up to the context / fallback timer when all accounts are busy.
// It returns the selected account and its *Slot lease; Release(slot) is
// the only way to free the slot.
func (p *Pool) Select(ctx context.Context, key string, maxConcurrent int) (*Account, *Slot, error) {
	return p.selectKeyed(ctx, key, maxConcurrent, "")
}

// SelectByProvider is like Select but only considers accounts belonging to
// the given provider. When provider is empty, it falls back to Select (all
// accounts).
func (p *Pool) SelectByProvider(ctx context.Context, key string, maxConcurrent int, provider string) (*Account, *Slot, error) {
	if provider == "" {
		return p.Select(ctx, key, maxConcurrent)
	}
	return p.selectKeyed(ctx, key, maxConcurrent, provider)
}

// selectKeyed is the single implementation behind Select and
// SelectByProvider: acquire a slot for (key, max) on an account of the
// given provider ("" = any), parking in the FIFO wait queue when every
// candidate account is at capacity. The waiter carries the SAME (provider,
// key, max) the acquisition used, so the wake scan (wakeNextUsable) applies
// the identical per-key + total-capacity gates as TryAcquire.
func (p *Pool) selectKeyed(ctx context.Context, key string, maxConcurrent int, provider string) (*Account, *Slot, error) {
	timer := time.NewTimer(2 * config.AccountSelectTimeout)
	defer timer.Stop()

	for {
		hasHealthy := false
		var minCooldown time.Duration
		now := time.Now()

		p.mu.Lock()
		for _, acc := range p.accounts {
			if provider != "" && acc.Provider() != provider {
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
				}
			}
		}

		if !hasHealthy {
			p.mu.Unlock()
			return nil, nil, ErrNoHealthyAccounts
		}

		if acc, s := p.trySelectLocked(provider, key, maxConcurrent); s != nil {
			p.mu.Unlock()
			return acc, s, nil
		}

		w := &waiter{
			ch:       make(chan struct{}),
			active:   true,
			provider: provider,
			key:      key,
			max:      maxConcurrent,
		}
		elem := p.waiters.PushBack(w)
		p.mu.Unlock()

		// Arm a timer for the nearest matching healthy cooldown, even when
		// another healthy sibling is only at capacity. Requiring every
		// healthy account to be in cooldown left waiters parked until
		// Release or the 2*AccountSelectTimeout safety net (429 cooldown
		// mixed with a long-stream full slot). The timer does not close
		// w.ch, so it cannot double-wake with Release/MarkHealthy.
		var cooldownChan <-chan time.Time
		var cooldownTimer *time.Timer
		if minCooldown > 0 {
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
			return nil, nil, selectErr
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
