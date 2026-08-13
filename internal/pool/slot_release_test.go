package pool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
)

// TestSlotDoubleReleaseDoesNotStealSiblingQuota is the core quota-stealing
// regression: with two active slots on the same key, releasing the FIRST
// slot a second time must not decrement the key or total counters again.
// The old Release decremented unconditionally, so the duplicate dropped
// key/total to 0 while the second request was still in flight — its quota
// silently stolen.
func TestSlotDoubleReleaseDoesNotStealSiblingQuota(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}
	s1 := acc.TryAcquire("m", 2)
	s2 := acc.TryAcquire("m", 2)
	if s1 == nil || s2 == nil {
		t.Fatal("two acquisitions on a max=2 key must both succeed")
	}
	if !acc.Release(s1) {
		t.Error("first Release of a live slot must succeed")
	}
	if acc.Release(s1) {
		t.Error("duplicate Release of the same slot must return false")
	}
	if got := acc.InFlightForKey("m"); got != 1 {
		t.Errorf("InFlightForKey after duplicate release = %d, want 1 (the second slot's quota was stolen)", got)
	}
	if got := acc.InFlightCount(); got != 1 {
		t.Errorf("InFlightCount after duplicate release = %d, want 1", got)
	}
	if !acc.Release(s2) {
		t.Error("Release of the second slot must succeed")
	}
	if got := acc.InFlightForKey("m"); got != 0 {
		t.Errorf("InFlightForKey after both slots released = %d, want 0", got)
	}
	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount after both slots released = %d, want 0", got)
	}
}

// TestSlotDoubleReleaseKeepsCapacityAccountingExact: after a duplicate
// release the key's capacity accounting must stay exact — the key accepts
// exactly ONE more acquisition (the slot the duplicate release was
// supposed to free) and then reports full. The old code had already
// counted that slot as freed twice, so a max=2 key admitted a 4th
// acquisition.
func TestSlotDoubleReleaseKeepsCapacityAccountingExact(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}
	s1 := acc.TryAcquire("m", 2)
	s2 := acc.TryAcquire("m", 2)
	if s1 == nil || s2 == nil {
		t.Fatal("two acquisitions on a max=2 key must both succeed")
	}
	if !acc.Release(s1) {
		t.Error("first Release of a live slot must succeed")
	}
	if acc.Release(s1) { // duplicate — must be ignored (return false)
		t.Error("duplicate Release of the same slot must return false")
	}
	s3 := acc.TryAcquire("m", 2)
	if s3 == nil {
		t.Fatal("after one legitimate release the key must accept exactly one more acquisition")
	}
	if s4 := acc.TryAcquire("m", 2); s4 != nil {
		t.Error("max=2 key accepted a 4th acquisition after a duplicate release — the duplicate stole a sibling slot's quota")
		acc.Release(s4)
	}
	acc.Release(s3)
	if !acc.Release(s2) {
		t.Error("Release of the second slot must succeed")
	}
	if acc.Release(s2) { // extra duplicate after both slots were released
		t.Error("duplicate Release of an already-released slot must return false")
	}
	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount after cleanup = %d, want 0", got)
	}
}

// TestSlotConcurrentDuplicateRelease: many goroutines releasing the SAME
// slot concurrently — the counters must be decremented exactly once (the
// old unconditional decrement let every goroutine through and drove the
// counters negative).
func TestSlotConcurrentDuplicateRelease(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}
	s1 := acc.TryAcquire("m", 2)
	s2 := acc.TryAcquire("m", 2)
	if s1 == nil || s2 == nil {
		t.Fatal("two acquisitions on a max=2 key must both succeed")
	}
	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if acc.Release(s1) {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("exactly one concurrent Release must win the one-shot gate, got %d", wins.Load())
	}
	if got := acc.InFlightForKey("m"); got != 1 {
		t.Errorf("InFlightForKey after 16 concurrent releases of one slot = %d, want 1", got)
	}
	if got := acc.InFlightCount(); got != 1 {
		t.Errorf("InFlightCount after 16 concurrent releases of one slot = %d, want 1", got)
	}
	if !acc.Release(s2) {
		t.Error("Release of the second slot must succeed")
	}
	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount after both slots released = %d, want 0", got)
	}
}

