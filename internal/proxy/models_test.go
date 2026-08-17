package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/cache"
	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

// newModelCacheWithMeta writes a provider cache file (with upstream meta) to a
// temp dir and loads it into a ModelCache so GetModelMeta returns the meta.
func newModelCacheWithMeta(t *testing.T, provider string, models []cache.ModelEntry, meta map[string]cache.ModelMeta) *cache.ModelCache {
	t.Helper()
	dir := t.TempDir()
	pc := struct {
		Models    []cache.ModelEntry         `json:"models"`
		Meta      map[string]cache.ModelMeta `json:"meta,omitempty"`
		UpdatedAt string                     `json:"updated_at"`
	}{Models: models, Meta: meta, UpdatedAt: "2024-01-01T00:00:00Z"}
	data, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, provider+".json"), data, 0644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "a", Provider: provider, BaseURL: "https://x.com/v1"}},
	}
	mc, err := cache.New(dir, pool.NewPool(nil), cfg)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	mc.LoadFromDisk()
	return mc
}

func findModel(data []map[string]any, id string) map[string]any {
	for _, m := range data {
		if m["id"] == id {
			return m
		}
	}
	return nil
}

func intPtr(i int) *int    { return &i }
func boolPtr(b bool) *bool { return &b }

// TestProxyModels_ReturnsUpstreamMeta verifies /v1/models returns upstream
// metadata in snake_case when present, even without config metadata.
func TestProxyModels_ReturnsUpstreamMeta(t *testing.T) {
	mc := newModelCacheWithMeta(t, "p",
		[]cache.ModelEntry{{ID: "glm-5.2", Object: "model", Created: 1, OwnedBy: "p"}},
		map[string]cache.ModelMeta{"glm-5.2": {ContextWindow: intPtr(1000000)}},
	)
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "a", Provider: "p", BaseURL: "https://x.com/v1"}}}

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("X-Prism-Provider", "p")
	w := httptest.NewRecorder()

	proxyModels(mc, w, r, cfg)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := findModel(resp.Data, "glm-5.2")
	if m == nil {
		t.Fatalf("glm-5.2 not in response: %v", resp.Data)
	}
	// snake_case, not camelCase
	cw, ok := m["context_window"].(float64)
	if !ok {
		t.Fatalf("context_window (snake_case) missing: %v", m)
	}
	if int(cw) != 1000000 {
		t.Fatalf("context_window = %v, want 1000000", int(cw))
	}
	if _, ok := m["contextWindow"]; ok {
		t.Fatalf("camelCase contextWindow should NOT appear in /v1/models: %v", m)
	}
}

// TestProxyModels_ConfigOverridesUpstream verifies config metadata wins over
// upstream metadata for the same /v1/models field.
func TestProxyModels_ConfigOverridesUpstream(t *testing.T) {
	mc := newModelCacheWithMeta(t, "p",
		[]cache.ModelEntry{{ID: "glm-5.2", Object: "model", Created: 1, OwnedBy: "p"}},
		map[string]cache.ModelMeta{"glm-5.2": {ContextWindow: intPtr(128000)}},
	)
	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "a", Provider: "p", BaseURL: "https://x.com/v1"}},
		ModelMetadata: config.ModelMetadataMap{
			"glm-5.2": {ContextWindow: intPtr(1000000)},
		},
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("X-Prism-Provider", "p")
	w := httptest.NewRecorder()

	proxyModels(mc, w, r, cfg)

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := findModel(resp.Data, "glm-5.2")
	if m == nil {
		t.Fatalf("glm-5.2 not in response: %v", resp.Data)
	}
	cw, ok := m["context_window"].(float64)
	if !ok {
		t.Fatalf("context_window missing: %v", m)
	}
	if int(cw) != 1000000 {
		t.Fatalf("context_window = %v, want 1000000 (config override)", int(cw))
	}
}

