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
	done chan *Account
}

func (g *gatedWaiter) run(p *Pool, provider string, releaseAfter bool) {
	<-g.goCh
	var acc *Account
	var err error
	if provider == "" {
		acc, err = p.Select(context.Background(), 1)
	} else {
		acc, err = p.SelectByProvider(context.Background(), 1, provider)
	}
	if err != nil {
		g.done <- nil
		return
	}
	g.done <- acc
	if releaseAfter {
		p.Release(acc) // cascade the slot to the next waiter
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
	accX, err := p.Select(context.Background(), 1)
	if err != nil {
		t.Fatalf("select acc-x: %v", err)
	}
	accY, err := p.Select(context.Background(), 1)
	if err != nil {
		t.Fatalf("select acc-y: %v", err)
	}

	// Deterministic queue order: gX first, then gAny, then gY.
	gX := &gatedWaiter{goCh: make(chan struct{}), done: make(chan *Account, 1)}
	gAny := &gatedWaiter{goCh: make(chan struct{}), done: make(chan *Account, 1)}
	gY := &gatedWaiter{goCh: make(chan struct{}), done: make(chan *Account, 1)}
	go gX.run(p, "X", false)
	go gAny.run(p, "", false)
	go gY.run(p, "Y", true) // gY releases after delivery (cascade is asserted separately)

	close(gX.goCh)
	time.Sleep(100 * time.Millisecond) // gX is now queued (it cannot return while acc-x is busy)
	close(gAny.goCh)
	time.Sleep(100 * time.Millisecond) // gAny queued behind gX
	close(gY.goCh)
	time.Sleep(100 * time.Millisecond) // gY queued behind gAny

	// Release the X account: the X waiter is the front-most X-matching waiter.
	p.Release(accX)
	if got := waitForAccount(t, gX.done, 2*time.Second, "X waiter"); got.Name() != "acc-x" {
		t.Fatalf("X waiter got %q, want acc-x", got.Name())
	}
	// The "" waiter matches X too but was queued behind the X waiter: it
	// must NOT be woken by this release (FIFO within the matching set).
	expectNoAccount(t, gAny.done, 200*time.Millisecond, "global waiter after X release")

	// Release the Y account: matching set is {gAny, gY}; gAny is front-most.
	p.Release(accY)
	if got := waitForAccount(t, gAny.done, 2*time.Second, "global waiter"); got.Name() != "acc-y" {
		t.Fatalf("global waiter got %q, want acc-y", got.Name())
	}
	// The Y waiter must still be queued.
	expectNoAccount(t, gY.done, 200*time.Millisecond, "Y waiter after global-waiter wake")

	// The Y waiter is served by a later matching release (the cascade path
	// is exercised in the FIFO and stress tests below); here we only verify
	// it was not spuriously woken while gX and gAny hold both slots.
	expectNoAccount(t, gY.done, 100*time.Millisecond, "Y waiter before any Y-slot release")
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

	accX, err := p.Select(context.Background(), 1)
	if err != nil {
		t.Fatalf("select acc-x: %v", err)
	}
	accY, err := p.Select(context.Background(), 1)
	if err != nil {
		t.Fatalf("select acc-y: %v", err)
	}
	_ = accX // stays busy for the whole test

	// Queue order: gY first, then gAny.
	gY := &gatedWaiter{goCh: make(chan struct{}), done: make(chan *Account, 1)}
	gAny := &gatedWaiter{goCh: make(chan struct{}), done: make(chan *Account, 1)}
	go gY.run(p, "Y", true)
	go gAny.run(p, "", true)
	close(gY.goCh)
	time.Sleep(100 * time.Millisecond) // gY queued
	close(gAny.goCh)
	time.Sleep(100 * time.Millisecond) // gAny queued behind gY

	// Release Y: gY is the front-most Y-matching waiter → gY wins the slot.
	p.Release(accY)
	if got := waitForAccount(t, gY.done, 2*time.Second, "Y waiter"); got.Name() != "acc-y" {
		t.Fatalf("Y waiter got %q, want acc-y", got.Name())
	}
	if got := waitForAccount(t, gAny.done, 2*time.Second, "global waiter"); got.Name() != "acc-y" {
		t.Fatalf("global waiter got %q, want acc-y", got.Name())
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

	accX, err := p.Select(context.Background(), 1)
	if err != nil {
		t.Fatalf("select acc-x: %v", err)
	}
	accY, err := p.Select(context.Background(), 1)
	if err != nil {
		t.Fatalf("select acc-y: %v", err)
	}

	chX := make(chan *Account, 1)
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 1, "X")
		if err != nil {
			chX <- nil
			return
		}
		chX <- acc
	}()
	time.Sleep(150 * time.Millisecond) // let it queue

	// Release the Y account: no Y or "" waiter is queued → nobody woken.
	p.Release(accY)
	expectNoAccount(t, chX, 200*time.Millisecond, "X waiter after Y release")

	// Release the X account → the X waiter is woken.
	p.Release(accX)
	if got := waitForAccount(t, chX, 2*time.Second, "X waiter"); got.Name() != "acc-x" {
		t.Fatalf("X waiter got %q, want acc-x", got.Name())
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
	var x1, x2, y1, y2 *Account
	for _, acc := range p.AllAccounts() {
		switch acc.Name() {
		case "x1":
			x1 = acc
		case "x2":
			x2 = acc
		case "y1":
			y1 = acc
		case "y2":
			y2 = acc
		}
	}
	if _, err := p.SelectByProvider(context.Background(), 1, "X"); err != nil { // x1
		t.Fatalf("select x1: %v", err)
	}
	if _, err := p.SelectByProvider(context.Background(), 1, "Y"); err != nil { // y1
		t.Fatalf("select y1: %v", err)
	}
	x2.MarkExhausted()
	y2.MarkExhausted()

	chX := make(chan *Account, 1)
	chY := make(chan *Account, 1)
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 1, "X")
		if err != nil {
			chX <- nil
			return
		}
		chX <- acc
	}()
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 1, "Y")
		if err != nil {
			chY <- nil
			return
		}
		chY <- acc
	}()
	time.Sleep(150 * time.Millisecond) // let both queue

	// Recover x2: the X waiter (front-most X-matching) must be woken and get
	// x2; the Y waiter must stay queued.
	p.MarkHealthy(x2)
	if got := waitForAccount(t, chX, 2*time.Second, "X waiter"); got.Name() != "x2" {
		t.Fatalf("X waiter got %q, want x2", got.Name())
	}
	expectNoAccount(t, chY, 200*time.Millisecond, "Y waiter after MarkHealthy(x2)")

	// Recover y2: the Y waiter wakes and gets y2.
	p.MarkHealthy(y2)
	if got := waitForAccount(t, chY, 2*time.Second, "Y waiter"); got.Name() != "y2" {
		t.Fatalf("Y waiter got %q, want y2", got.Name())
	}

	// Cleanup the still-held slots.
	p.Release(x1)
	p.Release(y1)
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
	held := make([]*Account, 0, 4)
	for i := 0; i < 4; i++ {
		acc, err := p.Select(context.Background(), 1)
		if err != nil {
			t.Fatalf("occupy slot %d: %v", i, err)
		}
		held = append(held, acc)
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
			var acc *Account
			var err error
			if provider == "" {
				acc, err = p.Select(context.Background(), 1)
			} else {
				acc, err = p.SelectByProvider(context.Background(), 1, provider)
			}
			if err != nil {
				t.Errorf("waiter(%q) failed: %v", provider, err)
				return
			}
			atomic.AddInt32(&done, 1)
			p.Release(acc) // cascade
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
	for _, acc := range held {
		p.Release(acc)
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

	accX, err := p.Select(context.Background(), 1)
	if err != nil {
		t.Fatalf("occupy acc-x: %v", err)
	}

	// Real second waiter: parks behind the simulated first waiter.
	ch2 := make(chan *Account, 1)
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 1, "X")
		if err != nil {
			ch2 <- nil
			return
		}
		ch2 <- acc
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "second waiter to park")

	// Simulated first waiter, queued AHEAD of the real one: it is the
	// front-most X-matching waiter, so the Release below wakes IT.
	p.mu.Lock()
	w1 := &waiter{ch: make(chan struct{}), active: true, provider: "X", max: 1}
	elem1 := p.waiters.PushFront(w1)
	p.mu.Unlock()

	// Release: the wakeup lands on w1 (removed from the queue, channel
	// closed) — while w1's select has already committed to ctx.Done.
	p.Release(accX)

	// w1 bails: removeWaiterAndTransfer must hand the consumed wakeup to
	// the next waiter that can use the freed slot (the real waiter).
	p.removeWaiterAndTransfer(elem1)

	got := waitForAccount(t, ch2, 2*time.Second, "second waiter after first waiter bailed")
	if got.Name() != "acc-x" {
		t.Fatalf("second waiter got %q, want acc-x", got.Name())
	}
	p.Release(got)
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

	accX, err := p.Select(context.Background(), 1)
	if err != nil {
		t.Fatalf("occupy acc-x: %v", err)
	}
	ch2 := make(chan *Account, 1)
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 1, "X")
		if err != nil {
			ch2 <- nil
			return
		}
		ch2 <- acc
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "second waiter to park")

	p.mu.Lock()
	w1 := &waiter{ch: make(chan struct{}), active: true, provider: "X", max: 1}
	elem1 := p.waiters.PushFront(w1)
	p.mu.Unlock()

	p.Release(accX) // wakeup consumed by w1
	// Another caller grabs the freed slot before w1 bails.
	accX2, err := p.Select(context.Background(), 1)
	if err != nil {
		t.Fatalf("re-acquire freed slot: %v", err)
	}
	p.removeWaiterAndTransfer(elem1) // w1 bails: no capacity left → no transfer

	expectNoAccount(t, ch2, 200*time.Millisecond, "second waiter after capacity gone")

	p.Release(accX2)
	got := waitForAccount(t, ch2, 2*time.Second, "second waiter after real release")
	if got.Name() != "acc-x" {
		t.Fatalf("second waiter got %q, want acc-x", got.Name())
	}
	p.Release(got)
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

	accX, err := p.SelectByProvider(context.Background(), 1, "X")
	if err != nil {
		t.Fatalf("occupy acc-x: %v", err)
	}
	accY, err := p.SelectByProvider(context.Background(), 1, "Y")
	if err != nil {
		t.Fatalf("occupy acc-y: %v", err)
	}

	chY := make(chan *Account, 1)
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 1, "Y")
		if err != nil {
			chY <- nil
			return
		}
		chY <- acc
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "Y waiter to park")

	p.mu.Lock()
	wX := &waiter{ch: make(chan struct{}), active: true, provider: "X", max: 1}
	elemX := p.waiters.PushFront(wX)
	p.mu.Unlock()

	// The X release's wakeup is consumed by the bailing X waiter; the
	// transfer must skip the Y waiter (provider mismatch, no Y capacity).
	p.Release(accX)
	p.removeWaiterAndTransfer(elemX)
	expectNoAccount(t, chY, 200*time.Millisecond, "Y waiter after X slot freed")

	// A real Y release wakes the Y waiter.
	p.Release(accY)
	got := waitForAccount(t, chY, 2*time.Second, "Y waiter after Y release")
	if got.Name() != "acc-y" {
		t.Fatalf("Y waiter got %q, want acc-y", got.Name())
	}
	p.Release(got)
}

