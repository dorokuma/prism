package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

// ErrNoHealthyAccount is returned by Fetch when the provider has no healthy
// account to fetch from. internal/proxy maps it to HTTP 503 with code
// no_healthy on the /v1/models cache-miss path.
var ErrNoHealthyAccount = errors.New("no healthy account for provider")

// ErrFetchSaturated is returned by Fetch when the provider's account is at
// its concurrency cap. internal/proxy maps it to HTTP 503 with code
// model_fetch_saturated on the /v1/models cache-miss path.
var ErrFetchSaturated = errors.New("model cache fetch saturated")

// ErrFetchAborted is returned to same-provider followers when the in-flight
// leader fetch exits abnormally (panic / goroutine exit) before publishing a
// result. It is deliberately generic: the abnormal-exit value is never
// propagated to followers, because it may embed credentials or internal
// state. The abnormal exit itself is NOT recovered — it keeps unwinding up
// the leader's stack; fetchWithContext only guarantees the cleanup.
var ErrFetchAborted = errors.New("model fetch leader aborted")

// ModelCache manages per-provider model list caches persisted to disk.
type ModelCache struct {
	dir    string
	caches map[string]*providerCache
	mu     sync.RWMutex
	pool   *pool.Pool
	cfg    *config.Config
	stop   chan struct{}

	// refreshMu guards ALL background-refresh scheduling state below.
	// Every background refresh — the manual SIGHUP round
	// (RefreshAllAsync), the startup fill (FetchAllAsync), and the 24h
	// stale ticker (StartRefreshLoop) — runs through this single
	// scheduler, so at most one round is ever in flight: a stale round and
	// a manual round can never refresh the same provider concurrently, and
	// Stop can cancel (through the shared lifecycle context) and wait
	// (refreshWG) for every goroutine the scheduler has spawned.
	//
	// The Add/Wait contract: every refreshWG.Add happens under refreshMu
	// (startRoundLocked and the runRefresh handover), and Stop's Wait is
	// ordered after its own refreshMu acquisition, so a counter of zero
	// implies no round is live or scheduled. Once stopped is set no new
	// round can start, so Wait can never race a future Add.
	refreshMu       sync.Mutex
	refreshing      bool
	pending         bool
	pendingKind     roundKind
	pendingOnDone   func()
	stopped         bool
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	refreshWG       sync.WaitGroup
	stopOnce        sync.Once

	// doneMu serializes onDone callbacks: at most one callback runs at a
	// time, and a later round's callback never starts before an earlier
	// round's callback has finished. Callbacks run on their own goroutine
	// (runOnDone), OUTSIDE refreshWG but INSIDE doneWG: Stop waits for the
	// refresh rounds (refreshWG) and the loop (loopWG) first, then for
	// every launched callback (doneWG) — after Stop returns no callback
	// (and therefore no models.json write) is still running. The contract
	// is that onDone must NOT call Stop synchronously (production callbacks
	// are pure SyncTools); a violating callback would deadlock Stop's
	// doneWG.Wait on itself — the contract, not the structure, is the
	// guarantee, and the async-Stop test pins the intended shape.
	doneMu sync.Mutex

	// doneWG tracks onDone callbacks that have been launched. Stop waits
	// for it after refreshWG/loopWG so no callback outlives Stop. The Add
	// happens synchronously inside runRefresh under refreshMu, and Stop's
	// Wait is ordered after its own refreshMu acquisition + refreshWG.Wait,
	// so the counter can never grow after doneWG.Wait starts.
	doneWG sync.WaitGroup

	// chownFile preserves uid/gid on the models.json temp file before the
	// atomic rename. It is a field so tests can inject a deterministic
	// failure (a non-root process gets EPERM) instead of depending on the
	// test runner's privileges. nil means os.Chown. A chown failure ABORTS
	// the sync with an error: renaming would silently change the deployed
	// file's owner, and there is deliberately no in-place fallback (a
	// non-atomic overwrite could leave a half-written file on a crash and
	// would hide the ownership misconfiguration instead of surfacing it).
	chownFile func(name string, uid, gid int) error

	// toolsMu serializes SyncTools callers (the onDone/onRefresh callbacks
	// wired in cmd/prism). Combined with the atomic temp-file+chmod+
	// close+rename write in syncPIModelsJSON this guarantees two SyncTools
	// calls can never interleave writes into a corrupt models.json, even if
	// a future caller skips the scheduler entirely.
	toolsMu sync.Mutex

	// fetchMu guards the per-provider inflight fetch map (fetches). It is
	// held only for map registration/lookup/removal, NEVER across the wait
	// for a shared fetch: followers park on the leader's done channel
	// outside the lock, so a slow upstream can never serialize different
	// providers (or the cache lock) behind the merge.
	fetchMu sync.Mutex

	// fetches tracks the in-flight upstream model fetch per provider (see
	// fetchWithContext): at most one leader per provider, all concurrent
	// same-provider callers share its result. Lazily initialized on first
	// use (tests construct ModelCache literals without New).
	fetches map[string]*inflightFetch

	// fetchLeaderHook is a test-only injection point called at the top of
	// fetchLeader, before any upstream work. Tests use it to force an
	// abnormal leader exit (panic) so the inflight-entry cleanup in
	// fetchWithContext can be pinned deterministically; nil means no hook
	// (production).
	fetchLeaderHook func()

	// fetchBudget overrides the ONE total failover timeout applied to a
	// whole provider fetch round (see fetchLeader). 0 means the production
	// default of 30s. Tests inject a short budget to pin the shared-budget
	// failover behavior without real 30-second waits.
	fetchBudget time.Duration

	// loopWG tracks the StartRefreshLoop goroutine: Stop waits for it, so
	// no ticker goroutine outlives Stop. Add happens under refreshMu (and
	// is refused after stopped), ordered before Stop's loopWG.Wait.
	loopWG sync.WaitGroup
}

type providerCache struct {
	Models    []ModelEntry         `json:"models"`
	Meta      map[string]ModelMeta `json:"meta,omitempty"`
	UpdatedAt time.Time            `json:"updated_at"`
}

// ModelMeta holds metadata pulled from a provider's upstream endpoint
// (e.g. ollama /api/show). It is persisted in the on-disk cache under the
// "meta" key (omitempty + nil keeps the cache backward compatible with the
// old format that had no such key).
type ModelMeta struct {
	ContextWindow *int     `json:"context_window,omitempty"`
	MaxTokens     *int     `json:"max_tokens,omitempty"`
	Reasoning     *bool    `json:"reasoning,omitempty"`
	Input         []string `json:"input,omitempty"`
}

// ModelEntry represents a single model from /v1/models response.
type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// upstreamModelsResponse is the raw response from GET /v1/models.
type upstreamModelsResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

// New creates a ModelCache with the given cache directory, pool, and config.
// The cache directory is created if it doesn't exist.
func New(dir string, p *pool.Pool, cfg *config.Config) (*ModelCache, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &ModelCache{
		dir:    dir,
		caches: make(map[string]*providerCache),
		pool:   p,
		cfg:    cfg,
		stop:   make(chan struct{}),
	}, nil
}

// providerCacheFileName validates a provider name for use as a cache file
// name and returns "<provider>.json". It is the write-path defense behind
// config load validation (LoadConfig already rejects path separators, "." /
// ".." and absolute provider names): a programmatically built ModelCache
// (tests, future callers) can still never read or write outside the cache
// dir. An invalid name yields a non-nil error and no file name.
func providerCacheFileName(provider string) (string, error) {
	if provider == "" {
		return "", fmt.Errorf("model cache: empty provider name")
	}
	if strings.ContainsAny(provider, `\/`) || strings.ContainsRune(provider, 0) {
		return "", fmt.Errorf("model cache: provider name %q must not contain path separators", provider)
	}
	if provider == "." || provider == ".." || filepath.IsAbs(provider) {
		return "", fmt.Errorf("model cache: provider name %q is not a valid cache file name", provider)
	}
	name := provider + ".json"
	if filepath.Base(name) != name {
		return "", fmt.Errorf("model cache: provider name %q escapes the cache directory", provider)
	}
	return name, nil
}

