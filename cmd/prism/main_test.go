package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/proxy"
	"github.com/dorokuma/prism/internal/usage"
	"github.com/dorokuma/prism/internal/usagemeta"
)

// fakeProxy stands in for the real proxy handler: it records that it was
// reached and returns a fixed status. Tests assert reachability + status, not
// proxy internals.
type fakeProxy struct {
	called int
	status int
}

func (f *fakeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.called++
	w.WriteHeader(f.status)
}

// testConfig builds a minimal valid config; mutate lets tests enable usage,
// add api_keys, etc.
func testConfig(t *testing.T, mutate func(*config.Config)) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Listen: "127.0.0.1:0",
		Accounts: []config.AccountConfig{
			{Name: "acc-1", Key: "test-key-12345", BaseURL: "https://api.example.com"},
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Recorder wiring degradation (highest-priority constraint)
// ---------------------------------------------------------------------------

// TestStartUsageRecorder_Disabled: usage disabled → nil recorder and nil
// store; the shutdown path (Close on nil) must not panic.
func TestStartUsageRecorder_Disabled(t *testing.T) {
	cfg := testConfig(t, nil) // usage zero-value → disabled
	rec, store := startUsageRecorder(cfg)
	if rec != nil {
		t.Fatalf("recorder must be nil when usage disabled, got %v", rec)
	}
	if store != nil {
		t.Fatalf("store must be nil when usage disabled, got %v", store)
	}
	rec.Close() // nil-safe shutdown, must not panic
}

// TestNilRecorderCloseNoPanic: the acceptance requirement "Recorder 为 nil 时
// 收尾不 panic", stated directly.
func TestNilRecorderCloseNoPanic(t *testing.T) {
	var rec *usage.Recorder
	rec.Close() // must not panic
}

// TestStartUsageRecorder_UnwritableDBPathDegradesToNoOp: when the DB file
// cannot be created (parent directory missing), the recorder must still be
// returned as a degraded no-op — Record never blocks/panics, Close never
// panics, and the store is handed to the summary handler (which 503s).
func TestStartUsageRecorder_UnwritableDBPathDegradesToNoOp(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Usage.Enabled = true
		c.Usage.DBPath = filepath.Join(t.TempDir(), "no-such-dir", "usage.db")
	})
	rec, store := startUsageRecorder(cfg)
	if rec == nil {
		t.Fatal("recorder must be returned (degraded no-op) even when the store cannot open")
	}
	if store == nil {
		t.Fatal("store must be returned for the summary handler even when open failed")
	}
	// Recording is a no-op: no panic, no block, no error.
	rec.Record(usage.Event{RequestID: "r1", Model: "m", PromptTokens: 1})
	// Shutdown of a degraded recorder must not panic.
	rec.Close()
	// Summary over the failed store yields an error → handler maps to 503.
	if _, err := store.Summary(context.Background(), usage.SummaryQuery{}); err == nil {
		t.Error("Summary over a store whose open failed must return an error")
	}
}

// ---------------------------------------------------------------------------
// HTTP wiring: usage degradation must never affect /v1 forwarding
// ---------------------------------------------------------------------------

// TestHTTPHandler_UsageDisabledProxyStillServes: with usage not configured at
// all, the HTTP handler must forward /v1 requests to the proxy handler and
// return its status (acceptance: 未配置 usage 时代理转发正常).
func TestHTTPHandler_UsageDisabledProxyStillServes(t *testing.T) {
	cfg := testConfig(t, nil)
	holder := config.NewConfigHolder(cfg)
	fp := &fakeProxy{status: http.StatusOK}
	h := newHTTPHandler(holder, fp, nil, nil, usage.NewSummaryHandler(nil))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("usage disabled: /v1 status = %d, want 200", rr.Code)
	}
	if fp.called != 1 {
		t.Errorf("proxy handler reached %d times, want 1", fp.called)
	}
}

