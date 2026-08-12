package pool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
)

// gatedWaiter is a waiter goroutine whose Select call is gated behind goCh,
// so tests can control the exact queue order deterministically (a goroutine
// released first is guaranteed to reach the wait queue before one released
// later, because the earlier one blocks inside Select until woken).
type gatedWaiter struct {
	goCh chan struct{}
	done chan slotResult
}

func (g *gatedWaiter) run(p *Pool, key string, provider string, max int, releaseAfter bool) {
	<-g.goCh
	var res slotResult
	var err error
	if provider == "" {
		res.acc, res.slot, err = p.Select(context.Background(), key, max)
	} else {
		res.acc, res.slot, err = p.SelectByProvider(context.Background(), key, max, provider)
	}
	if err != nil {
		g.done <- slotResult{}
		return
	}
	g.done <- res
	if releaseAfter {
		p.Release(res.slot) // cascade the slot to the next waiter
	}
}

// waitForAccount waits up to timeout for an *Account on ch and returns it.
func waitForAccount(t *testing.T, ch <-chan *Account, timeout time.Duration, what string) *Account {
	t.Helper()
	select {
	case v := <-ch:
		if v == nil {
			t.Fatalf("%s delivered nil account (select failed)", what)
		}
		return v
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for %s", what)
		return nil
	}
}

// expectNoAccount asserts that ch delivers nothing within wait (the waiter
// must NOT have been woken).
func expectNoAccount(t *testing.T, ch <-chan *Account, wait time.Duration, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s was woken unexpectedly with %q", what, v.Name())
	case <-time.After(wait):
	}
}

// TestReleaseWakesFirstMatchingProviderWaiter is the core waiter-provider
// regression test. Waiters queue in the deterministic order [X, "", Y].
//   - Release(X) wakes the X waiter: it is the front-most waiter that can
//     use an X slot (the "" waiter behind it also could, but FIFO within
//     the matching set gives the X waiter priority).
//   - Release(Y) wakes the "" waiter: it is queued ahead of the Y waiter
//     and can use any slot, so FIFO gives it the freed Y slot.
//   - The Y waiter is woken only when the "" waiter's cascade release
//     frees the Y slot again.
func TestReleaseWakesFirstMatchingProviderWaiter(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
		{Name: "acc-y", Key: "ky", BaseURL: "http://localhost:8002", Provider: "Y"},
	}
	p := NewPool(cfgs)

	// Occupy both accounts (maxConcurrent=1 each).
	_, slotX, err := p.Select(context.Background(), "m", 1)
	if err != nil {
		t.Fatalf("select acc-x: %v", err)
	}
	_, slotY, err := p.Select(context.Background(), "m", 1)
	if err != nil {
		t.Fatalf("select acc-y: %v", err)
	}

	// Deterministic queue order: gX first, then gAny, then gY.
	gX := &gatedWaiter{goCh: make(chan struct{}), done: make(chan slotResult, 1)}
	gAny := &gatedWaiter{goCh: make(chan struct{}), done: make(chan slotResult, 1)}
	gY := &gatedWaiter{goCh: make(chan struct{}), done: make(chan slotResult, 1)}
	go gX.run(p, "m", "X", 1, false)
	go gAny.run(p, "m", "", 1, false)
	go gY.run(p, "m", "Y", 1, true) // gY releases after delivery (cascade is asserted separately)

	close(gX.goCh)
	time.Sleep(100 * time.Millisecond) // gX is now queued (it cannot return while acc-x is busy)
	close(gAny.goCh)
	time.Sleep(100 * time.Millisecond) // gAny queued behind gX
	close(gY.goCh)
	time.Sleep(100 * time.Millisecond) // gY queued behind gAny

	// Release the X account: the X waiter is the front-most X-matching waiter.
	p.Release(slotX)
	if got := waitForSlotResult(t, gX.done, 2*time.Second, "X waiter"); got.acc.Name() != "acc-x" {
		t.Fatalf("X waiter got %q, want acc-x", got.acc.Name())
	}
	// The "" waiter matches X too but was queued behind the X waiter: it
	// must NOT be woken by this release (FIFO within the matching set).
	expectNoSlotResult(t, gAny.done, 200*time.Millisecond, "global waiter after X release")

	// Release the Y account: matching set is {gAny, gY}; gAny is front-most.
	p.Release(slotY)
	if got := waitForSlotResult(t, gAny.done, 2*time.Second, "global waiter"); got.acc.Name() != "acc-y" {
		t.Fatalf("global waiter got %q, want acc-y", got.acc.Name())
	}
	// The Y waiter must still be queued.
	expectNoSlotResult(t, gY.done, 200*time.Millisecond, "Y waiter after global-waiter wake")

	// The Y waiter is served by a later matching release (the cascade path
	// is exercised in the FIFO and stress tests below); here we only verify
	// it was not spuriously woken while gX and gAny hold both slots.
	expectNoSlotResult(t, gY.done, 100*time.Millisecond, "Y waiter before any Y-slot release")
}

