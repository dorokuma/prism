package pool

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
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
	_, slot1, err := p.Select(ctx, "m", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	slog.Info("TEST: Selecting acc2")
	_, slot2, err := p.Select(ctx, "m", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ch1 := make(chan *Account, 1)
	ch2 := make(chan *Account, 1)

	go func() {
		slog.Info("TEST: Goroutine 1 calling Select")
		acc, _, err := p.Select(ctx, "m", 1)
		slog.Info("TEST: Goroutine 1 Select returned", "account", acc.Name(), "error", err)
		ch1 <- acc
		slog.Info("TEST: Goroutine 1 sent to ch1")
	}()

	go func() {
		slog.Info("TEST: Goroutine 2 calling Select")
		acc, _, err := p.Select(ctx, "m", 1)
		slog.Info("TEST: Goroutine 2 Select returned", "account", acc.Name(), "error", err)
		ch2 <- acc
		slog.Info("TEST: Goroutine 2 sent to ch2")
	}()

	time.Sleep(100 * time.Millisecond)
	slog.Info("TEST: Releasing acc1 and acc2")
	p.Release(slot1)
	p.Release(slot2)

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
	// 验证获取到的账号确实是 acc-1 和 acc-2。选中本身现在是确定性的
	// round-robin（先 acc-1 再 acc-2，游标只在成功选中时前进 1），两个等待
	// 协程按 FIFO 依次被唤醒；但主循环从两个 channel 读回的先后仍可能因
	// 调度而异，所以这里按集合断言，不按顺序。
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
	_, slot1, _ := p.Select(ctx, "m", 1) // occupies the only account

	accChB := make(chan *Account, 1)
	go func() {
		acc, _, _ := p.Select(ctx, "m", 1)
		accChB <- acc
	}()

	time.Sleep(50 * time.Millisecond)

	ctxCancel, cancel := context.WithCancel(ctx)
	cancel() // cancel before entering Select

	errChA := make(chan error, 1)
	go func() {
		_, _, err := p.Select(ctxCancel, "m", 1)
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
	p.Release(slot1)

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
	acc1, slot1, _ := p.Select(ctx, "m", 1)
	acc2, slot2, _ := p.Select(ctx, "m", 1)

	acc1.MarkExhausted()
	p.Release(slot1) // 此时 acc1 在 Exhausted 状态，不能用于 Select

	acc2.SetCooldown(1 * time.Hour)
	p.Release(slot2) // 此时 acc2 在 Healthy 状态但在 cooldown，所以可以参与 Select 但需要等待

	// 此时启动 Select，因为 acc2 处于健康但 cooldown 中，会阻塞等待它的 cooldown 计时器。
	accCh := make(chan *Account, 1)
	go func() {
		acc, _, _ := p.Select(ctx, "m", 1)
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

// --- Round-robin selection tests ---

// TestSelectByProviderRoundRobinStrict verifies that consecutive
// SelectByProvider calls for a single two-account provider strictly alternate
// between the accounts in config (YAML) order.
func TestSelectByProviderRoundRobinStrict(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "a1", Key: "key-a1", BaseURL: "http://localhost:8001", Provider: "prov"},
		{Name: "a2", Key: "key-a2", BaseURL: "http://localhost:8002", Provider: "prov"},
	}
	p := NewPool(cfgs)
	ctx := context.Background()

	var got []string
	for i := 0; i < 6; i++ {
		acc, slot, err := p.SelectByProvider(ctx, "m", 1, "prov")
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		got = append(got, acc.Name())
		p.Release(slot)
	}
	want := []string{"a1", "a2", "a1", "a2", "a1", "a2"}
	if !slices.Equal(got, want) {
		t.Fatalf("round-robin sequence = %v, want %v", got, want)
	}
}

// TestSelectByProviderCrossProviderIsolation verifies that interleaved
// requests to two providers each rotate strictly within their own account
// subset: provider A gets a1,a2,a1,a2 and provider B gets b1,b2,b1,b2 — one
// provider's traffic never advances another provider's rotation.
func TestSelectByProviderCrossProviderIsolation(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "a1", Key: "key-a1", BaseURL: "http://localhost:8001", Provider: "provA"},
		{Name: "a2", Key: "key-a2", BaseURL: "http://localhost:8002", Provider: "provA"},
		{Name: "b1", Key: "key-b1", BaseURL: "http://localhost:8003", Provider: "provB"},
		{Name: "b2", Key: "key-b2", BaseURL: "http://localhost:8004", Provider: "provB"},
	}
	p := NewPool(cfgs)
	ctx := context.Background()

	var gotA, gotB []string
	for i := 0; i < 4; i++ {
		acc, slot, err := p.SelectByProvider(ctx, "m", 1, "provA")
		if err != nil {
			t.Fatalf("select A %d: %v", i, err)
		}
		gotA = append(gotA, acc.Name())
		p.Release(slot)

		acc, slot, err = p.SelectByProvider(ctx, "m", 1, "provB")
		if err != nil {
			t.Fatalf("select B %d: %v", i, err)
		}
		gotB = append(gotB, acc.Name())
		p.Release(slot)
	}
	if want := []string{"a1", "a2", "a1", "a2"}; !slices.Equal(gotA, want) {
		t.Errorf("provider A sequence = %v, want %v", gotA, want)
	}
	if want := []string{"b1", "b2", "b1", "b2"}; !slices.Equal(gotB, want) {
		t.Errorf("provider B sequence = %v, want %v", gotB, want)
	}
}