// filePath returns the path to the cache file for a provider. The provider
// name is validated (see providerCacheFileName): an invalid name yields a
// non-nil error, so callers fail instead of reading or writing outside the
// cache dir.
func (mc *ModelCache) filePath(provider string) (string, error) {
	name, err := providerCacheFileName(provider)
	if err != nil {
		return "", err
	}
	return filepath.Join(mc.dir, name), nil
}

// providerSkipPISync reports whether a provider is excluded from
// prism-managed pi models.json sync. A provider is skipped when any of its
// accounts sets skip_pi_sync=true: its pi metadata (e.g.
// agentrouter-anthropic with api: anthropic-messages) is hand-maintained and
// must not be overwritten by prism. The flag does NOT affect upstream model
// fetching (the model cache fetches like any other provider).
func providerSkipPISync(cfg *config.Config, provider string) bool {
	if cfg == nil {
		return false
	}
	for _, acc := range cfg.Accounts {
		if acc.Provider == provider && acc.SkipPISync {
			return true
		}
	}
	return false
}

// LoadFromDisk reads all cached provider model lists from disk.
// Missing files are silently skipped. Each successfully loaded provider
// cache file is re-tightened to 0600 (owner-only): an existing cache file
// written by an older prism version (or created under a wide umask) must
// not stay world-readable until the next fetch rename — the cache may
// embed upstream metadata that is not meant to be world-readable. Only
// prism's OWN provider cache files are touched, never pi's models.json.
// A tightening failure is logged loudly but does not abort the load (the
// file stays usable; the next fetch rewrite secures it).
func (mc *ModelCache) LoadFromDisk() {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	for _, acc := range mc.cfg.Accounts {
		provider := acc.Provider
		if provider == "" {
			continue
		}
		fp, err := mc.filePath(provider)
		if err != nil {
			slog.Warn("cache file path rejected, skipping provider", "provider", provider, "error", err)
			continue
		}
		data, err := os.ReadFile(fp)
		if err != nil {
			slog.Debug("cache file not found, will fetch async", "provider", provider, "path", fp)
			continue
		}
		var pc providerCache
		if err := json.Unmarshal(data, &pc); err != nil {
			slog.Warn("cache file corrupt, will fetch async", "provider", provider, "error", err)
			continue
		}
		// The file is adopted into the running cache: tighten it now (see
		// the doc comment). Failure is loud, never silent, but does not
		// discard the cached data.
		if err := os.Chmod(fp, 0600); err != nil {
			slog.Error("model cache: cannot tighten provider cache file to 0600", "provider", provider, "path", fp, "error", err)
		}
		mc.caches[provider] = &pc
		slog.Info("model cache loaded from disk", "provider", provider, "models", len(pc.Models), "updated", pc.UpdatedAt.Format(time.RFC3339))
	}
}

// GetModels returns the cached model list for a provider.
// Returns nil if not cached.
func (mc *ModelCache) GetModels(provider string) []ModelEntry {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	pc := mc.caches[provider]
	if pc == nil {
		return nil
	}
	return pc.Models
}

// snapshotConfig returns the current config under the cache lock. All
// background code must read mc.cfg through this (or another lock-held read):
// UpdateConfig (SIGHUP) can swap the config at any time, and an unlocked
// read would race it.
func (mc *ModelCache) snapshotConfig() *config.Config {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return mc.cfg
}

// roundKind selects which providers a background refresh round fetches.
type roundKind int

const (
	// roundManual refreshes every provider, ignoring cache state (the
	// SIGHUP path, RefreshAllAsync).
	roundManual roundKind = iota
	// roundStale refreshes only providers whose cache is missing or older
	// than 24h (the StartRefreshLoop ticker, RefreshStale).
	roundStale
	// roundFill refreshes only providers with no cached models, plus
	// ollama providers whose cache predates the Meta format (the startup
	// FetchAllAsync path).
	roundFill
)

// shouldFetch reports whether provider's cache (pc, may be nil) must be
// refreshed by a round of the given kind.
func (mc *ModelCache) shouldFetch(provider string, pc *providerCache, kind roundKind, cfg *config.Config) bool {
	switch kind {
	case roundManual:
		return true
	case roundFill:
		if pc == nil {
			return true
		}
		// Self-heal: old cache files (pre-Meta) have Models but nil/empty
		// Meta. Force a fetch so /api/show runs and populates Meta, without
		// needing a manual SIGHUP. New caches (Meta present) are untouched.
		return cfg != nil && cfg.EffortSchema(provider) == "ollama" && len(pc.Models) > 0 && (pc.Meta == nil || len(pc.Meta) == 0)
	case roundStale:
		if pc == nil {
			return true
		}
		return !pc.UpdatedAt.After(time.Now().Add(-24 * time.Hour))
	}
	return false
}

// FetchAllAsync fetches model lists from upstream for any providers missing
// cache. It is the startup path: the caller never blocks. It runs through
// the same single scheduler as the manual and stale refreshes, so it can
// never refresh a provider concurrently with another round. If a round is
// already in flight the missing-cache work is subsumed by it (every round
// fetches at least the missing providers) and the request is remembered as
// a PENDING fill round: when the running round finishes, exactly one more
// fill round runs and onDone (may be nil) fires once it completes — the
// startup tools sync is never silently dropped just because a refresh
// happened to be in flight. Like RefreshAllAsync's pending slot, the latest
// caller wins (a flag, not a queue: no buildup), and onDone never runs
// after Stop.
func (mc *ModelCache) FetchAllAsync(onDone func()) {
	mc.refreshMu.Lock()
	defer mc.refreshMu.Unlock()
	if mc.stopped {
		return
	}
	if mc.refreshing {
		// The fill work is subsumed by the running round, but the caller's
		// onDone must not be dropped: hand over to exactly one more fill
		// round when the running round finishes (or drop it if that round
		// is cancelled/stopped, as with every pending request).
		//
		// Priority rule: a pending MANUAL round (SIGHUP) refreshes every
		// provider and fully subsumes the fill work, so it must never be
		// downgraded to fill here — downgrading would silently drop
		// providers from the next round. Keep the manual kind; only the
		// onDone follows the latest caller.
		//
		// In every other case the queued round must be a FILL. Note that
		// pendingKind alone cannot decide: when NO request is pending yet,
		// pendingKind still reads roundManual (the default that
		// startRoundLocked/RefreshStale leave behind even though nothing
		// is queued). Treating that default as manual would escalate a
		// startup fill into a full manual refresh, re-fetching every
		// provider instead of only the missing caches. The pending flag —
		// not pendingKind — is what distinguishes "manual pending" from
		// "nothing pending"; an already-pending fill stays a fill
		// (latest caller wins, as before).
		if !mc.pending || mc.pendingKind != roundManual {
			mc.pendingKind = roundFill
		}
		mc.pending = true
		mc.pendingOnDone = onDone
		slog.Debug("model cache fill already in progress, queueing pending fill")
		return
	}
	mc.startRoundLocked(roundFill, onDone)
}

// Fetch calls the upstream /v1/models endpoint for a provider, caches
// the result to disk, and updates the in-memory cache. Public behavior is
// unchanged: it uses a background context with the usual 30s request
// timeout. When the shared background lifecycle context already exists it
// is used instead, so a Fetch that overlaps Stop() is aborted like every
// other refresh (no request is sent and no cache file is written after
// shutdown began). The SIGHUP background refresh goes through
// RefreshAllAsync, which drives fetchWithContext with the cancellable
// context directly.
func (mc *ModelCache) Fetch(provider string) error {
	ctx := context.Background()
	mc.refreshMu.Lock()
	if mc.lifecycleCtx != nil {
		ctx = mc.lifecycleCtx
	}
	mc.refreshMu.Unlock()
	return mc.fetchWithContext(ctx, provider)
}