// TestReleaseFIFOProviderWaiterBehindGlobal verifies the reverse FIFO order:
// with a Y waiter queued AHEAD of a "" waiter, a Y release wakes the Y
// waiter (front-most Y-matching), and the "" waiter is served by the Y
// waiter's cascade release.
func TestReleaseFIFOProviderWaiterBehindGlobal(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
		{Name: "acc-y", Key: "ky", BaseURL: "http://localhost:8002", Provider: "Y"},
	}
	p := NewPool(cfgs)

	_, slotX, err := p.Select(context.Background(), "m", 1)
	if err != nil {
		t.Fatalf("select acc-x: %v", err)
	}
	_, slotY, err := p.Select(context.Background(), "m", 1)
	if err != nil {
		t.Fatalf("select acc-y: %v", err)
	}
	_ = slotX // stays busy for the whole test

	// Queue order: gY first, then gAny.
	gY := &gatedWaiter{goCh: make(chan struct{}), done: make(chan slotResult, 1)}
	gAny := &gatedWaiter{goCh: make(chan struct{}), done: make(chan slotResult, 1)}
	go gY.run(p, "m", "Y", 1, true)
	go gAny.run(p, "m", "", 1, true)
	close(gY.goCh)
	time.Sleep(100 * time.Millisecond) // gY queued
	close(gAny.goCh)
	time.Sleep(100 * time.Millisecond) // gAny queued behind gY

	// Release Y: gY is the front-most Y-matching waiter → gY wins the slot.
	p.Release(slotY)
	if got := waitForSlotResult(t, gY.done, 2*time.Second, "Y waiter"); got.acc.Name() != "acc-y" {
		t.Fatalf("Y waiter got %q, want acc-y", got.acc.Name())
	}
	if got := waitForSlotResult(t, gAny.done, 2*time.Second, "global waiter"); got.acc.Name() != "acc-y" {
		t.Fatalf("global waiter got %q, want acc-y", got.acc.Name())
	}
}

// TestReleaseDoesNotWakeNonMatchingProvider verifies that a Release of a Y
// account never consumes the wakeup on an X-only waiter: the freed Y slot
// must not be stolen and the X waiter must stay queued until an X slot frees.
func TestReleaseDoesNotWakeNonMatchingProvider(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
		{Name: "acc-y", Key: "ky", BaseURL: "http://localhost:8002", Provider: "Y"},
	}
	p := NewPool(cfgs)

	_, slotX, err := p.Select(context.Background(), "m", 1)
	if err != nil {
		t.Fatalf("select acc-x: %v", err)
	}
	_, slotY, err := p.Select(context.Background(), "m", 1)
	if err != nil {
		t.Fatalf("select acc-y: %v", err)
	}

	chX := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "m", 1, "X")
		if err != nil {
			chX <- slotResult{}
			return
		}
		chX <- slotResult{acc: acc, slot: slot}
	}()
	time.Sleep(150 * time.Millisecond) // let it queue

	// Release the Y account: no Y or "" waiter is queued → nobody woken.
	p.Release(slotY)
	expectNoSlotResult(t, chX, 200*time.Millisecond, "X waiter after Y release")

	// Release the X account → the X waiter is woken.
	p.Release(slotX)
	if got := waitForSlotResult(t, chX, 2*time.Second, "X waiter"); got.acc.Name() != "acc-x" {
		t.Fatalf("X waiter got %q, want acc-x", got.acc.Name())
	}
}