// TestSelectByProviderHighTrafficIsolation reproduces the production defect:
// a single-account high-traffic provider must not pollute the rotation of a
// two-account provider. After 100 selections on the busy provider, the
// low-traffic provider still splits 2:2 in strict alternation (not 3:1 or
// 4:0).
func TestSelectByProviderHighTrafficIsolation(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "plan-3", Key: "key-plan", BaseURL: "http://localhost:8001", Provider: "plan"},
		{Name: "oai-1", Key: "key-oai1", BaseURL: "http://localhost:8002", Provider: "agentrouter-openai"},
		{Name: "oai-2", Key: "key-oai2", BaseURL: "http://localhost:8003", Provider: "agentrouter-openai"},
	}
	p := NewPool(cfgs)
	ctx := context.Background()

	// 100 selections on the high-traffic single-account provider.
	for i := 0; i < 100; i++ {
		acc, slot, err := p.SelectByProvider(ctx, "m", 1, "plan")
		if err != nil {
			t.Fatalf("plan select %d: %v", i, err)
		}
		if acc.Name() != "plan-3" {
			t.Fatalf("plan select %d got %s, want plan-3", i, acc.Name())
		}
		p.Release(slot)
	}

	// The low-traffic provider must still rotate strictly 2:2.
	var got []string
	for i := 0; i < 4; i++ {
		acc, slot, err := p.SelectByProvider(ctx, "m", 1, "agentrouter-openai")
		if err != nil {
			t.Fatalf("agent select %d: %v", i, err)
		}
		got = append(got, acc.Name())
		p.Release(slot)
	}
	want := []string{"oai-1", "oai-2", "oai-1", "oai-2"}
	if !slices.Equal(got, want) {
		t.Fatalf("low-traffic provider sequence = %v, want %v (high-traffic provider polluted the rotation)", got, want)
	}
}

// TestSelectByProviderCooldownRoundRobin verifies that rotation stays strict
// between the available accounts when one account is in cooldown: with three
// accounts and the middle one cooling down, requests must alternate between
// the other two (cooldown accounts are skipped without breaking the cycle).
func TestSelectByProviderCooldownRoundRobin(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "c1", Key: "key-c1", BaseURL: "http://localhost:8001", Provider: "prov"},
		{Name: "c2", Key: "key-c2", BaseURL: "http://localhost:8002", Provider: "prov"},
		{Name: "c3", Key: "key-c3", BaseURL: "http://localhost:8003", Provider: "prov"},
	}
	p := NewPool(cfgs)
	ctx := context.Background()

	for _, acc := range p.AllAccounts() {
		if acc.Name() == "c2" {
			acc.SetCooldown(time.Hour)
		}
	}

	var got []string
	for i := 0; i < 6; i++ {
		acc, slot, err := p.SelectByProvider(ctx, "m", 1, "prov")
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		got = append(got, acc.Name())
		p.Release(slot)
	}
	want := []string{"c1", "c3", "c1", "c3", "c1", "c3"}
	if !slices.Equal(got, want) {
		t.Fatalf("cooldown round-robin sequence = %v, want %v", got, want)
	}
}

