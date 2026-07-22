package pool

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
)

func TestPoolFIFOAndRelease(t *testing.T) {
	slog.Info("TEST: TestPoolFIFOAndRelease started")
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
		{Name: "acc-2", Key: "key-2", BaseURL: "http://localhost:8002"},
	}
	p := NewPool(cfgs)

	ctx := context.Background()
	slog.Info("TEST: Selecting acc1")
	acc1, err := p.Select(ctx, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	slog.Info("TEST: Selecting acc2")
	acc2, err := p.Select(ctx, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ch1 := make(chan *Account, 1)
	ch2 := make(chan *Account, 1)

	go func() {
		slog.Info("TEST: Goroutine 1 calling Select")
		acc, err := p.Select(ctx, 1)
		slog.Info("TEST: Goroutine 1 Select returned", "account", acc.Name(), "error", err)
		ch1 <- acc
		slog.Info("TEST: Goroutine 1 sent to ch1")
	}()

	go func() {
		slog.Info("TEST: Goroutine 2 calling Select")
		acc, err := p.Select(ctx, 1)
		slog.Info("TEST: Goroutine 2 Select returned", "account", acc.Name(), "error", err)
		ch2 <- acc
		slog.Info("TEST: Goroutine 2 sent to ch2")
	}()

	time.Sleep(100 * time.Millisecond)
	slog.Info("TEST: Releasing acc1 and acc2")
	p.Release(acc1)
	p.Release(acc2)

	// 此时两个协程应该都被唤醒并返回
	var results []*Account
	for i := 0; i < 2; i++ {
		select {
		case acc := <-ch1:
			slog.Info("TEST: Main read from ch1", "account", acc.Name())
			results = append(results, acc)
		case acc := <-ch2:
			slog.Info("TEST: Main read from ch2", "account", acc.Name())
			results = append(results, acc)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for workers to be woken up")
		}
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// 验证获取到的账号确实是 acc-1 和 acc-2 (顺序可能因调度而异)
	names := map[string]bool{}
	for _, acc := range results {
		names[acc.Name()] = true
	}
	if !names["acc-1"] || !names["acc-2"] {
		gotNames := make([]string, len(results))
		for i, a := range results {
			gotNames[i] = a.Name()
		}
		t.Errorf("expected to get acc-1 and acc-2, got: %v", gotNames)
	}
}

func TestPoolCancelAndSignalTransfer(t *testing.T) {
	// A is started with an already-cancelled context (returns immediately).
	// B waits for the single account. Release wakes B without race.
	slog.Info("TEST: TestPoolCancelAndSignalTransfer started")
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
	}
	p := NewPool(cfgs)

	ctx := context.Background()
	acc1, _ := p.Select(ctx, 1) // occupies the only account

	accChB := make(chan *Account, 1)
	go func() {
		acc, _ := p.Select(ctx, 1)
		accChB <- acc
	}()

	time.Sleep(50 * time.Millisecond)

	ctxCancel, cancel := context.WithCancel(ctx)
	cancel() // cancel before entering Select

	errChA := make(chan error, 1)
	go func() {
		_, err := p.Select(ctxCancel, 1)
		errChA <- err
	}()

	// A should return immediately with Canceled
	select {
	case err := <-errChA:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected A to fail with Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for A to exit")
	}

	// Now release to wake B
	p.Release(acc1)

	select {
	case acc := <-accChB:
		if acc == nil {
			t.Error("expected B to be woken up, got nil")
		} else if acc.Name() != "acc-1" {
			t.Errorf("expected B to get acc-1, got %s", acc.Name())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for B to get the transferred signal")
	}
}

func TestPoolMarkHealthyWakeup(t *testing.T) {
	slog.Info("TEST: TestPoolMarkHealthyWakeup started")
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
		{Name: "acc-2", Key: "key-2", BaseURL: "http://localhost:8002"},
	}
	p := NewPool(cfgs)

	ctx := context.Background()
	// 占满这两个账号，把 acc-1 变为 Exhausted，acc-2 在长冷却中
	acc1, _ := p.Select(ctx, 1)
	acc2, _ := p.Select(ctx, 1)

	acc1.MarkExhausted()
	p.Release(acc1) // 此时 acc1 在 Exhausted 状态，不能用于 Select

	acc2.SetCooldown(1 * time.Hour)
	p.Release(acc2) // 此时 acc2 在 Healthy 状态但在 cooldown，所以可以参与 Select 但需要等待

	// 此时启动 Select，因为 acc2 处于健康但 cooldown 中，会阻塞等待它的 cooldown 计时器。
	accCh := make(chan *Account, 1)
	go func() {
		acc, _ := p.Select(ctx, 1)
		accCh <- acc
	}()

	time.Sleep(50 * time.Millisecond)

	// 现在调用 p.MarkHealthy(acc1) 模拟 probe 成功将 acc1 从 Exhausted 状态捞出来，
	// 它必须立马清除冷却并唤醒等待协程！
	p.MarkHealthy(acc1)

	select {
	case acc := <-accCh:
		if acc == nil || acc.Name() != "acc-1" {
			if acc == nil {
				t.Error("expected to get acc-1 immediately, got nil")
			} else {
				t.Errorf("expected to get acc-1 immediately, got %s", acc.Name())
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: waiting worker was not woken up by MarkHealthy")
	}
}

func TestSetCooldownDoesNotShorten(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}

	// Set a long cooldown first
	acc.SetCooldown(5 * time.Minute)
	if !acc.IsInCooldown() {
		t.Fatal("expected account to be in cooldown after 5m set")
	}

	// Try to shorten with a shorter cooldown — should NOT shorten
	acc.SetCooldown(30 * time.Second)
	if !acc.IsInCooldown() {
		t.Fatal("expected account to still be in cooldown after short set")
	}
	// Should have at least 4 minutes remaining (5m - 30s overhead)
	remaining := time.Until(acc.cooldownUntil)
	if remaining < 4*time.Minute {
		t.Errorf("cooldown was shortened: remaining = %v, want >= 4m", remaining)
	}
}

func TestQuotaCooldownNotExhaustion(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}

	// Simulate quota error cooldown
	acc.SetCooldown(30 * time.Minute)

	if !acc.IsInCooldown() {
		t.Fatal("expected account to be in cooldown")
	}
	if !acc.IsHealthy() {
		t.Fatal("account should still be healthy after quota cooldown")
	}
}

func TestNewHTTPClient_ResponseHeaderTimeout(t *testing.T) {
	c := newHTTPClient()

	// http.Client.Timeout must remain 0 for streaming.
	if c.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0 (streaming must not be truncated)", c.Timeout)
	}

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("Transport is not *http.Transport")
	}

	if tr.ResponseHeaderTimeout == 0 {
		t.Error("ResponseHeaderTimeout is 0, want non-zero defence for stale upstream connections")
	}
}

