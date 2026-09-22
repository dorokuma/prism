package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/cache"
	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

// ---- Aggregate entry (provider_routing: auto) ----

type fakeUsageRecorder struct {
	mu  sync.Mutex
	evs []middleware.UsageEvent
}

func (f *fakeUsageRecorder) Record(e middleware.UsageEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evs = append(f.evs, e)
}

func (f *fakeUsageRecorder) events() []middleware.UsageEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]middleware.UsageEvent(nil), f.evs...)
}

func writeProviderCacheFile(t *testing.T, dir, provider string, models []cache.ModelEntry) {
	t.Helper()
	pc := struct {
		Models    []cache.ModelEntry `json:"models"`
		UpdatedAt string             `json:"updated_at"`
	}{Models: models, UpdatedAt: "2024-01-01T00:00:00Z"}
	data, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, provider+".json"), data, 0644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
}

func aggregateTestCache(t *testing.T, cfg *config.Config, modelsByProvider map[string][]cache.ModelEntry) *cache.ModelCache {
	t.Helper()
	dir := t.TempDir()
	for provider, models := range modelsByProvider {
		writeProviderCacheFile(t, dir, provider, models)
	}
	mc, err := cache.New(dir, pool.NewPool(nil), cfg)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	mc.LoadFromDisk()
	return mc
}

func aggregateAccounts() []config.AccountConfig {
	return []config.AccountConfig{
		{Name: "xai-acc", Provider: "xai", BaseURL: "https://x.ai/v1"},
		{Name: "gemini-acc", Provider: "gemini", BaseURL: "https://g.ai/v1"},
	}
}

func aggregateConfig(extra func(*config.Config)) *config.Config {
	cfg := &config.Config{
		ProviderRouting:  "auto",
		ProviderPriority: []string{"xai", "gemini"},
		Accounts:         aggregateAccounts(),
	}
	if extra != nil {
		extra(cfg)
	}
	return cfg
}

func okChatHandler(hits *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}
}

func TestProxyModels_AggregateUnion(t *testing.T) {
	cfg := aggregateConfig(nil)
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai":    {{ID: "grok-4.20", Object: "model", Created: 1, OwnedBy: "xai"}},
		"gemini": {{ID: "gemini-2.5-pro", Object: "model", Created: 2, OwnedBy: "gemini"}},
	})

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
	if len(resp.Data) != 2 {
		t.Fatalf("aggregate data = %v, want 2 entries", resp.Data)
	}
	if findModel(resp.Data, "grok-4.20") == nil || findModel(resp.Data, "gemini-2.5-pro") == nil {
		t.Fatalf("union missing models: %v", resp.Data)
	}
}

func TestProxyModels_AggregateExcludesAmbiguous(t *testing.T) {
	util.MetricsAggregateAmbiguousModels.Set(0)
	cfg := aggregateConfig(func(c *config.Config) { c.ProviderPriority = nil })
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai":    {{ID: "only-xai"}, {ID: "shared-1"}},
		"gemini": {{ID: "shared-1"}},
	})

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
	if len(resp.Data) != 1 || findModel(resp.Data, "shared-1") != nil {
		t.Fatalf("ambiguous model must be excluded: %v", resp.Data)
	}
	if got := util.MetricsAggregateAmbiguousModels.Value(); got != 1 {
		t.Fatalf("aggregate_ambiguous_models = %d, want 1", got)
	}
}

func TestProxyModels_AggregateOffKeepsOldBehavior(t *testing.T) {
	cfg := aggregateConfig(func(c *config.Config) { c.ProviderRouting = "" })
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai": {{ID: "grok-4.20"}},
	})

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	proxyModels(mc, w, r, cfg)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	// No header, no default_provider, aggregate off -> empty 200 list (old behavior).
	if len(resp.Data) != 0 {
		t.Fatalf("aggregate off must keep empty list, got %v", resp.Data)
	}
}