// TestSelectRoundRobinUniform verifies the full-pool Select path (no
// provider) also rotates uniformly: two accounts strictly alternate.
func TestSelectRoundRobinUniform(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
		{Name: "acc-2", Key: "key-2", BaseURL: "http://localhost:8002"},
	}
	p := NewPool(cfgs)
	ctx := context.Background()

	var got []string
	for i := 0; i < 6; i++ {
		acc, slot, err := p.Select(ctx, "m", 1)
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		got = append(got, acc.Name())
		p.Release(slot)
	}
	want := []string{"acc-1", "acc-2", "acc-1", "acc-2", "acc-1", "acc-2"}
	if !slices.Equal(got, want) {
		t.Fatalf("full-pool round-robin sequence = %v, want %v", got, want)
	}
}

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
	slots := make([]*Slot, maxc)
	for i := 0; i < maxc; i++ {
		var err error
		_, slots[i], err = p.Select(context.Background(), "m", maxc)
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
	}

	// N+1 should fail with timeout (short context).
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, err := p.Select(ctx, "m", maxc)
	if err == nil {
		t.Fatal("expected select to timeout when all slots are full")
	}

	// Release one, then N+1 should succeed.
	p.Release(slots[0])
	accN1, slotN1, err := p.Select(context.Background(), "m", maxc)
	if err != nil {
		t.Fatalf("select after release: %v", err)
	}
	if accN1.Name() != "acc-1" {
		t.Errorf("expected acc-1, got %s", accN1.Name())
	}
	// Cleanup
	for _, s := range slots[1:] {
		p.Release(s)
	}
	p.Release(slotN1)
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
	slots := make([]*Slot, maxc)
	for i := 0; i < maxc; i++ {
		var err error
		_, slots[i], err = p.Select(context.Background(), "m", maxc)
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
	}

	// Start a goroutine that waits for a slot.
	ch := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.Select(context.Background(), "m", maxc)
		if err != nil {
			ch <- slotResult{}
			return
		}
		ch <- slotResult{acc: acc, slot: slot}
	}()

	// Give the goroutine time to enter the waiter.
	time.Sleep(200 * time.Millisecond)

	// Release one slot.
	p.Release(slots[0])

	// Waiter should be woken up.
	select {
	case res := <-ch:
		if res.acc == nil {
			t.Fatal("waiter got nil account")
		}
		if res.acc.Name() != "acc-1" {
			t.Errorf("expected acc-1, got %s", res.acc.Name())
		}
		p.Release(res.slot)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: waiter was not woken up by Release")
	}

	// Cleanup remaining.
	for _, s := range slots[1:] {
		p.Release(s)
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
			if acc.TryAcquire("m", max) != nil {
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

// TestReleaseSafety verifies that double-Release (releasing the same lease
// twice — a caller bug) does not underflow inFlight.
func TestReleaseSafety(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "test"}, status: StatusHealthy, client: newHTTPClient()}

	// Acquire one slot
	slot := acc.TryAcquire("m", 1)
	if slot == nil {
		t.Fatal("TryAcquire failed unexpectedly")
	}
	if got := acc.InFlightCount(); got != 1 {
		t.Errorf("inFlight after acquire = %d, want 1", got)
	}
	acc.Release(slot) // first release: inFlight goes to 0
	acc.Release(slot) // second release: should trigger warn and clamp to 0

	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("inFlight after double release = %d, want 0", got)
	}
}

// TestReleaseMismatchedSlotIgnored verifies the lease contract: releasing a
// slot belonging to another account (or a nil slot) is a caller bug that is
// warned about and ignored — it must never decrement another account's
// counters.
func TestReleaseMismatchedSlotIgnored(t *testing.T) {
	a1 := &Account{cfg: config.AccountConfig{Name: "a1"}, status: StatusHealthy, client: newHTTPClient()}
	a2 := &Account{cfg: config.AccountConfig{Name: "a2"}, status: StatusHealthy, client: newHTTPClient()}

	s1 := a1.TryAcquire("m", 1)
	if s1 == nil {
		t.Fatal("a1 acquire failed")
	}
	s2 := a2.TryAcquire("m", 1)
	if s2 == nil {
		t.Fatal("a2 acquire failed")
	}
	// Releasing a1's slot on a2 must be ignored (a2 keeps its in-flight).
	a2.Release(s1)
	if got := a2.InFlightCount(); got != 1 {
		t.Errorf("a2 inFlight after mismatched release = %d, want 1", got)
	}
	if got := a1.InFlightCount(); got != 1 {
		t.Errorf("a1 inFlight after mismatched release = %d, want 1 (its slot must stay held)", got)
	}
	a2.Release(nil)
	if got := a2.InFlightCount(); got != 1 {
		t.Errorf("a2 inFlight after nil release = %d, want 1", got)
	}
	a1.Release(s1)
	a2.Release(s2)
}