// TestMarkHealthyWakesMatchingProviderWaiter verifies MarkHealthy applies the
// same provider-matched wakeup: when a probe recovers an exhausted X account
// while an X waiter is queued (waiting on the busy X account), the X waiter
// is woken and the Y waiter is not — MarkHealthy(Y) wakes it later.
func TestMarkHealthyWakesMatchingProviderWaiter(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "x1", Key: "kx1", BaseURL: "http://localhost:8001", Provider: "X"},
		{Name: "x2", Key: "kx2", BaseURL: "http://localhost:8002", Provider: "X"},
		{Name: "y1", Key: "ky1", BaseURL: "http://localhost:8003", Provider: "Y"},
		{Name: "y2", Key: "ky2", BaseURL: "http://localhost:8004", Provider: "Y"},
	}
	p := NewPool(cfgs)

	// Occupy x1 and y1; exhaust x2 and y2 (probe will recover them).
	var x2, y2 *Account
	for _, acc := range p.AllAccounts() {
		switch acc.Name() {
		case "x2":
			x2 = acc
		case "y2":
			y2 = acc
		}
	}
	_, slotX1, err := p.SelectByProvider(context.Background(), "m", 1, "X") // x1
	if err != nil {
		t.Fatalf("select x1: %v", err)
	}
	_, slotY1, err := p.SelectByProvider(context.Background(), "m", 1, "Y") // y1
	if err != nil {
		t.Fatalf("select y1: %v", err)
	}
	x2.MarkExhausted()
	y2.MarkExhausted()

	chX := make(chan slotResult, 1)
	chY := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "m", 1, "X")
		if err != nil {
			chX <- slotResult{}
			return
		}
		chX <- slotResult{acc: acc, slot: slot}
	}()
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "m", 1, "Y")
		if err != nil {
			chY <- slotResult{}
			return
		}
		chY <- slotResult{acc: acc, slot: slot}
	}()
	time.Sleep(150 * time.Millisecond) // let both queue

	// Recover x2: the X waiter (front-most X-matching) must be woken and get
	// x2; the Y waiter must stay queued.
	p.MarkHealthy(x2)
	if got := waitForSlotResult(t, chX, 2*time.Second, "X waiter"); got.acc.Name() != "x2" {
		t.Fatalf("X waiter got %q, want x2", got.acc.Name())
	}
	expectNoSlotResult(t, chY, 200*time.Millisecond, "Y waiter after MarkHealthy(x2)")

	// Recover y2: the Y waiter wakes and gets y2.
	p.MarkHealthy(y2)
	if got := waitForSlotResult(t, chY, 2*time.Second, "Y waiter"); got.acc.Name() != "y2" {
		t.Fatalf("Y waiter got %q, want y2", got.acc.Name())
	}

	// Cleanup the still-held slots.
	p.Release(slotX1)
	p.Release(slotY1)
}

// TestWaiterProviderStressMixed is the high-risk concurrency test: a pool
// with two providers (2 accounts each, maxConcurrent=1) and 40 queued
// waiters mixing provider X, provider Y, and global ("") selectors. Every
// account is released once at the start and each woken waiter releases its
// acquired slot, so the wakeups cascade. The test asserts all 40 waiters
// complete (no lost wakeup, no deadlock); running under -race it also
// detects double-close of waiter channels and unsynchronized list/waiter
// state access.
func TestWaiterProviderStressMixed(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "x1", Key: "kx1", BaseURL: "http://localhost:8001", Provider: "X"},
		{Name: "x2", Key: "kx2", BaseURL: "http://localhost:8002", Provider: "X"},
		{Name: "y1", Key: "ky1", BaseURL: "http://localhost:8003", Provider: "Y"},
		{Name: "y2", Key: "ky2", BaseURL: "http://localhost:8004", Provider: "Y"},
	}
	p := NewPool(cfgs)

	// Occupy all four slots.
	held := make([]*Slot, 0, 4)
	for i := 0; i < 4; i++ {
		_, slot, err := p.Select(context.Background(), "m", 1)
		if err != nil {
			t.Fatalf("occupy slot %d: %v", i, err)
		}
		held = append(held, slot)
	}

	const (
		nX    = 15
		nY    = 15
		nAny  = 10
		total = nX + nY + nAny
	)
	var done int32
	var wg sync.WaitGroup
	startWaiter := func(provider string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var res slotResult
			var err error
			if provider == "" {
				res.acc, res.slot, err = p.Select(context.Background(), "m", 1)
			} else {
				res.acc, res.slot, err = p.SelectByProvider(context.Background(), "m", 1, provider)
			}
			if err != nil {
				t.Errorf("waiter(%q) failed: %v", provider, err)
				return
			}
			atomic.AddInt32(&done, 1)
			p.Release(res.slot) // cascade
		}()
	}
	for i := 0; i < nX; i++ {
		startWaiter("X")
	}
	for i := 0; i < nY; i++ {
		startWaiter("Y")
	}
	for i := 0; i < nAny; i++ {
		startWaiter("")
	}
	time.Sleep(300 * time.Millisecond) // let all 40 queue

	// Release all four slots; the wakeups cascade through the waiters.
	for _, s := range held {
		p.Release(s)
	}

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("timeout: only %d/%d waiters completed (lost wakeup or deadlock)", atomic.LoadInt32(&done), total)
	}
	if got := atomic.LoadInt32(&done); got != total {
		t.Fatalf("completed waiters = %d, want %d", got, total)
	}
}