func TestProxyChat_AggregateResolvesToWinnerProvider(t *testing.T) {
	cfg := aggregateConfig(nil)
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai":    {{ID: "grok-4.20"}},
		"gemini": {{ID: "gemini-2.5-pro"}},
	})

	var xaiHits, geminiHits int32
	upXai := httptest.NewServer(okChatHandler(&xaiHits))
	defer upXai.Close()
	upGemini := httptest.NewServer(okChatHandler(&geminiHits))
	defer upGemini.Close()
	pool := pool.NewPool([]config.AccountConfig{
		{Name: "xai-acc", Provider: "xai", BaseURL: upXai.URL},
		{Name: "gemini-acc", Provider: "gemini", BaseURL: upGemini.URL},
	})

	send := func(model string) *httptest.ResponseRecorder {
		body := []byte(`{"model":"` + model + `"}`)
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(withModelCache(r.Context(), mc))
		rec := httptest.NewRecorder()
		proxyChatWithBody(pool, rec, r, body, time.Now(), ChatForwardOpts{Model: model}, cfg)
		return rec
	}

	if rec := send("grok-4.20"); rec.Code != http.StatusOK {
		t.Fatalf("grok-4.20 status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := send("gemini-2.5-pro"); rec.Code != http.StatusOK {
		t.Fatalf("gemini-2.5-pro status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&xaiHits); got != 1 {
		t.Fatalf("xai upstream hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&geminiHits); got != 1 {
		t.Fatalf("gemini upstream hits = %d, want 1", got)
	}
}

func TestProxyChat_AggregateAmbiguousRejects(t *testing.T) {
	cfg := aggregateConfig(func(c *config.Config) { c.ProviderPriority = nil })
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai":    {{ID: "shared-1"}},
		"gemini": {{ID: "shared-1"}},
	})
	pool := pool.NewPool(aggregateAccounts())

	body := []byte(`{"model":"shared-1"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withModelCache(r.Context(), mc))
	rec := httptest.NewRecorder()
	proxyChatWithBody(pool, rec, r, body, time.Now(), ChatForwardOpts{Model: "shared-1"}, cfg)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "ambiguous_provider" {
		t.Fatalf("error code = %q, want ambiguous_provider", code)
	}
	if !strings.Contains(rec.Body.String(), "shared-1") {
		t.Fatalf("error must name the model: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "candidates") {
		t.Fatalf("error response must NOT contain candidates detail: %s", rec.Body.String())
	}
	for _, p := range []string{"xai", "gemini"} {
		if strings.Contains(rec.Body.String(), p) {
			t.Fatalf("error response must NOT leak candidate provider name %q: %s", p, rec.Body.String())
		}
	}
}

func TestProxyChat_AggregateUnknownWithoutDefaultRejects(t *testing.T) {
	cfg := aggregateConfig(nil)
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai": {{ID: "grok-4.20"}},
	})
	pool := pool.NewPool(aggregateAccounts())

	body := []byte(`{"model":"nope"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withModelCache(r.Context(), mc))
	rec := httptest.NewRecorder()
	proxyChatWithBody(pool, rec, r, body, time.Now(), ChatForwardOpts{Model: "nope"}, cfg)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "unknown_model" {
		t.Fatalf("error code = %q, want unknown_model", code)
	}
}

func TestProxyChat_AggregateUnknownFallsBackToDefaultProvider(t *testing.T) {
	cfg := aggregateConfig(func(c *config.Config) { c.DefaultProvider = "xai" })
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai": {{ID: "grok-4.20"}},
	})

	var xaiHits int32
	upXai := httptest.NewServer(okChatHandler(&xaiHits))
	defer upXai.Close()
	pool := pool.NewPool([]config.AccountConfig{
		{Name: "xai-acc", Provider: "xai", BaseURL: upXai.URL},
		{Name: "gemini-acc", Provider: "gemini", BaseURL: "https://g.ai/v1"},
	})

	body := []byte(`{"model":"nope"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withModelCache(r.Context(), mc))
	rec := httptest.NewRecorder()
	proxyChatWithBody(pool, rec, r, body, time.Now(), ChatForwardOpts{Model: "nope"}, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 via default_provider", rec.Code)
	}
	if got := atomic.LoadInt32(&xaiHits); got != 1 {
		t.Fatalf("xai upstream hits = %d, want 1 (default_provider)", got)
	}
}

func TestProxyChat_AggregatePinHeaderBypassesRegistry(t *testing.T) {
	cfg := aggregateConfig(nil)
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai":    {{ID: "grok-4.20"}},
		"gemini": {{ID: "grok-4.20"}},
	})

	var geminiHits int32
	upGemini := httptest.NewServer(okChatHandler(&geminiHits))
	defer upGemini.Close()
	pool := pool.NewPool([]config.AccountConfig{
		{Name: "xai-acc", Provider: "xai", BaseURL: "https://x.ai/v1"},
		{Name: "gemini-acc", Provider: "gemini", BaseURL: upGemini.URL},
	})

	// Explicit pin wins even though the registry would pick xai (priority).
	body := []byte(`{"model":"grok-4.20"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "gemini")
	r = r.WithContext(withModelCache(r.Context(), mc))
	rec := httptest.NewRecorder()
	proxyChatWithBody(pool, rec, r, body, time.Now(), ChatForwardOpts{Model: "grok-4.20"}, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&geminiHits); got != 1 {
		t.Fatalf("gemini upstream hits = %d, want 1 (explicit pin)", got)
	}
}

