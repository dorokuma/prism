package cache

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

func TestCalculateBackoff(t *testing.T) {
	mc := &ModelCache{}
	mc.SetBackoffInitialForTest(30 * time.Second)

	tests := []struct {
		failCount int
		want      time.Duration
	}{
		{0, 0},
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 120 * time.Second},
		{4, 240 * time.Second},
		{5, 480 * time.Second},
		{6, 960 * time.Second},
		{7, 1920 * time.Second},
		{8, 1 * time.Hour}, // capped at 1h
		{9, 1 * time.Hour},
		{20, 1 * time.Hour},
	}

	for _, tt := range tests {
		got := mc.calculateBackoff(tt.failCount)
		if got != tt.want {
			t.Errorf("calculateBackoff(%d) = %v, want %v", tt.failCount, got, tt.want)
		}
	}
}

func TestJitterTimer(t *testing.T) {
	base := 3 * time.Hour
	for i := 0; i < 100; i++ {
		j := jitterTimer(base)
		minAllowed := time.Duration(float64(base) * 0.899)
		maxAllowed := time.Duration(float64(base) * 1.101)
		if j < minAllowed || j > maxAllowed {
			t.Fatalf("jitterTimer(%v) = %v, out of +/-10%% range [%v, %v]", base, j, minAllowed, maxAllowed)
		}
	}

	if got := jitterTimer(0); got != 0 {
		t.Fatalf("jitterTimer(0) = %v, want 0", got)
	}
}

func TestModelCache_BackoffAndManualBypass(t *testing.T) {
	var srvHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&srvHits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv.URL, Key: "k1"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv.URL, Key: "k1"},
		}),
		stop: make(chan struct{}),
	}
	mc.SetStaggerForTest(0)
	mc.SetBackoffInitialForTest(10 * time.Minute) // 10m initial backoff

	// 1. Initial stale/periodic refresh round fails and puts p1 into backoff
	mc.RefreshStale()
	if atomic.LoadInt32(&srvHits) != 1 {
		t.Fatalf("expected 1 hit, got %d", atomic.LoadInt32(&srvHits))
	}

	// 2. Snapshot shows in_backoff = true
	snap := mc.Snapshot()
	p1Snap, ok := snap["p1"]
	if !ok || !p1Snap.InBackoff {
		t.Fatalf("expected p1 in backoff, got %+v", snap)
	}

	// 3. Automated refresh is skipped because provider is in backoff
	mc.RefreshStale()
	if atomic.LoadInt32(&srvHits) != 1 {
		t.Fatalf("automated refresh should have been skipped in backoff, hits = %d", atomic.LoadInt32(&srvHits))
	}

	// 4. Manual refresh BYPASSES backoff
	mc.RefreshAll()
	if atomic.LoadInt32(&srvHits) != 2 {
		t.Fatalf("manual refresh must bypass backoff, hits = %d", atomic.LoadInt32(&srvHits))
	}
}

func TestModelCache_AcceptRefresh_SingleAndAll(t *testing.T) {
	var hits1, hits2 int32
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits1, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"p1"}]}`))
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits2, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m2","object":"model","created":1,"owned_by":"p2"}]}`))
	}))
	defer srv2.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k1"},
			{Name: "a2", Provider: "p2", BaseURL: srv2.URL, Key: "k2"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k1"},
			{Name: "a2", Provider: "p2", BaseURL: srv2.URL, Key: "k2"},
		}),
		stop: make(chan struct{}),
	}
	mc.SetStaggerForTest(0)

	// 1. AcceptRefresh for unknown provider returns error
	_, err := mc.AcceptRefresh("p_unknown", nil)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}

	// 2. AcceptRefresh for single provider p1
	doneCh := make(chan struct{})
	snaps, err := mc.AcceptRefresh("p1", func() {
		close(doneCh)
	})
	if err != nil {
		t.Fatalf("AcceptRefresh failed: %v", err)
	}
	if _, ok := snaps["p1"]; !ok {
		t.Fatalf("expected p1 in snapshots")
	}

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("onDone not called")
	}

	if atomic.LoadInt32(&hits1) != 1 || atomic.LoadInt32(&hits2) != 0 {
		t.Fatalf("expected only p1 to be fetched: hits1=%d, hits2=%d", atomic.LoadInt32(&hits1), atomic.LoadInt32(&hits2))
	}

	// 3. AcceptRefresh for all
	doneCh2 := make(chan struct{})
	_, err = mc.AcceptRefresh("", func() {
		close(doneCh2)
	})
	if err != nil {
		t.Fatalf("AcceptRefresh all failed: %v", err)
	}
	select {
	case <-doneCh2:
	case <-time.After(2 * time.Second):
		t.Fatal("onDone not called for all")
	}

	if atomic.LoadInt32(&hits1) != 2 || atomic.LoadInt32(&hits2) != 1 {
		t.Fatalf("expected both fetched: hits1=%d, hits2=%d", atomic.LoadInt32(&hits1), atomic.LoadInt32(&hits2))
	}
}

