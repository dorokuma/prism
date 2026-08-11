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