// TestRemoveWaiterTransferMixedMax is the deterministic mixed-max
// regression test: the bailing waiter A has a SMALL maxConcurrent and the
// next queued waiter B has a LARGE one. After a release the account's
// inFlight sits in the band [A.max, B.max) — A cannot use the freed
// capacity, B can. A consumes the release's wakeup and then bails; the
// transfer MUST wake B, which acquires the account immediately. Before the
// fix the transfer was gated on the bailing waiter's OWN max
// (capacityAvailableFor(A.provider, A.max)), which is false in this band,
// so B stayed parked until its 60s fallback timer — the mixed-max lost
// wakeup. wakeNextUsable applies each candidate waiter's own provider/max,
// so the FIFO scan wakes exactly the first waiter that can use the slot.
func TestRemoveWaiterTransferMixedMax(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
	}
	p := NewPool(cfgs)

	// Occupy acc-x with three max=3 holders: each Select adds one in-flight
	// slot (TryAcquire is +1 while inFlight < max), so inFlight = 3 — the
	// smallest value at which a max=3 waiter parks.
	held := make([]*Account, 0, 3)
	for i := 0; i < 3; i++ {
		acc, err := p.Select(context.Background(), 3)
		if err != nil {
			t.Fatalf("occupy slot %d: %v", i, err)
		}
		held = append(held, acc)
	}

	// Real waiter B with max=3: it parks (inFlight 3 >= 3) and needs the
	// band inFlight < 3 to become servable.
	chB := make(chan *Account, 1)
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 3, "X")
		if err != nil {
			chB <- nil
			return
		}
		chB <- acc
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "B waiter (max=3) to park")

	// Simulated bailing waiter A with max=1, queued AHEAD of B: the
	// release below wakes A (front-most X-matching waiter).
	p.mu.Lock()
	wA := &waiter{ch: make(chan struct{}), active: true, provider: "X", max: 1}
	elemA := p.waiters.PushFront(wA)
	p.mu.Unlock()

	// Release one holder: inFlight 3→2, which is in the band
	// [A.max=1, B.max=3). A consumes the wakeup...
	p.Release(held[0])

	// ...then bails: the transfer must skip the capacity gate and reach B.
	p.removeWaiterAndTransfer(elemA)

	// B must get the account promptly (no fallback-timer starvation).
	got := waitForAccount(t, chB, 2*time.Second, "B waiter (max=3) after A bailed")
	if got.Name() != "acc-x" {
		t.Fatalf("B waiter got %q, want acc-x", got.Name())
	}

	// Cleanup: B acquired one slot (inFlight 3); release everything.
	p.Release(got)
	p.Release(held[1])
	p.Release(held[2])
}