// -------------------------------------------------------------------------
// Per-model-KEY concurrency — the concurrency domain is the MODEL, not the
// max VALUE (two models with the same max never share a counter), bounded
// by an explicit account-wide total cap.
// -------------------------------------------------------------------------

// TestSameMaxDifferentModelsIsolated is the acceptance test for per-key
// isolation: two models with the SAME max value (both 2) must not share a
// counter — the old per-max-value grouping merged them and let one model's
// traffic starve the other. With per-model keys, model "a" filling its 2
// slots must NOT block model "b"'s 2 slots (the account total cap 4 admits
// both).
func TestSameMaxDifferentModelsIsolated(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
	}
	p := NewPoolWithTotalCap(cfgs, 4)

	// Fill model "a" to its cap of 2.
	slotsA := make([]*Slot, 0, 2)
	for i := 0; i < 2; i++ {
		acc, slot, err := p.Select(context.Background(), "a", 2)
		if err != nil {
			t.Fatalf("occupy a slot %d: %v", i, err)
		}
		if acc.Name() != "acc-1" {
			t.Fatalf("a slot %d got %q, want acc-1", i, acc.Name())
		}
		slotsA = append(slotsA, slot)
	}
	// A third "a" request must park (a's own counter is full).
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, _, err := p.Select(ctx, "a", 2); err == nil {
		t.Fatal("third a-slot must wait (per-key cap must hold)")
	}

	// Model "b" (same max 2, DIFFERENT key) must acquire immediately — its
	// counter is empty even though "a" holds 2. Fill b to its own cap of 2.
	slotsB := make([]*Slot, 0, 2)
	for i := 0; i < 2; i++ {
		accB, slot, err := p.Select(context.Background(), "b", 2)
		if err != nil {
			t.Fatalf("b select starved by same-max a traffic: %v (per-max-value grouping would block it)", err)
		}
		if accB.Name() != "acc-1" {
			t.Fatalf("b got %q, want acc-1", accB.Name())
		}
		slotsB = append(slotsB, slot)
	}
	// Account-wide total is the SUM of both keys (2 + 2), bounded by the
	// total cap 4.
	acc := p.AllAccounts()[0]
	if got := acc.InFlightCount(); got != 4 {
		t.Fatalf("total in-flight = %d, want 4 (2×a + 2×b)", got)
	}
	// A third key "c" must now park: the account total cap (4) is reached
	// even though c's own counter is empty — the aggregate bound holds.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if _, _, err := p.Select(ctx2, "c", 2); err == nil {
		t.Fatal("c must wait: account total cap reached (aggregate bound must hold)")
	}

	// Releasing an "a" slot must not touch b's counter.
	p.Release(slotsA[0])
	if got := acc.InFlightForKey("b"); got != 2 {
		t.Errorf("b in-flight after a release = %d, want 2 (keys must be independent)", got)
	}
	if got := acc.InFlightCount(); got != 3 {
		t.Errorf("total in-flight after a release = %d, want 3", got)
	}

	// Cleanup.
	for _, s := range slotsB {
		p.Release(s)
	}
	for _, s := range slotsA[1:] {
		p.Release(s)
	}
	if got := p.AllAccounts()[0].InFlightCount(); got != 0 {
		t.Fatalf("total in-flight after cleanup = %d, want 0", got)
	}
}