// TestProxyModels_PerProviderNoCrossTalk verifies the same model resolved via
// different providers gets different metadata (T2): ollama-cloud deepseek-v4-pro
// reports upstream 512K (no per-provider context_window override), while
// opencode-go deepseek-v4-pro reports the default 1M config value.
func TestProxyModels_PerProviderNoCrossTalk(t *testing.T) {
	providers := []string{"ollama-cloud", "opencode-go"}
	caches := map[string]*cache.ModelCache{}
	for _, p := range providers {
		caches[p] = newModelCacheWithMeta(t, p,
			[]cache.ModelEntry{{ID: "deepseek-v4-pro", Object: "model", Created: 1, OwnedBy: p}},
			map[string]cache.ModelMeta{"deepseek-v4-pro": {ContextWindow: intPtr(512000)}},
		)
	}
	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a", Provider: "ollama-cloud", BaseURL: "https://ollama.com/v1"},
			{Name: "b", Provider: "opencode-go", BaseURL: "https://opencode.ai/zen"},
		},
		ModelMetadata: config.ModelMetadataMap{
			"deepseek-v4-pro": {ContextWindow: intPtr(1000000)},
		},
		ModelMetadataPerProvider: map[string]config.ModelMetadataMap{
			"ollama-cloud": {
				"deepseek-v4-pro": {Reasoning: boolPtr(true)},
			},
		},
	}

	for _, p := range providers {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r.Header.Set("X-Prism-Provider", p)
		w := httptest.NewRecorder()
		proxyModels(caches[p], w, r, cfg)

		if w.Code != http.StatusOK {
			t.Fatalf("provider %s: status = %d, want 200", p, w.Code)
		}
		var resp struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("provider %s: decode: %v", p, err)
		}
		m := findModel(resp.Data, "deepseek-v4-pro")
		if m == nil {
			t.Fatalf("provider %s: deepseek-v4-pro not in response", p)
		}
		cw, ok := m["context_window"].(float64)
		if !ok {
			t.Fatalf("provider %s: context_window missing: %v", p, m)
		}
		want := 512000
		if p == "opencode-go" {
			want = 1000000
		}
		if int(cw) != want {
			t.Fatalf("provider %s: context_window = %v, want %d", p, int(cw), want)
		}
	}
}

// TestEnrichModel_EmptyProviderFallsBackToDefault verifies enrichModel with an
// empty provider string uses the default layer, while a concrete provider uses
// its per-provider override.
func TestEnrichModel_EmptyProviderFallsBackToDefault(t *testing.T) {
	cfg := &config.Config{
		ModelMetadata: config.ModelMetadataMap{
			"deepseek-v4-pro": {ContextWindow: intPtr(1000000)},
		},
		ModelMetadataPerProvider: map[string]config.ModelMetadataMap{
			"ollama-cloud": {
				"deepseek-v4-pro": {ContextWindow: intPtr(512000)},
			},
		},
	}

	// Empty provider → default layer (1M).
	empty := enrichModel(map[string]any{"id": "deepseek-v4-pro"}, "", "deepseek-v4-pro", cfg)
	cw, ok := empty["context_window"].(int)
	if !ok || cw != 1000000 {
		t.Fatalf("empty provider context_window = %v (%T), want 1000000 (default)", empty["context_window"], empty["context_window"])
	}

	// Concrete provider ollama-cloud → per-provider override (512K).
	withP := enrichModel(map[string]any{"id": "deepseek-v4-pro"}, "ollama-cloud", "deepseek-v4-pro", cfg)
	pcw, ok := withP["context_window"].(int)
	if !ok || pcw != 512000 {
		t.Fatalf("ollama-cloud context_window = %v (%T), want 512000 (per-provider)", withP["context_window"], withP["context_window"])
	}
}

// X-Prism-Provider header, an empty list is returned.
func TestProxyModels_NoProviderHeader_Empty(t *testing.T) {
	mc := newModelCacheWithMeta(t, "p",
		[]cache.ModelEntry{{ID: "glm-5.2", Object: "model", Created: 1, OwnedBy: "p"}},
		map[string]cache.ModelMeta{"glm-5.2": {ContextWindow: intPtr(1000000)}},
	)
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "a", Provider: "p", BaseURL: "https://x.com/v1"}}}

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()

	proxyModels(mc, w, r, cfg)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 0 {
		t.Fatalf("data len = %d, want 0 (no provider header)", len(resp.Data))
	}
}