// waitUntil polls until cond is true or the deadline expires. Used to
// observe waiter-queue entry deterministically (a waiter is registered
// under the pool lock, so WaitingCount is a reliable park signal).
func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRemoveWaiterTransfersWokenWakeupOnBail is the deterministic
// lost-wakeup regression test for the exact interleaving from the bug:
// waiter 1's cancel races a Release. The first waiter is simulated (a
// parked waiter struct pushed at the queue front); the Release consumes
// its wakeup (the release's wake scan removes it and closes its channel), and then
// waiter 1's select — which had already picked ctx.Done — bails through
// removeWaiterAndTransfer. The freed slot's wakeup MUST be transferred to
// the next waiter that can use it (the real second waiter), which gets the
// account promptly. Before the fix, the second waiter stayed parked until
// its 60s fallback timer.
func TestRemoveWaiterTransfersWokenWakeupOnBail(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
	}
	p := NewPool(cfgs)

	_, slotX, err := p.Select(context.Background(), "m", 1)
	if err != nil {
		t.Fatalf("occupy acc-x: %v", err)
	}

	// Real second waiter: parks behind the simulated first waiter.
	ch2 := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "m", 1, "X")
		if err != nil {
			ch2 <- slotResult{}
			return
		}
		ch2 <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "second waiter to park")

	// Simulated first waiter, queued AHEAD of the real one: it is the
	// front-most X-matching waiter, so the Release below wakes IT.
	p.mu.Lock()
	w1 := &waiter{ch: make(chan struct{}), active: true, provider: "X", key: "m", max: 1}
	elem1 := p.waiters.PushFront(w1)
	p.mu.Unlock()

	// Release: the wakeup lands on w1 (removed from the queue, channel
	// closed) — while w1's select has already committed to ctx.Done.
	p.Release(slotX)

	// w1 bails: removeWaiterAndTransfer must hand the consumed wakeup to
	// the next waiter that can use the freed slot (the real waiter).
	p.removeWaiterAndTransfer(elem1)

	got := waitForSlotResult(t, ch2, 2*time.Second, "second waiter after first waiter bailed")
	if got.acc.Name() != "acc-x" {
		t.Fatalf("second waiter got %q, want acc-x", got.acc.Name())
	}
	p.Release(got.slot)
}

// TestRemoveWaiterNoTransferWhenCapacityGone: the wakeup is NOT transferred
// when the freed slot was re-acquired before the woken waiter bailed —
// there is no capacity left, so waking the next waiter would only make it
// re-park. The second waiter stays parked until a real release.
func TestRemoveWaiterNoTransferWhenCapacityGone(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
	}
	p := NewPool(cfgs)

	_, slotX, err := p.Select(context.Background(), "m", 1)
	if err != nil {
		t.Fatalf("occupy acc-x: %v", err)
	}
	ch2 := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "m", 1, "X")
		if err != nil {
			ch2 <- slotResult{}
			return
		}
		ch2 <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "second waiter to park")

	p.mu.Lock()
	w1 := &waiter{ch: make(chan struct{}), active: true, provider: "X", key: "m", max: 1}
	elem1 := p.waiters.PushFront(w1)
	p.mu.Unlock()

	p.Release(slotX) // wakeup consumed by w1
	// Another caller grabs the freed slot before w1 bails.
	_, slotX2, err := p.Select(context.Background(), "m", 1)
	if err != nil {
		t.Fatalf("re-acquire freed slot: %v", err)
	}
	p.removeWaiterAndTransfer(elem1) // w1 bails: no capacity left → no transfer

	expectNoSlotResult(t, ch2, 200*time.Millisecond, "second waiter after capacity gone")

	p.Release(slotX2)
	got := waitForSlotResult(t, ch2, 2*time.Second, "second waiter after real release")
	if got.acc.Name() != "acc-x" {
		t.Fatalf("second waiter got %q, want acc-x", got.acc.Name())
	}
	p.Release(got.slot)
}

