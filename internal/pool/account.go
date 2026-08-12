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

// Slot is the non-mismatchable lease for ONE acquired concurrency slot. It
// is created by Account.TryAcquire and consumed by Account.Release / Pool.
// Release takes the Slot — never a bare key/max pair — so a caller can
// never release a slot it did not acquire or pass a wrong max: the lease
// carries the exact account, key and max the slot was acquired with.
//
// released is the slot's one-shot release gate: the FIRST Release that
// wins the CAS consumes the slot; every later Release of the same *Slot is
// warned about and ignored without touching any counter — a duplicate
// release can never decrement the key/total counters a second time and
// steal the quota of a sibling slot on the same key.
type Slot struct {
	acc      *Account
	key      string
	max      int
	released atomic.Bool
}

// Account represents an upstream API account with its key, base URL, HTTP client,
// and health/cooldown state for pool selection.
//
// Concurrency accounting is per-model-KEY (the key is the model name the
// request was resolved with; "" for model-less model-cache fetches): each
// key owns its OWN in-flight counter, so two models are isolated even when
// they share the same max_concurrent_per_account value (a max=2 model can
// never be starved by another max=2 model's traffic — the old per-max-value
// grouping merged them into one shared counter). The per-key caps are
// additionally bounded by the account TOTAL cap (totalCap, 0 = no aggregate
// bound): per-key counters alone would let N configured tiers stack N×max
// in-flight requests on one account, so the total gate keeps the account
// concurrency explicit and finite. The total in-flight count stays
// observable via InFlightCount.
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

	// totalCap is the account-wide aggregate concurrency bound across ALL
	// keys (0 = no aggregate bound). It is set at pool construction (see
	// NewPoolWithTotalCap / config.ResolveAccountTotalCap).
	totalCap int

	// inFlightByKey holds one atomic counter per concurrency key (created
	// lazily; entries are never deleted — the key set is the small number
	// of models served through this account). keyMu guards the map itself,
	// the counters inside are atomic.
	keyMu         sync.Mutex
	inFlightByKey map[string]*atomic.Int32
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

// TotalCap returns the account-wide aggregate concurrency bound (0 = no
// aggregate bound, the NewPool default).
func (a *Account) TotalCap() int { return a.totalCap }

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

// keyCounter returns the atomic counter for the given concurrency key,
// creating it on first use. Entries are never removed: the map key set is
// bounded by the number of models served through this account.
func (a *Account) keyCounter(key string) *atomic.Int32 {
	a.keyMu.Lock()
	defer a.keyMu.Unlock()
	if a.inFlightByKey == nil {
		a.inFlightByKey = make(map[string]*atomic.Int32)
	}
	c := a.inFlightByKey[key]
	if c == nil {
		c = &atomic.Int32{}
		a.inFlightByKey[key] = c
	}
	return c
}

// TryAcquire acquires one concurrency slot for the given key and returns
// its lease (*Slot), or nil when the account is at capacity. It succeeds
// only when BOTH gates hold:
//   - the key's own in-flight count is below max (the per-key cap — the
//     concurrency domain is the MODEL, so other models can never starve it
//     even when they share the same max value), and
//   - the account-wide total is below totalCap (0 = no aggregate bound).
//
// The returned Slot is the ONLY valid argument for Release: it carries the
// exact key and max this acquisition used, so a mismatched release is
// impossible by construction. The total in-flight counter is incremented in
// lockstep so InFlightCount keeps reporting the account-wide sum.
func (a *Account) TryAcquire(key string, max int) *Slot {
	if max <= 0 {
		return nil
	}
	g := a.keyCounter(key)
	for {
		cur := g.Load()
		if cur >= int32(max) {
			return nil
		}
		if !g.CompareAndSwap(cur, cur+1) {
			continue
		}
		// The per-key slot is reserved; now reserve the account-wide total
		// slot with its own CAS loop so the aggregate bound is exact even
		// under concurrent acquisitions. On failure the per-key reservation
		// is returned.
		if !a.reserveTotal() {
			g.Add(-1)
			return nil
		}
		a.totalRequests.Add(1)
		return &Slot{acc: a, key: key, max: max}
	}
}