// FetchWithContext is Fetch with a caller-supplied context that bounds ONLY
// the wait for the result, never the shared fetch work: the request's
// context (e.g. an HTTP request context from /v1/models) is used to wait —
// a cancelled caller returns immediately — while the leader's upstream round
// runs on the shared work context (the background lifecycle context, or a
// plain Background when none exists yet), exactly like Fetch. A cancelled
// request therefore never aborts a fetch that concurrent requests (or a
// background fill) are sharing, and the caller can distinguish "the wait was
// cancelled" (context error) from "the fetch failed" (upstream error). The
// proxy maps the context error to "answer nothing" instead of writing an
// empty model list to a client that is gone.
func (mc *ModelCache) FetchWithContext(ctx context.Context, provider string) error {
	return mc.fetchWait(ctx, mc.workContext(), provider)
}

// workContext returns the shared context for model-fetch WORK (the leader's
// upstream round): the shared background lifecycle context when it exists
// (so Stop aborts an in-flight request-triggered fetch like every other
// refresh), otherwise a plain Background context. The caller's own context
// never drives the work — only the wait (see FetchWithContext).
func (mc *ModelCache) workContext() context.Context {
	mc.refreshMu.Lock()
	defer mc.refreshMu.Unlock()
	if mc.lifecycleCtx != nil {
		return mc.lifecycleCtx
	}
	return context.Background()
}

// fetchWithContext is fetchWait with a single context for both the wait and
// the work: internal callers (Fetch, the refresh rounds in runRefresh) drive
// the shared fetch with the shared lifecycle context, where cancelling the
// wait must also cancel the work.
func (mc *ModelCache) fetchWithContext(ctx context.Context, provider string) error {
	return mc.fetchWait(ctx, ctx, provider)
}

// fetchWait is Fetch with explicit wait/work contexts (see FetchWithContext
// and fetchWithContext for the two splittings). It snapshots mc.cfg
// once (no race with UpdateConfig) and merges concurrent same-provider
// fetches: at most one leader per provider performs the upstream round, and
// every concurrent caller for the same provider — /v1/models cache misses,
// background refresh rounds, manual RefreshAll — shares the leader's result
// or error instead of issuing its own upstream call (see inflightFetch).
// Different providers always run in parallel: the merge lock is never held
// across the wait.
//
// waitCtx bounds the caller's wait: a follower parks on the leader's done
// channel and returns ctx.Err() as soon as waitCtx is cancelled — the
// leader is untouched. workCtx drives the leader's upstream round
// (fetchLeader); a follower cancellation never cancels the leader, which
// only observes workCtx (the shared lifecycle/Background context, or the
// lifecycle context for internal refresh rounds).
//
// The leader's upstream round is fetchLeader: multi-account failover across
// all healthy, non-cooldown accounts of the provider, sharing ONE 30-second
// total context budget. Every concurrency slot acquired by the round is
// released exactly once through the pool (mc.pool.Release, never the bare
// account.Release): the pool wakeup is what lets a provider waiter parked in
// SelectByProvider proceed with the freed slot — releasing the slot without
// the wakeup would strand waiters until some business request happened to
// release the same account.
func (mc *ModelCache) fetchWait(waitCtx, workCtx context.Context, provider string) (err error) {
	// Join an in-flight same-provider fetch when one exists. A follower
	// waits on the leader's done channel OUTSIDE fetchMu (the lock never
	// spans the wait) and can cancel its OWN wait independently: a follower
	// cancellation returns immediately and never cancels the leader, which
	// only observes its own work context.
	mc.fetchMu.Lock()
	if f, ok := mc.fetches[provider]; ok {
		mc.fetchMu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}
	if mc.fetches == nil {
		mc.fetches = make(map[string]*inflightFetch)
	}
	f := &inflightFetch{done: make(chan struct{})}
	mc.fetches[provider] = f
	mc.fetchMu.Unlock()

	// Leader lifecycle state machine (deferred, so it runs on EVERY exit
	// path — the normal return AND panic/Goexit unwinding): remove the map
	// entry, publish the final error, and close(done). A leader that dies
	// abnormally must never leave followers parked on done forever (their
	// own context may be Background, so they would wait forever) or leave a
	// dead entry that makes the next same-provider Fetch join a fetch that
	// will never finish. On the abnormal path the published error is a fixed
	// safe sentinel (ErrFetchAborted): the panic value is deliberately NOT
	// captured, logged or propagated to followers — it may embed
	// credentials or internal state. The abnormal exit itself is NOT
	// swallowed: this is cleanup only, the panic/Goexit keeps unwinding up
	// the leader's stack.
	//
	// The published error is the leader's FINAL outcome on EVERY path:
	// normal success publishes nil, a normal failure publishes the real
	// upstream error UNMODIFIED (never ErrFetchAborted — the proxy
	// classifies 502/503 from it), a context-cancelled leader publishes
	// ctx.Err, and only an abnormal exit is replaced by ErrFetchAborted.
	// f.err is written BEFORE close(done), so every follower that receives
	// from done observes the final value — never a zero-value nil after a
	// failed leader (a nil follower error would make the proxy answer 200
	// with an empty model list).
	//
	// The entry is removed under fetchMu BEFORE close(done): a caller
	// arriving after the removal starts its own round (correct — no fetch
	// is live), while a caller registered before it still receives the
	// result. The in-memory cache is already updated by fetchLeader before
	// it returns, so followers unblocked by close(done) observe the fresh
	// cache.
	normalExit := false
	defer func() {
		mc.fetchMu.Lock()
		delete(mc.fetches, provider)
		mc.fetchMu.Unlock()
		if !normalExit {
			slog.Error("model cache fetch leader exited abnormally; in-flight fetch entry cleaned up", "provider", provider)
			err = fmt.Errorf("%w: provider %q", ErrFetchAborted, provider)
		}
		// Publish the leader's final outcome on every exit path (normal
		// success, normal failure, context cancel, abnormal exit) before
		// close(done), so a follower that receives from done always reads
		// the final error.
		f.err = err
		close(f.done)
	}()

	err = mc.fetchLeader(workCtx, provider)
	normalExit = true
	return err
}

// inflightFetch tracks one in-flight upstream model fetch for a single
// provider. The leader performs the upstream work and publishes the outcome
// by closing done (err is written before the close, so every follower that
// receives from done observes the final value).
type inflightFetch struct {
	done chan struct{}
	err  error
}