// TestRemoveWaiterNoTransferToNonMatchingProvider: a bailing X waiter's
// wakeup must NOT wake a Y waiter — the freed X slot is unusable for it —
// and the Y waiter stays parked until a Y slot frees. FIFO and provider
// matching survive the transfer.
func TestRemoveWaiterNoTransferToNonMatchingProvider(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
		{Name: "acc-y", Key: "ky", BaseURL: "http://localhost:8002", Provider: "Y"},
	}
	p := NewPool(cfgs)

	_, slotX, err := p.SelectByProvider(context.Background(), "m", 1, "X")
	if err != nil {
		t.Fatalf("occupy acc-x: %v", err)
	}
	_, slotY, err := p.SelectByProvider(context.Background(), "m", 1, "Y")
	if err != nil {
		t.Fatalf("occupy acc-y: %v", err)
	}

	chY := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "m", 1, "Y")
		if err != nil {
			chY <- slotResult{}
			return
		}
		chY <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "Y waiter to park")

	p.mu.Lock()
	wX := &waiter{ch: make(chan struct{}), active: true, provider: "X", key: "m", max: 1}
	elemX := p.waiters.PushFront(wX)
	p.mu.Unlock()

	// The X release's wakeup is consumed by the bailing X waiter; the
	// transfer must skip the Y waiter (provider mismatch, no Y capacity).
	p.Release(slotX)
	p.removeWaiterAndTransfer(elemX)
	expectNoSlotResult(t, chY, 200*time.Millisecond, "Y waiter after X slot freed")

	// A real Y release wakes the Y waiter.
	p.Release(slotY)
	got := waitForSlotResult(t, chY, 2*time.Second, "Y waiter after Y release")
	if got.acc.Name() != "acc-y" {
		t.Fatalf("Y waiter got %q, want acc-y", got.acc.Name())
	}
	p.Release(got.slot)
}

// TestRemoveWaiterTransferMixedKey is the deterministic mixed-key
// regression test under per-model-key concurrency accounting: the bailing
// waiter A has a SMALL max on key "lo" and the next queued waiter B has a
// LARGE max on key "hi". A is woken by a release that frees capacity in a
// DIFFERENT key (the release wakes the first waiter whose own key has room —
// A's "lo" counter is empty, so A is servable even though the freed slot
// belongs to the "hi" key). A then bails; the transfer MUST wake B (its
// "hi" key now has room), so the consumed wakeup is never lost.
func TestRemoveWaiterTransferMixedKey(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
	}
	p := NewPool(cfgs)

	// Occupy acc-x with three "hi" (max=3) holders plus one "lo" (max=1)
	// holder: the "hi" counter sits at 3 (a "hi" waiter parks) and the "lo"
	// counter at 1 (a "lo" waiter parks).
	heldHi := make([]*Slot, 0, 3)
	for i := 0; i < 3; i++ {
		_, slot, err := p.Select(context.Background(), "hi", 3)
		if err != nil {
			t.Fatalf("occupy hi slot %d: %v", i, err)
		}
		heldHi = append(heldHi, slot)
	}
	_, heldLo, err := p.Select(context.Background(), "lo", 1)
	if err != nil {
		t.Fatalf("occupy lo slot: %v", err)
	}

	// Real waiter B with key "hi" (max=3): it parks ("hi" counter at 3).
	chB := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "hi", 3, "X")
		if err != nil {
			chB <- slotResult{}
			return
		}
		chB <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "B waiter (hi) to park")

	// Simulated bailing waiter A with key "lo" (max=1), queued AHEAD of B:
	// the release below wakes A (front-most X-matching waiter with room in
	// its "lo" counter).
	p.mu.Lock()
	wA := &waiter{ch: make(chan struct{}), active: true, provider: "X", key: "lo", max: 1}
	elemA := p.waiters.PushFront(wA)
	p.mu.Unlock()

	// Release one "hi" holder: A ("lo" counter empty) consumes the
	// wakeup...
	p.Release(heldHi[0])

	// ...then bails: the transfer must reach the next waiter whose own key
	// has room (B, "hi" counter now at 2).
	p.removeWaiterAndTransfer(elemA)

	// B must get the account promptly (no fallback-timer starvation).
	got := waitForSlotResult(t, chB, 2*time.Second, "B waiter (hi) after A bailed")
	if got.acc.Name() != "acc-x" {
		t.Fatalf("B waiter got %q, want acc-x", got.acc.Name())
	}

	// Cleanup: B acquired one slot ("hi" counter back to 3); release
	// everything.
	p.Release(got.slot)
	p.Release(heldHi[1])
	p.Release(heldHi[2])
	p.Release(heldLo)
}