// --- New concurrency tests ---

// TestConcurrentLimitN verifies that N goroutines can all acquire the same
// account when maxConcurrent=N, and the N+1th enters the waiter (fails on
// short context or succeeds after a Release).
func TestConcurrentLimitN(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
	}
	p := NewPool(cfgs)
	const maxc = 5

	// Acquire maxc slots on the single account.
	accs := make([]*Account, maxc)
	for i := 0; i < maxc; i++ {
		var err error
		accs[i], err = p.Select(context.Background(), maxc)
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
	}

	// N+1 should fail with timeout (short context).
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := p.Select(ctx, maxc)
	if err == nil {
		t.Fatal("expected select to timeout when all slots are full")
	}

	// Release one, then N+1 should succeed.
	p.Release(accs[0])
	accN1, err := p.Select(context.Background(), maxc)
	if err != nil {
		t.Fatalf("select after release: %v", err)
	}
	if accN1.Name() != "acc-1" {
		t.Errorf("expected acc-1, got %s", accN1.Name())
	}
	// Cleanup
	for _, a := range accs[1:] {
		p.Release(a)
	}
	p.Release(accN1)
}

// TestReleaseWakesWaiter verifies that when maxc slots are full, releasing one
// wakes a waiter which can then acquire the slot.
func TestReleaseWakesWaiterWithConcurrency(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
	}
	p := NewPool(cfgs)
	const maxc = 3

	// Fill all maxc slots.
	accs := make([]*Account, maxc)
	for i := 0; i < maxc; i++ {
		var err error
		accs[i], err = p.Select(context.Background(), maxc)
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
	}

	// Start a goroutine that waits for a slot.
	ch := make(chan *Account, 1)
	go func() {
		acc, err := p.Select(context.Background(), maxc)
		if err != nil {
			t.Errorf("waiter select error: %v", err)
			ch <- nil
			return
		}
		ch <- acc
	}()

	// Give the goroutine time to enter the waiter.
	time.Sleep(200 * time.Millisecond)

	// Release one slot.
	p.Release(accs[0])

	// Waiter should be woken up.
	select {
	case acc := <-ch:
		if acc == nil {
			t.Fatal("waiter got nil account")
		}
		if acc.Name() != "acc-1" {
			t.Errorf("expected acc-1, got %s", acc.Name())
		}
		p.Release(acc)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: waiter was not woken up by Release")
	}

	// Cleanup remaining.
	for _, a := range accs[1:] {
		p.Release(a)
	}
}

