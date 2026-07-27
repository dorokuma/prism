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

func intPtr(i int) *int { return &i }

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

// TestProxyModels_NoProviderHeader_Empty is a regression: without the
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