// TestRemoveWaiterTransferSkipsUnusableMixedMax is the FIFO companion: with
// a max=1 waiter C queued AHEAD of the max=3 waiter B, a bailing max=1
// waiter's transfer must NOT wake C (inFlight is in the band where C is
// unusable) — it wakes the first waiter that CAN use the freed capacity (B),
// preserving FIFO among the usable set. The unusable C stays parked.
func TestRemoveWaiterTransferSkipsUnusableMixedMax(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
	}
	p := NewPool(cfgs)

	// Occupy acc-x with three max=3 holders (each Select adds one in-flight
	// slot): inFlight = 3, the smallest value at which a max=3 waiter parks.
	held := make([]*Account, 0, 3)
	for i := 0; i < 3; i++ {
		acc, err := p.Select(context.Background(), 3)
		if err != nil {
			t.Fatalf("occupy slot %d: %v", i, err)
		}
		held = append(held, acc)
	}

	// Real waiters: C (max=1) queues first, B (max=3) second.
	chC := make(chan *Account, 1)
	chB := make(chan *Account, 1)
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 1, "X")
		if err != nil {
			chC <- nil
			return
		}
		chC <- acc
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "C waiter (max=1) to park")
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 3, "X")
		if err != nil {
			chB <- nil
			return
		}
		chB <- acc
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 2 }, "B waiter (max=3) to park")

	// Simulated bailing waiter A (max=1) queued ahead of both.
	p.mu.Lock()
	wA := &waiter{ch: make(chan struct{}), active: true, provider: "X", max: 1}
	elemA := p.waiters.PushFront(wA)
	p.mu.Unlock()

	// Release one holder: inFlight 3→2. A consumes the wakeup and bails;
	// the transfer scan passes over C (max=1, unusable at inFlight=2) and
	// wakes B (max=3, usable) — FIFO within the usable set, C not woken.
	p.Release(held[0])
	p.removeWaiterAndTransfer(elemA)

	expectNoAccount(t, chC, 200*time.Millisecond, "C waiter (max=1) after transfer at inFlight=2")
	got := waitForAccount(t, chB, 2*time.Second, "B waiter (max=3) after A bailed")
	if got.Name() != "acc-x" {
		t.Fatalf("B waiter got %q, want acc-x", got.Name())
	}
	p.Release(got)

	// A further release (inFlight 2→1) still cannot serve C (needs < 1);
	// the final release (1→0) wakes C.
	p.Release(held[1])
	expectNoAccount(t, chC, 200*time.Millisecond, "C waiter at inFlight=1")
	p.Release(held[2])
	gotC := waitForAccount(t, chC, 2*time.Second, "C waiter (max=1) after inFlight reached 0")
	if gotC.Name() != "acc-x" {
		t.Fatalf("C waiter got %q, want acc-x", gotC.Name())
	}
	p.Release(gotC)
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
		accX, err := p.Select(context.Background(), 1)
		if err != nil {
			t.Fatalf("iter %d: occupy acc-x: %v", i, err)
		}

		ctx1, cancel1 := context.WithCancel(context.Background())
		ch1 := make(chan *Account, 1)
		ch2 := make(chan *Account, 1)
		go1 := make(chan struct{})
		go2 := make(chan struct{})
		go func() {
			<-go1
			acc, err := p.SelectByProvider(ctx1, 1, "X")
			if err != nil {
				ch1 <- nil
				return
			}
			ch1 <- acc
		}()
		go func() {
			<-go2
			acc, err := p.SelectByProvider(context.Background(), 1, "X")
			if err != nil {
				ch2 <- nil
				return
			}
			ch2 <- acc
		}()
		close(go1)
		waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "waiter 1 to park")
		close(go2)
		waitUntil(t, func() bool { return p.WaitingCount() == 2 }, "waiter 2 to park")

		cancel1()       // waiter 1's select may pick ctx.Done...
		p.Release(accX) // ...while the release wakes the front-most waiter (1)

		// Waiter 1 exits either way: bailed (nil) or woken-then-acquired
		// (account; its release cascades the slot to waiter 2).
		select {
		case acc := <-ch1:
			if acc != nil {
				p.Release(acc)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: waiter 1 never exited", i)
		}
		// Waiter 2 must get the slot promptly — no lost wakeup.
		select {
		case acc := <-ch2:
			if acc == nil || acc.Name() != "acc-x" {
				t.Fatalf("iter %d: waiter 2 got %v, want acc-x", i, acc)
			}
			p.Release(acc)
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: waiter 2 starved — wakeup swallowed by the bailing waiter 1", i)
		}
	}
}