// TestHTTPHandler_UsageStoreFailureProxyStillServes: DBPath pointing at an
// unwritable location must not stop the service: /v1 still forwards with a
// normal status code (acceptance requirement), and the summary endpoint
// degrades to 503.
func TestHTTPHandler_UsageStoreFailureProxyStillServes(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.Usage.Enabled = true
		c.Usage.DBPath = filepath.Join(t.TempDir(), "no-such-dir", "usage.db")
	})
	rec, store := startUsageRecorder(cfg)
	if rec == nil {
		t.Fatal("service must start with a degraded (no-op) recorder")
	}
	defer rec.Close()

	holder := config.NewConfigHolder(cfg)
	fp := &fakeProxy{status: http.StatusOK}
	h := newHTTPHandler(holder, fp, nil, nil, usage.NewSummaryHandler(store))

	req := httptest.NewRequest("POST", "/v1/responses", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("unwritable usage DBPath: /v1 status = %d, want 200", rr.Code)
	}
	if fp.called != 1 {
		t.Errorf("proxy handler reached %d times, want 1", fp.called)
	}

	// The summary route is still mounted; the failed store answers 503.
	req2 := httptest.NewRequest("GET", "/admin/usage/summary", nil)
	req2.RemoteAddr = "127.0.0.1:12345" // localhost: own auth allows
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusServiceUnavailable {
		t.Errorf("summary over failed store: status = %d, want 503", rr2.Code)
	}
}

// TestHTTPHandler_SummaryRouteBeforeAuthGate: /admin/usage/summary is mounted
// BEFORE the global api_keys gate (like /metrics) and uses its own
// PRISM_ADMIN_TOKEN auth: a localhost request without any API key passes the
// gate and reaches the handler (503 store_unavailable here, not a 401 from
// the global gate); a remote request without PRISM_ADMIN_TOKEN is 401.
func TestHTTPHandler_SummaryRouteBeforeAuthGate(t *testing.T) {
	cfg := testConfig(t, func(c *config.Config) {
		c.APIKeys = []config.APIKey{{Name: "k", Token: "sk-tok"}}
	})
	holder := config.NewConfigHolder(cfg)
	fp := &fakeProxy{status: http.StatusOK}
	h := newHTTPHandler(holder, fp, nil, nil, usage.NewSummaryHandler(nil))

	// Localhost + no API key → own auth allows (localhost), store nil → 503.
	req := httptest.NewRequest("GET", "/admin/usage/summary", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("summary from localhost without API key: status = %d, want 503 (mounted before global auth gate)", rr.Code)
	}
	if fp.called != 0 {
		t.Error("summary route must never reach the proxy handler")
	}

	// Remote + no PRISM_ADMIN_TOKEN → 401 from the summary handler itself.
	req2 := httptest.NewRequest("GET", "/admin/usage/summary", nil) // RemoteAddr defaults to non-localhost
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("summary from remote without token: status = %d, want 401", rr2.Code)
	}
}

// ---------------------------------------------------------------------------
// key_id attribution through the HTTP wiring (usage.default_key_id)
// ---------------------------------------------------------------------------

// auditProxyHandler runs the real proxy audit path (proxyChatWithBody) behind
// the HTTP wiring, mirroring what cmd/prism does for /v1/chat/completions, so
// the key_id end-to-end tests exercise the same code that produces usage rows
// in production.
type auditProxyHandler struct {
	p      *pool.Pool
	holder *config.ConfigHolder
}

func (h *auditProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusInternalServerError)
		return
	}
	proxy.ProxyChatWithBody(h.p, w, r, body, time.Now(), proxy.ChatForwardOpts{}, h.holder.Load())
}

// wireUsageRecorder mirrors main()'s usage wiring for tests: starts the
// recorder from cfg, injects the adapter into middleware, and installs the
// default key id exactly like the real startup sequence.
func wireUsageRecorder(t *testing.T, cfg *config.Config) *usage.Recorder {
	t.Helper()
	rec, _ := startUsageRecorder(cfg)
	if rec == nil {
		t.Fatal("recorder must start with a writable DBPath")
	}
	holder := config.NewConfigHolder(cfg)
	adapter := &usageRecorderAdapter{holder: holder, rec: rec}
	middleware.SetUsageRecorder(adapter)
	middleware.SetUsagePricer(adapter.Price)
	middleware.SetUsageDefaultKeyID(cfg.Usage.DefaultKeyID)
	t.Cleanup(func() {
		middleware.SetUsageRecorder(nil)
		middleware.SetUsagePricer(nil)
		middleware.SetUsageDefaultKeyID("anonymous")
	})
	return rec
}