// TestProxyModels_NoProviderHeader_UsesDefaultProvider: a configured
// default_provider is the same fallback chat already uses. Clients that
// list models without X-Prism-Provider must see that provider's catalog.
func TestProxyModels_NoProviderHeader_UsesDefaultProvider(t *testing.T) {
	mc := newModelCacheWithMeta(t, "p",
		[]cache.ModelEntry{{ID: "glm-5.2", Object: "model", Created: 1, OwnedBy: "p"}},
		map[string]cache.ModelMeta{"glm-5.2": {ContextWindow: intPtr(1000000)}},
	)
	cfg := &config.Config{
		DefaultProvider: "p",
		Accounts:        []config.AccountConfig{{Name: "a", Provider: "p", BaseURL: "https://x.com/v1"}},
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	proxyModels(mc, w, r, cfg)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("data len = %d, want 1 (default_provider p)", len(resp.Data))
	}
	if resp.Data[0]["id"] != "glm-5.2" {
		t.Errorf("id = %v, want glm-5.2", resp.Data[0]["id"])
	}
}

// ---------------------------------------------------------------------------
// Batch-2 audit: cache-miss fetch failures are classified (503/502), never
// a 200 empty list
// ---------------------------------------------------------------------------

// emptyCacheMC builds a ModelCache with no on-disk cache (every lookup is a
// miss → fetch) over the given pool/config.
func emptyCacheMC(t *testing.T, p *pool.Pool, cfg *config.Config) *cache.ModelCache {
	t.Helper()
	mc, err := cache.New(t.TempDir(), p, cfg)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	return mc
}

func getModelsError(t *testing.T, mc *cache.ModelCache, cfg *config.Config) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("X-Prism-Provider", "p")
	w := httptest.NewRecorder()
	proxyModels(mc, w, r, cfg)
	return w.Code, decodeErrorCode(t, w.Body.String())
}

// TestProxyModels_FetchNoHealthy503: a cache miss over a provider whose only
// account is exhausted must answer 503 no_healthy, never 200 with an empty
// list (a client would cache "no models" forever).
func TestProxyModels_FetchNoHealthy503(t *testing.T) {
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "a1", Provider: "p", BaseURL: "http://localhost:1", Key: "k"}}}
	p := pool.NewPool(cfg.Accounts)
	p.AllAccounts()[0].MarkExhausted()
	mc := emptyCacheMC(t, p, cfg)

	status, code := getModelsError(t, mc, cfg)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no healthy account)", status)
	}
	if code != "no_healthy" {
		t.Errorf("error code = %q, want no_healthy", code)
	}
}

// TestProxyModels_FetchSaturated503: a cache miss while the provider's
// account is at its concurrency cap must answer 503 model_fetch_saturated.
func TestProxyModels_FetchSaturated503(t *testing.T) {
	cfg := &config.Config{
		Accounts:                []config.AccountConfig{{Name: "a1", Provider: "p", BaseURL: "http://localhost:1", Key: "k"}},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	p := pool.NewPool(cfg.Accounts)
	slot0 := p.AllAccounts()[0].TryAcquire("", 1)
	if slot0 == nil {
		t.Fatal("test setup: TryAcquire failed")
	}
	defer p.AllAccounts()[0].Release(slot0)
	mc := emptyCacheMC(t, p, cfg)

	status, code := getModelsError(t, mc, cfg)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (saturated)", status)
	}
	if code != "model_fetch_saturated" {
		t.Errorf("error code = %q, want model_fetch_saturated", code)
	}
}