// TestReleaseInitialWakeMixedMax is the deterministic mixed-max regression
// test for the INITIAL Release wakeup (no bailing waiter involved): a
// max=1 waiter C queues AHEAD of a max=3 waiter B while the account's
// inFlight is 3. One release brings inFlight to 2 — the band where C is
// unusable (needs inFlight < 1) and B is usable (needs inFlight < 3). The
// release MUST skip C and wake B: waking the front-most provider-matching
// waiter without the capacity check (the old wakeWaiterFor) would let C
// consume the wakeup, fail to select and re-park at the back while B stays
// parked until its 60s fallback timer — the mixed-max lost wakeup on the
// plain Release path. FIFO among the usable set is preserved: B is the
// first waiter that can use the freed capacity.
func TestReleaseInitialWakeMixedMax(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-x", Key: "kx", BaseURL: "http://localhost:8001", Provider: "X"},
	}
	p := NewPool(cfgs)

	// Occupy acc-x with three max=3 holders: inFlight = 3, the smallest
	// value at which a max=3 waiter parks.
	held := make([]*Account, 0, 3)
	for i := 0; i < 3; i++ {
		acc, err := p.Select(context.Background(), 3)
		if err != nil {
			t.Fatalf("occupy slot %d: %v", i, err)
		}
		held = append(held, acc)
	}

	// Real waiters: C (max=1) queues first, B (max=3) second.
	chC := make(chan *Account, 1)
	chB := make(chan *Account, 1)
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 1, "X")
		if err != nil {
			chC <- nil
			return
		}
		chC <- acc
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "C waiter (max=1) to park")
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 3, "X")
		if err != nil {
			chB <- nil
			return
		}
		chB <- acc
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 2 }, "B waiter (max=3) to park")

	// Release one holder: inFlight 3→2, in the band [C.max=1, B.max=3).
	// The wake scan must pass over C (unusable) and wake B — the first
	// waiter that can use the freed capacity.
	p.Release(held[0])

	expectNoAccount(t, chC, 200*time.Millisecond, "C waiter (max=1) after initial release at inFlight=2")
	got := waitForAccount(t, chB, 2*time.Second, "B waiter (max=3) after initial release")
	if got.Name() != "acc-x" {
		t.Fatalf("B waiter got %q, want acc-x", got.Name())
	}

	// Cleanup: B holds one slot (inFlight back to 3). C needs inFlight < 1
	// to become servable; it is woken only by the final release (1→0).
	p.Release(held[1]) // 3→2
	expectNoAccount(t, chC, 200*time.Millisecond, "C waiter at inFlight=2")
	p.Release(held[2]) // 2→1
	expectNoAccount(t, chC, 200*time.Millisecond, "C waiter at inFlight=1")
	p.Release(got) // 1→0
	gotC := waitForAccount(t, chC, 2*time.Second, "C waiter (max=1) after inFlight reached 0")
	if gotC.Name() != "acc-x" {
		t.Fatalf("C waiter got %q, want acc-x", gotC.Name())
	}
	p.Release(gotC)
}