// fetchLeader runs the upstream model fetch for one provider with
// multi-account failover:
//   - candidates are every healthy, non-cooldown account of the provider, in
//     pool order;
//   - each candidate is TryAcquire'd at config.ResolveFetchConcurrency
//     (wildcard "*" if configured, otherwise the smallest positive per-model
//     value, otherwise the built-in default) — a saturated candidate is
//     skipped and the next candidate is tried;
//   - network / 5xx / auth / parse failures also move to the next candidate;
//   - no healthy candidate → ErrNoHealthyAccount;
//   - EVERY candidate saturated (none could be acquired, at least one
//     healthy candidate exists) → ErrFetchSaturated;
//   - otherwise (at least one account WAS acquired and every attempt
//     failed) the last attempt's error is returned — NEVER ErrFetchSaturated:
//     a fetch that actually reached an upstream and failed (network / 5xx /
//     parse / write) must surface that failure (proxy maps it to 502
//     model_fetch_failed), not a misleading "saturated" that would make the
//     client think the provider is merely busy.
//
// The whole failover shares ONE total context budget (30s by default,
// overridable via mc.fetchBudget for tests): the timeout wraps the attempt
// loop, so N accounts never multiply into N×30s — the budget spent by an
// earlier account is simply unavailable to later accounts. Every successful
// Acquire is released exactly once via mc.pool.Release; the release is
// deferred so a panic inside fetchFromAccount can never leak the concurrency
// slot (deferred calls run during panic unwinding).
func (mc *ModelCache) fetchLeader(ctx context.Context, provider string) error {
	if mc.fetchLeaderHook != nil {
		mc.fetchLeaderHook()
	}
	cfg := mc.snapshotConfig()
	accounts := mc.selectAccounts(provider)
	if len(accounts) == 0 {
		return fmt.Errorf("%w %q", ErrNoHealthyAccount, provider)
	}

	// The budget is created ONCE for the whole failover loop and shared by
	// every account attempt (never one timeout per account). 0 means the
	// default 30s; tests inject a short budget via mc.fetchBudget.
	budget := mc.fetchBudget
	if budget <= 0 {
		budget = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	maxConcurrent := config.ResolveFetchConcurrency(cfg)
	var lastErr error      // last real attempt error (acquire succeeded, fetch failed)
	var saturatedErr error // last saturated-candidate error (only returned when NOTHING was acquired)
	acquired := false
	for _, account := range accounts {
		// The fetch is not tied to a single business model: it acquires a
		// slot under the empty concurrency key (""), so it never borrows or
		// blocks any per-model counter; the account-wide total cap still
		// bounds it like any other request. The returned *Slot is the only
		// valid Release argument (it carries the exact account/key/max).
		slot := account.TryAcquire("", maxConcurrent)
		if slot == nil {
			saturatedErr = fmt.Errorf("%w: provider %q account %s saturated (%d in flight)", ErrFetchSaturated, provider, account.Name(), account.InFlightCount())
			continue
		}
		acquired = true
		err := func() error {
			// Deferred release: covers every normal error path AND a panic
			// inside fetchFromAccount (defer runs during unwinding), so a
			// concurrency slot can never leak out of the fetch round.
			defer mc.pool.Release(slot)
			return mc.fetchFromAccount(ctx, account, provider, cfg)
		}()
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			// Cancelled (Stop / shared budget exhausted): failover must not
			// continue — the budget is spent or shutdown began.
			return err
		}
	}
	if !acquired {
		if saturatedErr != nil {
			return saturatedErr
		}
		return fmt.Errorf("%w %q", ErrNoHealthyAccount, provider)
	}
	return lastErr
}

// fetchFromAccount performs the complete upstream round for ONE account:
// GET /v1/models with account headers and credential, redacted non-200
// handling, bounded success read, parse, the ollama /api/show metadata
// fan-out, and the atomic cache write (temp file + rename; a cancelled
// fetch never publishes a cache file and never leaves temp litter). It
// returns an error on any failure so the failover loop (fetchLeader) can
// try the next account. The caller owns the concurrency slot lifecycle
// (TryAcquire before, defer mc.pool.Release after — the deferred release
// also covers a panic inside this function).
//
// Transport errors are returned SAFE for logging: the raw *url.Error is
// never included because it embeds the full upstream URL — query parameters
// and any credentials in the base URL — which must not reach logs or
// client-visible error text; only a short classification (upstream_timeout /
// upstream_refused / ...) is kept (util.ClassifyConnError).
func (mc *ModelCache) fetchFromAccount(ctx context.Context, account *pool.Account, provider string, cfg *config.Config) error {
	// Resolve and validate the cache file path up front (write-path defense
	// for path traversal): an invalid provider name fails the fetch BEFORE
	// any upstream work and never reaches os.Rename with an escaping path.
	cachePath, err := mc.filePath(provider)
	if err != nil {
		return err
	}
	url := util.JoinURLPath(account.BaseURL(), "/v1/models")
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request failed: %s", util.ClassifyConnError(err))
	}
	// Header semantics identical to doUpstreamRequest: account-level headers
	// (Set, override same-named defaults) → account credential (Bearer, or
	// the custom auth_header) → Content-Type default only when unset.
	// Gateways that authenticate on client identity headers (e.g.
	// Originator/x-app) reject fetches that only carry Authorization, so the
	// account headers must be applied here too.
	pool.ApplyAccountHeaders(req.Header, account)
	pool.ApplyAuthHeader(req.Header, account)
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := account.Client().Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %s", util.ClassifyConnError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// Redact the body (including this account's key as a literal) before
		// it enters the error/log path: an upstream may echo the credential
		// it received (or other secrets) back in the error payload, and the
		// key may not look like an sk-/Bearer token (custom auth_header).
		return fmt.Errorf("upstream returned %d: %s", resp.StatusCode, util.RedactBodyBytesWithKeys(body, []string{account.Key()}))
	}

	// Bounded success read: the /v1/models body is capped by the current
	// max_upstream_response_bytes config (default 32 MiB) like upstream chat
	// responses. An invalid cap is rejected by ReadBodyLimited — never an
	// unbounded read.
	raw, err := util.ReadBodyLimited(resp.Body, fetchResponseCap(cfg))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	var upstream upstreamModelsResponse
	if err := json.Unmarshal(raw, &upstream); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	pc := &providerCache{
		Models:    upstream.Data,
		Meta:      mc.fetchOllamaMeta(ctx, account, upstream.Data, cfg), // ollama only; non-ollama returns nil
		UpdatedAt: time.Now(),
	}
	// A refresh cancelled by Stop() must not persist a half-observed result:
	// the HTTP request may have completed just as the cancellation landed.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	data, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	// Final cancellation check immediately before the disk write: the
	// marshal above takes time, and a fetch whose context was cancelled
	// (Stop) must not write a cache file at all.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Atomic cache write: the data goes to a temp file in the cache dir
	// (same filesystem, so the rename is atomic) and is renamed over the
	// final path only after a final cancellation check, so a concurrent
	// reader never observes a partial file and a cancelled fetch (Stop)
	// never publishes a cache file. The temp file is removed on every
	// failure/cancellation path; the final file is 0600 (owner-only): the
	// cache may embed upstream metadata and model ids that are not meant
	// to be world-readable. Repeated runs are idempotent: the rename
	// atomically replaces any previous cache file.
	tmp, err := os.CreateTemp(mc.dir, provider+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp cache file: %w", err)
	}
	tmpName := tmp.Name()
	abort := func(cause error) error {
		tmp.Close()
		os.Remove(tmpName)
		return cause
	}
	if _, err := tmp.Write(data); err != nil {
		return abort(fmt.Errorf("write temp cache file: %w", err))
	}
	if err := tmp.Chmod(0600); err != nil {
		return abort(fmt.Errorf("chmod temp cache file: %w", err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp cache file: %w", err)
	}
	// Final cancellation check immediately before the rename: a fetch
	// cancelled (Stop) between the write and the rename must not publish
	// the cache file; the temp file is cleaned up instead.
	if ctx.Err() != nil {
		os.Remove(tmpName)
		return ctx.Err()
	}
	if err := os.Rename(tmpName, cachePath); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename cache file: %w", err)
	}

	mc.mu.Lock()
	mc.caches[provider] = pc
	mc.mu.Unlock()

	slog.Info("model cache fetched", "provider", provider, "models", len(pc.Models))
	return nil
}

// fetchResponseCap returns the cap for model-cache success-body reads: the
// configured max_upstream_response_bytes when positive, otherwise the 32 MiB
// default (LoadConfig maps 0/absent to the default and rejects negatives). A
// non-positive programmatic value falls back to the default here; the
// ReadBodyLimited invalid-cap rejection is the backstop for direct callers.
func fetchResponseCap(cfg *config.Config) int64 {
	if cfg != nil && cfg.MaxUpstreamResponseBytes > 0 {
		return cfg.MaxUpstreamResponseBytes
	}
	return config.MaxUpstreamResponseBytesDefault
}