// TestDifferentMaxModelsIsolated: a low-max model must never be starved by
// high-max traffic on the same account — the two models own independent
// per-key counters (the old single shared counter starved the low-max
// class). With the total cap set to the sum (10+1), both classes coexist.
func TestDifferentMaxModelsIsolated(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
	}
	p := NewPoolWithTotalCap(cfgs, 11)

	// Fill the max=10 model "hi" to its cap.
	heldHi := make([]*Slot, 0, 10)
	for i := 0; i < 10; i++ {
		_, slot, err := p.Select(context.Background(), "hi", 10)
		if err != nil {
			t.Fatalf("occupy hi slot %d: %v", i, err)
		}
		heldHi = append(heldHi, slot)
	}
	if got := p.AllAccounts()[0].InFlightCount(); got != 10 {
		t.Fatalf("total in-flight = %d, want 10", got)
	}

	// The max=1 model "lo" must succeed IMMEDIATELY (its own counter is
	// empty) — 200ms context proves it did not park.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	accLo, slotLo, err := p.Select(ctx, "lo", 1)
	if err != nil {
		t.Fatalf("lo select starved by hi traffic: %v (per-key isolation broken)", err)
	}
	if accLo.Name() != "acc-1" {
		t.Fatalf("lo select got %q, want acc-1", accLo.Name())
	}
	// Total in-flight is the SUM of both keys (10 + 1).
	if got := accLo.InFlightCount(); got != 11 {
		t.Fatalf("total in-flight after mixed acquire = %d, want 11", got)
	}

	// Releasing a hi slot must not touch the lo counter.
	p.Release(heldHi[0])
	if got := accLo.InFlightForKey("lo"); got != 1 {
		t.Errorf("lo key after hi release = %d, want 1 (keys must be independent)", got)
	}

	// Cleanup.
	p.Release(slotLo)
	for _, s := range heldHi[1:] {
		p.Release(s)
	}
	if got := p.AllAccounts()[0].InFlightCount(); got != 0 {
		t.Fatalf("total in-flight after cleanup = %d, want 0", got)
	}
}

// slotResult carries the account and its lease out of a waiter goroutine.
type slotResult struct {
	acc  *Account
	slot *Slot
}

// waitForSlotResult waits up to timeout for a slotResult on ch (a failed
// select delivers a zero slotResult).
func waitForSlotResult(t *testing.T, ch <-chan slotResult, timeout time.Duration, what string) slotResult {
	t.Helper()
	select {
	case v := <-ch:
		if v.acc == nil || v.slot == nil {
			t.Fatalf("%s delivered no slot (select failed)", what)
		}
		return v
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for %s", what)
		return slotResult{}
	}
}

// expectNoSlotResult asserts that ch delivers nothing within wait.
func expectNoSlotResult(t *testing.T, ch <-chan slotResult, wait time.Duration, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s was woken unexpectedly with %q", what, v.acc.Name())
	case <-time.After(wait):
	}
}

// TestAccountTotalCapEnforced: the account-wide aggregate cap is explicit
// and finite — per-key counters alone would let N configured models stack
// N×max in-flight on one account. With totalCap=4, two full keys (2+2)
// exhaust the account; a third key parks even though its own counter is
// empty, and is served as soon as the total drops below the cap.
func TestAccountTotalCapEnforced(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
	}
	p := NewPoolWithTotalCap(cfgs, 4)

	// Two keys each fill 2 of the 4 total slots.
	slotsA := make([]*Slot, 0, 2)
	for i := 0; i < 2; i++ {
		_, slot, err := p.Select(context.Background(), "a", 2)
		if err != nil {
			t.Fatalf("occupy a slot %d: %v", i, err)
		}
		slotsA = append(slotsA, slot)
	}
	slotsB := make([]*Slot, 0, 2)
	for i := 0; i < 2; i++ {
		_, slot, err := p.Select(context.Background(), "b", 2)
		if err != nil {
			t.Fatalf("occupy b slot %d: %v", i, err)
		}
		slotsB = append(slotsB, slot)
	}

	// A waiter for key "c" parks: its own counter is empty, but the total
	// cap is reached — the waiter must NOT be woken by an unrelated
	// capacity event that does not lower the total.
	ch := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.Select(context.Background(), "c", 2)
		if err != nil {
			ch <- slotResult{}
			return
		}
		ch <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "c waiter to park")

	// Releasing a slot of key "a" lowers the total to 3 → the c waiter is
	// servable (its own counter empty, total 3 < 4) and must be woken.
	p.Release(slotsA[0])
	res := waitForSlotResult(t, ch, 2*time.Second, "c waiter after total drops below cap")
	if res.acc.Name() != "acc-1" {
		t.Fatalf("c waiter got %q, want acc-1", res.acc.Name())
	}
	if v := res.acc.InFlightCount(); v != 4 {
		t.Fatalf("total in-flight after c acquired = %d, want 4 (back at the cap)", v)
	}

	// Cleanup: the remaining a/b slots and the c slot.
	p.Release(res.slot)
	for _, s := range append(slotsB, slotsA[1:]...) {
		p.Release(s)
	}
	if got := p.AllAccounts()[0].InFlightCount(); got != 0 {
		t.Fatalf("total in-flight after cleanup = %d, want 0", got)
	}
}