func TestProxyModels_AggregateNilMC(t *testing.T) {
	cfg := aggregateConfig(nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	proxyModels(nil, w, r, cfg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 0 {
		t.Fatalf("expected empty data, got %v", resp.Data)
	}
}

func TestProxyModels_AggregatePureOverride(t *testing.T) {
	cfg := aggregateConfig(func(c *config.Config) {
		c.ModelProviderOverrides = map[string]string{
			"pure-override": "xai",
		}
	})
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai": {{ID: "grok-4.20"}},
	})
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
	if findModel(resp.Data, "pure-override") == nil {
		t.Fatalf("pure-override model missing from /v1/models: %v", resp.Data)
	}
}

func TestProxyChat_AggregateMissingModel(t *testing.T) {
	cfg := aggregateConfig(nil)
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai": {{ID: "grok-4.20"}},
	})
	pool := pool.NewPool(aggregateAccounts())

	fake := &fakeUsageRecorder{}
	middleware.SetUsageRecorder(fake)
	defer middleware.SetUsageRecorder(nil)

	body := []byte(`{"model":""}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withModelCache(r.Context(), mc))
	rec := httptest.NewRecorder()
	proxyChatWithBody(pool, rec, r, body, time.Now(), ChatForwardOpts{Model: ""}, cfg)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "missing_model" {
		t.Fatalf("error code = %q, want missing_model", code)
	}
	if !strings.Contains(rec.Body.String(), "model is required") {
		t.Fatalf("error message must contain 'model is required': %s", rec.Body.String())
	}
	evts := fake.events()
	if len(evts) != 1 {
		t.Fatalf("recorded events = %d, want 1", len(evts))
	}
	if evts[0].Model != "<unknown>" {
		t.Fatalf("recorded model = %q, want <unknown>", evts[0].Model)
	}
}

func TestProxyMessages_AggregateAuto(t *testing.T) {
	cfg := aggregateConfig(nil)
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai":    {{ID: "claude-3-5-sonnet"}},
		"gemini": {{ID: "gemini-2.5-pro"}},
	})

	var xaiHits int32
	upXai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&xaiHits, 1)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upXai.Close()

	pool := pool.NewPool([]config.AccountConfig{
		{Name: "xai-acc", Provider: "xai", BaseURL: upXai.URL},
		{Name: "gemini-acc", Provider: "gemini", BaseURL: "https://g.ai/v1"},
	})

	// Resolved routing test
	body := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`)
	r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withModelCache(r.Context(), mc))
	rec := httptest.NewRecorder()
	proxyMessages(pool, rec, r, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&xaiHits); got != 1 {
		t.Fatalf("xai upstream hits = %d, want 1", got)
	}

	// Ambiguous provider test
	cfgAmbig := aggregateConfig(func(c *config.Config) { c.ProviderPriority = nil })
	mcAmbig := aggregateTestCache(t, cfgAmbig, map[string][]cache.ModelEntry{
		"xai":    {{ID: "shared-msg-model"}},
		"gemini": {{ID: "shared-msg-model"}},
	})
	bodyAmbig := []byte(`{"model":"shared-msg-model","messages":[{"role":"user","content":"hi"}]}`)
	rAmbig := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bodyAmbig))
	rAmbig.Header.Set("Content-Type", "application/json")
	rAmbig = rAmbig.WithContext(withModelCache(rAmbig.Context(), mcAmbig))
	recAmbig := httptest.NewRecorder()
	proxyMessages(pool, recAmbig, rAmbig, cfgAmbig)

	if recAmbig.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous messages status = %d, want 400", recAmbig.Code)
	}
	if code := decodeErrorCode(t, recAmbig.Body.String()); code != "ambiguous_provider" {
		t.Fatalf("ambiguous messages error code = %q, want ambiguous_provider", code)
	}
	if strings.Contains(recAmbig.Body.String(), "candidates") {
		t.Fatalf("ambiguous messages response should not have candidates: %s", recAmbig.Body.String())
	}
	for _, p := range []string{"xai", "gemini"} {
		if strings.Contains(recAmbig.Body.String(), p) {
			t.Fatalf("ambiguous messages response must NOT leak candidate provider name %q: %s", p, recAmbig.Body.String())
		}
	}
}