// RefreshStale fetches model lists for any providers whose cache is missing
// or older than 24h. It runs synchronously on the caller's goroutine through
// the same cancellable scheduler as every other background refresh: the
// per-provider fetches use the shared lifecycle context (Stop aborts them)
// and the round is tracked in the refresh WaitGroup, so Stop cannot return
// while it is still running. If another refresh round is already in flight
// the stale work is subsumed (every round fetches at least the missing
// providers) and this returns immediately; if Stop has begun it is a no-op.
func (mc *ModelCache) RefreshStale() {
	mc.refreshMu.Lock()
	if mc.stopped || mc.refreshing {
		mc.refreshMu.Unlock()
		return
	}
	mc.refreshing = true
	mc.pending = false
	mc.pendingKind = roundManual
	mc.pendingOnDone = nil
	ctx := mc.ensureLifecycleLocked()
	// Add under refreshMu: Stop takes refreshMu before Wait, so the counter
	// is guaranteed to be >= 1 by the time Stop waits.
	mc.refreshWG.Add(1)
	mc.refreshMu.Unlock()
	// Run the round synchronously. refreshing stays true for the whole
	// pass, so a concurrent RefreshAllAsync/FetchAllAsync queues pending
	// and is handed over to exactly one more round when this one finishes.
	mc.runRefresh(ctx, roundStale, nil)
}

// RefreshAll fetches model lists for all providers, ignoring cache state.
// It runs SYNCHRONOUSLY on the caller's goroutine through the same
// scheduler as every other background refresh — it must never bypass it:
// the round is tracked in the refresh WaitGroup, runs on the shared
// cancellable lifecycle context (Stop aborts it and waits for it), and is
// refused (returns immediately) while another round is in flight or once
// Stop has begun, so it can never refresh a provider concurrently with
// another round. The SIGHUP path must use RefreshAllAsync instead so the
// signal loop never blocks.
func (mc *ModelCache) RefreshAll() {
	mc.refreshMu.Lock()
	if mc.stopped || mc.refreshing {
		mc.refreshMu.Unlock()
		return
	}
	mc.refreshing = true
	mc.pending = false
	mc.pendingKind = roundManual
	mc.pendingOnDone = nil
	ctx := mc.ensureLifecycleLocked()
	// Add under refreshMu: Stop takes refreshMu before Wait, so the counter
	// is guaranteed to be >= 1 by the time Stop waits.
	mc.refreshWG.Add(1)
	mc.refreshMu.Unlock()
	// Run the round synchronously. refreshing stays true for the whole
	// pass, so a concurrent RefreshAllAsync/FetchAllAsync queues pending
	// and is handed over to exactly one more round when this one finishes.
	mc.runRefresh(ctx, roundManual, nil)
}

// RefreshAllAsync triggers a controlled background refresh of every provider
// and calls onDone (if non-nil) after the refresh completes. It is the SIGHUP
// path: the caller never blocks. The refresh
//   - snapshots mc.cfg once at each round start, so a concurrent
//     UpdateConfig (SIGHUP) cannot race the refresh's config reads;
//   - forbids concurrent reentry: while a round is running, further calls
//     are coalesced into a PENDING round — when the current round finishes
//     it starts exactly ONE more round (a flag, not a queue: no buildup)
//     that snapshots the LATEST config and carries the LATEST caller's
//     onDone. The superseded round's onDone is dropped, so a stale config
//     never syncs tools after a newer SIGHUP;
//   - refuses to start once Stop() has begun: no refresh work runs after
//     shutdown started (the onDone is dropped with it);
//   - runs on the shared cancellable lifecycle context: Stop() aborts the
//     in-flight round (HTTP requests are cancelled, no cache file is
//     written after cancellation, no onDone runs after cancellation) and
//     waits for ALL rounds to exit, so nothing leaks.
func (mc *ModelCache) RefreshAllAsync(onDone func()) {
	mc.refreshMu.Lock()
	defer mc.refreshMu.Unlock()
	if mc.stopped {
		slog.Debug("model cache refresh refused: cache is stopped")
		return
	}
	if mc.refreshing {
		// Coalesce: remember the latest request; the running round hands
		// over to a pending round when it finishes (or drops it if it is
		// being stopped). The pending kind is manual: a SIGHUP wants a
		// full refresh, whatever the in-flight round was.
		mc.pending = true
		mc.pendingKind = roundManual
		mc.pendingOnDone = onDone
		slog.Debug("model cache refresh already in progress, queueing pending refresh")
		return
	}
	mc.startRoundLocked(roundManual, onDone)
}

// ensureLifecycleLocked returns the shared background lifecycle context,
// creating it lazily on first use. Stop cancels it, which aborts every
// in-flight round (manual, stale, fill) at its next cancellation check. Must
// be called with refreshMu held.
func (mc *ModelCache) ensureLifecycleLocked() context.Context {
	if mc.lifecycleCtx == nil {
		mc.lifecycleCtx, mc.lifecycleCancel = context.WithCancel(context.Background())
	}
	return mc.lifecycleCtx
}

// startRoundLocked begins a new refresh round of the given kind. Must be
// called with refreshMu held and refreshing == false.
func (mc *ModelCache) startRoundLocked(kind roundKind, onDone func()) {
	mc.refreshing = true
	mc.pending = false
	mc.pendingKind = roundManual
	mc.pendingOnDone = nil
	ctx := mc.ensureLifecycleLocked()
	// Add under refreshMu: Stop takes refreshMu before Wait, so the counter
	// is guaranteed to be >= 1 by the time Stop waits — Stop can never
	// return before this goroutine has started (and exited).
	mc.refreshWG.Add(1)
	go mc.runRefresh(ctx, kind, onDone)
}

// runRefresh executes one refresh round of the given kind. When the round
// finishes it either hands over to a pending round (a SIGHUP manual round
// or a startup fill, requested while it was running, carrying the latest
// config snapshot and the latest onDone) or completes and releases the
// scheduler state. refreshing stays true across a handover, so Stop()'s
// Wait cannot return while any round is live; a round cancelled or stopped
// never hands over (pending work is dropped during shutdown) and never runs
// onDone.
func (mc *ModelCache) runRefresh(ctx context.Context, kind roundKind, onDone func()) {
	defer mc.refreshWG.Done()

	cancelled := false
	cfg := mc.snapshotConfig()
	for _, acc := range cfg.Accounts {
		provider := acc.Provider
		if provider == "" {
			continue
		}
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		mc.mu.RLock()
		pc := mc.caches[provider]
		mc.mu.RUnlock()
		if !mc.shouldFetch(provider, pc, kind, cfg) {
			continue
		}
		if err := mc.fetchWithContext(ctx, provider); err != nil {
			if ctx.Err() != nil {
				cancelled = true
				break
			}
			slog.Warn("model cache refresh failed", "provider", provider, "error", err)
		}
	}

	mc.refreshMu.Lock()
	stopped := mc.stopped
	if mc.pending && ctx.Err() == nil && !stopped {
		// A newer request (SIGHUP or startup fill) arrived while this round
		// was running: hand over. The new round snapshots the LATEST config
		// and carries the latest onDone; this round's onDone is dropped so a
		// stale config never syncs tools after a newer request. A stopped
		// cache must NOT hand over: Stop drains every round before returning.
		nextDone := mc.pendingOnDone
		nextKind := mc.pendingKind
		mc.pending = false
		mc.pendingKind = roundManual
		mc.pendingOnDone = nil
		// The counter is still >= 1 from this round, so the positive-delta
		// Add below is safe even while Stop's Wait runs concurrently.
		mc.refreshWG.Add(1)
		mc.refreshMu.Unlock()
		go mc.runRefresh(ctx, nextKind, nextDone)
		return
	}
	mc.refreshing = false
	// No handover happened: any queued pending request was dropped (a round
	// cancelled or stopped must not start more work), so clear the stale flag.
	mc.pending = false
	mc.pendingKind = roundManual
	mc.pendingOnDone = nil
	// Launch the callback under refreshMu, atomically with the stopped check
	// above: once Stop has set stopped, no new callback starts. The callback
	// runs on its own goroutine (runOnDone) — never inside this
	// refreshWG-counted goroutine — but is counted in doneWG: the Add
	// happens here, synchronously and before this goroutine exits, so Stop's
	// refreshWG.Wait (which returns only after this goroutine is done) is
	// ordered before its doneWG.Wait and can never miss a launched
	// callback. doneMu serializes it against a previous round's still-
	// running callback.
	if !cancelled && ctx.Err() == nil && !stopped && onDone != nil {
		mc.doneWG.Add(1)
		go mc.runOnDone(onDone)
	}
	mc.refreshMu.Unlock()
}