// TestRemoveWaiterTransferSkipsUnusableMixedKey is the FIFO companion under
// per-model-key accounting: with a "lo" waiter C queued AHEAD of the "hi"
// waiter B, and the "lo" counter full (C unusable), a release that frees
// "hi" capacity must NOT wake C (its own key has no room) — it wakes the
// first waiter that CAN use the freed capacity (B), preserving FIFO among
// the usable set. The unusable C stays parked until its own key frees up.
func TestRemoveWaiterTransferSkipsUnusableMixedKey(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
	}
	p := NewPool(cfgs)

	// Occupy acc-x with three "hi" (max=3) holders and one "lo" (max=1)
	// holder: the "hi" counter sits at 3 (a "hi" waiter parks) and the "lo"
	// counter at 1 (a "lo" waiter parks).
	heldHi := make([]*Slot, 0, 3)
	for i := 0; i < 3; i++ {
		_, slot, err := p.Select(context.Background(), "hi", 3)
		if err != nil {
			t.Fatalf("occupy hi slot %d: %v", i, err)
		}
		heldHi = append(heldHi, slot)
	}
	_, heldLo, err := p.Select(context.Background(), "lo", 1)
	if err != nil {
		t.Fatalf("occupy lo slot: %v", err)
	}

	// Real waiters: C ("lo") queues first, B ("hi") second.
	chC := make(chan slotResult, 1)
	chB := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "lo", 1, "X")
		if err != nil {
			chC <- slotResult{}
			return
		}
		chC <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "C waiter (lo) to park")
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "hi", 3, "X")
		if err != nil {
			chB <- slotResult{}
			return
		}
		chB <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 2 }, "B waiter (hi) to park")

	// Simulated bailing waiter A ("lo") queued ahead of both. Its key is
	// full too, so the release below skips it along with C.
	p.mu.Lock()
	wA := &waiter{ch: make(chan struct{}), active: true, provider: "X", key: "lo", max: 1}
	elemA := p.waiters.PushFront(wA)
	p.mu.Unlock()

	// Release one "hi" holder: the "hi" counter drops to 2. The wake scan
	// passes over A ("lo" full) and C ("lo" full) and wakes B ("hi" at 2) —
	// FIFO within the usable set, C not woken.
	p.Release(heldHi[0])
	p.removeWaiterAndTransfer(elemA) // A was never woken: no transfer needed

	expectNoSlotResult(t, chC, 200*time.Millisecond, "C waiter (lo) after hi release")
	got := waitForSlotResult(t, chB, 2*time.Second, "B waiter (hi) after release")
	if got.acc.Name() != "acc-x" {
		t.Fatalf("B waiter got %q, want acc-x", got.acc.Name())
	}
	p.Release(got.slot)

	// Further "hi" releases still cannot serve C (its own key is full);
	// releasing the "lo" holder wakes C.
	p.Release(heldHi[1])
	expectNoSlotResult(t, chC, 200*time.Millisecond, "C waiter while lo counter still full")
	p.Release(heldLo)
	gotC := waitForSlotResult(t, chC, 2*time.Second, "C waiter (lo) after lo counter freed")
	if gotC.acc.Name() != "acc-x" {
		t.Fatalf("C waiter got %q, want acc-x", gotC.acc.Name())
	}
	p.Release(gotC.slot)
	p.Release(heldHi[2])
}