// TestTryAcquireStrictMax verifies that TryAcquire never exceeds max
// even under high concurrency.
func TestTryAcquireStrictMax(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}

	const max = 10
	const goroutines = 100

	var wg sync.WaitGroup
	acquired := make([]bool, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if acc.TryAcquire(max) {
				acquired[idx] = true
			}
		}(i)
	}
	wg.Wait()

	// Count how many acquired.
	total := 0
	for _, a := range acquired {
		if a {
			total++
		}
	}
	if total > max {
		t.Errorf("TryAcquire allowed %d > max %d", total, max)
	}
	if total != max {
		t.Errorf("expected exactly %d acquired, got %d", max, total)
	}

	// inFlight must be <= max.
	inFlight := acc.InFlightCount()
	if inFlight > max {
		t.Errorf("inFlight %d > max %d", inFlight, max)
	}
}

// TestReleaseSafety verifies that double-Release does not underflow inFlight.
func TestReleaseSafety(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}

	// Acquire one slot
	if !acc.TryAcquire(1) {
		t.Fatal("TryAcquire failed unexpectedly")
	}
	if got := acc.InFlightCount(); got != 1 {
		t.Errorf("inFlight after acquire = %d, want 1", got)
	}
	acc.Release() // first release: inFlight goes to 0
	acc.Release() // second release: should trigger warn and clamp to 0

	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("inFlight after double release = %d, want 0", got)
	}
}

// TestSnapshotStats verifies that SnapshotStats returns reasonable values.
func TestSnapshotStats(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
		{Name: "acc-2", Key: "key-2", BaseURL: "http://localhost:8002"},
	}
	p := NewPool(cfgs)

	snap := p.SnapshotStats()
	if snap.Total != 2 {
		t.Errorf("Total = %d, want 2", snap.Total)
	}
	if snap.Healthy != 2 {
		t.Errorf("Healthy = %d, want 2", snap.Healthy)
	}
	if snap.Exhausted != 0 {
		t.Errorf("Exhausted = %d, want 0", snap.Exhausted)
	}
	if snap.InFlightSum != 0 {
		t.Errorf("InFlightSum = %d, want 0", snap.InFlightSum)
	}
}

// TestCooldownExhaustCount verifies that cooldownCount tracks SetCooldown calls
// and exhaustCount tracks MarkExhausted calls (without double-counting).
func TestCooldownExhaustCount(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}

	// SetCooldown 3 times
	acc.SetCooldown(1 * time.Minute)
	acc.SetCooldown(2 * time.Minute)
	acc.SetCooldown(3 * time.Minute)
	if got := acc.CooldownCount(); got != 3 {
		t.Errorf("CooldownCount after 3 SetCooldown calls = %d, want 3", got)
	}

	// MarkExhausted once
	acc.MarkExhausted()
	if got := acc.ExhaustCount(); got != 1 {
		t.Errorf("ExhaustCount after 1 MarkExhausted = %d, want 1", got)
	}

	// Repeat MarkExhausted — should NOT increment count (only transitions)
	acc.MarkExhausted()
	acc.MarkExhausted()
	if got := acc.ExhaustCount(); got != 1 {
		t.Errorf("ExhaustCount after 3 MarkExhausted calls = %d, want 1 (no double-count)", got)
	}

	// CooldownCount should still be 3
	if got := acc.CooldownCount(); got != 3 {
		t.Errorf("CooldownCount after exhaust = %d, want 3", got)
	}
}