// persistedKeyIDs reopens the usage DB read-only and returns the key_id
// groups, one entry per distinct key_id (empty string included if any row
// ever managed to persist one).
func persistedKeyIDs(t *testing.T, dbPath string) []string {
	t.Helper()
	check := usage.NewSQLiteStore(dbPath)
	if err := check.Open(); err != nil {
		t.Fatalf("reopen for verification: %v", err)
	}
	defer check.Close()
	rows, err := check.Summary(context.Background(), usage.SummaryQuery{GroupBy: []string{"key_id"}})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		kid, _ := r.Groups["key_id"].(string)
		out = append(out, kid)
	}
	return out
}

// TestHTTPHandler_MissingProvider400_KeyIDFilled is the regression test for
// the empty-key_id leak: a request rejected early (400, missing
// X-Prism-Provider header) returns before key attribution, and its usage row
// must record the default key_id ("anonymous") — never "". An empty and a
// default value are two different GROUP BY groups and would split one row
// into two.
func TestHTTPHandler_MissingProvider400_KeyIDFilled(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	cfg := testConfig(t, func(c *config.Config) {
		c.Usage.Enabled = true
		c.Usage.DBPath = dbPath
		c.Usage.BatchSize = 1 // flush immediately
	})
	rec := wireUsageRecorder(t, cfg)
	holder := config.NewConfigHolder(cfg)
	h := newHTTPHandler(holder, &auditProxyHandler{p: pool.NewPool(cfg.Accounts), holder: holder}, nil, nil, usage.NewSummaryHandler(nil))

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing X-Prism-Provider: status = %d, want 400", rr.Code)
	}
	rec.Close() // drain buffered events

	kids := persistedKeyIDs(t, dbPath)
	if len(kids) != 1 || kids[0] != "anonymous" {
		t.Fatalf("persisted key_id(s) = %v, want exactly [anonymous] (the early-400 path must never record an empty key_id)", kids)
	}
}

// TestHTTPHandler_DefaultKeyIDConfigApplied: with auth disabled (no api_keys)
// and usage.default_key_id explicitly configured, every request is recorded
// under the configured value.
func TestHTTPHandler_DefaultKeyIDConfigApplied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	cfg := testConfig(t, func(c *config.Config) {
		c.Usage.Enabled = true
		c.Usage.DBPath = dbPath
		c.Usage.BatchSize = 1
		c.Usage.DefaultKeyID = "gateway-pi"
		c.Accounts = []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL, Provider: "test"}}
	})
	rec := wireUsageRecorder(t, cfg)
	holder := config.NewConfigHolder(cfg)
	h := newHTTPHandler(holder, &auditProxyHandler{p: pool.NewPool(cfg.Accounts), holder: holder}, nil, nil, usage.NewSummaryHandler(nil))

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Prism-Provider", "test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	rec.Close()

	kids := persistedKeyIDs(t, dbPath)
	if len(kids) != 1 || kids[0] != "gateway-pi" {
		t.Fatalf("persisted key_id(s) = %v, want exactly [gateway-pi] (auth disabled → configured default)", kids)
	}
}