// TestSelectCancelReleaseRaceNoLostWakeup hammers the real interleaving
// behind the lost-wakeup bug: waiter 1 (cancelable ctx) parks AHEAD of
// waiter 2, then the ctx is cancelled and the slot released concurrently.
// Waiter 1 either bails (its consumed wakeup must transfer to waiter 2) or
// wins the slot and releases it (cascading to waiter 2). Either way waiter
// 2 must get the account promptly — never starve until its 60s fallback
// timer. Queue order is forced by gates (waiter 1 parks first), so the
// release always targets waiter 1; without the transfer fix roughly half
// the iterations lose the wakeup and the test fails. Under -race it also
// pins no double-close and no unsynchronized waiter/list state.
func TestSelectCancelReleaseRaceNoLostWakeup(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
	}
	for i := 0; i < 200; i++ {
		p := NewPool(cfgs)
		_, slotX, err := p.Select(context.Background(), "m", 1)
		if err != nil {
			t.Fatalf("iter %d: occupy acc-x: %v", i, err)
		}

		ctx1, cancel1 := context.WithCancel(context.Background())
		ch1 := make(chan slotResult, 1)
		ch2 := make(chan slotResult, 1)
		go1 := make(chan struct{})
		go2 := make(chan struct{})
		go func() {
			<-go1
			acc, slot, err := p.SelectByProvider(ctx1, "m", 1, "X")
			if err != nil {
				ch1 <- slotResult{}
				return
			}
			ch1 <- slotResult{acc: acc, slot: slot}
		}()
		go func() {
			<-go2
			acc, slot, err := p.SelectByProvider(context.Background(), "m", 1, "X")
			if err != nil {
				ch2 <- slotResult{}
				return
			}
			ch2 <- slotResult{acc: acc, slot: slot}
		}()
		close(go1)
		waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "waiter 1 to park")
		close(go2)
		waitUntil(t, func() bool { return p.WaitingCount() == 2 }, "waiter 2 to park")

		cancel1()        // waiter 1's select may pick ctx.Done...
		p.Release(slotX) // ...while the release wakes the front-most waiter (1)

		// Waiter 1 exits either way: bailed (zero) or woken-then-acquired
		// (account; its release cascades the slot to waiter 2).
		select {
		case res := <-ch1:
			if res.slot != nil {
				p.Release(res.slot)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: waiter 1 never exited", i)
		}
		// Waiter 2 must get the slot promptly — no lost wakeup.
		select {
		case res := <-ch2:
			if res.acc == nil || res.acc.Name() != "acc-x" {
				t.Fatalf("iter %d: waiter 2 got %v, want acc-x", i, res.acc)
			}
			p.Release(res.slot)
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: waiter 2 starved — wakeup swallowed by the bailing waiter 1", i)
		}
	}
}

// TestReleaseInitialWakeMixedKey is the deterministic mixed-key regression
// test for the INITIAL Release wakeup under per-model-key accounting: a
// "lo" waiter C queues AHEAD of a "hi" waiter B while the "lo" counter is
// full (one holder) and the "hi" counter is full (three holders). One "hi"
// release frees room in the "hi" counter only; the release MUST skip C (its
// own key still has no room) and wake B — waking the front-most
// provider-matching waiter without the per-key capacity check would let C
// consume the wakeup, fail to select and re-park at the back while B stays
// parked until its 60s fallback timer — the mixed-key lost wakeup on the
// plain Release path. FIFO among the usable set is preserved: B is the
// first waiter that can use the freed capacity.
func TestReleaseInitialWakeMixedKey(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
	}
	p := NewPool(cfgs)

	// Occupy acc-x with three "hi" (max=3) holders plus one "lo" (max=1)
	// holder: the "hi" counter sits at 3 and the "lo" counter at 1.
	heldHi := make([]*Slot, 0, 3)
	for i := 0; i < 3; i++ {
		_, slot, err := p.Select(context.Background(), "hi", 3)
		if err != nil {
			t.Fatalf("occupy hi slot %d: %v", i, err)
		}
		heldHi = append(heldHi, slot)
	}
	_, heldLo, err := p.Select(context.Background(), "lo", 1)
	if err != nil {
		t.Fatalf("occupy lo slot: %v", err)
	}

	// Real waiters: C ("lo") queues first, B ("hi") second.
	chC := make(chan slotResult, 1)
	chB := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "lo", 1, "X")
		if err != nil {
			chC <- slotResult{}
			return
		}
		chC <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "C waiter (lo) to park")
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "hi", 3, "X")
		if err != nil {
			chB <- slotResult{}
			return
		}
		chB <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 2 }, "B waiter (hi) to park")

	// Release one "hi" holder: the "hi" counter drops to 2. The wake scan
	// must pass over C ("lo" full) and wake B — the first waiter that can
	// use the freed capacity.
	p.Release(heldHi[0])

	expectNoSlotResult(t, chC, 200*time.Millisecond, "C waiter (lo) after hi release")
	got := waitForSlotResult(t, chB, 2*time.Second, "B waiter (hi) after initial release")
	if got.acc.Name() != "acc-x" {
		t.Fatalf("B waiter got %q, want acc-x", got.acc.Name())
	}

	// Cleanup: B holds one slot ("hi" counter back to 3). C is servable
	// only once the "lo" counter frees up.
	p.Release(heldHi[1])
	expectNoSlotResult(t, chC, 200*time.Millisecond, "C waiter while lo counter still full")
	p.Release(heldLo) // lo counter 1→0: C becomes servable
	gotC := waitForSlotResult(t, chC, 2*time.Second, "C waiter (lo) after lo counter freed")
	if gotC.acc.Name() != "acc-x" {
		t.Fatalf("C waiter got %q, want acc-x", gotC.acc.Name())
	}
	p.Release(gotC.slot)
	p.Release(got.slot)
	p.Release(heldHi[2])
}