// reserveTotal reserves one account-wide slot: succeeds when the total is
// below totalCap (or totalCap == 0, meaning no aggregate bound), using a
// CAS loop so N concurrent acquisitions can never overshoot the cap.
func (a *Account) reserveTotal() bool {
	for {
		total := a.inFlight.Load()
		if a.totalCap > 0 && total >= int32(a.totalCap) {
			return false
		}
		if a.inFlight.CompareAndSwap(total, total+1) {
			return true
		}
	}
}

// Release releases the concurrency slot identified by s — the lease
// returned by TryAcquire — and reports whether the slot was actually
// released. Because the slot carries the exact key and max of the
// acquisition, a caller cannot release a slot it never acquired or pass
// a wrong max (the old Release(max) API corrupted the accounting when a
// caller passed the wrong value). Releasing a nil slot, a slot belonging
// to another account, or a slot that was ALREADY released is a caller
// bug: it is warned about, ignored (no counter is touched) and returns
// false. The one-shot gate (Slot.released) makes the release idempotent
// under concurrent duplicate calls: exactly one caller wins the CAS, so
// N racing Releases of the same slot decrement the key and total counters
// exactly once.
func (a *Account) Release(s *Slot) bool {
	if s == nil {
		slog.Warn("Release of nil slot ignored", "account", a.Name())
		return false
	}
	if s.acc != a {
		slog.Warn("Release of a slot belonging to another account ignored", "account", a.Name(), "slot_account", s.acc.Name())
		return false
	}
	// One-shot gate: the first successful CAS consumes the slot; every
	// later Release of the same *Slot (sequential or concurrent) returns
	// false without touching any counter.
	if !s.released.CompareAndSwap(false, true) {
		slog.Warn("duplicate Release of slot ignored", "account", a.Name(), "key", s.key, "max", s.max)
		return false
	}
	g := a.keyCounter(s.key)
	for {
		cur := g.Load()
		if cur <= 0 {
			slog.Warn("Release on zero in-flight", "account", a.Name(), "key", s.key, "max", s.max)
			return false
		}
		if g.CompareAndSwap(cur, cur-1) {
			a.inFlight.Add(-1)
			return true
		}
	}
}

// InFlightCount returns the account-wide total of in-flight slots (the sum
// across every concurrency key).
func (a *Account) InFlightCount() int {
	return int(a.inFlight.Load())
}

// InFlightForKey returns the in-flight count of the given concurrency key
// (0 when no slot of that key was ever acquired). Capacity checks in the
// pool must use this per-key view — never InFlightCount alone — so a
// waiter's max is compared against its own key's counter, which is exactly
// the TryAcquire per-key gate.
func (a *Account) InFlightForKey(key string) int {
	a.keyMu.Lock()
	g := a.inFlightByKey[key]
	a.keyMu.Unlock()
	if g == nil {
		return 0
	}
	return int(g.Load())
}

// canAcquire mirrors TryAcquire's two gates without reserving anything: the
// key's counter is below max AND the account total is below totalCap. It is
// the waiter-wake capacity probe — the wake path must apply exactly the
// same semantics as the acquire path so a woken waiter never immediately
// re-parks.
func (a *Account) canAcquire(key string, max int) bool {
	a.keyMu.Lock()
	g := a.inFlightByKey[key]
	a.keyMu.Unlock()
	if g != nil && int(g.Load()) >= max {
		return false
	}
	if a.totalCap > 0 && int(a.inFlight.Load()) >= a.totalCap {
		return false
	}
	return true
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
// provider is the provider the waiter's SelectByProvider asked for; empty
// means the waiter can use any provider's slot (the global Select path). key
// is the concurrency key the waiter's Select asked for; max is the
// per-key cap it requested — the capacity probe used when a bailing
// waiter's wakeup is transferred to the next waiter.
type waiter struct {
	ch       chan struct{}
	active   bool
	provider string
	key      string
	max      int
}