// runOnDone executes a round's completion callback on a dedicated goroutine.
// Callbacks are serialized by doneMu and run outside refreshWG but inside
// doneWG, so:
//   - a later round's callback never overlaps an earlier round's callback
//     (no concurrent SyncTools from consecutive rounds), and
//   - Stop waits for every launched callback (doneWG.Wait) before returning:
//     after Stop, no callback — and therefore no models.json write — is
//     still running.
//
// Contract: onDone must NOT call Stop synchronously. Stop waits for doneWG,
// so a violating callback would deadlock Stop on itself. This is the
// deliberate trade: production callbacks are pure SyncTools (the three main
// call sites in cmd/prism pass only SyncTools closures), so Stop's wait is
// always satisfied; the guarantee is the contract plus the narrow call
// sites, verified by TestRefreshAllAsync_OnDoneAsyncStopNoDeadlock and
// TestRefreshAllAsync_StopWaitsForBlockingOnDone.
func (mc *ModelCache) runOnDone(fn func()) {
	mc.doneMu.Lock()
	defer mc.doneMu.Unlock()
	defer mc.doneWG.Done()
	fn()
}

// StartRefreshLoop runs a background goroutine that checks for stale caches
// every checkInterval and refreshes them through the shared refresh
// scheduler (a stale round can never overlap a manual RefreshAllAsync round,
// and Stop cancels and waits for it like every other round). Call Stop() to
// shut down. onRefresh runs after each stale round that actually completes
// (never after Stop). The loop goroutine is tracked in loopWG: Stop returns
// only after the loop has exited.
func (mc *ModelCache) StartRefreshLoop(checkInterval time.Duration, onRefresh func()) {
	mc.refreshMu.Lock()
	if mc.stopped {
		mc.refreshMu.Unlock()
		slog.Debug("model cache refresh loop refused: cache is stopped")
		return
	}
	// Add under refreshMu, ordered before Stop's own refreshMu acquisition
	// (and Stop's loopWG.Wait): Stop can never observe a zero counter and
	// return while this loop is about to start, and a loop requested after
	// Stop is refused above — the counter can never grow after Wait began.
	mc.loopWG.Add(1)
	mc.refreshMu.Unlock()
	go func() {
		defer mc.loopWG.Done()
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-mc.stop:
				slog.Info("model cache refresh loop stopped")
				return
			case <-ticker.C:
				mc.requestStaleRefresh(onRefresh)
			}
		}
	}()
}

// requestStaleRefresh queues a stale-only refresh round. If a round is
// already in flight the stale work is subsumed (a manual round refreshes
// everything; a fill round fetches the missing caches) and onRefresh is
// skipped with it; after Stop it is a no-op.
func (mc *ModelCache) requestStaleRefresh(onRefresh func()) {
	mc.refreshMu.Lock()
	defer mc.refreshMu.Unlock()
	if mc.stopped || mc.refreshing {
		return
	}
	mc.startRoundLocked(roundStale, onRefresh)
}

// Stop shuts down the refresh loop, cancels the shared lifecycle context
// (aborting any in-flight refresh round — manual RefreshAllAsync, startup
// FetchAllAsync fill, or the stale ticker — at its next cancellation check)
// and waits for ALL refresh rounds AND every launched onDone callback to
// exit, so nothing leaks and no callback (no models.json write) can run
// after Stop returns. Once Stop has begun, no new refresh round can start:
// RefreshAllAsync, FetchAllAsync, RefreshStale and the stale ticker all
// refuse once stopped is set. It is idempotent: a second call is a no-op.
//
// The Wait cannot return before a round that is about to start: every Add
// happens under refreshMu (startRoundLocked, RefreshStale and the runRefresh
// handover), and this Wait is ordered after this method's own refreshMu
// acquisition, so a counter of zero implies no round is live or scheduled —
// and stopped=true blocks any future Add. The same ordering holds for
// callbacks: their doneWG.Add happens inside runRefresh under refreshMu
// before refreshWG.Done, so by the time refreshWG.Wait returns every
// launched callback is already counted and doneWG.Wait below waits for all
// of them. Contract: onDone must NOT call Stop synchronously (production
// callbacks are pure SyncTools); a violating callback would deadlock here.
func (mc *ModelCache) Stop() {
	mc.stopOnce.Do(func() { close(mc.stop) })
	mc.refreshMu.Lock()
	mc.stopped = true
	if mc.lifecycleCancel != nil {
		mc.lifecycleCancel()
	}
	mc.refreshMu.Unlock()
	mc.refreshWG.Wait()
	// The loop goroutine exits on mc.stop (closed above); wait for it so no
	// ticker goroutine outlives Stop.
	mc.loopWG.Wait()
	// Wait for every launched onDone: after this returns no callback is
	// still running, so no models.json write can follow Stop. Safe under
	// the onDone contract (see runOnDone): production callbacks are pure
	// SyncTools and never call Stop.
	mc.doneWG.Wait()
}

// UpdateConfig replaces the stored config reference. Call after SIGHUP reloads config.
func (mc *ModelCache) UpdateConfig(cfg *config.Config) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.cfg = cfg
}

// selectAccounts returns every healthy, non-cooldown account for the given
// provider, in pool order. The full candidate set drives the multi-account
// failover in fetchLeader. Must not hold mc.mu when calling (calls pool
// methods).
func (mc *ModelCache) selectAccounts(provider string) []*pool.Account {
	var out []*pool.Account
	for _, acc := range mc.pool.AllAccounts() {
		if acc.Provider() == provider && acc.IsHealthy() && !acc.IsInCooldown() {
			out = append(out, acc)
		}
	}
	return out
}

// SyncTools writes model configuration to each tool's config file
// based on current cache state. Only updates providers managed by Prism;
// leaves other providers in the file untouched.
func (mc *ModelCache) SyncTools(cfg *config.Config) {
	// Serialize all SyncTools callers: a read-modify-write cycle on a tool's
	// models.json must complete before the next one starts, so concurrent
	// callers (e.g. two rounds' onDone callbacks, or a callback racing a
	// direct caller) can never interleave. syncPIModelsJSON additionally
	// writes atomically (temp file + rename), so even a writer that skipped
	// this lock can never publish a partially-written file.
	mc.toolsMu.Lock()
	defer mc.toolsMu.Unlock()
	if cfg.Tools == nil {
		return
	}

	// Resolve the base URL: always 127.0.0.1 with the port from config.Listen.
	port := "18790"
	if hostPort := cfg.Listen; hostPort != "" {
		if _, p, err := net.SplitHostPort(hostPort); err == nil {
			port = p
		}
	}
	baseURL := "http://127.0.0.1:" + port + "/v1"

	for toolName, toolPath := range cfg.Tools {
		switch toolName {
		case "pi":
			if err := mc.syncPIModelsJSON(toolPath, baseURL, cfg); err != nil {
				slog.Error("sync tool config failed", "tool", toolName, "path", toolPath, "error", err)
			}
		default:
			slog.Warn("unsupported tool, skipping sync", "tool", toolName)
		}
	}
}