// TestMarkHealthyInitialWakeMixedMax is the MarkHealthy companion: an
// exhausted account with inFlight=2 (probe exhaustion while requests are
// still in flight) recovers with C (max=1) queued ahead of B (max=3) while
// the other X account sits at inFlight=3. MarkHealthy makes x2 selectable
// again, but its 2 in-flight slots can only serve B (needs < 3), not C
// (needs < 1). The wake must skip the unusable front waiter C and wake B
// directly — waking C first would burn the wakeup (C re-parks at the back)
// and strand B until its fallback timer.
func TestMarkHealthyInitialWakeMixedMax(t *testing.T) {
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
	// Occupy x1 to inFlight=3 (three max=3 holders — unusable for both C
	// and B) and x2 to inFlight=2 (two max=2 holders). Direct TryAcquire
	// keeps the per-account occupancy deterministic regardless of the
	// round-robin rotation.
	for i := 0; i < 3; i++ {
		if !x1.TryAcquire(3) {
			t.Fatalf("occupy x1 slot %d", i)
		}
	}
	for i := 0; i < 2; i++ {
		if !x2.TryAcquire(2) {
			t.Fatalf("occupy x2 slot %d", i)
		}
	}
	// Simulate a probe marking x2 exhausted while its two requests are
	// still in flight.
	x2.MarkExhausted()

	// C (max=1) parks first, B (max=3) second: x1 is busy at inFlight=3
	// (too full for either), x2 is exhausted (not selectable).
	chC := make(chan *Account, 1)
	chB := make(chan *Account, 1)
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 1, "X")
		if err != nil {
			chC <- nil
			return
		}
		chC <- acc
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "C waiter (max=1) to park")
	go func() {
		acc, err := p.SelectByProvider(context.Background(), 3, "X")
		if err != nil {
			chB <- nil
			return
		}
		chB <- acc
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 2 }, "B waiter (max=3) to park")

	// Recover x2: inFlight stays 2, in the band [C.max=1, B.max=3) — the
	// wake scan must skip C (x2: 2<1 false; x1: 3<1 false) and wake B
	// (x2: 2<3 true).
	p.MarkHealthy(x2)

	expectNoAccount(t, chC, 200*time.Millisecond, "C waiter (max=1) after MarkHealthy at inFlight=2")
	got := waitForAccount(t, chB, 2*time.Second, "B waiter (max=3) after MarkHealthy")
	if got.Name() != "x2" {
		t.Fatalf("B waiter got %q, want x2", got.Name())
	}

	// Cleanup: B holds one x2 slot (inFlight(x2) = 2 holders + B = 3). C
	// needs some X account below 1; x2 reaches 0 only on the final release.
	p.Release(x2) // 3→2
	expectNoAccount(t, chC, 200*time.Millisecond, "C waiter at inFlight=2")
	p.Release(x2) // 2→1
	expectNoAccount(t, chC, 200*time.Millisecond, "C waiter at inFlight=1")
	p.Release(x2) // 1→0
	gotC := waitForAccount(t, chC, 2*time.Second, "C waiter (max=1) after inFlight reached 0")
	if gotC.Name() != "x2" {
		t.Fatalf("C waiter got %q, want x2", gotC.Name())
	}
	p.Release(gotC)
	// Release the x1 holders so no slot leaks.
	for i := 0; i < 3; i++ {
		p.Release(x1)
	}
}