// TestHTTPHandler_AuthenticatedKeyNameWins: with api_keys configured and the
// request carrying the correct key, the recorded key_id is the key's NAME —
// the configured default must never override a real authentication result.
// A wrong key is rejected with 401 and records nothing at all.
func TestHTTPHandler_AuthenticatedKeyNameWins(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	cfg := testConfig(t, func(c *config.Config) {
		c.Usage.Enabled = true
		c.Usage.DBPath = dbPath
		c.Usage.BatchSize = 1
		c.Usage.DefaultKeyID = "gateway-pi"
		c.APIKeys = []config.APIKey{{Name: "ci-bot", Token: "sk-ci-111"}}
		c.Accounts = []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL, Provider: "test"}}
	})
	rec := wireUsageRecorder(t, cfg)
	holder := config.NewConfigHolder(cfg)
	h := newHTTPHandler(holder, &auditProxyHandler{p: pool.NewPool(cfg.Accounts), holder: holder}, nil, nil, usage.NewSummaryHandler(nil))

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-ci-111")
	req.Header.Set("X-Prism-Provider", "test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	// A wrong key is rejected by the gate before any audit exists: 401 and
	// no additional usage row.
	reqBad := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	reqBad.Header.Set("Content-Type", "application/json")
	reqBad.Header.Set("Authorization", "Bearer sk-wrong")
	reqBad.Header.Set("X-Prism-Provider", "test")
	rrBad := httptest.NewRecorder()
	h.ServeHTTP(rrBad, reqBad)
	if rrBad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status = %d, want 401", rrBad.Code)
	}
	rec.Close()

	kids := persistedKeyIDs(t, dbPath)
	if len(kids) != 1 || kids[0] != "ci-bot" {
		t.Fatalf("persisted key_id(s) = %v, want exactly [ci-bot] (real key name wins over default gateway-pi; rejected requests record nothing)", kids)
	}
}

// ---------------------------------------------------------------------------
// Graceful shutdown ordering: srv.Shutdown BEFORE usageRec.Close
// ---------------------------------------------------------------------------

// TestShutdownFlushesInFlightUsage is the acceptance test for the shutdown
// order fix: an in-flight request that finishes during graceful shutdown
// must still have its usage event persisted. The real shutdown sequence
// (shutdownHTTPAndDrainUsage) is exercised: srv.Shutdown waits for the
// in-flight handler, whose deferred EmitAudit → Record runs while the
// recorder is still accepting, and only then does usageRec.Close drain the
// queue. With the old order (Close before Shutdown) the event would be
// recorded after the recorder stopped and silently lost.
func TestShutdownFlushesInFlightUsage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	cfg := testConfig(t, func(c *config.Config) {
		c.Usage.Enabled = true
		c.Usage.DBPath = dbPath
		c.Usage.BatchFlushMS = 60000 // no ticker flush; only the drain writes
		c.ModelMetadata = config.ModelMetadataMap{
			"m": {Cost: &config.ModelCost{Input: 1, Output: 1}},
		}
	})
	rec, _ := startUsageRecorder(cfg)
	if rec == nil {
		t.Fatal("recorder must start with a writable DBPath")
	}
	holder := config.NewConfigHolder(cfg)
	adapter := &usageRecorderAdapter{holder: holder, rec: rec}
	middleware.SetUsageRecorder(adapter)
	middleware.SetUsagePricer(adapter.Price)
	defer middleware.SetUsageRecorder(nil)
	defer middleware.SetUsagePricer(nil)

	handlerStarted := make(chan struct{})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Mirror the real proxy path: the audit is emitted in a deferred
			// call when the request finishes.
			a := &middleware.RequestAudit{
				Req: "in-flight-1", Method: "POST", Path: "/v1/chat/completions",
				Model: "m", Provider: "p", KeyID: "k", Success: true, Status: 200,
			}
			a.ApplyUsage(usagemeta.Usage{Prompt: 100, Completion: 50, Total: 150, Source: usagemeta.SourceOpenAI})
			defer middleware.EmitAudit(a)
			close(handlerStarted)
			// Simulate a request still in flight when Shutdown is called:
			// Shutdown must wait for it to finish.
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(200)
		}),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	clientDone := make(chan error, 1)
	go func() {
		_, err := http.Get("http://" + ln.Addr().String() + "/v1/chat/completions")
		clientDone <- err
	}()

	// Wait until the request is genuinely in flight, then run the real
	// shutdown sequence (HTTP first, recorder drain second).
	select {
	case <-handlerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}
	shutdownHTTPAndDrainUsage(srv, rec)
	if err := <-clientDone; err != nil {
		t.Fatalf("in-flight request failed during graceful shutdown: %v", err)
	}

	// The in-flight request's usage must be in the DB: Shutdown finished
	// the request (its deferred EmitAudit enqueued the event), then Close
	// drained the recorder. The event must also be priced with the
	// openai-source formula (cost = 100/1e6*1 + 50/1e6*1 = 0.00015).
	check := usage.NewSQLiteStore(dbPath)
	if err := check.Open(); err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := check.Summary(context.Background(), usage.SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatalf("persisted %+v, want exactly the 1 in-flight request's usage", rows)
	}
	if rows[0].CostUSD == nil || math.Abs(*rows[0].CostUSD-0.00015) > 1e-12 {
		t.Errorf("cost_usd = %v, want 0.00015 (priced once, on the synchronous path, from config)", rows[0].CostUSD)
	}
}

