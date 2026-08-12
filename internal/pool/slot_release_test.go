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