func TestProxyResponses_AggregateAuto(t *testing.T) {
	cfg := aggregateConfig(nil)
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai":    {{ID: "grok-4.20"}},
		"gemini": {{ID: "gemini-2.5-pro"}},
	})

	var xaiHits int32
	upXai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&xaiHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"chatcmpl-1","model":"grok-4.20","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upXai.Close()

	pool := pool.NewPool([]config.AccountConfig{
		{Name: "xai-acc", Provider: "xai", BaseURL: upXai.URL},
		{Name: "gemini-acc", Provider: "gemini", BaseURL: "https://g.ai/v1"},
	})

	// Resolved routing test
	body := []byte(`{"model":"grok-4.20","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withModelCache(r.Context(), mc))
	rec := httptest.NewRecorder()
	proxyResponses(pool, rec, r, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&xaiHits); got != 1 {
		t.Fatalf("xai upstream hits = %d, want 1", got)
	}

	// Ambiguous provider test
	cfgAmbig := aggregateConfig(func(c *config.Config) { c.ProviderPriority = nil })
	mcAmbig := aggregateTestCache(t, cfgAmbig, map[string][]cache.ModelEntry{
		"xai":    {{ID: "shared-resp-model"}},
		"gemini": {{ID: "shared-resp-model"}},
	})
	bodyAmbig := []byte(`{"model":"shared-resp-model","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	rAmbig := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(bodyAmbig))
	rAmbig.Header.Set("Content-Type", "application/json")
	rAmbig = rAmbig.WithContext(withModelCache(rAmbig.Context(), mcAmbig))
	recAmbig := httptest.NewRecorder()
	proxyResponses(pool, recAmbig, rAmbig, cfgAmbig)

	if recAmbig.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous responses status = %d, want 400", recAmbig.Code)
	}
	if code := decodeErrorCode(t, recAmbig.Body.String()); code != "ambiguous_provider" {
		t.Fatalf("ambiguous responses error code = %q, want ambiguous_provider", code)
	}
	if strings.Contains(recAmbig.Body.String(), "candidates") {
		t.Fatalf("ambiguous responses response should not have candidates: %s", recAmbig.Body.String())
	}
	for _, p := range []string{"xai", "gemini"} {
		if strings.Contains(recAmbig.Body.String(), p) {
			t.Fatalf("ambiguous responses response must NOT leak candidate provider name %q: %s", p, recAmbig.Body.String())
		}
	}
}