// TestProxyModels_FetchAcquiredThenSaturated502 pins the final-review
// classification end-to-end: account A is acquired and its upstream fetch
// fails (500), account B is saturated. The mixed failure must answer 502
// model_fetch_failed (the fetch really reached an upstream and failed) —
// NOT 503 model_fetch_saturated, which is reserved for the case where no
// account could be acquired at all.
func TestProxyModels_FetchAcquiredThenSaturated502(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv1.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv1.URL, Key: "k"},
			{Name: "a2", Provider: "p", BaseURL: "http://127.0.0.1:1", Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	p := pool.NewPool(cfg.Accounts)
	// Occupy a2's single slot so only a1 can be acquired (and then fails).
	slot1 := p.AllAccounts()[1].TryAcquire("", 1)
	if slot1 == nil {
		t.Fatal("test setup: TryAcquire failed")
	}
	defer p.AllAccounts()[1].Release(slot1)
	mc := emptyCacheMC(t, p, cfg)

	status, code := getModelsError(t, mc, cfg)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (acquired-then-failed must not be saturated)", status)
	}
	if code != "model_fetch_failed" {
		t.Errorf("error code = %q, want model_fetch_failed", code)
	}
}

// TestProxyModels_FetchUpstreamError502: a cache miss whose upstream fetch
// fails (5xx here) must answer 502 model_fetch_failed.
func TestProxyModels_FetchUpstreamError502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "a1", Provider: "p", BaseURL: upstream.URL, Key: "k"}}}
	p := pool.NewPool(cfg.Accounts)
	mc := emptyCacheMC(t, p, cfg)

	status, code := getModelsError(t, mc, cfg)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (upstream error)", status)
	}
	if code != "model_fetch_failed" {
		t.Errorf("error code = %q, want model_fetch_failed", code)
	}
}

// TestProxyModels_FetchParseError502: a 200 upstream body that is not valid
// JSON must answer 502 model_fetch_failed (parse error).
func TestProxyModels_FetchParseError502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json at all`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "a1", Provider: "p", BaseURL: upstream.URL, Key: "k"}}}
	p := pool.NewPool(cfg.Accounts)
	mc := emptyCacheMC(t, p, cfg)

	status, code := getModelsError(t, mc, cfg)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (parse error)", status)
	}
	if code != "model_fetch_failed" {
		t.Errorf("error code = %q, want model_fetch_failed", code)
	}
}

// TestProxyModels_FetchSizeError502: a 200 upstream body larger than the
// configured max_upstream_response_bytes must answer 502 model_fetch_failed
// (size error), not buffer the whole body.
func TestProxyModels_FetchSizeError502(t *testing.T) {
	big := `{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x","padding":"` + strings.Repeat("a", 4096) + `"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(big))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Accounts:                 []config.AccountConfig{{Name: "a1", Provider: "p", BaseURL: upstream.URL, Key: "k"}},
		MaxUpstreamResponseBytes: 512,
	}
	p := pool.NewPool(cfg.Accounts)
	mc := emptyCacheMC(t, p, cfg)

	status, code := getModelsError(t, mc, cfg)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (size error)", status)
	}
	if code != "model_fetch_failed" {
		t.Errorf("error code = %q, want model_fetch_failed", code)
	}
}

// TestProxyModels_CancelledRequestWritesNothing pins the request-context
// wiring of the cache-miss path: /v1/models waits on the REQUEST context
// (ModelCache.FetchWithContext), so a client that disconnects while the
// fetch is running gets NO response at all — neither a 502 error nor a 200
// empty list (a client would cache "no models" forever). The shared fetch
// work itself is NOT cancelled with the request (it runs on the cache's own
// work context) — a concurrent request still gets the result.
func TestProxyModels_CancelledRequestWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"fetch failure", "boom"},                    // upstream 500
		{"empty upstream list", `{"object":"list"}`}, // 200 without a data key → cache publishes no models
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-entered:
				default:
					close(entered)
				}
				<-release
				if tc.name == "fetch failure" {
					w.WriteHeader(http.StatusInternalServerError)
				} else {
					w.WriteHeader(http.StatusOK)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()

			cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "a1", Provider: "p", BaseURL: upstream.URL, Key: "k"}}}
			p := pool.NewPool(cfg.Accounts)
			mc := emptyCacheMC(t, p, cfg)

			ctx, cancel := context.WithCancel(context.Background())
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
			r.Header.Set("X-Prism-Provider", "p")
			w := httptest.NewRecorder()

			done := make(chan struct{})
			go func() {
				proxyModels(mc, w, r, cfg)
				close(done)
			}()

			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("fetch never reached the upstream")
			}
			cancel()
			close(release)

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("proxyModels never returned")
			}
			// Nothing may reach the wire: no body, no headers (WriteJSON sets
			// Content-Type before the status, so any written response carries
			// a header; the recorder's Code stays at its initial 200).
			if w.Body.Len() != 0 || len(w.Header()) != 0 {
				t.Errorf("cancelled request must not write a response, got headers=%v body=%q", w.Header(), w.Body.String())
			}
		})
	}
}