// TestTotalCapPerAccountIndependent: the aggregate cap is applied to EACH
// account independently — two accounts can each hold totalCap in flight.
func TestTotalCapPerAccountIndependent(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
		{Name: "acc-2", Key: "key-2", BaseURL: "http://localhost:8002"},
	}
	p := NewPoolWithTotalCap(cfgs, 4)

	// Fill both accounts to their own cap of 4 (8 total across the pool).
	held := make([]*Slot, 0, 8)
	for i := 0; i < 8; i++ {
		_, slot, err := p.Select(context.Background(), "m", 4)
		if err != nil {
			t.Fatalf("occupy slot %d: %v", i, err)
		}
		held = append(held, slot)
	}
	for _, a := range p.AllAccounts() {
		if got := a.InFlightCount(); got != 4 {
			t.Errorf("account %s in-flight = %d, want 4 (per-account cap)", a.Name(), got)
		}
	}
	// The 9th must park (both accounts at their own cap).
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, _, err := p.Select(ctx, "m", 4); err == nil {
		t.Fatal("9th slot must wait (every account at its own total cap)")
	}
	for _, s := range held {
		p.Release(s)
	}
}

// TestMixedKeyGroupBoundStillEnforced: per-key isolation must not weaken the
// cap WITHIN a key — after one "lo" (max=1) slot is taken, a second "lo"
// request still parks (and fails on a short context) exactly like the
// ungrouped behavior, and succeeds once the first is released.
func TestMixedKeyGroupBoundStillEnforced(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
	}
	p := NewPool(cfgs)

	// Occupy the single "lo" slot while a "hi" (max=10) holder is also in
	// flight: the two keys coexist.
	_, holdHi, err := p.Select(context.Background(), "hi", 10)
	if err != nil {
		t.Fatalf("occupy hi slot: %v", err)
	}
	_, holdLo, err := p.Select(context.Background(), "lo", 1)
	if err != nil {
		t.Fatalf("occupy lo slot: %v", err)
	}

	// Second "lo" request must park (lo counter full) and time out on a
	// short context.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := p.Select(ctx, "lo", 1); err == nil {
		t.Fatal("second lo select must wait (per-key cap must still hold)")
	}

	// Release the lo slot: a new lo request succeeds immediately.
	p.Release(holdLo)
	_, slotLo2, err := p.Select(context.Background(), "lo", 1)
	if err != nil {
		t.Fatalf("lo select after release: %v", err)
	}
	p.Release(slotLo2)
	p.Release(holdHi)
	if got := p.AllAccounts()[0].InFlightCount(); got != 0 {
		t.Fatalf("total in-flight after cleanup = %d, want 0", got)
	}
}

// TestMixedKeyWakeupRespectsKeys: the wake path must apply the same per-key
// + total gate as TryAcquire. A "lo" waiter parks only when its OWN key is
// full; a release of the "hi" key must NOT wake it (its key still has no
// room), and releasing the "lo" holder wakes it.
func TestMixedKeyWakeupRespectsKeys(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
	}
	p := NewPool(cfgs)

	// Fill the "hi" key and take the single "lo" slot.
	heldHi := make([]*Slot, 0, 10)
	for i := 0; i < 10; i++ {
		_, slot, err := p.Select(context.Background(), "hi", 10)
		if err != nil {
			t.Fatalf("occupy hi slot %d: %v", i, err)
		}
		heldHi = append(heldHi, slot)
	}
	_, heldLo, err := p.Select(context.Background(), "lo", 1)
	if err != nil {
		t.Fatalf("occupy lo slot: %v", err)
	}

	// A lo waiter parks behind the full lo counter.
	ch := make(chan slotResult, 1)
	go func() {
		acc, slot, err := p.Select(context.Background(), "lo", 1)
		if err != nil {
			ch <- slotResult{}
			return
		}
		ch <- slotResult{acc: acc, slot: slot}
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "lo waiter to park")

	// Releasing a hi slot must NOT wake it (its own key is still full) —
	// key isolation in the wake path.
	p.Release(heldHi[0])
	expectNoSlotResult(t, ch, 200*time.Millisecond, "lo waiter after hi release")

	// Releasing the lo holder wakes it.
	p.Release(heldLo)
	res := waitForSlotResult(t, ch, 2*time.Second, "lo waiter after lo release")
	p.Release(res.slot)

	for _, s := range heldHi[1:] {
		p.Release(s)
	}
	if got := p.AllAccounts()[0].InFlightCount(); got != 0 {
		t.Fatalf("total in-flight after cleanup = %d, want 0", got)
	}
}