// ---------------------------------------------------------------------------
// Price mapping (config → usage.Price)
// ---------------------------------------------------------------------------

// TestPriceFor_MapsConfigCost: a model_metadata cost entry maps field-for-field
// onto usage.Price; unknown model / nil config yield nil (missing_price).
func TestPriceFor_MapsConfigCost(t *testing.T) {
	cfg := &config.Config{
		ModelMetadata: config.ModelMetadataMap{
			"deepseek-v4-pro": {
				Cost: &config.ModelCost{Input: 0.6, Output: 2.4, CacheRead: 0.12, CacheWrite: 0.6},
			},
		},
	}
	p := priceFor(cfg, "opencode-go", "deepseek-v4-pro")
	if p == nil {
		t.Fatal("price must resolve from the default model_metadata layer")
	}
	if p.Input != 0.6 || p.Output != 2.4 || p.CacheRead != 0.12 || p.CacheWrite != 0.6 {
		t.Errorf("price mismatch: %+v", p)
	}
	if got := priceFor(cfg, "opencode-go", "unknown-model"); got != nil {
		t.Errorf("unknown model must yield nil price, got %+v", got)
	}
	if got := priceFor(nil, "p", "m"); got != nil {
		t.Errorf("nil config must yield nil price, got %+v", got)
	}
}

// TestPriceFor_PerProviderOverride: the per-provider metadata layer wins over
// the default layer for its provider; other providers fall back to default.
func TestPriceFor_PerProviderOverride(t *testing.T) {
	cfg := &config.Config{
		ModelMetadata: config.ModelMetadataMap{
			"m": {Cost: &config.ModelCost{Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1}},
		},
		ModelMetadataPerProvider: map[string]config.ModelMetadataMap{
			"p1": {"m": {Cost: &config.ModelCost{Input: 2, Output: 3, CacheRead: 4, CacheWrite: 5}}},
		},
	}
	p := priceFor(cfg, "p1", "m")
	if p == nil || p.Input != 2 || p.Output != 3 || p.CacheRead != 4 || p.CacheWrite != 5 {
		t.Errorf("per-provider override must win, got %+v", p)
	}
	p2 := priceFor(cfg, "other-provider", "m")
	if p2 == nil || p2.Input != 1 || p2.Output != 1 {
		t.Errorf("default layer must apply for other providers, got %+v", p2)
	}
}