func TestProxy_AggregateWithModelRemap(t *testing.T) {
	cfg := aggregateConfig(func(c *config.Config) {
		c.ModelRemapEnabled = true
		c.ModelRemap = map[string]string{
			"virtual-fast": "fast-tier",
		}
		c.ModelTiers = map[string]string{
			"fast-tier": "grok-4.20",
		}
	})
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai":    {{ID: "grok-4.20"}},
		"gemini": {{ID: "gemini-2.5-pro"}},
	})

	var xaiHits int32
	upXai := httptest.NewServer(okChatHandler(&xaiHits))
	defer upXai.Close()

	pool := pool.NewPool([]config.AccountConfig{
		{Name: "xai-acc", Provider: "xai", BaseURL: upXai.URL},
		{Name: "gemini-acc", Provider: "gemini", BaseURL: "https://g.ai/v1"},
	})

	// 1. Virtual model routes to physical model's provider
	body := []byte(`{"model":"virtual-fast"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withModelCache(r.Context(), mc))
	rec := httptest.NewRecorder()
	proxyChatWithBody(pool, rec, r, body, time.Now(), ChatForwardOpts{Model: "virtual-fast"}, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("virtual-fast status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&xaiHits); got != 1 {
		t.Fatalf("xai hits = %d, want 1", got)
	}

	// 2. /v1/models short-circuits when model_remap_enabled is true
	rModels := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	wModels := httptest.NewRecorder()
	proxyModels(mc, wModels, rModels, cfg)

	if wModels.Code != http.StatusOK {
		t.Fatalf("/v1/models status = %d, want 200", wModels.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(wModels.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	if findModel(resp.Data, "virtual-fast") == nil {
		t.Fatalf("virtual-fast should be returned by /v1/models under model_remap_enabled: %v", resp.Data)
	}
}

func TestProxy_AggregateModelRemapDecoupled(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	cfg := aggregateConfig(func(c *config.Config) {
		c.ModelRemapEnabled = true
		c.ModelRemap = map[string]string{
			"virtual-fast": "fast-tier",
		}
		c.ModelTiers = map[string]string{
			"fast-tier": "grok-4.20",
		}
		c.MaxConcurrentPerAccount = map[string]int{
			"virtual-fast": 7,
			"grok-4.20":    3,
		}
	})
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai": {{ID: "grok-4.20"}},
	})

	var receivedModel string
	upXai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		receivedModel, _ = util.RawStringField(raw, "model")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"grok-4.20","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upXai.Close()

	pool := pool.NewPool([]config.AccountConfig{
		{Name: "xai-acc", Provider: "xai", BaseURL: upXai.URL},
	})

	// Test case 1: model with leading and trailing spaces
	bodyWithSpaces := []byte(`{"model":"   virtual-fast   ","messages":[{"role":"user","content":"hi"}]}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bodyWithSpaces))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withModelCache(r.Context(), mc))
	rec := httptest.NewRecorder()
	proxyChatWithBody(pool, rec, r, bodyWithSpaces, time.Now(), ChatForwardOpts{Model: "   virtual-fast   "}, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if receivedModel != "grok-4.20" {
		t.Errorf("upstream received body model = %q, want grok-4.20", receivedModel)
	}

	out := h.output()
	if !strings.Contains(out, `"model":"virtual-fast"`) {
		t.Errorf("audit must keep requested virtual model (trimmed): %s", out)
	}
	if !strings.Contains(out, `"upstream_model":"grok-4.20"`) {
		t.Errorf("audit must record physical upstream model: %s", out)
	}

	// Verify concurrency matches virtual model limit (7), not physical model limit (3)
	if got := config.ResolveMaxConcurrent("virtual-fast", cfg); got != 7 {
		t.Errorf("ResolveMaxConcurrent(virtual-fast) = %d, want 7", got)
	}

	// Test case 2: clean model name without extra spaces
	receivedModel = ""
	bodyClean := []byte(`{"model":"virtual-fast","messages":[{"role":"user","content":"hi"}]}`)
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bodyClean))
	r2.Header.Set("Content-Type", "application/json")
	r2 = r2.WithContext(withModelCache(r2.Context(), mc))
	rec2 := httptest.NewRecorder()
	proxyChatWithBody(pool, rec2, r2, bodyClean, time.Now(), ChatForwardOpts{Model: "virtual-fast"}, cfg)

	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec2.Code, rec2.Body.String())
	}
	if receivedModel != "grok-4.20" {
		t.Errorf("upstream received body model = %q, want grok-4.20", receivedModel)
	}
}