// TestSlotReleaseZeroedKeyReturnsTotal is the zeroed per-key counter
// regression: when a live slot's per-key counter is abnormally reset to 0
// (an invariant break — Release is only ever called with a genuine Slot),
// Release must still return the slot's account-wide total unit instead of
// leaking it forever, and the total can never go negative. The return
// value must stay true (capacity WAS freed) so Pool.Release wakes a
// waiter, and a duplicate Release of the same slot must not decrement
// anything again.
func TestSlotReleaseZeroedKeyReturnsTotal(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}
	s1 := acc.TryAcquire("m", 2)
	if s1 == nil {
		t.Fatal("acquisition must succeed")
	}
	if got := acc.InFlightCount(); got != 1 {
		t.Fatalf("InFlightCount after acquire = %d, want 1", got)
	}

	// Abnormal state: zero the per-key counter out from under the live
	// slot (simulates the invariant break the fix targets).
	acc.keyCounter("m").Store(0)

	// Release must return the total unit: true (capacity freed), total back
	// to 0, and the per-key counter stays 0.
	if !acc.Release(s1) {
		t.Error("Release of a slot whose per-key counter was zeroed must return true (the total unit was returned)")
	}
	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount after zeroed-key Release = %d, want 0 (total must not leak)", got)
	}
	if got := acc.InFlightForKey("m"); got != 0 {
		t.Errorf("InFlightForKey after zeroed-key Release = %d, want 0", got)
	}

	// Duplicate release of the same slot: the one-shot gate was consumed,
	// so it must return false and decrement NOTHING (total stays 0 — it can
	// never go negative).
	if acc.Release(s1) {
		t.Error("duplicate Release after the zeroed-key Release must return false")
	}
	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount after duplicate Release = %d, want 0 (a duplicate must never decrement below zero)", got)
	}
}

// TestSlotReleaseZeroedKeyMixedWithLiveSlots: with a healthy second slot on
// the same key, zeroing the key counter and releasing BOTH slots must end
// with total == 0 — the zeroed slot returns its total unit, the live slot
// releases normally, and the key counter is not over-decremented (it ends
// at 0, never negative).
func TestSlotReleaseZeroedKeyMixedWithLiveSlots(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}
	s1 := acc.TryAcquire("m", 2)
	s2 := acc.TryAcquire("m", 2)
	if s1 == nil || s2 == nil {
		t.Fatal("two acquisitions on a max=2 key must both succeed")
	}
	if got := acc.InFlightCount(); got != 2 {
		t.Fatalf("InFlightCount after two acquires = %d, want 2", got)
	}

	// Zero the key counter while BOTH slots are live: the total (2) now
	// exceeds the key counter (0) by exactly the two leaked units.
	acc.keyCounter("m").Store(0)

	// Both releases must succeed and the total must drain to 0.
	if !acc.Release(s1) || !acc.Release(s2) {
		t.Error("both releases must return true (every slot's total unit is returned)")
	}
	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount after both releases = %d, want 0", got)
	}
	if got := acc.InFlightForKey("m"); got != 0 {
		t.Errorf("InFlightForKey = %d, want 0 (never negative)", got)
	}
}

// TestSlotReleaseZeroedKeyAndTotalNothingToReturn: when BOTH the per-key
// counter and the account total are zero, Release has nothing to return —
// it must report false (no capacity freed → Pool.Release must not wake a
// waiter) and leave both counters at 0.
func TestSlotReleaseZeroedKeyAndTotalNothingToReturn(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}
	s1 := acc.TryAcquire("m", 2)
	if s1 == nil {
		t.Fatal("acquisition must succeed")
	}
	acc.keyCounter("m").Store(0)
	acc.inFlight.Store(0)

	if acc.Release(s1) {
		t.Error("Release with key AND total already zero must return false (nothing was freed)")
	}
	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount = %d, want 0 (never negative)", got)
	}
}

