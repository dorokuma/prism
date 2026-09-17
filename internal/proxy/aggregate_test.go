package proxy

import (
	"bytes"
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
	"github.com/dorokuma/prism/internal/util"
)

// ---- Aggregate entry (provider_routing: auto) ----

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