// TestUsageAdapter_PersistsPricedEvent is the end-to-end acceptance test for
// the single-pricing-point rule: an event emitted through middleware.EmitAudit
// with the real adapter + pricer wired (pricer computes the cost synchronously
// → request.complete log line → usage event carrying the SAME cost → Recorder
// → SQLite) must come back from Summary with a non-zero cost, and the audit
// log line must carry the identical amount. The async write path never
// re-prices, so the log and the database cannot diverge.
func TestUsageAdapter_PersistsPricedEvent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	cfg := testConfig(t, func(c *config.Config) {
		c.Usage.Enabled = true
		c.Usage.DBPath = dbPath
		c.Usage.BatchSize = 1 // flush immediately
		c.ModelMetadata = config.ModelMetadataMap{
			"deepseek-v4-pro": {
				Cost: &config.ModelCost{Input: 1.0, Output: 2.0, CacheRead: 0.2, CacheWrite: 0.5},
			},
		}
	})
	holder := config.NewConfigHolder(cfg)
	rec, _ := startUsageRecorder(cfg)
	if rec == nil {
		t.Fatal("recorder must start with a writable DBPath")
	}
	defer rec.Close()

	adapter := &usageRecorderAdapter{holder: holder, rec: rec}
	middleware.SetUsageRecorder(adapter)
	middleware.SetUsagePricer(adapter.Price)
	defer middleware.SetUsageRecorder(nil)
	defer middleware.SetUsagePricer(nil)

	// Capture the request.complete log line while EmitAudit runs.
	var logBuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(old)

	aud := &middleware.RequestAudit{
		Req: "req-1", Method: "POST", Path: "/v1/chat/completions",
		Model: "deepseek-v4-pro", Provider: "opencode-go", KeyID: "ci-bot",
		Stream: false, Success: true, Status: 200, DurationMs: 10.0,
	}
	aud.ApplyUsage(usagemeta.Usage{
		Prompt: 1_000_000, Completion: 500_000, Total: 1_500_000,
		Cached: 200_000, CacheWrite: 50_000, Source: usagemeta.SourceOpenAI,
	})
	middleware.EmitAudit(aud)
	rec.Close() // drain buffered events (also closes the store)

	// Recorder.Close closes its store; verify through a fresh store on the
	// same file, like the internal/usage tests do.
	check := usage.NewSQLiteStore(dbPath)
	if err := check.Open(); err != nil {
		t.Fatalf("reopen for verification: %v", err)
	}
	defer check.Close()
	rows, err := check.Summary(context.Background(), usage.SummaryQuery{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Requests != 1 {
		t.Errorf("requests = %d, want 1", row.Requests)
	}
	if row.TotalTokens != 1_500_000 {
		t.Errorf("total_tokens = %d, want 1500000", row.TotalTokens)
	}
	// cost = (1M-0.2M)/1e6*1.0 + 0.2M/1e6*0.2 + 0.05M/1e6*0.5 + 0.5M/1e6*2.0
	//      = 0.8 + 0.04 + 0.025 + 1.0 = 1.865
	want := 1.865
	if row.CostUSD == nil {
		t.Fatal("cost must be non-nil (price was resolved from config)")
	}
	if math.Abs(*row.CostUSD-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", *row.CostUSD, want)
	}
	if row.CostMissingRequests != 0 {
		t.Errorf("cost_missing_requests = %d, want 0", row.CostMissingRequests)
	}
	if row.CostUSD == nil || math.Abs(*row.CostUSD) < 1e-12 {
		t.Fatalf("acceptance: DB cost_usd must be non-zero, got %v", row.CostUSD)
	}

	// The audit log line must carry the SAME amount (single pricing point):
	// the log and the database cannot diverge.
	var logCost float64
	logCost = math.NaN()
	for _, line := range strings.Split(logBuf.String(), "\n") {
		if !strings.Contains(line, "request.complete") {
			continue
		}
		var parsed struct {
			CostUSD float64 `json:"cost_usd"`
		}
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("parse audit line: %v", err)
		}
		logCost = parsed.CostUSD
		break
	}
	if logCost == 0 || math.IsNaN(logCost) {
		t.Fatalf("audit log must carry a non-zero cost_usd; log:\n%s", logBuf.String())
	}
	if math.Abs(logCost-*row.CostUSD) > 1e-12 {
		t.Errorf("audit log cost_usd = %v, DB cost_usd = %v, want identical values", logCost, *row.CostUSD)
	}
}