// TestMarkHealthyInitialWakeMixedKey is the MarkHealthy companion under
// per-model-key accounting: every X account has its "lo" counter FULL (C is
// unusable everywhere) while the "hi" counters are also full (B is unusable
// on x1; x2 is exhausted). MarkHealthy makes x2 selectable again, but its
// "mid" counter at 2 can only serve B (needs < 3), not C (needs < 1). The
// wake must skip the unusable front waiter C and wake B directly — waking
// C first would burn the wakeup (C re-parks at the back) and strand B
// until its fallback timer.
func TestMarkHealthyInitialWakeMixedKey(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "x1", Key: "kx1", BaseURL: "http://localhost:8001", Provider: "X"},
		{Name: "x2", Key: "kx2", BaseURL: "http://localhost:8002", Provider: "X"},
	}
	p := NewPool(cfgs)

	var x1, x2 *Account
	for _, acc := range p.AllAccounts() {
		switch acc.Name() {
		case "x1":
			x1 = acc
		case "x2":
			x2 = acc
		}
	}
	// Occupy x1 ("lo" at 1, "hi" at 3) and x2 ("lo" at 1, "mid" at 2).
	// Direct TryAcquire keeps the per-key occupancy deterministic
	// regardless of the round-robin rotation.
	loX1 := x1.TryAcquire("lo", 1)
	if loX1 == nil {
		t.Fatal("occupy x1 lo counter")
	}
	hiX1 := make([]*Slot, 0, 3)
	for i := 0; i < 3; i++ {
		s := x1.TryAcquire("hi", 3)
		if s == nil {
			t.Fatalf("occupy x1 hi counter slot %d", i)
		}
		hiX1 = append(hiX1, s)
	}
	loX2 := x2.TryAcquire("lo", 1)
	if loX2 == nil {
		t.Fatal("occupy x2 lo counter")
	}
	midX2 := make([]*Slot, 0, 2)
	for i := 0; i < 2; i++ {
		s := x2.TryAcquire("mid", 2)
		if s == nil {
			t.Fatalf("occupy x2 mid counter slot %d", i)
		}
		midX2 = append(midX2, s)
	}
	// Simulate a probe marking x2 exhausted while its requests are still
	// in flight.
	x2.MarkExhausted()

	// C ("lo") parks first, B ("hi") second: x1 has both counters full (C
	// needs lo < 1, B needs hi < 3) and x2 is exhausted.
	chC := make(chan slotResult, 1)
	chB := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "lo", 1, "X")
		if err != nil {
			chC <- slotResult{}
			return
		}
		chC <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "C waiter (lo) to park")
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "hi", 3, "X")
		if err != nil {
			chB <- slotResult{}
			return
		}
		chB <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 2 }, "B waiter (hi) to park")

	// Recover x2: its "mid" counter sits at 2, in the band [C.max=1,
	// B.max=3) — the wake scan must skip C (x2 lo counter: 1<1 false; x1 lo
	// counter: 1<1 false) and wake B (x2 mid counter: 2<3 true).
	p.MarkHealthy(x2)

	expectNoSlotResult(t, chC, 200*time.Millisecond, "C waiter (lo) after MarkHealthy")
	got := waitForSlotResult(t, chB, 2*time.Second, "B waiter (hi) after MarkHealthy")
	if got.acc.Name() != "x2" {
		t.Fatalf("B waiter got %q, want x2", got.acc.Name())
	}

	// Cleanup: B holds one x2 "hi" slot. C is servable only once some X
	// account's "lo" counter frees up; x2's "lo" holder is released last.
	p.Release(midX2[0]) // mid 2→1
	expectNoSlotResult(t, chC, 200*time.Millisecond, "C waiter while lo counters still full")
	p.Release(midX2[1]) // mid 1→0
	expectNoSlotResult(t, chC, 200*time.Millisecond, "C waiter while lo counters still full")
	p.Release(loX2) // x2 lo 1→0: C becomes servable on x2
	gotC := waitForSlotResult(t, chC, 2*time.Second, "C waiter (lo) after lo counter freed")
	if gotC.acc.Name() != "x2" {
		t.Fatalf("C waiter got %q, want x2", gotC.acc.Name())
	}
	p.Release(gotC.slot)
	p.Release(got.slot) // B's x2 "hi" slot
	p.Release(loX1)     // x1 lo holder
	for _, s := range hiX1 {
		p.Release(s)
	}
}