// syncPIModelsJSON merges upstream model IDs into pi's models.json.
//
// For each Prism-managed provider:
//   - Existing models → rebuilt entry: prism-managed fields (contextWindow/
//     maxTokens/reasoning/input/cost/thinkingLevelMap + config.Extra keys)
//     are overwritten from upstream meta + config metadata; any other keys
//     on the previous entry (e.g. a hand-edited "name") are preserved.
//     This guarantees already-existing models also receive metadata written
//     by config/upstream (fixes e.g. glm-5.2 showing context=128k).
//   - New models      → entry created with { "id": "..." } + metadata from
//     config.ModelMetadata when available
//
// Non-Prism providers in the file are untouched.
func (mc *ModelCache) syncPIModelsJSON(path string, baseURL string, cfg *config.Config) error {
	type piProvider struct {
		BaseURL string            `json:"baseUrl"`
		API     string            `json:"api"`
		APIKey  string            `json:"apiKey"`
		Headers map[string]string `json:"headers,omitempty"`
		Models  []map[string]any  `json:"models"`
	}
	type piConfig struct {
		Providers map[string]piProvider `json:"providers"`
	}

	// Read existing file (or start fresh)
	pc := piConfig{Providers: make(map[string]piProvider)}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &pc); err != nil {
			slog.Warn("pi models.json parse error, overwriting", "path", path, "error", err)
			pc = piConfig{Providers: make(map[string]piProvider)}
		}
	}

	// Create directory if needed
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	for _, provider := range cfg.ProviderNames() {
		if providerSkipPISync(cfg, provider) {
			// Preserve the existing models.json entry untouched (it is
			// hand-maintained, e.g. agentrouter-anthropic with
			// api: anthropic-messages).
			slog.Info("skip_pi_sync, preserving models.json entry", "provider", provider)
			continue
		}
		models := mc.GetModels(provider)
		if models == nil {
			slog.Warn("no cache for provider, skipping sync", "provider", provider)
			continue
		}

		// Build set of upstream model IDs
		upstreamIDs := make(map[string]bool, len(models))
		for _, m := range models {
			upstreamIDs[m.ID] = true
		}

		// Filter existing entries: keep only those still in upstream
		existingByID := make(map[string]map[string]any)
		if oldProvider, ok := pc.Providers[provider]; ok {
			for _, entry := range oldProvider.Models {
				id, _ := entry["id"].(string)
				if id != "" && upstreamIDs[id] {
					existingByID[id] = entry
				}
			}
		}

		// Build new model list (unified rebuild).
		//
		// Every upstream model gets a freshly-built entry. Prism-managed
		// fields (contextWindow/maxTokens/reasoning/input/cost/
		// thinkingLevelMap + config.Extra keys) are always overwritten from
		// upstream meta + config metadata; any other keys present on a
		// previous entry (e.g. a hand-edited "name") are preserved. This
		// guarantees already-existing models also receive metadata written
		// by config/upstream (fixes e.g. glm-5.2 showing context=128k).
		entries := make([]map[string]any, 0, len(models))

		// Keys that prism fully controls and always overwrites.
		prismManaged := map[string]bool{
			"contextWindow":    true,
			"maxTokens":        true,
			"reasoning":        true,
			"input":            true,
			"cost":             true,
			"thinkingLevelMap": true,
		}
		// Config-declared extra keys are also prism-managed.
		for _, cm := range cfg.ModelMetadata {
			for k := range cm.Extra {
				prismManaged[k] = true
			}
		}
		// Per-provider override entries may also declare extra keys.
		for _, pp := range cfg.ModelMetadataPerProvider {
			for _, cm := range pp {
				for k := range cm.Extra {
					prismManaged[k] = true
				}
			}
		}

		for _, m := range models {
			existing := existingByID[m.ID] // may be nil
			upMeta, _ := mc.GetModelMeta(provider, m.ID)
			cfgMeta, _ := cfg.LookupModelMetadata(provider, m.ID)
			merged := mergeMeta(upMeta, cfgMeta)

			entry := map[string]any{"id": m.ID}
			if existing != nil {
				// Preserve non-managed keys from the old entry
				// (e.g. hand-edited name), dropping managed ones that
				// will be re-derived below.
				for k, v := range existing {
					if k != "id" && !prismManaged[k] {
						entry[k] = v
					}
				}
			}
			applyMergedCamel(entry, merged, cfgMeta)
			entries = append(entries, entry)
		}

		pc.Providers[provider] = piProvider{
			BaseURL: baseURL,
			API:     "openai-completions",
			APIKey:  "prism-dummy-key",
			Headers: map[string]string{"X-Prism-Provider": provider},
			Models:  entries,
		}
	}

	data, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	// Atomic replace: the data goes to a temp file in the SAME directory
	// (same filesystem, so the rename is atomic) and is renamed over the
	// final path only after a complete write. A concurrent reader never
	// observes a partial file, and two concurrent SyncTools calls can never
	// interleave their writes into a corrupt models.json — every published
	// file is one complete writer's output. The temp file is removed on
	// every failure path; repeated runs are idempotent.
	//
	// Ownership: rename creates a new inode owned by the process user,
	// which would break a deployed root:pi-sync models.json (the prism
	// service user writes through group permissions, and pi reads its own
	// config). To keep that working we preserve the previous file's mode
	// and its uid/gid via Chown on the temp file. If the Chown fails (EPERM
	// with NoNewPrivileges and an empty CapabilityBoundingSet when the
	// process is not root — or any other error) the sync ABORTS with an
	// error: the rename must NEVER proceed (the replacement would silently
	// change the file's owner), and there is deliberately no in-place
	// fallback — a non-atomic overwrite could publish a half-written file
	// on a crash, and a silently degraded sync would hide the ownership
	// misconfiguration instead of surfacing it. The old file, its inode and
	// its content stay untouched, and the temp file is removed. When the
	// file does not exist yet there is no owner to preserve: the normal
	// atomic rename path applies (install-time models.json should be owned
	// by the service user, or prism must be allowed to preserve the
	// existing owner — see README).
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	abort := func(cause error) error {
		tmp.Close()
		os.Remove(tmpName)
		return cause
	}
	if _, err := tmp.Write(data); err != nil {
		return abort(fmt.Errorf("write temp file: %w", err))
	}
	mode := os.FileMode(0644)
	var uid, gid int
	preserveOwner := false
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
			preserveOwner = true
		}
	}
	if preserveOwner {
		if err := mc.preserveOwnerOnTemp(tmpName, uid, gid); err != nil {
			// Abort, never fall back: see the ownership comment above. The
			// old file/inode/content stay untouched; abort removes the temp.
			return abort(fmt.Errorf("preserve models.json owner (uid/gid %d/%d): %w", uid, gid, err))
		}
	}
	if err := tmp.Chmod(mode); err != nil {
		return abort(fmt.Errorf("chmod temp file: %w", err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename over models.json: %w", err)
	}

	slog.Info("pi models.json synced", "path", path, "providers", len(pc.Providers))
	return nil
}

// preserveOwnerOnTemp applies the original file's uid/gid to the temp file
// before the atomic rename. The injected chownFile (tests) is used when set;
// otherwise os.Chown. A failure ABORTS the sync (see syncPIModelsJSON): the
// rename must NOT proceed.
func (mc *ModelCache) preserveOwnerOnTemp(name string, uid, gid int) error {
	if mc.chownFile != nil {
		return mc.chownFile(name, uid, gid)
	}
	return os.Chown(name, uid, gid)
}

// rootURL strips the trailing "/v1" (and any trailing slash) from a base URL
// so it can be joined with ollama's "/api/show" endpoint. JoinURLPath is not
// suitable because /api/show is not under /v1.
func rootURL(base string) string {
	base = strings.TrimSuffix(base, "/")
	return strings.TrimSuffix(base, "/v1")
}

// deriveContextLength scans an ollama model_info map for a key ending in
// ".context_length" and returns its (int) value. The exact key is
// architecture-dependent (e.g. "llama.context_length"), so we match by suffix
// rather than hard-coding it.
func deriveContextLength(modelInfo map[string]any) *int {
	if modelInfo == nil {
		return nil
	}
	for k, v := range modelInfo {
		if !strings.HasSuffix(k, ".context_length") {
			continue
		}
		switch n := v.(type) {
		case float64:
			i := int(n)
			return &i
		case int:
			return &n
		case json.Number:
			if i, err := n.Int64(); err == nil {
				i2 := int(i)
				return &i2
			}
		}
	}
	return nil
}

// fetchOllamaShow queries ollama's /api/show endpoint for a single model and
// extracts its metadata (currently just context_window from model_info).
// A non-200, timeout, over-limit body, or parse error is returned as an
// error so the caller can skip the model without failing the whole fetch.
// ctx must be the enclosing fetch's context (cancellable for the background
// refresh); the per-request 15s timeout is applied on top of it.
func (mc *ModelCache) fetchOllamaShow(ctx context.Context, acc *pool.Account, id string) (ModelMeta, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body, err := json.Marshal(map[string]string{"name": id})
	if err != nil {
		return ModelMeta{}, fmt.Errorf("marshal show body: %w", err)
	}
	url := rootURL(acc.BaseURL()) + "/api/show"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// Safe classification: the raw error may embed the URL (parse
		// errors) and must never reach logs.
		return ModelMeta{}, fmt.Errorf("create show request failed: %s", util.ClassifyConnError(err))
	}
	req.Header.Set("Content-Type", "application/json")
	// Account-level headers + credential, same semantics as doUpstreamRequest:
	// account headers override via Set (an account-configured Content-Type
	// wins over the default above); the credential comes from acc.Key() via
	// ApplyAuthHeader (Bearer, or the custom auth_header).
	pool.ApplyAccountHeaders(req.Header, acc)
	pool.ApplyAuthHeader(req.Header, acc)

	resp, err := acc.Client().Do(req)
	if err != nil {
		// Safe classification: the raw *url.Error embeds the full upstream
		// URL (query parameters, credentials) and must never reach logs.
		return ModelMeta{}, fmt.Errorf("show request failed: %s", util.ClassifyConnError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// Redact the body (including this account's key as a literal) before
		// it enters the error/log path: an upstream may echo the credential
		// back in the error payload, and the key may not look like an
		// sk-/Bearer token (custom auth_header).
		return ModelMeta{}, fmt.Errorf("api/show returned %d: %s", resp.StatusCode, util.RedactBodyBytesWithKeys(b, []string{acc.Key()}))
	}

	// Bounded success read, same cap as the /v1/models fetch
	// (max_upstream_response_bytes config, default 32 MiB).
	raw, err := util.ReadBodyLimited(resp.Body, fetchResponseCap(mc.snapshotConfig()))
	if err != nil {
		return ModelMeta{}, fmt.Errorf("read show response: %w", err)
	}
	var show struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err := json.Unmarshal(raw, &show); err != nil {
		return ModelMeta{}, fmt.Errorf("decode show response: %w", err)
	}
	return ModelMeta{ContextWindow: deriveContextLength(show.ModelInfo)}, nil
}

// fetchOllamaMeta fetches metadata for every model from ollama's /api/show
// endpoint. Only ollama providers (per cfg.EffortSchema) are queried; all other
// providers return nil. A single model's failure is logged and skipped — it
// never fails the enclosing Fetch. Up to 4 requests run concurrently.
func (mc *ModelCache) fetchOllamaMeta(ctx context.Context, acc *pool.Account, models []ModelEntry, cfg *config.Config) map[string]ModelMeta {
	if acc == nil || cfg == nil || cfg.EffortSchema(acc.Provider()) != "ollama" {
		return nil
	}
	return mc.collectOllamaMeta(ctx, acc, models)
}

// collectOllamaMeta performs the concurrent /api/show fan-out. It is the
// testable core of fetchOllamaMeta (which adds the ollama-only gate).
func (mc *ModelCache) collectOllamaMeta(ctx context.Context, acc *pool.Account, models []ModelEntry) map[string]ModelMeta {
	if len(models) == 0 {
		return nil
	}
	result := make(map[string]ModelMeta)
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, m := range models {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			meta, err := mc.fetchOllamaShow(ctx, acc, id)
			if err != nil {
				slog.Warn("fetch ollama /api/show failed, skipping model",
					"provider", acc.Provider(), "model", id, "error", err)
				return
			}
			mu.Lock()
			result[id] = meta
			mu.Unlock()
		}(m.ID)
	}
	wg.Wait()
	if len(result) == 0 {
		return nil
	}
	return result
}