// TestSlotReleaseNormalPathTotalZeroedReturnsFalse pins the normal-path
// symmetry of the fix: when the per-key CAS succeeds (per-key still held
// its unit) but the account-wide TOTAL was reset to 0 out from under the
// live slot (invariant break: total 0 while per-key == 1), Release must
// return false — the per-key unit is returned, but no total unit exists to
// free, so Pool.Release must not wake a waiter onto account-wide capacity
// that did not change (false wakeup) — and the total must never go
// negative. A duplicate Release of the same slot still decrements nothing.
func TestSlotReleaseNormalPathTotalZeroedReturnsFalse(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}
	s1 := acc.TryAcquire("m", 2)
	if s1 == nil {
		t.Fatal("acquisition must succeed")
	}
	if got := acc.InFlightCount(); got != 1 {
		t.Fatalf("InFlightCount after acquire = %d, want 1", got)
	}

	// Abnormal state: zero the account-wide TOTAL only (the per-key counter
	// still holds its unit — the exact case the normal-path returnTotal
	// must handle).
	acc.inFlight.Store(0)
	if got := acc.InFlightForKey("m"); got != 1 {
		t.Fatalf("InFlightForKey before release = %d, want 1 (per-key still held)", got)
	}

	// The per-key CAS succeeds (1 → 0) but returnTotal finds the total
	// already 0: nothing was freed at the total level → false, no wake.
	if acc.Release(s1) {
		t.Error("Release must return false when the total was already 0 (no total unit was freed → no false wakeup)")
	}
	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount after release = %d, want 0 (the total must never go negative)", got)
	}
	if got := acc.InFlightForKey("m"); got != 0 {
		t.Errorf("InFlightForKey after release = %d, want 0 (the per-key unit was returned)", got)
	}

	// Duplicate release of the same slot: the one-shot gate was consumed,
	// so it returns false and decrements NOTHING.
	if acc.Release(s1) {
		t.Error("duplicate Release must return false")
	}
	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount after duplicate Release = %d, want 0 (never negative)", got)
	}
}

