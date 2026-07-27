package cache

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

// NewForTest builds a ModelCache backed by pre-populated provider caches for
// use in tests. It bypasses disk setup (New) and is only visible to tests.
func NewForTest(caches map[string]*providerCache) *ModelCache {
	return &ModelCache{
		caches: caches,
	}
}

func intPtr(i int) *int    { return &i }
func boolPtr(b bool) *bool { return &b }

// writePIModelsJSON writes an initial pi models.json with the given providers.
func writePIModelsJSON(t *testing.T, path string, providers map[string]any) {
	t.Helper()
	m := map[string]any{"providers": providers}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// readPIModel returns the first model entry for provider with the given id.
func readPIModel(t *testing.T, path, provider, id string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read synced file: %v", err)
	}
	var pc struct {
		Providers map[string]struct {
			Models []map[string]any `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &pc); err != nil {
		t.Fatalf("unmarshal synced file: %v", err)
	}
	p, ok := pc.Providers[provider]
	if !ok {
		t.Fatalf("provider %q not found in synced file", provider)
	}
	for _, m := range p.Models {
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("model %q not found for provider %q", id, provider)
	return nil
}

// TestSyncPIModelsJSON_ExistingModelGetsMetadata verifies the core 128k fix:
// an already-existing model entry that previously had NO metadata now receives
// metadata from config (e.g. glm-5.2 context_window=1M instead of default 128k).
func TestSyncPIModelsJSON_ExistingModelGetsMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")

	// Pre-existing pi models.json: glm-5.2 exists but has NO contextWindow.
	writePIModelsJSON(t, path, map[string]any{
		"ollama-cloud": map[string]any{
			"baseUrl": "http://127.0.0.1:18790/v1",
			"api":     "openai-completions",
			"apiKey":  "prism-dummy-key",
			"models": []map[string]any{
				{"id": "glm-5.2"},
			},
		},
	})

	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "a", Provider: "ollama-cloud", BaseURL: "https://ollama.com/v1"}},
		ModelMetadata: config.ModelMetadataMap{
			"glm-5.2": {ContextWindow: intPtr(1000000), MaxTokens: intPtr(32768)},
		},
	}

	mc := NewForTest(map[string]*providerCache{
		"ollama-cloud": {Models: []ModelEntry{{ID: "glm-5.2", Object: "model", Created: 1, OwnedBy: "ollama"}}},
	})

	if err := mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg); err != nil {
		t.Fatalf("syncPIModelsJSON: %v", err)
	}

	entry := readPIModel(t, path, "ollama-cloud", "glm-5.2")
	if entry["id"] != "glm-5.2" {
		t.Fatalf("id mismatch: %v", entry["id"])
	}
	cw, ok := entry["contextWindow"].(float64)
	if !ok {
		t.Fatalf("contextWindow not written for existing model: %v", entry)
	}
	if int(cw) != 1000000 {
		t.Fatalf("contextWindow = %v, want 1000000", cw)
	}
	if mt, ok := entry["maxTokens"].(float64); !ok || int(mt) != 32768 {
		t.Fatalf("maxTokens = %v, want 32768", entry["maxTokens"])
	}
}

// TestSyncPIModelsJSON_PreservesUserKeys verifies non-managed keys (e.g. a
// hand-edited "name") survive a rebuild, while prism-managed keys (contextWindow)
// are dropped when neither upstream nor config provides them.
func TestSyncPIModelsJSON_PreservesUserKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")

	writePIModelsJSON(t, path, map[string]any{
		"p": map[string]any{
			"baseUrl": "http://127.0.0.1:18790/v1",
			"api":     "openai-completions",
			"apiKey":  "prism-dummy-key",
			"models": []map[string]any{
				{"id": "x", "name": "X", "contextWindow": 999},
			},
		},
	})

	cfg := &config.Config{
		Accounts:      []config.AccountConfig{{Name: "a", Provider: "p", BaseURL: "https://x.com/v1"}},
		ModelMetadata: config.ModelMetadataMap{},
	}

	mc := NewForTest(map[string]*providerCache{
		"p": {Models: []ModelEntry{{ID: "x", Object: "model", Created: 1, OwnedBy: "p"}}},
	})

	if err := mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg); err != nil {
		t.Fatalf("syncPIModelsJSON: %v", err)
	}

	entry := readPIModel(t, path, "p", "x")
	if entry["name"] != "X" {
		t.Fatalf("user key 'name' not preserved: %v", entry)
	}
	if _, ok := entry["contextWindow"]; ok {
		t.Fatalf("stale managed key 'contextWindow' should have been dropped: %v", entry)
	}
}

// TestSyncPIModelsJSON_ConfigOverridesUpstream verifies config metadata wins
// over upstream metadata for the same field.
func TestSyncPIModelsJSON_ConfigOverridesUpstream(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")

	writePIModelsJSON(t, path, map[string]any{
		"p": map[string]any{
			"baseUrl": "http://127.0.0.1:18790/v1",
			"api":     "openai-completions",
			"apiKey":  "prism-dummy-key",
			"models": []map[string]any{
				{"id": "glm-5.2", "contextWindow": 999},
			},
		},
	})

	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "a", Provider: "p", BaseURL: "https://x.com/v1"}},
		ModelMetadata: config.ModelMetadataMap{
			"glm-5.2": {ContextWindow: intPtr(1000000)},
		},
	}

	// Upstream reports context_window=128000; config says 1M → config wins.
	mc := NewForTest(map[string]*providerCache{
		"p": {
			Models: []ModelEntry{{ID: "glm-5.2", Object: "model", Created: 1, OwnedBy: "p"}},
			Meta:   map[string]ModelMeta{"glm-5.2": {ContextWindow: intPtr(128000)}},
		},
	})

	if err := mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg); err != nil {
		t.Fatalf("syncPIModelsJSON: %v", err)
	}

	entry := readPIModel(t, path, "p", "glm-5.2")
	cw, ok := entry["contextWindow"].(float64)
	if !ok {
		t.Fatalf("contextWindow not written: %v", entry)
	}
	if int(cw) != 1000000 {
		t.Fatalf("contextWindow = %v, want 1000000 (config should override upstream)", int(cw))
	}
}

// TestMergeMeta exercises the field-level override matrix.
func TestMergeMeta(t *testing.T) {
	up := ModelMeta{
		ContextWindow: intPtr(128000),
		MaxTokens:     intPtr(4096),
		Reasoning:     boolPtr(false),
		Input:         []string{"text"},
	}
	cfg := config.ModelMetadata{
		ContextWindow: intPtr(1000000), // override
		MaxTokens:     nil,             // keep upstream
		Reasoning:     boolPtr(true),   // override
		Input:         nil,             // keep upstream
	}
	merged := mergeMeta(up, cfg)
	if merged.ContextWindow == nil || *merged.ContextWindow != 1000000 {
		t.Fatalf("ContextWindow = %v, want 1000000 (config override)", merged.ContextWindow)
	}
	if merged.MaxTokens == nil || *merged.MaxTokens != 4096 {
		t.Fatalf("MaxTokens = %v, want 4096 (upstream kept)", merged.MaxTokens)
	}
	if merged.Reasoning == nil || *merged.Reasoning != true {
		t.Fatalf("Reasoning = %v, want true (config override)", merged.Reasoning)
	}
	if len(merged.Input) != 1 || merged.Input[0] != "text" {
		t.Fatalf("Input = %v, want [text] (upstream kept)", merged.Input)
	}
}

// TestFetchOllamaShow_DeriveContextLength verifies /api/show parsing: the
// model_info .context_length (architecture-dependent key) is extracted.
func TestFetchOllamaShow_DeriveContextLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model_info": map[string]any{
				"llama.context_length": 128000,
				"foo.bar":              "ignored",
			},
		})
	}))
	defer srv.Close()

	acc := pool.NewPool([]config.AccountConfig{
		{Name: "a", Provider: "ollama-cloud", BaseURL: srv.URL + "/v1"},
	}).AllAccounts()[0]

	mc := &ModelCache{}
	meta, err := mc.fetchOllamaShow(acc, "llama")
	if err != nil {
		t.Fatalf("fetchOllamaShow: %v", err)
	}
	if meta.ContextWindow == nil || *meta.ContextWindow != 128000 {
		t.Fatalf("ContextWindow = %v, want 128000", meta.ContextWindow)
	}
}

// TestFetchOllamaShow_PerModelErrorNoFail verifies a single failing /api/show
// (500) is skipped without failing the whole operation; the healthy model still
// gets its metadata.
func TestFetchOllamaShow_PerModelErrorNoFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Name == "bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model_info": map[string]any{"x.context_length": 64000},
		})
	}))
	defer srv.Close()

	acc := pool.NewPool([]config.AccountConfig{
		{Name: "a", Provider: "ollama-cloud", BaseURL: srv.URL + "/v1"},
	}).AllAccounts()[0]

	mc := &ModelCache{}
	result := mc.collectOllamaMeta(acc, []ModelEntry{
		{ID: "good"},
		{ID: "bad"},
	})
	if len(result) != 1 {
		t.Fatalf("result size = %d, want 1 (bad model skipped)", len(result))
	}
	meta, ok := result["good"]
	if !ok {
		t.Fatalf("good model missing from result: %v", result)
	}
	if meta.ContextWindow == nil || *meta.ContextWindow != 64000 {
		t.Fatalf("good ContextWindow = %v, want 64000", meta.ContextWindow)
	}
	if _, ok := result["bad"]; ok {
		t.Fatalf("bad model should have been skipped: %v", result)
	}
}

// TestRootURL verifies base URL normalization for ollama /api/show joins.
func TestRootURL(t *testing.T) {
	cases := map[string]string{
		"https://ollama.com/v1":      "https://ollama.com",
		"https://ollama.com/v1/":     "https://ollama.com",
		"https://ollama.com":         "https://ollama.com",
		"http://127.0.0.1:11434":     "http://127.0.0.1:11434",
		"http://127.0.0.1:11434/v1/": "http://127.0.0.1:11434",
	}
	for in, want := range cases {
		if got := rootURL(in); got != want {
			t.Errorf("rootURL(%q) = %q, want %q", in, got, want)
		}
	}
}
