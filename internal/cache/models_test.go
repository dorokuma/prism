package cache

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestSyncPIModelsJSON_PerProviderNoCrossTalk is the core T2 verification:
// the same model (deepseek-v4-pro) configured with different context windows
// per provider must NOT leak between providers. ollama-cloud gets its upstream
// 512K (per-provider entry omits context_window → mergeMeta keeps upstream);
// opencode-go has no upstream and no per-provider override, so it falls back
// to the default 1M config value.
func TestSyncPIModelsJSON_PerProviderNoCrossTalk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")

	// No pre-existing models.json; the test focuses on the rebuilt entries.
	writePIModelsJSON(t, path, map[string]any{})

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a", Provider: "ollama-cloud", BaseURL: "https://ollama.com/v1"},
			{Name: "b", Provider: "opencode-go", BaseURL: "https://opencode.ai/zen"},
		},
		ModelMetadata: config.ModelMetadataMap{
			// Default layer: deepseek-v4-pro = 1M.
			"deepseek-v4-pro": {ContextWindow: intPtr(1000000)},
		},
		ModelMetadataPerProvider: map[string]config.ModelMetadataMap{
			"ollama-cloud": {
				// Per-provider override: entry present but context_window omitted
				// (nil). The upstream 512K must win (full replace, not merge).
				"deepseek-v4-pro": {Reasoning: boolPtr(true)},
			},
		},
	}

	mc := NewForTest(map[string]*providerCache{
		"ollama-cloud": {
			Models: []ModelEntry{{ID: "deepseek-v4-pro", Object: "model", Created: 1, OwnedBy: "ollama"}},
			Meta:   map[string]ModelMeta{"deepseek-v4-pro": {ContextWindow: intPtr(512000)}},
		},
		"opencode-go": {
			Models: []ModelEntry{{ID: "deepseek-v4-pro", Object: "model", Created: 1, OwnedBy: "opencode"}},
			Meta:   nil,
		},
	})

	if err := mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg); err != nil {
		t.Fatalf("syncPIModelsJSON: %v", err)
	}

	// ollama-cloud: upstream 512K, NOT the default 1M.
	ollamaEntry := readPIModel(t, path, "ollama-cloud", "deepseek-v4-pro")
	ocw, ok := ollamaEntry["contextWindow"].(float64)
	if !ok {
		t.Fatalf("ollama contextWindow not written: %v", ollamaEntry)
	}
	if int(ocw) != 512000 {
		t.Fatalf("ollama contextWindow = %v, want 512000 (upstream, no crosstalk)", int(ocw))
	}

	// opencode-go: default 1M.
	opEntry := readPIModel(t, path, "opencode-go", "deepseek-v4-pro")
	opcw, ok := opEntry["contextWindow"].(float64)
	if !ok {
		t.Fatalf("opencode contextWindow not written: %v", opEntry)
	}
	if int(opcw) != 1000000 {
		t.Fatalf("opencode contextWindow = %v, want 1000000 (default fallback)", int(opcw))
	}
}

// TestFetchAllAsync_ForceFetchWhenMetaEmpty is the core T1 verification: an
// ollama provider whose on-disk cache has Models but a nil/empty Meta (the old
// pre-Meta cache format) must be force-fetched on startup, populating Meta via
// /api/show, without a manual SIGHUP.
func TestFetchAllAsync_ForceFetchWhenMetaEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data": []map[string]any{
					{"id": "deepseek-v4-pro", "object": "model", "created": 1, "owned_by": "ollama"},
				},
			})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_info": map[string]any{"x.context_length": 512000},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// The account's configured base_url uses an ollama.com host so that
	// LoadConfig derives the "ollama" effort schema for it (this is what gates
	// the Meta-empty force-fetch). Actual requests go through the pool account,
	// whose base_url points at the httptest server below.
	cfg := loadOllamaSchemaCfg(t)

	dir := t.TempDir()
	mc := &ModelCache{
		dir: dir,
		caches: map[string]*providerCache{
			"ollama-cloud": {
				// Old cache: Models present, Meta nil.
				Models: []ModelEntry{{ID: "deepseek-v4-pro", Object: "model", Created: 1, OwnedBy: "ollama"}},
				Meta:   nil,
			},
		},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "ollama-acc", Provider: "ollama-cloud", BaseURL: srv.URL + "/v1"},
		}),
		cfg:  cfg,
		stop: make(chan struct{}),
	}

	done := make(chan struct{})
	mc.FetchAllAsync(func() {
		close(done)
	})
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("FetchAllAsync did not complete in time")
	}

	meta, ok := mc.GetModelMeta("ollama-cloud", "deepseek-v4-pro")
	if !ok {
		t.Fatalf("expected Meta to be populated after force-fetch")
	}
	if meta.ContextWindow == nil || *meta.ContextWindow != 512000 {
		t.Fatalf("Meta ContextWindow = %v, want 512000 (fetched via /api/show)", meta.ContextWindow)
	}
}