// TestSelectCancelNoSlotLeak: cancelling a parked Select must never leak a
// concurrency slot — after the cancel the account's in-flight returns to
// the pre-wait level and a subsequent Select succeeds.
func TestSelectCancelNoSlotLeak(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
	}
	p := NewPool(cfgs)

	// Occupy the only slot.
	_, held, err := p.Select(context.Background(), "m", 1)
	if err != nil {
		t.Fatalf("occupy slot: %v", err)
	}

	ctxCancel, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := p.Select(ctxCancel, "m", 1)
		done <- err
	}()
	waitUntil(t, func() bool { return p.WaitingCount() == 1 }, "waiter to park")
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel select err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for cancelled select")
	}
	if got := p.AllAccounts()[0].InFlightCount(); got != 1 {
		t.Fatalf("in-flight after cancelled wait = %d, want 1 (only the held slot)", got)
	}
	p.Release(held)
	if got := p.AllAccounts()[0].InFlightCount(); got != 0 {
		t.Fatalf("in-flight after release = %d, want 0", got)
	}
}

// TestRetryAcquireReleasePairing simulates the proxy retry loop: every
// attempt acquires its own lease and releases it before the next attempt —
// the accounting must return to zero after each release, and repeated
// acquire/release cycles must not drift (the old Release(max) API could
// corrupt a counter when the caller passed a wrong max; the lease API makes
// the pairing exact by construction).
func TestRetryAcquireReleasePairing(t *testing.T) {
	cfgs := []config.AccountConfig{
		{Name: "acc-1", Key: "key-1", BaseURL: "http://localhost:8001"},
	}
	p := NewPoolWithTotalCap(cfgs, 3)
	acc := p.AllAccounts()[0]

	// Interleave two keys like the retry loop would: acquire m1, release,
	// acquire m1 again — with the total cap binding at 3.
	for i := 0; i < 50; i++ {
		_, s1, err := p.Select(context.Background(), "m1", 2)
		if err != nil {
			t.Fatalf("iter %d select m1: %v", i, err)
		}
		if got := acc.InFlightCount(); got != 1 {
			t.Fatalf("iter %d in-flight after m1 acquire = %d, want 1", i, got)
		}
		p.Release(s1) // attempt failed → release before the next attempt
		if got := acc.InFlightCount(); got != 0 {
			t.Fatalf("iter %d in-flight after m1 release = %d, want 0 (slot must be freed on retry)", i, got)
		}
	}
	// Mixed keys: the total never exceeds the cap and always returns to 0.
	for i := 0; i < 25; i++ {
		_, sA, err := p.Select(context.Background(), "a", 2)
		if err != nil {
			t.Fatalf("iter %d select a: %v", i, err)
		}
		_, sB, err := p.Select(context.Background(), "b", 2)
		if err != nil {
			t.Fatalf("iter %d select b: %v", i, err)
		}
		_, sC, err := p.Select(context.Background(), "c", 2)
		if err != nil {
			t.Fatalf("iter %d select c: %v", i, err)
		}
		if got := acc.InFlightCount(); got != 3 {
			t.Fatalf("iter %d in-flight = %d, want 3 (at the total cap)", i, got)
		}
		// A fourth key must park (cap reached), like the retry loop waiting.
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, _, err = p.Select(ctx, "d", 2)
		cancel()
		if err == nil {
			t.Fatalf("iter %d: d must wait at the total cap", i)
		}
		p.Release(sA)
		p.Release(sB)
		p.Release(sC)
		if got := acc.InFlightCount(); got != 0 {
			t.Fatalf("iter %d in-flight after cleanup = %d, want 0", i, got)
		}
	}
	if got := acc.TotalRequests(); got != 50+25*3 {
		t.Fatalf("total requests = %d, want %d (every acquire counted exactly once)", got, int64(50+25*3))
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
