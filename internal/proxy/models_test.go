package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