// TestPoolReleaseZeroedKeyWakesWaiter: the pool-level consequence of the
// zeroed-key fix — Pool.Release must wake a parked waiter when the
// zeroed-key release actually returned the total unit (capacity was
// freed), exactly like a normal release.
func TestPoolReleaseZeroedKeyWakesWaiter(t *testing.T) {
	cfgs := []config.AccountConfig{{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"}}
	p := NewPool(cfgs)
	ctx := context.Background()
	acc, s1, err := p.Select(ctx, "m", 1)
	if err != nil {
		t.Fatalf("first Select: %v", err)
	}

	ch := make(chan slotResult, 1)
	go func() {
		acc2, slot, err := p.Select(ctx, "m", 1)
		if err != nil {
			ch <- slotResult{}
			return
		}
		ch <- slotResult{acc: acc2, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "one waiter to park")

	// Abnormal zeroing of the held slot's per-key counter, then release:
	// the waiter must be woken (the total unit was returned).
	acc.keyCounter("m").Store(0)
	p.Release(s1)
	got := waitForSlotResult(t, ch, 2*time.Second, "waiter after zeroed-key release")
	if got.acc == nil {
		t.Fatal("the waiter must be woken by the zeroed-key release (capacity was freed)")
	}
	p.Release(got.slot)
	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount after full drain = %d, want 0", got)
	}
}

// TestPoolReadyCooldownBoundary pins the /ready cooldown boundary: Ready
// uses !now.Before(cooldownUntil) while Select uses
// !cooldownUntil.After(now) — the SAME comparison direction, so both treat
// an expired cooldown (cooldownUntil <= now) as out of cooldown and /ready
// can never report not-ready for an account Select would pick at the same
// instant. The test uses strictly-past and strictly-future cooldownUntil
// values (the stable semantics): an expired cooldown is ready, a future
// cooldown is not ready, and an exhausted account is never ready.
func TestPoolReadyCooldownBoundary(t *testing.T) {
	cfgs := []config.AccountConfig{{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"}}
	p := NewPool(cfgs)
	acc := p.AllAccounts()[0]

	// cooldownUntil strictly in the past: expired — selectable AND ready.
	acc.mu.Lock()
	acc.cooldownUntil = time.Now().Add(-time.Minute)
	acc.mu.Unlock()
	if !p.Ready() {
		t.Error("Ready must be true when the cooldown has expired (cooldownUntil in the past, matching Select)")
	}

	// cooldownUntil strictly in the future: not ready (Select would wait on
	// the cooldown timer, so /ready must report not-ready).
	acc.mu.Lock()
	acc.cooldownUntil = time.Now().Add(time.Minute)
	acc.mu.Unlock()
	if p.Ready() {
		t.Error("Ready must be false while cooldownUntil is in the future")
	}

	// Exhausted accounts are never ready, regardless of cooldown.
	acc.MarkExhausted()
	acc.mu.Lock()
	acc.cooldownUntil = time.Now().Add(-time.Minute)
	acc.mu.Unlock()
	if p.Ready() {
		t.Error("Ready must be false for an exhausted account even out of cooldown")
	}
}

// TestPoolReleaseDuplicateDoesNotWakeWaiter: Pool.Release must wake a
// waiter ONLY when a slot was actually released. A duplicate release of an
// already-released slot frees nothing, so the second waiter must stay
// parked — the old Pool.Release always ran the wake scan, so the duplicate
// decremented the counters again and woke the next waiter onto capacity
// that was never freed (a false wakeup that re-parks or, worse, acquires on
// corrupted counters).
func TestPoolReleaseDuplicateDoesNotWakeWaiter(t *testing.T) {
	cfgs := []config.AccountConfig{{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"}}
	p := NewPool(cfgs)
	ctx := context.Background()
	_, s1, err := p.Select(ctx, "m", 2)
	if err != nil {
		t.Fatalf("first Select: %v", err)
	}
	_, s2, err := p.Select(ctx, "m", 2)
	if err != nil {
		t.Fatalf("second Select: %v", err)
	}

	ch1 := make(chan slotResult, 1)
	ch2 := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.Select(ctx, "m", 2)
		if err != nil {
			ch1 <- slotResult{}
			return
		}
		ch1 <- slotResult{acc: acc, slot: slot}
	}()
	go func() {
		acc, slot, err := p.Select(ctx, "m", 2)
		if err != nil {
			ch2 <- slotResult{}
			return
		}
		ch2 <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 2 }, "two waiters to park")

	// Legitimate release: frees one slot and wakes the FRONT waiter. Which
	// goroutine that is depends on park order (scheduling), so accept
	// either ch1 or ch2 — the other must stay parked.
	p.Release(s1)
	var first slotResult
	var otherCh chan slotResult
	select {
	case first = <-ch1:
		otherCh = ch2
	case first = <-ch2:
		otherCh = ch1
	case <-time.After(2 * time.Second):
		t.Fatal("a waiter must be woken by the legitimate release")
	}
	if first.acc == nil || first.slot == nil {
		t.Fatal("the woken waiter delivered no slot")
	}
	// Duplicate release of the same slot: no capacity was freed, so the
	// still-parked waiter must NOT be woken.
	p.Release(s1)
	select {
	case v := <-otherCh:
		t.Fatalf("duplicate Release woke a waiter although no slot was freed (got account %v)", v.acc)
	case <-time.After(300 * time.Millisecond):
	}
	if got := p.WaitingCount(); got != 1 {
		t.Errorf("WaitingCount = %d, want 1 (one waiter must stay parked)", got)
	}
	// The parked waiter is woken only by the genuine release of the other
	// slot.
	p.Release(s2)
	got := waitForSlotResult(t, otherCh, 2*time.Second, "second waiter after real release")
	if got.acc == nil {
		t.Fatal("second waiter must be woken by the release of the second slot")
	}
	// Drain the slots the waiters acquired.
	p.Release(first.slot)
	p.Release(got.slot)
}