// TestUsageAdapter_MissingPriceLogAndDBNil: with no model_metadata price, the
// pricer returns nil cost + missing_price: the DB row must store NULL with
// missing_price and the log line must carry cost_status missing_price (no
// amount). Guards the "单价查不到时仍然是 cost 为 NULL 加 missing_price 状态"
// constraint after the pricing move.
func TestUsageAdapter_MissingPriceLogAndDBNil(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	cfg := testConfig(t, func(c *config.Config) {
		c.Usage.Enabled = true
		c.Usage.DBPath = dbPath
		c.Usage.BatchSize = 1
		// no ModelMetadata: the model has no known price
	})
	holder := config.NewConfigHolder(cfg)
	rec, _ := startUsageRecorder(cfg)
	if rec == nil {
		t.Fatal("recorder must start with a writable DBPath")
	}
	defer rec.Close()

	adapter := &usageRecorderAdapter{holder: holder, rec: rec}
	middleware.SetUsageRecorder(adapter)
	middleware.SetUsagePricer(adapter.Price)
	defer middleware.SetUsageRecorder(nil)
	defer middleware.SetUsagePricer(nil)

	var logBuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(old)

	aud := &middleware.RequestAudit{
		Req: "req-2", Method: "POST", Path: "/v1/chat/completions",
		Model: "unpriced-model", Provider: "p", KeyID: "k",
		Success: true, Status: 200,
	}
	aud.ApplyUsage(usagemeta.Usage{Prompt: 100, Completion: 50, Total: 150, Source: usagemeta.SourceOpenAI})
	middleware.EmitAudit(aud)
	rec.Close()

	check := usage.NewSQLiteStore(dbPath)
	if err := check.Open(); err != nil {
		t.Fatalf("reopen for verification: %v", err)
	}
	defer check.Close()
	rows, err := check.Summary(context.Background(), usage.SummaryQuery{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(rows) != 1 || rows[0].CostMissingRequests != 1 {
		t.Fatalf("rows = %+v, want 1 row counted as missing-price", rows)
	}
	if rows[0].CostUSD != nil {
		t.Errorf("cost_usd = %v, want NULL for a model without price", rows[0].CostUSD)
	}

	if !strings.Contains(logBuf.String(), `"cost_status":"missing_price"`) {
		t.Errorf("audit log must carry cost_status missing_price; log:\n%s", logBuf.String())
	}
	// slog emits the cost_usd attribute unconditionally; for a missing price
	// its value must be 0 (no amount) while the DB stores NULL — both sides
	// agree there is no cost.
	var parsed struct {
		CostUSD float64 `json:"cost_usd"`
	}
	for _, line := range strings.Split(logBuf.String(), "\n") {
		if strings.Contains(line, "request.complete") {
			if err := json.Unmarshal([]byte(line), &parsed); err != nil {
				t.Fatalf("parse audit line: %v", err)
			}
			break
		}
	}
	if parsed.CostUSD != 0 {
		t.Errorf("audit log cost_usd = %v, want 0 for a missing price; log:\n%s", parsed.CostUSD, logBuf.String())
	}
}

// ---------------------------------------------------------------------------
// /metrics loopback auth (fail-closed): with METRICS_TOKEN configured every
// request must present it, loopback included; only when it is unset does a
// direct loopback request (no X-Forwarded-For / X-Real-IP) pass without a
// token.
// ---------------------------------------------------------------------------

func TestHTTPHandler_MetricsRequiresTokenBehindForwardHeaders(t *testing.T) {
	t.Setenv("METRICS_TOKEN", "metrics-sekret")
	cfg := testConfig(t, nil)
	h := newHTTPHandler(config.NewConfigHolder(cfg), &fakeProxy{status: http.StatusOK}, nil, nil, usage.NewSummaryHandler(nil))

	// Fail-closed: direct loopback without forwarding headers and WITHOUT a
	// token is 401 once METRICS_TOKEN is configured.
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("direct loopback /metrics without token: status = %d, want 401 (fail-closed when METRICS_TOKEN is set)", rr.Code)
	}

	// The same direct loopback request WITH the token is allowed.
	reqOK := httptest.NewRequest("GET", "/metrics", nil)
	reqOK.RemoteAddr = "127.0.0.1:12345"
	reqOK.Header.Set("Authorization", "Bearer metrics-sekret")
	rrOK := httptest.NewRecorder()
	h.ServeHTTP(rrOK, reqOK)
	if rrOK.Code != http.StatusOK {
		t.Errorf("direct loopback /metrics with token: status = %d, want 200", rrOK.Code)
	}

	// Loopback + X-Forwarded-For (same-machine reverse proxy): token required.
	for name, hdr := range map[string]string{"X-Forwarded-For": "10.0.0.9", "X-Real-IP": "10.0.0.9"} {
		req2 := httptest.NewRequest("GET", "/metrics", nil)
		req2.RemoteAddr = "127.0.0.1:12345"
		req2.Header.Set(name, hdr)
		rr2 := httptest.NewRecorder()
		h.ServeHTTP(rr2, req2)
		if rr2.Code != http.StatusUnauthorized {
			t.Errorf("loopback /metrics with %s and no token: status = %d, want 401", name, rr2.Code)
		}

		// Same request with the token: allowed.
		req3 := httptest.NewRequest("GET", "/metrics", nil)
		req3.RemoteAddr = "127.0.0.1:12345"
		req3.Header.Set(name, hdr)
		req3.Header.Set("Authorization", "Bearer metrics-sekret")
		rr3 := httptest.NewRecorder()
		h.ServeHTTP(rr3, req3)
		if rr3.Code != http.StatusOK {
			t.Errorf("loopback /metrics with %s and token: status = %d, want 200", name, rr3.Code)
		}
	}
}

// TestHTTPHandler_MetricsLoopbackNoTokenNoHeader401 pins the exact
// acceptance case from the fail-closed review: loopback RemoteAddr +
// METRICS_TOKEN configured + no forwarding headers + no auth => 401.
func TestHTTPHandler_MetricsLoopbackNoTokenNoHeader401(t *testing.T) {
	t.Setenv("METRICS_TOKEN", "metrics-sekret")
	cfg := testConfig(t, nil)
	h := newHTTPHandler(config.NewConfigHolder(cfg), &fakeProxy{status: http.StatusOK}, nil, nil, usage.NewSummaryHandler(nil))

	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:12345" // loopback, no X-Forwarded-For / X-Real-IP
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("loopback + METRICS_TOKEN configured + no header + no auth: status = %d, want 401", rr.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Error.Code != "unauthorized" {
		t.Errorf("body must be the standard unauthorized envelope, got %q", rr.Body.String())
	}
}

// TestHTTPHandler_MetricsRemoteNoTokenDenied guards the fail-closed side:
// a remote /metrics request without METRICS_TOKEN is 401 even with no
// forwarding headers, while a direct loopback request still passes (auth
// disabled) and a loopback request carrying a forwarding header is denied.
func TestHTTPHandler_MetricsRemoteNoTokenDenied(t *testing.T) {
	t.Setenv("METRICS_TOKEN", "")
	cfg := testConfig(t, nil)
	h := newHTTPHandler(config.NewConfigHolder(cfg), &fakeProxy{status: http.StatusOK}, nil, nil, usage.NewSummaryHandler(nil))

	req := httptest.NewRequest("GET", "/metrics", nil) // RemoteAddr is non-loopback by default
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("remote /metrics without token: status = %d, want 401", rr.Code)
	}

	// Token unset + direct loopback: allowed (the only token-free path).
	reqLoop := httptest.NewRequest("GET", "/metrics", nil)
	reqLoop.RemoteAddr = "127.0.0.1:12345"
	rrLoop := httptest.NewRecorder()
	h.ServeHTTP(rrLoop, reqLoop)
	if rrLoop.Code != http.StatusOK {
		t.Errorf("direct loopback /metrics with unset token: status = %d, want 200", rrLoop.Code)
	}

	// Token unset + loopback with a forwarding header: denied (proxied
	// request cannot be distinguished from remote without a token).
	reqFwd := httptest.NewRequest("GET", "/metrics", nil)
	reqFwd.RemoteAddr = "127.0.0.1:12345"
	reqFwd.Header.Set("X-Forwarded-For", "10.0.0.9")
	rrFwd := httptest.NewRecorder()
	h.ServeHTTP(rrFwd, reqFwd)
	if rrFwd.Code != http.StatusUnauthorized {
		t.Errorf("loopback /metrics with forwarding header and unset token: status = %d, want 401", rrFwd.Code)
	}
}