func TestProxyChat_AggregateTrimSpaceModelAndForward(t *testing.T) {
	cfg := aggregateConfig(func(c *config.Config) {
		c.ModelProviderOverrides = map[string]string{
			"grok-4.20": "xai",
		}
	})
	mc := aggregateTestCache(t, cfg, map[string][]cache.ModelEntry{
		"xai": {{ID: "grok-4.20"}},
	})

	var receivedModel string
	upXai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		receivedModel, _ = util.RawStringField(raw, "model")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"grok-4.20","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upXai.Close()

	pool := pool.NewPool([]config.AccountConfig{
		{Name: "xai-acc", Provider: "xai", BaseURL: upXai.URL},
	})

	body := []byte(`{"model":"   grok-4.20   "}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withModelCache(r.Context(), mc))
	rec := httptest.NewRecorder()
	proxyChatWithBody(pool, rec, r, body, time.Now(), ChatForwardOpts{Model: "   grok-4.20   "}, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if receivedModel != "grok-4.20" {
		t.Fatalf("upstream received model = %q, want %q", receivedModel, "grok-4.20")
	}
}

func TestProxy_AggregatePureStaticProviderWithoutAccounts(t *testing.T) {
	content := `
listen: 127.0.0.1:8080
auth_token: tok
provider_routing: auto
provider_priority: [static-prov, other-prov]
providers:
  static-prov:
    models: [pure-static-model]
  other-prov:
    accounts:
      - name: other-acc
        key: k
        base_url: https://other.example.com
`
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	mc, err := cache.New(t.TempDir(), pool.NewPool(nil), cfg)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	mc.LoadFromDisk()

	// 1. Static model appears in /v1/models
	rModels := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	wModels := httptest.NewRecorder()
	proxyModels(mc, wModels, rModels, cfg)
	if wModels.Code != http.StatusOK {
		t.Fatalf("/v1/models status = %d, want 200", wModels.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(wModels.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if findModel(resp.Data, "pure-static-model") == nil {
		t.Fatalf("pure-static-model must appear in /v1/models, got %v", resp.Data)
	}

	// 2. Request routes to static-prov and fails closed with no_accounts when pool is empty
	pEmpty := pool.NewPool(nil)
	body := []byte(`{"model":"pure-static-model"}`)
	rChat := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	rChat.Header.Set("Content-Type", "application/json")
	rChat = rChat.WithContext(withModelCache(rChat.Context(), mc))
	recChat := httptest.NewRecorder()
	proxyChatWithBody(pEmpty, recChat, rChat, body, time.Now(), ChatForwardOpts{Model: "pure-static-model"}, cfg)

	if recChat.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty pool chat status = %d, want 503", recChat.Code)
	}
	if code := decodeErrorCode(t, recChat.Body.String()); code != "no_accounts" {
		t.Fatalf("error code = %q, want no_accounts", code)
	}

	// 3. Request routes to static-prov and fails closed with no_healthy when other providers have accounts
	pOther := pool.NewPool([]config.AccountConfig{
		{Name: "other-acc", Provider: "other-prov", BaseURL: "https://other.example.com"},
	})
	rChat2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	rChat2.Header.Set("Content-Type", "application/json")
	rChat2 = rChat2.WithContext(withModelCache(rChat2.Context(), mc))
	recChat2 := httptest.NewRecorder()
	proxyChatWithBody(pOther, recChat2, rChat2, body, time.Now(), ChatForwardOpts{Model: "pure-static-model"}, cfg)

	if recChat2.Code != http.StatusServiceUnavailable {
		t.Fatalf("other provider pool chat status = %d, want 503", recChat2.Code)
	}
	if code := decodeErrorCode(t, recChat2.Body.String()); code != "no_healthy" {
		t.Fatalf("error code = %q, want no_healthy", code)
	}
}