func TestModelCache_PendingUpgradeSemantics(t *testing.T) {
	// Tests pending state coalescing and upgrade:
	// Running Full, single P1 arriving -> pending P1. Then single P2 arriving -> upgrades pending to Full!
	blockSrv := make(chan struct{})
	var hits1, hits2 int32

	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits1, 1)
		<-blockSrv
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits2, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m2"}]}`))
	}))
	defer srv2.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k1"},
			{Name: "a2", Provider: "p2", BaseURL: srv2.URL, Key: "k2"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k1"},
			{Name: "a2", Provider: "p2", BaseURL: srv2.URL, Key: "k2"},
		}),
		stop: make(chan struct{}),
	}
	mc.SetStaggerForTest(0)

	// Start round 1 (p1 blocks)
	mc.RefreshOneAsync("p1", nil)

	// Wait until p1 is in flight
	for atomic.LoadInt32(&hits1) < 1 {
		time.Sleep(2 * time.Millisecond)
	}

	// While round 1 is running, request single refresh for p1, then for p2
	mc.RefreshOneAsync("p1", nil)
	// Check that pendingTarget is "p1"
	mc.refreshMu.Lock()
	if !mc.pending || mc.pendingTarget != "p1" {
		t.Fatalf("expected pendingTarget p1, got %q", mc.pendingTarget)
	}
	mc.refreshMu.Unlock()

	// Now request p2 with onDone -> must upgrade pendingTarget to "" (all providers)
	donePending := make(chan struct{})
	mc.RefreshOneAsync("p2", func() {
		close(donePending)
	})
	mc.refreshMu.Lock()
	if !mc.pending || mc.pendingTarget != "" {
		t.Fatalf("expected pendingTarget upgraded to all (''), got %q", mc.pendingTarget)
	}
	mc.refreshMu.Unlock()

	// Unblock round 1
	close(blockSrv)

	// Wait for handover and round 2 completion
	select {
	case <-donePending:
	case <-time.After(3 * time.Second):
		t.Fatal("pending upgraded round never completed")
	}

	mc.Stop()

	// hits1 should be 2 (round 1 + round 2 full), hits2 should be 1 (round 2 full)
	if atomic.LoadInt32(&hits1) != 2 || atomic.LoadInt32(&hits2) != 1 {
		t.Fatalf("expected hits1=2, hits2=1, got hits1=%d, hits2=%d", atomic.LoadInt32(&hits1), atomic.LoadInt32(&hits2))
	}
}

func TestModelCache_UpdateConfig_TriggersFillForNewProviders(t *testing.T) {
	var hits1, hits2 int32
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits1, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits2, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m2"}]}`))
	}))
	defer srv2.Close()

	cfgOld := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k1"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	p := pool.NewPool([]config.AccountConfig{
		{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k1"},
		{Name: "a2", Provider: "p2", BaseURL: srv2.URL, Key: "k2"},
	})
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfgOld,
		pool:   p,
		stop:   make(chan struct{}),
	}
	mc.SetStaggerForTest(0)

	// Fetch p1 initially
	if err := mc.Fetch("p1"); err != nil {
		t.Fatalf("Fetch p1 failed: %v", err)
	}
	if atomic.LoadInt32(&hits1) != 1 {
		t.Fatalf("hits1 = %d, want 1", atomic.LoadInt32(&hits1))
	}

	// UpdateConfig with new provider p2
	cfgNew := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k1"},
			{Name: "a2", Provider: "p2", BaseURL: srv2.URL, Key: "k2"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}

	mc.UpdateConfig(cfgNew)

	// Wait for background fill to complete
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&hits2) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("p2 fill was not triggered by UpdateConfig")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// p1 already had cache, so fill should NOT have fetched p1 again
	if atomic.LoadInt32(&hits1) != 1 {
		t.Fatalf("p1 should not have been refetched by fill: hits1=%d", atomic.LoadInt32(&hits1))
	}
}
