package cache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

// blockingModelsUpstream serves a /v1/models response only after release is
// closed; entered is closed as soon as the first request arrives and hits
// counts every request atomically. It is the deterministic rendezvous for
// the fetch wait/work separation tests below.
func blockingModelsUpstream(t *testing.T) (srv *httptest.Server, entered, release chan struct{}, hits *atomic.Int32) {
	t.Helper()
	entered = make(chan struct{})
	release = make(chan struct{})
	hits = new(atomic.Int32)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, entered, release, hits
}

// fetchCtxMC builds a ModelCache over a single healthy account pointing at
// srvURL, with no on-disk cache (every lookup is a miss → fetch) and no
// lifecycle context (workContext falls back to Background).
func fetchCtxMC(t *testing.T, srvURL string) *ModelCache {
	t.Helper()
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "a1", Provider: "p", BaseURL: srvURL, Key: "k"}}}
	return &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool:   pool.NewPool(cfg.Accounts),
		stop:   make(chan struct{}),
	}
}

// TestFetchWithContext_FollowerCancelReturnsImmediately pins the wait/work
// separation on the follower side: while a leader fetch is in flight, a
// FetchWithContext caller whose context is already cancelled returns
// context.Canceled immediately (never blocks until the leader finishes), and
// the leader's shared work continues to completion and publishes its result
// — the cancelled caller issues no upstream request of its own.
func TestFetchWithContext_FollowerCancelReturnsImmediately(t *testing.T) {
	srv, entered, release, hits := blockingModelsUpstream(t)
	mc := fetchCtxMC(t, srv.URL)

	// Leader: a plain Fetch on the shared work context, blocked upstream.
	leaderDone := make(chan error, 1)
	go func() { leaderDone <- mc.Fetch("p") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader fetch never reached the upstream")
	}

	// Follower with an already-cancelled context: must return immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := mc.FetchWithContext(ctx, "p")
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled follower: err = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("cancelled follower took %v, want immediate return (the wait must be bounded by the caller's context)", elapsed)
	}

	// Release the leader: its work continues untouched and publishes the
	// result despite the follower cancellation.
	close(release)
	select {
	case err := <-leaderDone:
		if err != nil {
			t.Fatalf("leader Fetch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("leader fetch never completed")
	}
	if models := mc.GetModels("p"); len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("leader result not published: %v", models)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (the cancelled follower must not issue its own fetch)", hits.Load())
	}
}

// TestFetchWithContext_LeaderWorkNotCancelledByRequestContext pins the
// wait/work separation on the LEADER side: a request that becomes the leader
// runs its upstream round on the SHARED work context, not on its own request
// context. Cancelling the request context mid-fetch therefore does NOT abort
// the work — the fetch completes, publishes the result, and the (cancelled)
// caller observes the completed fetch.
func TestFetchWithContext_LeaderWorkNotCancelledByRequestContext(t *testing.T) {
	srv, entered, release, _ := blockingModelsUpstream(t)
	mc := fetchCtxMC(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mc.FetchWithContext(ctx, "p") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request-triggered fetch never reached the upstream")
	}

	// The request goes away while its fetch is still running: the shared
	// work must NOT be aborted with it.
	cancel()
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FetchWithContext: err = %v, want nil (the work runs on the shared context, not the request's)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fetch never completed after the request context was cancelled")
	}
	if models := mc.GetModels("p"); len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("fetch result not published despite request cancellation: %v", models)
	}
}

// TestFetchWithContext_SharedResultAcrossCallers pins the merge across the
// two entry points: a Fetch (background fill) and a FetchWithContext
// (request) for the same provider collapse into ONE leader upstream round —
// the follower is PROVABLY parked on the leader's done channel before the
// leader is released (the inflight map is polled under fetchMu, like the
// leader's own registration) — and both callers observe the same result.
func TestFetchWithContext_SharedResultAcrossCallers(t *testing.T) {
	srv, entered, release, hits := blockingModelsUpstream(t)
	mc := fetchCtxMC(t, srv.URL)

	// The request-triggered call becomes the leader (work on the shared
	// context); the background Fetch joins it.
	reqDone := make(chan error, 1)
	go func() { reqDone <- mc.FetchWithContext(context.Background(), "p") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request-triggered fetch never reached the upstream")
	}
	fillDone := make(chan error, 1)
	go func() { fillDone <- mc.Fetch("p") }()
	// Provably wait until the follower is registered on the leader's entry
	// (the leader is still blocked at the upstream, so the entry cannot be
	// cleaned up yet), then release the leader.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mc.fetchMu.Lock()
		_, ok := mc.fetches["p"]
		mc.fetchMu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("in-flight fetch entry never registered")
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(release)

	for i, ch := range []chan error{reqDone, fillDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("caller %d: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("caller %d never completed", i)
		}
	}
	if models := mc.GetModels("p"); len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("models = %v, want [m1]", models)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1 (the follower must join the leader, not refetch)", hits.Load())
	}
}
