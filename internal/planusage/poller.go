package planusage

import (
	"context"
	"errors"
	"expvar"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

var fetchErrors = expvar.NewInt("quota_fetch_errors")

// Poller refreshes plan snapshots in the background. It never borrows a
// pool concurrency slot.
type Poller struct {
	fetchers []Fetcher
	cache    *Cache

	mu       sync.Mutex
	accounts []AccountView
	interval time.Duration
	timeout  time.Duration
	enabled  atomic.Bool
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	started  bool

	grokSum      GrokTokenSum
	estimatePath string
	grokImport   GrokBuildImporter
}

// GrokBuildImporter pulls Grok Build CLI session usage into the usage
// database for [fromUnix, toUnix] before the week-limit estimate runs.
type GrokBuildImporter func(ctx context.Context, fromUnix, toUnix int64) error

func NewPoller(fetchers []Fetcher, cache *Cache, interval, timeout time.Duration) *Poller {
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	p := &Poller{
		fetchers: fetchers,
		cache:    cache,
		interval: interval,
		timeout:  timeout,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	p.enabled.Store(true)
	return p
}

func (p *Poller) Cache() *Cache { return p.cache }

func (p *Poller) SetAccounts(accounts []AccountView) {
	p.mu.Lock()
	p.accounts = append([]AccountView(nil), accounts...)
	p.mu.Unlock()
}

func (p *Poller) SetOptions(enabled bool, interval, timeout time.Duration) {
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	p.mu.Lock()
	p.interval = interval
	p.timeout = timeout
	p.mu.Unlock()
	p.enabled.Store(enabled)
}

func (p *Poller) Enabled() bool { return p.enabled.Load() }

// SetGrokEstimate wires SuperGrok 限额估算 (previous week's grok-*
// tokens / week-pool percent). path is DefaultGrokEstimatePath in production.
func (p *Poller) SetGrokEstimate(sum GrokTokenSum, path string) {
	p.mu.Lock()
	p.grokSum = sum
	p.estimatePath = path
	p.mu.Unlock()
}

func (p *Poller) SetGrokBuildImport(imp GrokBuildImporter) {
	p.mu.Lock()
	p.grokImport = imp
	p.mu.Unlock()
}

func (p *Poller) Start() {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.mu.Unlock()
	go p.loop()
}

// Stop is idempotent. After the first call, later calls wait for the
// already-closed done channel and return immediately.
func (p *Poller) Stop() {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	p.stopOnce.Do(func() {
		close(p.stop)
	})
	if !started {
		return
	}
	<-p.done
}

func (p *Poller) loop() {
	defer close(p.done)
	p.Refresh()
	for {
		p.mu.Lock()
		iv := p.interval
		p.mu.Unlock()
		timer := time.NewTimer(iv)
		select {
		case <-p.stop:
			timer.Stop()
			return
		case <-timer.C:
			if p.enabled.Load() {
				p.Refresh()
			}
		}
	}
}

// Refresh fetches every unique key concurrently. Stop closes p.stop, which
// cancels the shared context so in-flight fetches abort.
func (p *Poller) Refresh() {
	if !p.enabled.Load() {
		return
	}
	p.mu.Lock()
	accounts := append([]AccountView(nil), p.accounts...)
	timeout := p.timeout
	p.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-p.stop:
			cancel()
		case <-stopWatch:
		}
	}()
	defer close(stopWatch)

	groups := GroupByKey(accounts, p.fetchers)
	live := make(map[string]struct{}, len(groups))
	var wg sync.WaitGroup
	for _, g := range groups {
		live[g.Fingerprint] = struct{}{}
		wg.Add(1)
		go func(g KeyGroup) {
			defer wg.Done()
			p.fetchOne(ctx, g, timeout)
		}(g)
	}
	wg.Wait()
	p.cache.ForgetMissing(live)
}

func (p *Poller) fetchOne(parent context.Context, g KeyGroup, timeout time.Duration) {
	names := make([]string, 0, len(g.Accounts))
	for _, a := range g.Accounts {
		names = append(names, a.Name())
	}
	acc := g.Accounts[0]
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	snap, err := g.Fetcher.Fetch(ctx, acc)
	snap.Accounts = names
	if err != nil {
		fetchErrors.Add(1)
		code := ErrorCode(err)
		slog.Warn("quota fetch failed", "provider", snap.Provider, "accounts", names, "error", code)
		p.cache.StoreFailed(g.Fingerprint, snap.Provider, names, code)
		return
	}
	p.mu.Lock()
	sum := p.grokSum
	estPath := p.estimatePath
	imp := p.grokImport
	p.mu.Unlock()
	if imp != nil {
		from, to := GrokBuildImportWindow(snap, time.Now())
		ictx, icancel := context.WithTimeout(context.Background(), 30*time.Second)
		if ierr := imp(ictx, from, to); ierr != nil {
			slog.Warn("quota grok-build import failed", "error", ierr)
		}
		icancel()
	}
	if sum != nil {
		snap = ApplyGrokWeekEstimate(ctx, snap, sum, estPath, time.Now())
	}
	p.cache.Store(g.Fingerprint, snap)
}

// ErrorCode maps a fetch error to a short, non-secret code for logs and CLI.
func ErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrNoSubscription):
		return "no_subscription"
	case errors.Is(err, ErrUnexpectedStatus):
		return "unexpected_status"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "fetch_failed"
	default:
		return "fetch_failed"
	}
}