// GetModelMeta returns the upstream metadata for a single model, if known.
func (mc *ModelCache) GetModelMeta(provider, id string) (ModelMeta, bool) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	pc := mc.caches[provider]
	if pc == nil || pc.Meta == nil {
		return ModelMeta{}, false
	}
	meta, ok := pc.Meta[id]
	return meta, ok
}

// GetMeta returns the upstream metadata map for a provider.
func (mc *ModelCache) GetMeta(provider string) map[string]ModelMeta {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	pc := mc.caches[provider]
	if pc == nil {
		return nil
	}
	return pc.Meta
}

// mergeMeta combines upstream metadata with config metadata. Config fields
// that are set (non-nil/non-empty) override the upstream value of the same
// field. cost/thinkingLevelMap/Extra come only from config and are not part
// of ModelMeta, so they are handled separately by applyMergedCamel.
func mergeMeta(up ModelMeta, cfg config.ModelMetadata) ModelMeta {
	merged := up
	if cfg.ContextWindow != nil {
		merged.ContextWindow = cfg.ContextWindow
	}
	if cfg.MaxTokens != nil {
		merged.MaxTokens = cfg.MaxTokens
	}
	if cfg.Reasoning != nil {
		merged.Reasoning = cfg.Reasoning
	}
	if len(cfg.Input) > 0 {
		merged.Input = cfg.Input
	}
	return merged
}

// applyMergedCamel writes the merged metadata into a pi models.json entry using
// camelCase keys (models.json convention). ContextWindow/MaxTokens/Reasoning/
// Input come from merged (upstream + config override); cost/thinkingLevelMap/
// extra come only from cfgMeta (config-only). Prism-managed fields already on
// the entry are overwritten; this is the single place models.json metadata is
// written.
func applyMergedCamel(entry map[string]any, merged ModelMeta, cfgMeta config.ModelMetadata) {
	if merged.ContextWindow != nil {
		entry["contextWindow"] = *merged.ContextWindow
	}
	if merged.MaxTokens != nil {
		entry["maxTokens"] = *merged.MaxTokens
	}
	if merged.Reasoning != nil {
		entry["reasoning"] = *merged.Reasoning
	}
	if len(merged.Input) > 0 {
		entry["input"] = merged.Input
	}
	if cfgMeta.Cost != nil {
		entry["cost"] = map[string]float64{
			"input":      cfgMeta.Cost.Input,
			"output":     cfgMeta.Cost.Output,
			"cacheRead":  cfgMeta.Cost.CacheRead,
			"cacheWrite": cfgMeta.Cost.CacheWrite,
		}
	}
	if len(cfgMeta.ThinkingLevelMap) > 0 {
		entry["thinkingLevelMap"] = cfgMeta.ThinkingLevelMap
	}
	if len(cfgMeta.Extra) > 0 {
		for k, v := range cfgMeta.Extra {
			entry[k] = v
		}
	}
}