// loadOllamaSchemaCfg builds a Config via LoadConfig whose provider ollama-cloud
// has an ollama.com base_url host, so the "ollama" effort schema is derived.
// The generated API key is a dummy (the cache test never authenticates).
func loadOllamaSchemaCfg(t *testing.T) *config.Config {
	t.Helper()
	content := `
providers:
  ollama-cloud:
    accounts:
      - name: ollama-acc
        key: test-key-12345
        base_url: https://ollama.com/v1
`
	f, err := os.CreateTemp("", "cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cfg, err := config.LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// TestFetchAllAsync_NoForceFetchForNonOllama verifies the Meta-empty detection
// is gated on the ollama schema: a non-ollama provider with Models but nil
// Meta is NOT force-fetched.
func TestFetchAllAsync_NoForceFetchForNonOllama(t *testing.T) {
	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a", Provider: "opencode-go", BaseURL: "https://opencode.ai/zen"},
		},
	}
	dir := t.TempDir()
	mc := &ModelCache{
		dir: dir,
		caches: map[string]*providerCache{
			"opencode-go": {
				Models: []ModelEntry{{ID: "glm-5.2", Object: "model", Created: 1, OwnedBy: "opencode"}},
				Meta:   nil,
			},
		},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a", Provider: "opencode-go", BaseURL: "https://opencode.ai/zen"},
		}),
		cfg:  cfg,
		stop: make(chan struct{}),
	}

	done := make(chan struct{})
	mc.FetchAllAsync(func() {
		close(done)
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchAllAsync did not complete in time")
	}

	// Nothing was fetched → Meta still empty (no panic, no fetch attempt).
	if _, ok := mc.GetModelMeta("opencode-go", "glm-5.2"); ok {
		t.Fatalf("non-ollama provider should not have been force-fetched")
	}
}

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

// TestSyncPIModelsJSON_SkipPISync verifies that a provider whose account sets
// skip_pi_sync=true keeps its hand-maintained models.json entry untouched
// (e.g. agentrouter-anthropic with api: anthropic-messages), while a normal
// provider is still synced.
func TestSyncPIModelsJSON_SkipPISync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")

	writePIModelsJSON(t, path, map[string]any{
		"agentrouter-anthropic": map[string]any{
			"baseUrl": "https://gw.example.com/",
			"api":     "anthropic-messages",
			"apiKey":  "prism-dummy-key",
			"headers": map[string]any{"X-Prism-Provider": "agentrouter-anthropic"},
			"models": []map[string]any{
				{"id": "claude-opus-5", "contextWindow": 1000000},
			},
		},
		"opencode-go": map[string]any{
			"baseUrl": "http://127.0.0.1:18790/v1",
			"api":     "openai-completions",
			"apiKey":  "prism-dummy-key",
			"models": []map[string]any{
				{"id": "deepseek-v4-pro"},
			},
		},
	})

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "agentrouter-anthropic", BaseURL: "https://gw.example.com/", SkipPISync: true},
			{Name: "a2", Provider: "opencode-go", BaseURL: "https://opencode.ai/zen/go/v1"},
		},
	}

	mc := NewForTest(map[string]*providerCache{
		"agentrouter-anthropic": {Models: []ModelEntry{{ID: "claude-opus-5", Object: "model", Created: 1, OwnedBy: "anthropic"}}},
		"opencode-go":           {Models: []ModelEntry{{ID: "deepseek-v4-pro", Object: "model", Created: 1, OwnedBy: "opencode"}}},
	})

	if err := mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg); err != nil {
		t.Fatalf("syncPIModelsJSON: %v", err)
	}

	// Read back the file and verify the skip provider entry is byte-identical
	// to what we wrote (hand-maintained, api: anthropic-messages preserved).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read synced file: %v", err)
	}
	var pc struct {
		Providers map[string]struct {
			BaseURL string           `json:"baseUrl"`
			API     string           `json:"api"`
			Models  []map[string]any `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &pc); err != nil {
		t.Fatalf("unmarshal synced file: %v", err)
	}
	ant, ok := pc.Providers["agentrouter-anthropic"]
	if !ok {
		t.Fatal("agentrouter-anthropic entry missing after sync")
	}
	if ant.API != "anthropic-messages" {
		t.Errorf("agentrouter-anthropic api = %q, want anthropic-messages (must be preserved)", ant.API)
	}
	if ant.BaseURL != "https://gw.example.com/" {
		t.Errorf("agentrouter-anthropic baseUrl = %q, want https://gw.example.com/ (must be preserved)", ant.BaseURL)
	}
	if len(ant.Models) != 1 || ant.Models[0]["contextWindow"] == nil {
		t.Errorf("agentrouter-anthropic models should be preserved untouched, got %v", ant.Models)
	}
	// The normal provider must still be synced/rewritten to point at prism.
	oc, ok := pc.Providers["opencode-go"]
	if !ok {
		t.Fatal("opencode-go entry missing after sync")
	}
	if oc.BaseURL != "http://127.0.0.1:18790/v1" {
		t.Errorf("opencode-go baseUrl = %q, want prism base", oc.BaseURL)
	}
}

// TestFetchAllAsync_SkipPISync verifies skip_pi_sync providers are not fetched
// (no upstream HTTP) — avoids useless /v1/models requests for providers whose
// pi metadata is hand-maintained.
func TestFetchAllAsync_SkipPISync(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "agentrouter-anthropic", BaseURL: upstream.URL, SkipPISync: true},
		},
	}
	mc := NewForTest(map[string]*providerCache{})
	mc.cfg = cfg

	done := make(chan struct{})
	mc.FetchAllAsync(func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchAllAsync timed out")
	}
	if hits != 0 {
		t.Errorf("expected 0 upstream fetches for skip_pi_sync provider, got %d", hits)
	}
}