// TestProxyModels_FetchSucceedsOnMiss guards the happy path: a cache miss
// with a healthy account fetches successfully and returns the models with
// 200 (the new error mapping must not break the success case).
func TestProxyModels_FetchSucceedsOnMiss(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "a1", Provider: "p", BaseURL: upstream.URL, Key: "k"}}}
	p := pool.NewPool(cfg.Accounts)
	mc := emptyCacheMC(t, p, cfg)

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("X-Prism-Provider", "p")
	w := httptest.NewRecorder()
	proxyModels(mc, w, r, cfg)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0]["id"] != "m1" {
		t.Errorf("data = %v, want [m1]", resp.Data)
	}
}

// TestProxyModels_FetchFailureConcurrentMissNo200Empty pins the final-review
// must-fix end-to-end: two concurrent /v1/models cache misses for the same
// provider collapse into ONE leader fetch (the second request joins the
// in-flight entry). When the upstream fails (500 here), BOTH requests must
// answer 502 model_fetch_failed — never 200 with data=[] (a client would
// cache "no models" forever). Before the fix the leader's normal failure
// was never published to followers, so the joined request read a nil error
// and answered 200 with an empty list.
//
// Determinism: the leader request is provably blocked at the upstream
// (entered) while the second request starts, so the in-flight entry is
// still registered and the second request MUST join it. Release happens
// only after the second request is launched, and the upstream adds a slow
// tail after release (200ms before the 500), so the leader cannot finish
// before the follower has parked — the hits == 1 assertion proves the join.
func TestProxyModels_FetchFailureConcurrentMissNo200Empty(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		time.Sleep(200 * time.Millisecond) // slow tail: see the comment above
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "a1", Provider: "p", BaseURL: upstream.URL, Key: "k"}}}
	p := pool.NewPool(cfg.Accounts)
	mc := emptyCacheMC(t, p, cfg)

	type result struct {
		status int
		code   string
		body   string
	}
	doRequest := func() result {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r.Header.Set("X-Prism-Provider", "p")
		w := httptest.NewRecorder()
		proxyModels(mc, w, r, cfg)
		return result{status: w.Code, code: decodeErrorCode(t, w.Body.String()), body: w.Body.String()}
	}

	// First request (leader): provably blocked at the upstream.
	firstCh := make(chan result, 1)
	go func() { firstCh <- doRequest() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader request never reached the upstream")
	}

	// Second request: the cache is still empty and the leader's in-flight
	// entry is registered, so it MUST join and share the leader's failure.
	secondStarted := make(chan struct{})
	secondCh := make(chan result, 1)
	go func() {
		close(secondStarted)
		secondCh <- doRequest()
	}()
	select {
	case <-secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("second request never started")
	}
	close(release)

	var r1, r2 result
	select {
	case r1 = <-firstCh:
	case <-time.After(10 * time.Second):
		t.Fatal("first request never completed")
	}
	select {
	case r2 = <-secondCh:
	case <-time.After(10 * time.Second):
		t.Fatal("second request never completed")
	}

	if r1.status != http.StatusBadGateway || r1.code != "model_fetch_failed" {
		t.Errorf("leader response = %d code %q, want 502 model_fetch_failed", r1.status, r1.code)
	}
	// The regression: the follower request must NOT answer 200 data=[] — a
	// cache-miss fetch failure must never look like a successful empty list.
	if r2.status != http.StatusBadGateway || r2.code != "model_fetch_failed" {
		t.Errorf("follower response = %d code %q body %q, want 502 model_fetch_failed (never 200 with an empty list)", r2.status, r2.code, r2.body)
	}
	// Exactly one upstream request: the second request must have joined the
	// leader, not issued its own fetch.
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (the second request must join the leader, not refetch)", got)
	}
}
