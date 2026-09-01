package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
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

// TestProviderCacheFileName_RejectsTraversal pins item 10 at the write
// path: provider names with path separators, "." / ".." / absolute paths
// (and empty names) are rejected, so a cache file can never be read or
// written outside the cache dir.
func TestProviderCacheFileName_RejectsTraversal(t *testing.T) {
	bad := []string{"", "../evil", "a/b", "a\\b", "/abs", "..", ".", "a/../../x"}
	for _, name := range bad {
		if _, err := providerCacheFileName(name); err == nil {
			t.Errorf("provider name %q must be rejected, got nil error", name)
		}
	}
	if name, err := providerCacheFileName("opencode-go"); err != nil || name != "opencode-go.json" {
		t.Errorf("providerCacheFileName(opencode-go) = (%q, %v), want (opencode-go.json, nil)", name, err)
	}
}

// TestModelCache_FilePathStaysInDir pins item 10 at the ModelCache level: a
// valid provider resolves inside the cache dir; a traversal provider yields
// an error, so the caller fails instead of escaping.
func TestModelCache_FilePathStaysInDir(t *testing.T) {
	dir := t.TempDir()
	mc := &ModelCache{dir: dir}
	fp, err := mc.filePath("p")
	if err != nil {
		t.Fatalf("filePath(p): %v", err)
	}
	if filepath.Dir(fp) != dir {
		t.Errorf("filePath(p) = %q, want inside %q", fp, dir)
	}
	if filepath.Base(fp) != "p.json" {
		t.Errorf("filePath(p) base = %q, want p.json", filepath.Base(fp))
	}
	if _, err := mc.filePath("../evil"); err == nil {
		t.Error("filePath(../evil) must be rejected")
	}
}

// TestFetch_ProviderPathTraversalFailsBeforeUpstream pins item 10
// end-to-end on the fetch path: even a programmatically built ModelCache
// (bypassing config validation) fails the fetch with a path error BEFORE
// any upstream request is made — the traversal never reaches the disk
// write.
func TestFetch_ProviderPathTraversalFailsBeforeUpstream(t *testing.T) {
	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "../evil"}}}
	p := pool.NewPool(cfg.Accounts)
	mc := &ModelCache{
		dir:  t.TempDir(),
		pool: p,
		cfg:  cfg,
	}

	err := mc.Fetch("../evil")
	if err == nil {
		t.Fatal("fetch with a traversal provider must fail")
	}
	if !strings.Contains(err.Error(), "provider name") {
		t.Errorf("error must name the rejected provider: %v", err)
	}
	if n := atomic.LoadInt32(&upstreamCalls); n != 0 {
		t.Errorf("upstream was called %d times, want 0 (path validation must run before any request)", n)
	}
	// No file must have been written outside the cache dir.
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(mc.dir), "evil.json")); statErr == nil {
		t.Error("traversal must not create a file outside the cache dir")
	}
}

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

// TestLoadFromDiskTightensExistingCacheTo0600 pins the load-path file-mode
// tightening: a pre-existing provider cache file (written by an older
// version or created under a wide umask, 0644) is re-tightened to 0600 on
// load — without waiting for the next fetch rename. Only prism's own
// provider cache files are touched (LoadFromDisk never reads pi's
// models.json).
func TestLoadFromDiskTightensExistingCacheTo0600(t *testing.T) {
	dir := t.TempDir()
	provider := "p"
	fp := filepath.Join(dir, provider+".json")
	pc := providerCache{Models: []ModelEntry{{ID: "m1", Object: "model", Created: 1, OwnedBy: "p"}}, UpdatedAt: time.Now()}
	data, err := json.Marshal(pc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fp, data, 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "a", Provider: provider, BaseURL: "https://x.com/v1"}}}
	mc := &ModelCache{dir: dir, caches: make(map[string]*providerCache), cfg: cfg}
	mc.LoadFromDisk()

	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatalf("stat provider cache: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("provider cache mode after load = %o, want 0600 (tightened on the load path)", perm)
	}
	models := mc.GetModels(provider)
	if len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("cache content after load = %+v, want [m1]", models)
	}
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
	meta, err := mc.fetchOllamaShow(context.Background(), acc, "llama")
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
	result := mc.collectOllamaMeta(context.Background(), acc, []ModelEntry{
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

// TestFetch_AppliesAccountHeaders guards the v0.10.1 bug: /v1/models cache
// fetches previously carried only Authorization and were rejected with 401 by
// gateways that authenticate on client identity headers (e.g. Originator/
// x-app). The upstream below returns 401 unless the account-level headers are
// present — Fetch must succeed, proving the headers are applied.
func TestFetch_AppliesAccountHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Originator") != "codex_cli_rs" || r.Header.Get("x-app") != "cli" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"unauthorized client detected","type":"unauthorized_client_error"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "claude-opus-4-8", "object": "model", "created": 1, "owned_by": "agentrouter"},
			},
		})
	}))
	defer srv.Close()

	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		pool: pool.NewPool([]config.AccountConfig{
			{
				Name:     "a1",
				Provider: "agentrouter",
				BaseURL:  srv.URL + "/v1",
				Key:      "test-key-12345",
				Headers:  map[string]string{"Originator": "codex_cli_rs", "x-app": "cli"},
			},
		}),
		stop: make(chan struct{}),
	}

	if err := mc.Fetch("agentrouter"); err != nil {
		t.Fatalf("Fetch with account headers: %v", err)
	}
	models := mc.GetModels("agentrouter")
	if len(models) != 1 || models[0].ID != "claude-opus-4-8" {
		t.Fatalf("expected fetched model list, got %v", models)
	}
}

// TestFetch_AuthHeaderCustom verifies a custom auth_header (e.g. x-api-key)
// applies to /v1/models fetches with the same semantics as doUpstreamRequest:
// the upstream receives the raw key in x-api-key and NO Authorization header.
func TestFetch_AuthHeaderCustom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "raw-key-98765" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
			return
		}
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Authorization must not be sent"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "gpt-5.6-sol", "object": "model", "created": 1, "owned_by": "agentrouter"},
			},
		})
	}))
	defer srv.Close()

	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		pool: pool.NewPool([]config.AccountConfig{
			{
				Name:       "a1",
				Provider:   "agentrouter",
				BaseURL:    srv.URL + "/v1",
				Key:        "raw-key-98765",
				AuthHeader: "x-api-key",
			},
		}),
		stop: make(chan struct{}),
	}

	if err := mc.Fetch("agentrouter"); err != nil {
		t.Fatalf("Fetch with custom auth_header: %v", err)
	}
	models := mc.GetModels("agentrouter")
	if len(models) != 1 || models[0].ID != "gpt-5.6-sol" {
		t.Fatalf("expected fetched model list, got %v", models)
	}
}

// TestFetchOllamaShow_AppliesAccountHeaders verifies the /api/show path also
// carries account-level headers and the custom auth_header (same semantics as
// the /v1/models fetch): the server rejects requests without them.
func TestFetchOllamaShow_AppliesAccountHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Originator") != "codex_cli_rs" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-api-key") != "raw-key-98765" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model_info": map[string]any{"x.context_length": 128000},
		})
	}))
	defer srv.Close()

	acc := pool.NewPool([]config.AccountConfig{
		{
			Name:       "a",
			Provider:   "ollama-cloud",
			BaseURL:    srv.URL + "/v1",
			Key:        "raw-key-98765",
			AuthHeader: "x-api-key",
			Headers:    map[string]string{"Originator": "codex_cli_rs"},
		},
	}).AllAccounts()[0]

	mc := &ModelCache{}
	meta, err := mc.fetchOllamaShow(context.Background(), acc, "llama")
	if err != nil {
		t.Fatalf("fetchOllamaShow: %v", err)
	}
	if meta.ContextWindow == nil || *meta.ContextWindow != 128000 {
		t.Fatalf("ContextWindow = %v, want 128000", meta.ContextWindow)
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

func TestSyncPIModelsJSON_SplitsOpenAIAndAnthropicByModelID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	mixed := []ModelEntry{
		{ID: "gpt-5.4", Object: "model"},
		{ID: "claude-sonnet-4", Object: "model"},
		{ID: "anyrouter/claude-opus-4", Object: "model"},
		{ID: "gemini-3-pro", Object: "model"},
	}
	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "oai", Provider: "anyrouter-openai", BaseURL: "https://example.fcapp.run/v1"},
			{Name: "ant", Provider: "anyrouter-anthropic", BaseURL: "https://example.fcapp.run/"},
		},
	}
	mc := NewForTest(map[string]*providerCache{
		"anyrouter-openai":    {Models: mixed},
		"anyrouter-anthropic": {Models: mixed},
	})
	if err := mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg); err != nil {
		t.Fatalf("syncPIModelsJSON: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pc struct {
		Providers map[string]struct {
			BaseURL string           `json:"baseUrl"`
			API     string           `json:"api"`
			Models  []map[string]any `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &pc); err != nil {
		t.Fatal(err)
	}
	oai := pc.Providers["anyrouter-openai"]
	if oai.API != "openai-completions" {
		t.Errorf("openai api = %q", oai.API)
	}
	if oai.BaseURL != "http://127.0.0.1:18790/v1" {
		t.Errorf("openai baseUrl = %q", oai.BaseURL)
	}
	gotOAI := map[string]bool{}
	for _, m := range oai.Models {
		id, _ := m["id"].(string)
		gotOAI[id] = true
	}
	if !gotOAI["gpt-5.4"] || !gotOAI["gemini-3-pro"] {
		t.Errorf("openai models = %v, want gpt + gemini", gotOAI)
	}
	if gotOAI["claude-sonnet-4"] || gotOAI["anyrouter/claude-opus-4"] {
		t.Errorf("openai must drop claude, got %v", gotOAI)
	}

	ant := pc.Providers["anyrouter-anthropic"]
	if ant.API != "anthropic-messages" {
		t.Errorf("anthropic api = %q", ant.API)
	}
	if ant.BaseURL != "http://127.0.0.1:18790/" {
		t.Errorf("anthropic baseUrl = %q", ant.BaseURL)
	}
	gotAnt := map[string]bool{}
	for _, m := range ant.Models {
		id, _ := m["id"].(string)
		gotAnt[id] = true
	}
	if !gotAnt["claude-sonnet-4"] || !gotAnt["anyrouter/claude-opus-4"] {
		t.Errorf("anthropic models = %v, want claude ids", gotAnt)
	}
	if gotAnt["gpt-5.4"] || gotAnt["gemini-3-pro"] {
		t.Errorf("anthropic must drop openai-family, got %v", gotAnt)
	}
}

func TestAnthropicModelID(t *testing.T) {
	yes := []string{"claude-sonnet-4", "Claude-Opus-4", "anthropic.claude-3", "gw/claude-haiku-4"}
	no := []string{"gpt-5.4", "gemini-3-pro", "deepseek-v4-pro", "grok-4.6"}
	for _, id := range yes {
		if !anthropicModelID(id) {
			t.Errorf("%q should be anthropic", id)
		}
	}
	for _, id := range no {
		if anthropicModelID(id) {
			t.Errorf("%q should not be anthropic", id)
		}
	}
}

// TestFetchAllAsync_SkipPISync verifies the narrowed v0.10.1 semantics:
// skip_pi_sync no longer suppresses upstream fetching — the provider IS
// fetched and cached like any other — but syncPIModelsJSON still refuses to
// overwrite its hand-maintained pi models.json entry.
func TestFetchAllAsync_SkipPISync(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"object":"list","data":[{"id":"claude-opus-5","object":"model","created":1,"owned_by":"agentrouter"}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "agentrouter-anthropic", BaseURL: upstream.URL, SkipPISync: true},
		},
	}
	dir := t.TempDir()
	mc := &ModelCache{
		dir:    dir,
		caches: map[string]*providerCache{},
		pool:   pool.NewPool(cfg.Accounts),
		cfg:    cfg,
		stop:   make(chan struct{}),
	}

	// 1) FetchAllAsync must fetch the skip_pi_sync provider (no longer skipped).
	done := make(chan struct{})
	mc.FetchAllAsync(func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchAllAsync timed out")
	}
	if hits != 1 {
		t.Errorf("expected exactly 1 upstream fetch for skip_pi_sync provider, got %d", hits)
	}
	models := mc.GetModels("agentrouter-anthropic")
	if len(models) != 1 || models[0].ID != "claude-opus-5" {
		t.Fatalf("expected fetched models for skip_pi_sync provider, got %v", models)
	}

	// 2) syncPIModelsJSON must still preserve the hand-maintained entry.
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
	})
	if err := mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg); err != nil {
		t.Fatalf("syncPIModelsJSON: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read synced file: %v", err)
	}
	var pc struct {
		Providers map[string]struct {
			API    string           `json:"api"`
			Models []map[string]any `json:"models"`
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
	if len(ant.Models) != 1 || ant.Models[0]["contextWindow"] == nil {
		t.Errorf("agentrouter-anthropic models should be preserved untouched, got %v", ant.Models)
	}
}

// TestFetch_Non200BodyRedacted guards the models error path: a non-200 body
// that echoes the account credential (or any sk- token) must be redacted by
// the existing redaction tool before it enters the returned error (which is
// logged by callers).
func TestFetch_Non200BodyRedacted(t *testing.T) {
	const key = "sk-super-secret-987654321"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key ` + key + `","code":"invalid_api_key"}}`))
	}))
	defer srv.Close()

	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: key},
		}),
		stop: make(chan struct{}),
	}

	err := mc.Fetch("p")
	if err == nil {
		t.Fatal("Fetch over a 401 upstream must return an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error must not leak the account key: %v", err)
	}
	if !strings.Contains(err.Error(), "sk-***") {
		t.Errorf("error should carry the redacted token marker: %v", err)
	}
}

// TestFetchOllamaShow_Non200BodyRedacted guards the same redaction on the
// ollama /api/show error path.
func TestFetchOllamaShow_Non200BodyRedacted(t *testing.T) {
	const key = "sk-ollama-secret-12345678"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom ` + key + ` failed`))
	}))
	defer srv.Close()

	mc := &ModelCache{
		dir: t.TempDir(),
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: key},
		}),
	}
	account := mc.pool.AllAccounts()[0]

	_, err := mc.fetchOllamaShow(context.Background(), account, "m1")
	if err == nil {
		t.Fatal("api/show over a 500 upstream must return an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error must not leak the account key: %v", err)
	}
}

// TestFetch_TryAcquireSaturatedFailsFast guards the models concurrency
// accounting: a Fetch counts against the same per-account concurrency limit
// as business requests (config.ResolveMaxConcurrent with the wildcard
// default). When the account is saturated the fetch fails immediately with a
// saturation error instead of parking on the 30s request timeout, and it
// never touches the upstream.
func TestFetch_TryAcquireSaturatedFailsFast(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	// Occupy the account's single slot (the wildcard limit of 1).
	acc := mc.pool.AllAccounts()[0]
	slot := acc.TryAcquire("", 1)
	if slot == nil {
		t.Fatal(`test setup: TryAcquire("", 1) failed`)
	}

	start := time.Now()
	err := mc.Fetch("p")
	elapsed := time.Since(start)
	acc.Release(slot)

	if err == nil {
		t.Fatal("Fetch must fail when the account is saturated")
	}
	if !strings.Contains(err.Error(), "saturated") {
		t.Errorf("error = %v, want a saturation error", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("saturated fetch took %v, want fail-fast (no 30s wait)", elapsed)
	}
	select {
	case <-hit:
		t.Error("saturated fetch must not send any upstream request")
	default:
	}
	if acc.InFlightCount() != 0 {
		t.Errorf("in-flight after fetch = %d, want 0 (slot released)", acc.InFlightCount())
	}
}

// TestFetch_TryAcquireUsesMinOfSpecificModels guards the should-fix rule: a
// Fetch must NOT look only at the "*" wildcard. With only specific per-model
// limits configured (no "*"), the fetch cap is the smallest positive value
// (config.ResolveFetchConcurrency) — here 1 — so a saturated account makes
// the fetch fail fast exactly like the wildcard case.
func TestFetch_TryAcquireUsesMinOfSpecificModels(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	// No "*" entry: only two specific model caps (min = 1).
	cfg := &config.Config{
		MaxConcurrentPerAccount: map[string]int{"deepseek-v4-pro": 5, "deepseek-v4-flash": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	// Occupy the single slot (the fetch cap must be the min, 1 — not the
	// built-in default of 450).
	acc := mc.pool.AllAccounts()[0]
	slot := acc.TryAcquire("", 1)
	if slot == nil {
		t.Fatal(`test setup: TryAcquire("", 1) failed`)
	}

	start := time.Now()
	err := mc.Fetch("p")
	elapsed := time.Since(start)
	acc.Release(slot)

	if err == nil {
		t.Fatal("Fetch must fail when the account is saturated at the min-of-specific-models cap")
	}
	if !strings.Contains(err.Error(), "saturated") {
		t.Errorf("error = %v, want a saturation error", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("saturated fetch took %v, want fail-fast (no 30s wait)", elapsed)
	}
	select {
	case <-hit:
		t.Error("saturated fetch must not send any upstream request")
	default:
	}
}

// TestFetch_TryAcquireReleasesSlot verifies a successful Fetch holds and
// releases exactly one concurrency slot: in-flight returns to 0 and the
// fetch succeeds.
func TestFetch_TryAcquireReleasesSlot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	if err := mc.Fetch("p"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	acc := mc.pool.AllAccounts()[0]
	if got := acc.InFlightCount(); got != 0 {
		t.Errorf("in-flight after successful fetch = %d, want 0", got)
	}
	models := mc.GetModels("p")
	if len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("fetched models = %+v, want [m1]", models)
	}
}

// ---------------------------------------------------------------------------
// Batch-2 audit: Fetch slot release wakes provider waiters (pool.Release)
// ---------------------------------------------------------------------------

// TestFetch_ReleaseWakesProviderWaiter is the cross-module waiter test
// (cache → pool): a Fetch holds a concurrency slot on the account for its
// whole duration; when it finishes, the slot must be released through the
// POOL (mc.pool.Release), which wakes the first matching provider waiter.
// Releasing only the account slot (account.Release) would leave a waiter
// parked in SelectByProvider until some unrelated business request happened
// to free the same account. The fetch and the waiter run concurrently, so
// -race exercises the release/wakeup path.
//
// Waiter queue entry is observed through the pool's read-only wait-queue
// state (controlled polling, no fixed sleep and no global test hook): the
// slot is freed only AFTER the waiter is provably parked, so the test is
// deterministic.
func TestFetch_ReleaseWakesProviderWaiter(t *testing.T) {
	entered := make(chan struct{})
	releaseUpstream := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-releaseUpstream
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	p := pool.NewPool([]config.AccountConfig{
		{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
	})
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool:   p,
		stop:   make(chan struct{}),
	}

	fetchDone := make(chan error, 1)
	go func() { fetchDone <- mc.Fetch("p") }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never reached the upstream")
	}

	// The fetch holds the account's only slot; a provider waiter now parks.
	type waiterResult struct {
		acc  *pool.Account
		slot *pool.Slot
	}
	waiterDone := make(chan waiterResult, 1)
	go func() {
		acc, slot, err := p.SelectByProvider(context.Background(), "", 1, "p")
		if err != nil {
			waiterDone <- waiterResult{}
			return
		}
		waiterDone <- waiterResult{acc: acc, slot: slot}
	}()

	// Deterministic synchronization: poll the pool's read-only wait-queue
	// count until the provider waiter is PROVABLY parked (it is registered
	// in the queue under the pool lock), then free the slot. A waiter that
	// is not yet parked when the slot is released would just select the
	// freed account directly and the wakeup path would not be exercised.
	deadline := time.Now().Add(5 * time.Second)
	for p.WaitingCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("provider waiter never entered the wait queue")
		}
		time.Sleep(2 * time.Millisecond)
	}

	close(releaseUpstream)

	select {
	case err := <-fetchDone:
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fetch did not complete")
	}

	select {
	case res := <-waiterDone:
		if res.acc == nil {
			t.Fatal("waiter select failed")
		}
		if res.acc.Name() != "a1" {
			t.Errorf("waiter got %q, want a1", res.acc.Name())
		}
		p.Release(res.slot)
	case <-time.After(3 * time.Second):
		t.Fatal("provider waiter was not woken by the fetch's slot release (pool.Release missing?)")
	}
}

// ---------------------------------------------------------------------------
// Batch-2 audit: bounded success reads on /v1/models and /api/show
// ---------------------------------------------------------------------------

// TestFetch_ResponseOverCapFails guards the bounded success read on
// /v1/models: a body larger than the configured max_upstream_response_bytes
// fails the fetch with util.ErrBodyTooLarge (instead of being buffered
// whole), and nothing is cached.
func TestFetch_ResponseOverCapFails(t *testing.T) {
	big := `{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x","padding":"` + strings.Repeat("a", 4096) + `"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	cfg := &config.Config{
		MaxConcurrentPerAccount:  map[string]int{"*": 1},
		MaxUpstreamResponseBytes: 512,
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	err := mc.Fetch("p")
	if err == nil {
		t.Fatal("Fetch must fail when the /v1/models body exceeds max_upstream_response_bytes")
	}
	if !errors.Is(err, util.ErrBodyTooLarge) {
		t.Errorf("err = %v, want ErrBodyTooLarge", err)
	}
	if models := mc.GetModels("p"); models != nil {
		t.Errorf("over-cap response must not be cached, got %v", models)
	}
}

// TestFetchOllamaShow_ResponseOverCapFails guards the same bounded read on
// ollama /api/show: an over-cap body fails that model's show fetch (and the
// model is skipped, never failing the enclosing Fetch).
func TestFetchOllamaShow_ResponseOverCapFails(t *testing.T) {
	big := `{"model_info":{"x.context_length":128000,"padding":"` + strings.Repeat("b", 4096) + `"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	acc := pool.NewPool([]config.AccountConfig{
		{Name: "a", Provider: "ollama-cloud", BaseURL: srv.URL + "/v1"},
	}).AllAccounts()[0]

	mc := &ModelCache{cfg: &config.Config{MaxUpstreamResponseBytes: 512}}
	_, err := mc.fetchOllamaShow(context.Background(), acc, "llama")
	if err == nil {
		t.Fatal("api/show must fail when the body exceeds max_upstream_response_bytes")
	}
	if !errors.Is(err, util.ErrBodyTooLarge) {
		t.Errorf("err = %v, want ErrBodyTooLarge", err)
	}
}

// ---------------------------------------------------------------------------
// Batch-2 audit: key-aware redaction for non-sk/Bearer account keys
// ---------------------------------------------------------------------------

// TestFetch_NonSKKeyRedacted guards key-aware redaction on the models fetch
// error path: an upstream echoing the account key whose format is NOT an
// sk-/Bearer token (custom auth_header keys) must still be scrubbed from the
// returned error by RedactBodyBytesWithKeys(..., acc.Key()).
func TestFetch_NonSKKeyRedacted(t *testing.T) {
	const key = "raw-key-98765"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key ` + key + `","code":"invalid_api_key"}}`))
	}))
	defer srv.Close()

	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: key},
		}),
		stop: make(chan struct{}),
	}

	err := mc.Fetch("p")
	if err == nil {
		t.Fatal("Fetch over a 401 upstream must return an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error must not leak the non-sk account key: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("error should carry the redaction marker: %v", err)
	}
}

// TestFetchOllamaShow_NonSKKeyRedacted guards the same key-aware redaction
// on the ollama /api/show error path.
func TestFetchOllamaShow_NonSKKeyRedacted(t *testing.T) {
	const key = "raw-key-98765"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom ` + key + ` failed`))
	}))
	defer srv.Close()

	mc := &ModelCache{
		dir: t.TempDir(),
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: key},
		}),
	}
	account := mc.pool.AllAccounts()[0]

	_, err := mc.fetchOllamaShow(context.Background(), account, "m1")
	if err == nil {
		t.Fatal("api/show over a 500 upstream must return an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error must not leak the non-sk account key: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("error should carry the redaction marker: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Batch-2 audit: controlled background refresh (reentry + cancellation)
// ---------------------------------------------------------------------------

// TestRefreshAllAsync_PendingRerunsWithLatestConfig verifies the pending
// handover: a second RefreshAllAsync while a round is in flight must NOT be
// lost — it is remembered as pending, and when the running round finishes
// exactly ONE more round starts with the LATEST config snapshot (a provider
// added by the new config is fetched) and the LATEST onDone. The superseded
// round's onDone is dropped (a stale config must never sync tools after a
// newer SIGHUP). No concurrent reentry: the second round starts only after
// the first completes.
func TestRefreshAllAsync_PendingRerunsWithLatestConfig(t *testing.T) {
	var hits int32
	entered := make(chan struct{})
	releaseFirst := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-releaseFirst
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv.Close()

	// Both providers are healthy in the pool from the start; cfg1 only knows
	// p1. The pending round must snapshot the LATEST config (cfg2, with p2
	// added) and fetch p2 too.
	cfg1 := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	cfg2 := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv.URL, Key: "k"},
			{Name: "a2", Provider: "p2", BaseURL: srv.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg1,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv.URL, Key: "k"},
			{Name: "a2", Provider: "p2", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	var firstRuns int32
	var secondRuns int32
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	mc.RefreshAllAsync(func() {
		atomic.AddInt32(&firstRuns, 1)
		close(firstDone)
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first refresh never reached the upstream")
	}

	// A second SIGHUP arrives while round 1 is in flight: it updates the
	// config and must be remembered as pending, not lost.
	mc.UpdateConfig(cfg2)
	mc.RefreshAllAsync(func() {
		atomic.AddInt32(&secondRuns, 1)
		close(secondDone)
	})
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("upstream hits = %d, want 1 while round 1 is in flight (no concurrent reentry)", n)
	}

	close(releaseFirst)

	// The pending round runs after round 1 completes, fetching with the
	// LATEST config: p2 is fetched too (2 more hits), and the latest onDone
	// runs exactly once.
	select {
	case <-secondDone:
	case <-time.After(10 * time.Second):
		t.Fatal("pending round never completed")
	}
	if n := atomic.LoadInt32(&hits); n != 3 {
		t.Fatalf("upstream hits = %d, want 3 (round 1: p1; pending round: p1+p2)", n)
	}
	if atomic.LoadInt32(&secondRuns) != 1 {
		t.Errorf("final round's onDone ran %d times, want exactly 1", atomic.LoadInt32(&secondRuns))
	}
	// The superseded round's onDone must never have run (deterministic: it
	// would have fired before the pending round's onDone, which we already
	// observed).
	select {
	case <-firstDone:
		t.Fatal("superseded round's onDone must not run (stale config must not sync tools)")
	default:
	}
	if atomic.LoadInt32(&firstRuns) != 0 {
		t.Errorf("superseded round's onDone ran %d times, want 0", atomic.LoadInt32(&firstRuns))
	}
	if models := mc.GetModels("p2"); models == nil {
		t.Error("pending round must fetch with the latest config (p2 must be cached)")
	}
}

// TestRefreshAllAsync_StopCancelsInFlight verifies Stop() aborts a running
// background refresh: the upstream request is cancelled (the server observes
// its request context done), the refresh goroutine exits (Stop waits on it,
// so a leaked goroutine would hang this test), and neither the cache file
// nor the in-memory cache is populated for the aborted fetch.
func TestRefreshAllAsync_StopCancelsInFlight(t *testing.T) {
	srvStarted := make(chan struct{})
	srvCancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(srvStarted)
		<-r.Context().Done()
		close(srvCancelled)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    dir,
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	done := make(chan struct{})
	mc.RefreshAllAsync(func() { close(done) })
	select {
	case <-srvStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never reached the upstream")
	}

	// Stop must cancel the in-flight refresh and wait for the goroutine.
	mc.Stop()

	select {
	case <-srvCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight refresh request was not cancelled by Stop")
	}
	select {
	case <-done:
		t.Fatal("onDone must not run for a cancelled refresh")
	default:
	}
	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatalf("read cache dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("aborted refresh must not write cache files, found %v", entries)
	}
	if models := mc.GetModels("p"); models != nil {
		t.Errorf("aborted refresh must not populate the in-memory cache, got %v", models)
	}
	// Stop is idempotent (no double-close panic).
	mc.Stop()
}

// TestRefreshAllAsync_StopCancelsPendingRound verifies Stop() drains a
// pending handover: with a round in flight and a pending refresh queued,
// Stop cancels the in-flight round; the cancelled round must NOT start the
// pending round (no second upstream hit, no onDone at all, no cache
// writes), and Stop waits for the goroutine to fully exit.
func TestRefreshAllAsync_StopCancelsPendingRound(t *testing.T) {
	srvStarted := make(chan struct{})
	srvCancelled := make(chan struct{})
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-srvStarted:
		default:
			close(srvStarted)
		}
		<-r.Context().Done()
		select {
		case <-srvCancelled:
		default:
			close(srvCancelled)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    dir,
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	firstDone := make(chan struct{})
	pendingDone := make(chan struct{})
	mc.RefreshAllAsync(func() { close(firstDone) })
	select {
	case <-srvStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never reached the upstream")
	}

	// A pending refresh is queued while round 1 is in flight.
	mc.RefreshAllAsync(func() { close(pendingDone) })

	mc.Stop()

	select {
	case <-srvCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight refresh request was not cancelled by Stop")
	}
	// The cancelled round must not hand over to the pending round.
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("upstream hits = %d, want 1 (cancelled round must not start the pending round)", n)
	}
	select {
	case <-firstDone:
		t.Fatal("cancelled round's onDone must not run")
	default:
	}
	select {
	case <-pendingDone:
		t.Fatal("pending round's onDone must not run after Stop")
	default:
	}
	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatalf("read cache dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("aborted refresh must not write cache files, found %v", entries)
	}
	// Stop waited: the refresh goroutine fully exited and released its state.
	mc.refreshMu.Lock()
	refreshing := mc.refreshing
	stopped := mc.stopped
	mc.refreshMu.Unlock()
	if refreshing {
		t.Errorf("refresh state after Stop = refreshing:%v, want clean", refreshing)
	}
	if !stopped {
		t.Errorf("refresh state after Stop = stopped:%v, want true (no new rounds may start)", stopped)
	}
}

// TestRefreshAllAsync_StopWaitsForGoroutineExit guards the Add/Wait race:
// a Stop that runs while a refresh round is live must NOT return before the
// refresh goroutine has fully exited. The old code added to the WaitGroup
// AFTER releasing refreshMu, so Stop's Wait could observe an empty counter
// and return while the goroutine was still running (a leak). With the
// refresh blocked in flight, Stop returning while refreshing is still true
// would prove that race; the fixed code always returns with clean state.
func TestRefreshAllAsync_StopWaitsForGoroutineExit(t *testing.T) {
	srvStarted := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-srvStarted:
		default:
			close(srvStarted)
		}
		<-release
	}))
	defer srv.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	done := make(chan struct{})
	mc.RefreshAllAsync(func() { close(done) })
	select {
	case <-srvStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never reached the upstream")
	}

	mc.Stop()

	// Stop returned: the goroutine must have exited and released its state
	// (a leaked goroutine would still hold refreshing=true).
	mc.refreshMu.Lock()
	refreshing := mc.refreshing
	stopped := mc.stopped
	mc.refreshMu.Unlock()
	if refreshing {
		t.Fatal("Stop returned while the refresh goroutine is still running (Add/Wait race)")
	}
	if !stopped {
		t.Fatal("Stop returned with stopped=false (no new rounds may start after Stop)")
	}
	select {
	case <-done:
		t.Fatal("onDone must not run for a cancelled refresh")
	default:
	}
	close(release) // let the server handler return so the test server can shut down
}

// TestRefreshAllAsync_StopRace hammers Stop against concurrent
// RefreshAllAsync calls (including a queued pending refresh). Under -race it
// exercises the refreshMu/refreshing/pending/stopped state transitions; the
// old Add-outside-the-lock code let Stop's Wait return before the refresh
// goroutine was registered, leaving a live goroutine after Stop. It also
// pins the post-Stop contract: RefreshAllAsync is refused once Stop has
// begun (no round may start, its onDone never runs).
func TestRefreshAllAsync_StopRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	for i := 0; i < 20; i++ {
		mc := &ModelCache{
			dir:    t.TempDir(),
			caches: map[string]*providerCache{},
			cfg: &config.Config{
				Accounts: []config.AccountConfig{
					{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
				},
				MaxConcurrentPerAccount: map[string]int{"*": 1},
			},
			pool: pool.NewPool([]config.AccountConfig{
				{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
			}),
			stop: make(chan struct{}),
		}
		if i%2 == 0 {
			mc.RefreshAllAsync(nil)
			mc.RefreshAllAsync(nil) // queued as pending while round 1 runs
		} else {
			mc.RefreshAllAsync(nil)
		}
		mc.Stop()
		// Either the rounds completed before Stop or they were cancelled —
		// either way Stop must have drained everything: clean state.
		mc.refreshMu.Lock()
		refreshing := mc.refreshing
		pending := mc.pending
		stopped := mc.stopped
		mc.refreshMu.Unlock()
		if refreshing || pending {
			t.Fatalf("iteration %d: Stop returned with refreshing=%v pending=%v (live round or stuck handover)", i, refreshing, pending)
		}
		if !stopped {
			t.Fatalf("iteration %d: Stop returned with stopped=%v, want true", i, stopped)
		}
		// After Stop no new refresh round may start: RefreshAllAsync is
		// refused and its onDone must never run.
		afterStop := make(chan struct{})
		mc.RefreshAllAsync(func() { close(afterStop) })
		select {
		case <-afterStop:
			t.Fatalf("iteration %d: RefreshAllAsync started a round after Stop (must be refused)", i)
		case <-time.After(150 * time.Millisecond):
		}
		mc.refreshMu.Lock()
		refreshing = mc.refreshing
		pending = mc.pending
		mc.refreshMu.Unlock()
		if refreshing || pending {
			t.Fatalf("iteration %d: refused refresh left state refreshing=%v pending=%v", i, refreshing, pending)
		}
		mc.Stop() // still idempotent after a refused refresh
	}
}

// ---------------------------------------------------------------------------
// Final-cleanup audit: stale-refresh lifecycle + atomic cache writes
// ---------------------------------------------------------------------------

// TestRefreshLoop_StopCancelsStaleRound verifies the stale-refresh ticker
// runs through the same cancellable lifecycle as the manual refresh: with a
// stale round blocked on a slow upstream, Stop cancels the request, waits
// for the round to exit (no goroutine leak), and neither the cache file nor
// the in-memory cache is populated. The old RefreshStale used a background
// context, so Stop could not abort it and the fetch could write the cache
// file after shutdown. The scheduler also guarantees at most one stale round
// in flight (exactly one upstream hit).
func TestRefreshLoop_StopCancelsStaleRound(t *testing.T) {
	var hits int32
	srvStarted := make(chan struct{})
	srvCancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-srvStarted:
		default:
			close(srvStarted)
		}
		<-r.Context().Done()
		select {
		case <-srvCancelled:
		default:
			close(srvCancelled)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    dir,
		caches: map[string]*providerCache{}, // nothing cached → every provider is stale
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	mc.StartRefreshLoop(10*time.Millisecond, nil)
	select {
	case <-srvStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("stale refresh never reached the upstream")
	}

	mc.Stop()

	select {
	case <-srvCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight stale refresh request was not cancelled by Stop")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("upstream hits = %d, want 1 (the scheduler must never run concurrent stale rounds)", n)
	}
	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatalf("read cache dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("aborted stale refresh must not write cache files, found %v", entries)
	}
	if models := mc.GetModels("p"); models != nil {
		t.Errorf("aborted stale refresh must not populate the in-memory cache, got %v", models)
	}
}

// TestRefreshLoop_SkipsStaleWhileManualRoundInFlight verifies the single
// scheduler: while a manual RefreshAllAsync round is blocked in flight, the
// stale ticker must NOT start a concurrent round (no second upstream request,
// no same-provider concurrent refresh). The stale work is subsumed by the
// manual round.
func TestRefreshLoop_SkipsStaleWhileManualRoundInFlight(t *testing.T) {
	var hits int32
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}
	defer mc.Stop()

	mc.StartRefreshLoop(10*time.Millisecond, nil)
	mc.RefreshAllAsync(nil)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("manual refresh never reached the upstream")
	}

	// The manual round is blocked in flight; several ticker firings must
	// not start a stale round on top of it.
	time.Sleep(150 * time.Millisecond)
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("upstream hits = %d, want 1 (stale round must be skipped while the manual round is in flight)", n)
	}

	close(release)
	// Wait until the manual round visibly completed (cache populated) so the
	// deferred Stop drains only the ticker goroutine and no live round.
	deadline := time.Now().Add(5 * time.Second)
	for mc.GetModels("p") == nil {
		if time.Now().After(deadline) {
			t.Fatal("manual round never completed after release")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestFetch_WritesCacheAtomically pins the temp-file write path: a
// successful fetch leaves exactly the provider cache file (no temp litter),
// with 0600 owner-only permissions (the cache is not world-readable), and a
// second fetch over it is idempotent (no error, file atomically replaced).
func TestFetch_WritesCacheAtomically(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := &config.Config{MaxConcurrentPerAccount: map[string]int{"*": 1}}
	mc := &ModelCache{
		dir:    dir,
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	if err := mc.Fetch("p"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "p.json" {
		t.Fatalf("cache dir after fetch = %v, want exactly [p.json] (no temp litter)", entries)
	}
	info, err := os.Stat(filepath.Join(dir, "p.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("cache file mode = %o, want 0600 (owner-only; the cache must not be world-readable)", perm)
	}

	// Idempotent rerun: a second fetch over the existing cache succeeds and
	// still leaves exactly one file.
	if err := mc.Fetch("p"); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "p.json" {
		t.Fatalf("cache dir after second fetch = %v, want exactly [p.json]", entries)
	}
	if models := mc.GetModels("p"); len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("cached models = %+v, want [m1]", models)
	}
}

// TestFetchWithContext_CancelDuringMetaFanOutNoWrite verifies a fetch whose
// /v1/models response completed but was cancelled while the ollama /api/show
// fan-out was still running returns the cancellation error and writes
// nothing — neither the cache file nor a temp file (the pre-write ctx checks
// prevent the old code from persisting the half-observed result).
func TestFetchWithContext_CancelDuringMetaFanOutNoWrite(t *testing.T) {
	modelsHit := make(chan struct{})
	showHit := make(chan struct{})
	releaseShow := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			close(modelsHit)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data": []map[string]any{
					{"id": "m1", "object": "model", "created": 1, "owned_by": "ollama"},
					{"id": "m2", "object": "model", "created": 1, "owned_by": "ollama"},
				},
			})
		case "/api/show":
			select {
			case <-showHit:
			default:
				close(showHit)
			}
			<-releaseShow
			_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"x.context_length": 64000}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// The config's ollama.com base_url derives the "ollama" effort schema
	// (this gates the /api/show fan-out); the pool account points at the
	// test server, which is where the actual requests go.
	dir := t.TempDir()
	mc := &ModelCache{
		dir:    dir,
		caches: map[string]*providerCache{},
		cfg:    loadOllamaSchemaCfg(t),
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "ollama-acc", Provider: "ollama-cloud", BaseURL: srv.URL + "/v1", Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- mc.fetchWithContext(ctx, "ollama-cloud") }()

	select {
	case <-modelsHit:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never reached /v1/models")
	}
	select {
	case <-showHit:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never reached /api/show")
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("fetchWithContext error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetchWithContext did not return after cancellation")
	}

	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatalf("read cache dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("cancelled fetch must leave no files (no cache file, no temp litter), found %v", entries)
	}
	if models := mc.GetModels("ollama-cloud"); models != nil {
		t.Errorf("cancelled fetch must not populate the in-memory cache, got %v", models)
	}

	close(releaseShow) // let the /api/show handlers return so the test server can shut down
}

// TestRefreshStale_RefusedAfterStop verifies no refresh work starts after
// Stop: the synchronous stale path is a no-op once stopped is set.
func TestRefreshStale_RefusedAfterStop(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg: &config.Config{
			Accounts: []config.AccountConfig{
				{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
			},
			MaxConcurrentPerAccount: map[string]int{"*": 1},
		},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	mc.Stop()
	mc.RefreshStale()
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("upstream hits = %d, want 0 (RefreshStale must be refused after Stop)", n)
	}
}

// ---------------------------------------------------------------------------
// Final round: SyncTools atomicity, callback lifecycle, loop Stop, fill pending
// ---------------------------------------------------------------------------

// TestSyncTools_ConcurrentSyncNoCorruption drives two SyncTools writers
// concurrently against the same pi models.json (the runRefresh onDone path
// in production, but toolsMu + atomic replace must protect any caller). The
// final file must be ONE complete, parseable models.json containing BOTH
// providers' entries — never an interleaved or truncated mix — and no temp
// files may be left behind. Without the toolsMu serialization (or without
// the atomic rename) at least one of the assertions fails: concurrent
// os.WriteFile calls interleave or one writer's read-modify-write cycle is
// lost.
func TestSyncTools_ConcurrentSyncNoCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")

	mc := &ModelCache{
		dir: dir,
		caches: map[string]*providerCache{
			"pA": {Models: []ModelEntry{{ID: "mA", Object: "model", Created: 1, OwnedBy: "x"}}},
			"pB": {Models: []ModelEntry{{ID: "mB", Object: "model", Created: 1, OwnedBy: "x"}}},
		},
	}
	cfgA := &config.Config{
		Accounts: []config.AccountConfig{{Name: "a1", Provider: "pA", BaseURL: "http://127.0.0.1:1/v1"}},
		Tools:    map[string]string{"pi": path},
	}
	cfgB := &config.Config{
		Accounts: []config.AccountConfig{{Name: "a2", Provider: "pB", BaseURL: "http://127.0.0.1:2/v1"}},
		Tools:    map[string]string{"pi": path},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		mc.SyncTools(cfgA)
	}()
	go func() {
		defer wg.Done()
		mc.SyncTools(cfgB)
	}()
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read synced models.json: %v", err)
	}
	var pc struct {
		Providers map[string]struct {
			Models []map[string]any `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &pc); err != nil {
		t.Fatalf("synced models.json is corrupt (interleaved write?): %v", err)
	}
	for _, prov := range []string{"pA", "pB"} {
		if _, ok := pc.Providers[prov]; !ok {
			t.Errorf("provider %q missing from synced models.json (one writer's read-modify-write was lost): providers=%v", prov, pc.Providers)
		}
	}
	// The atomic replace must not leave temp litter.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read sync dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "models.json" {
		t.Fatalf("sync dir after concurrent syncs = %v, want exactly [models.json]", entries)
	}
}

// TestSyncPIModelsJSON_PreservesExistingMode: the atomic replace must keep
// the existing models.json permission bits (deployed files are e.g. 0664
// root:pi-sync so the service user can write through group permissions).
func TestSyncPIModelsJSON_PreservesExistingMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	writePIModelsJSON(t, path, map[string]any{})
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "a", Provider: "p", BaseURL: "http://127.0.0.1:1/v1"}},
	}
	mc := NewForTest(map[string]*providerCache{
		"p": {Models: []ModelEntry{{ID: "m1", Object: "model", Created: 1, OwnedBy: "x"}}},
	})
	if err := mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg); err != nil {
		t.Fatalf("syncPIModelsJSON: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0664 {
		t.Errorf("models.json mode after atomic replace = %o, want 0664", perm)
	}
	readPIModel(t, path, "p", "m1") // Fatals if missing/corrupt
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "models.json" {
		t.Fatalf("sync dir = %v, want exactly [models.json]", entries)
	}
}

// TestSyncPIModelsJSON_ChownFailAborts pins the item-4 guarantee: when the
// temp file's owner cannot be preserved (chown fails, e.g. EPERM for a
// non-root process under NoNewPrivileges), syncPIModelsJSON must ABORT with
// an error — there is no in-place fallback anymore. The original file, its
// inode, owner/group, mode and CONTENT stay untouched (byte-for-byte), and
// the aborted temp file leaves no litter. The chown failure is injected via
// chownFile, so the test is deterministic and independent of the test
// runner's privileges.
func TestSyncPIModelsJSON_ChownFailAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	old, ino0 := modelsJSONFixture(t, path)
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}

	fi0, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st0 := fi0.Sys().(*syscall.Stat_t)
	uid0, gid0 := st0.Uid, st0.Gid

	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "a", Provider: "p", BaseURL: "http://127.0.0.1:1/v1"}},
	}
	mc := NewForTest(map[string]*providerCache{
		"p": {Models: []ModelEntry{{ID: "m1", Object: "model", Created: 1, OwnedBy: "x"}}},
	})
	mc.chownFile = func(name string, uid, gid int) error { return syscall.EPERM }
	err = mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg)
	if err == nil {
		t.Fatal("syncPIModelsJSON must return an error when the temp chown fails")
	}

	// The old file is byte-for-byte intact on the SAME inode.
	assertFileUntouched(t, path, old, ino0)
	fi1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st1 := fi1.Sys().(*syscall.Stat_t)
	if st1.Uid != uid0 || st1.Gid != gid0 {
		t.Errorf("owner changed (uid/gid %d/%d → %d/%d): a chown-failed rename must never happen", uid0, gid0, st1.Uid, st1.Gid)
	}
	if perm := fi1.Mode().Perm(); perm != 0664 {
		t.Errorf("mode after aborted sync = %o, want 0664", perm)
	}
	// No temp litter from the aborted rename path.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "models.json" {
		t.Fatalf("sync dir = %v, want exactly [models.json]", entries)
	}
}

// TestSyncPIModelsJSON_ChownFailOnDirectoryAborts covers the same abort
// when the target path is a directory: the sync must fail (chown injected
// to fail) and leave the directory untouched with no temp litter — it must
// not attempt any in-place overwrite of a directory.
func TestSyncPIModelsJSON_ChownFailOnDirectoryAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "a", Provider: "p", BaseURL: "http://127.0.0.1:1/v1"}},
	}
	mc := NewForTest(map[string]*providerCache{
		"p": {Models: []ModelEntry{{ID: "m1", Object: "model", Created: 1, OwnedBy: "x"}}},
	})
	mc.chownFile = func(name string, uid, gid int) error { return syscall.EPERM }
	if err := mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg); err == nil {
		t.Fatal("syncPIModelsJSON must return an error when the temp chown fails")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Fatalf("target %s is no longer a directory after the aborted sync", path)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "models.json" {
		t.Fatalf("sync dir = %v, want exactly [models.json]", entries)
	}
}

// modelsJSONFixture writes an old models.json (a JSON document with a
// hand-edited "name" key, so the old content is distinguishable from any
// prism output) and returns its exact bytes plus the original inode.
func modelsJSONFixture(t *testing.T, path string) (old []byte, ino0 uint64) {
	t.Helper()
	oldJSON := `{"providers":{"p":{"baseUrl":"http://old","api":"openai-completions","models":[{"id":"m1","name":"hand-edited"}]}}}`
	if err := os.WriteFile(path, []byte(oldJSON), 0644); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return old, fi.Sys().(*syscall.Stat_t).Ino
}

// assertFileUntouched checks that path holds exactly old bytes on the same
// inode (an aborted sync must leave the deployed file untouched).
func assertFileUntouched(t *testing.T, path string, old []byte, ino0 uint64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file after aborted sync: %v", err)
	}
	if !bytes.Equal(data, old) {
		t.Fatalf("file after aborted sync = %q, want old content %q (must be byte-exact)", data, old)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if ino := fi.Sys().(*syscall.Stat_t).Ino; ino != ino0 {
		t.Errorf("inode changed (%d → %d): aborted sync must keep the original inode", ino0, ino)
	}
}

// TestFetchAllAsync_PendingManualNotDowngradedToFill verifies the priority
// rule with a correctly-set-up scenario: a MANUAL round is genuinely
// PENDING (a second RefreshAllAsync queued it while round 1 was in flight)
// when the startup fill arrives — the fill must NOT downgrade that pending
// manual to fill. A manual round refreshes every provider (the fill work is
// fully subsumed); downgrading would silently drop cached providers from
// the next round. Provider p1 already has a cache (a fill round would not
// fetch it, a manual round would), so the hit counts prove the pending kind
// stayed manual both structurally (pendingKind) and behaviorally (p1
// fetched twice: once per manual round). The first-time-while-busy case
// (no pending request yet) is the counterpart covered by
// TestFetchAllAsync_PendingFirstTimeWhileBusyIsFill.
func TestFetchAllAsync_PendingManualNotDowngradedToFill(t *testing.T) {
	var p1Hits int32
	entered1 := make(chan struct{})
	release1 := make(chan struct{})
	done := make(chan struct{})
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p1Hits, 1)
		if atomic.LoadInt32(&p1Hits) == 1 {
			select {
			case <-entered1:
			default:
				close(entered1)
			}
			select {
			case <-release1:
			case <-done:
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv1.Close()
	defer close(done) // runs before srv1.Close: unblocks the p1 handler if the test fails early

	var p2Hits int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv2.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k"},
			{Name: "a2", Provider: "p2", BaseURL: srv2.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir: t.TempDir(),
		caches: map[string]*providerCache{
			// p1 already cached (non-ollama): a fill round would NOT fetch
			// it; a manual round would.
			"p1": {Models: []ModelEntry{{ID: "m1", Object: "model", Created: 1, OwnedBy: "x"}}},
		},
		cfg:  cfg,
		pool: pool.NewPool(cfg.Accounts),
		stop: make(chan struct{}),
	}
	defer mc.Stop()

	fillDone := make(chan struct{})
	mc.RefreshAllAsync(nil) // round 1: manual, blocked on p1
	select {
	case <-entered1:
	case <-time.After(5 * time.Second):
		t.Fatal("round 1 never reached p1")
	}

	// A second manual request while round 1 is in flight: this is what
	// queues a genuinely PENDING manual round.
	mc.RefreshAllAsync(nil)
	mc.refreshMu.Lock()
	pend := mc.pending
	mc.refreshMu.Unlock()
	if !pend {
		t.Fatal("second RefreshAllAsync while busy must mark a pending round")
	}

	// The startup fill arrives while a MANUAL pending round is queued: the
	// pending round must stay MANUAL (a fill must never downgrade it).
	mc.FetchAllAsync(func() { close(fillDone) })
	mc.refreshMu.Lock()
	kind := mc.pendingKind
	mc.refreshMu.Unlock()
	if kind != roundManual {
		t.Fatalf("pendingKind after FetchAllAsync while manual pending = %v, want roundManual (existing pending manual must not be downgraded to fill)", kind)
	}

	close(release1)
	select {
	case <-fillDone:
	case <-time.After(10 * time.Second):
		t.Fatal("pending round never completed")
	}
	// Behavior: both rounds were manual — p1 (already cached) was fetched
	// twice. A downgraded fill round would have fetched p1 only once.
	if got := atomic.LoadInt32(&p1Hits); got != 2 {
		t.Errorf("p1 upstream hits = %d, want 2 (pending round must stay manual, not downgraded to fill)", got)
	}
	if got := atomic.LoadInt32(&p2Hits); got != 2 {
		t.Errorf("p2 upstream hits = %d, want 2 (manual rounds fetch every provider)", got)
	}
}

// TestFetchAllAsync_PendingWhenBusyKeepsOnDone: FetchAllAsync called while
// a manual round is in flight must NOT drop its onDone. The fill work is
// subsumed by the manual round, but the caller's onDone is remembered as a
// PENDING fill round and runs exactly once after it completes — the startup
// tools sync is never silently lost.
func TestFetchAllAsync_PendingWhenBusyKeepsOnDone(t *testing.T) {
	var hits int32
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}
	defer mc.Stop()

	// A manual round is in flight...
	mc.RefreshAllAsync(nil)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("manual round never reached the upstream")
	}

	// ...when the startup fill arrives. Its onDone must not be dropped.
	var fillRuns int32
	fillDone := make(chan struct{})
	mc.FetchAllAsync(func() {
		atomic.AddInt32(&fillRuns, 1)
		close(fillDone)
	})

	close(release)
	select {
	case <-fillDone:
	case <-time.After(10 * time.Second):
		t.Fatal("pending fill's onDone never ran (FetchAllAsync dropped it while busy)")
	}
	if n := atomic.LoadInt32(&fillRuns); n != 1 {
		t.Errorf("fill onDone ran %d times, want exactly 1", n)
	}
	if models := mc.GetModels("p"); models == nil {
		t.Error("manual round must have populated the cache (fill work subsumed)")
	}
}

// TestFetchAllAsync_PendingFirstTimeWhileBusyIsFill is the counterpart of
// the manual-not-downgraded rule: a fill requested while a round is in
// flight but NOTHING is pending yet must queue a FILL round, not a manual
// one. pendingKind alone cannot distinguish "manual pending" from "nothing
// pending" (startRoundLocked leaves roundManual behind as the default), so
// treating the default as manual would escalate the startup fill into a
// full manual refresh, re-fetching providers whose caches are fresh.
//
// Structure: after the first FetchAllAsync the pending kind must be
// roundFill; a second FetchAllAsync keeps it a fill and the pendingOnDone
// is latest-wins (the superseded onDone never runs). Behavior: provider p1
// is already cached (non-ollama, so a fill round must NOT fetch it) — with
// the fix the queued fill round fetches nothing after the manual round
// populated p2, so both upstreams see exactly ONE hit (the manual round);
// a wrongly-queued manual round would fetch both a second time.
func TestFetchAllAsync_PendingFirstTimeWhileBusyIsFill(t *testing.T) {
	var p1Hits int32
	entered1 := make(chan struct{})
	release1 := make(chan struct{})
	done := make(chan struct{})
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p1Hits, 1)
		if atomic.LoadInt32(&p1Hits) == 1 {
			select {
			case <-entered1:
			default:
				close(entered1)
			}
			select {
			case <-release1:
			case <-done:
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv1.Close()
	defer close(done) // runs before srv1.Close: unblocks the p1 handler if the test fails early

	var p2Hits int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv2.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k"},
			{Name: "a2", Provider: "p2", BaseURL: srv2.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir: t.TempDir(),
		caches: map[string]*providerCache{
			// p1 already cached (non-ollama): a fill round must NOT fetch it.
			"p1": {Models: []ModelEntry{{ID: "m1", Object: "model", Created: 1, OwnedBy: "x"}}},
		},
		cfg:  cfg,
		pool: pool.NewPool(cfg.Accounts),
		stop: make(chan struct{}),
	}
	defer mc.Stop()

	mc.RefreshAllAsync(nil) // round 1: manual, blocked on p1
	select {
	case <-entered1:
	case <-time.After(5 * time.Second):
		t.Fatal("round 1 never reached p1")
	}

	// First fill while busy with NO pending request: must queue a FILL.
	firstDone := make(chan struct{})
	mc.FetchAllAsync(func() { close(firstDone) })
	mc.refreshMu.Lock()
	pend := mc.pending
	kind := mc.pendingKind
	mc.refreshMu.Unlock()
	if !pend {
		t.Fatal("FetchAllAsync while busy must mark a pending round")
	}
	if kind != roundFill {
		t.Fatalf("pendingKind after first FetchAllAsync while busy = %v, want roundFill (the roundManual default must not be mistaken for a pending manual)", kind)
	}

	// Second fill while a fill is already pending: stays a fill, onDone
	// latest-wins (the first fill's onDone is superseded and must never
	// run).
	secondDone := make(chan struct{})
	mc.FetchAllAsync(func() { close(secondDone) })
	mc.refreshMu.Lock()
	kind = mc.pendingKind
	mc.refreshMu.Unlock()
	if kind != roundFill {
		t.Fatalf("pendingKind after second FetchAllAsync = %v, want roundFill (existing pending fill must stay a fill)", kind)
	}

	close(release1)
	select {
	case <-secondDone:
	case <-time.After(10 * time.Second):
		t.Fatal("pending round never completed")
	}
	select {
	case <-firstDone:
		t.Fatal("superseded fill's onDone ran (pendingOnDone must be latest-wins)")
	case <-time.After(200 * time.Millisecond):
	}

	// Behavior: the queued round was a FILL. Round 1 (manual) fetched p1
	// and p2 once each; the fill round fetched neither (both cached by
	// then). A wrongly-queued manual round would have fetched both again.
	if got := atomic.LoadInt32(&p1Hits); got != 1 {
		t.Errorf("p1 upstream hits = %d, want 1 (pending round must be fill: cached p1 must not be re-fetched)", got)
	}
	if got := atomic.LoadInt32(&p2Hits); got != 1 {
		t.Errorf("p2 upstream hits = %d, want 1 (pending fill round must not do a full manual refresh)", got)
	}
}

// TestFetchAllAsync_UpgradePendingManualToFullAndMergeOnDone tests that when
// FetchAllAsync is called while a manual single-target round is pending, it
// upgrades the pending target to full manual (target="") so no providers are
// missed, and merges both completion callbacks without dropping either.
func TestFetchAllAsync_UpgradePendingManualToFullAndMergeOnDone(t *testing.T) {
	var p1Hits, p2Hits int32
	entered1 := make(chan struct{})
	release1 := make(chan struct{})
	done := make(chan struct{})
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p1Hits, 1)
		if atomic.LoadInt32(&p1Hits) == 1 {
			select {
			case <-entered1:
			default:
				close(entered1)
			}
			select {
			case <-release1:
			case <-done:
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv1.Close()
	defer close(done)

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m2","object":"model","created":1,"owned_by":"y"}]}`))
	}))
	defer srv2.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k"},
			{Name: "a2", Provider: "p2", BaseURL: srv2.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool:   pool.NewPool(cfg.Accounts),
		stop:   make(chan struct{}),
	}
	defer mc.Stop()

	// Round 1: manual refresh, blocks on p1
	mc.RefreshAllAsync(nil)
	select {
	case <-entered1:
	case <-time.After(5 * time.Second):
		t.Fatal("round 1 never reached p1")
	}

	// Queue pending manual for single provider "p1"
	manualDone := make(chan struct{})
	mc.RefreshOneAsync("p1", func() { close(manualDone) })

	mc.refreshMu.Lock()
	if !mc.pending || mc.pendingKind != roundManual || mc.pendingTarget != "p1" {
		mc.refreshMu.Unlock()
		t.Fatalf("expected pending manual round targeting p1, got pending=%v kind=%v target=%q", mc.pending, mc.pendingKind, mc.pendingTarget)
	}
	mc.refreshMu.Unlock()

	// Now startup fill arrives: must upgrade pending target from "p1" to "" (full)
	// and preserve/merge onDone
	fillDone := make(chan struct{})
	mc.FetchAllAsync(func() { close(fillDone) })

	mc.refreshMu.Lock()
	if !mc.pending || mc.pendingKind != roundManual || mc.pendingTarget != "" {
		mc.refreshMu.Unlock()
		t.Fatalf("expected pending manual upgraded to full (target=\"\"), got pending=%v kind=%v target=%q", mc.pending, mc.pendingKind, mc.pendingTarget)
	}
	mc.refreshMu.Unlock()

	close(release1)

	select {
	case <-manualDone:
	case <-time.After(10 * time.Second):
		t.Fatal("manual onDone never ran")
	}

	select {
	case <-fillDone:
	case <-time.After(10 * time.Second):
		t.Fatal("fill onDone never ran")
	}

	// Round 1 fetched p1 and p2 (1 each).
	// Pending round was upgraded to manual full, so it fetched p1 and p2 again (2 hits each total).
	if got := atomic.LoadInt32(&p1Hits); got != 2 {
		t.Errorf("p1 hits = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&p2Hits); got != 2 {
		t.Errorf("p2 hits = %d, want 2 (upgraded full manual must fetch both)", got)
	}
}

// TestRefreshAllAsync_OnDoneSerializedAcrossRounds: a later round's onDone
// must not start until the earlier round's onDone has finished. Round 1's
// callback blocks; round 2 is requested, runs and finishes — its callback
// is queued behind doneMu, not executed concurrently.
func TestRefreshAllAsync_OnDoneSerializedAcrossRounds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}
	defer mc.Stop()

	onDone1Entered := make(chan struct{})
	releaseOnDone1 := make(chan struct{})
	var runs1, runs2 int32
	mc.RefreshAllAsync(func() {
		atomic.AddInt32(&runs1, 1)
		close(onDone1Entered)
		<-releaseOnDone1
	})
	select {
	case <-onDone1Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("round 1's onDone never started")
	}

	// Round 2 runs and finishes while round 1's callback is still blocked:
	// its callback must be queued, not run concurrently.
	onDone2Entered := make(chan struct{})
	mc.RefreshAllAsync(func() {
		atomic.AddInt32(&runs2, 1)
		close(onDone2Entered)
	})
	select {
	case <-onDone2Entered:
		t.Fatal("round 2's onDone ran while round 1's onDone was still running (callbacks must be serialized)")
	case <-time.After(300 * time.Millisecond):
	}

	close(releaseOnDone1)
	select {
	case <-onDone2Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("round 2's onDone never ran after round 1's completed")
	}
	if atomic.LoadInt32(&runs1) != 1 || atomic.LoadInt32(&runs2) != 1 {
		t.Errorf("onDone runs: round1=%d round2=%d, want 1 each", atomic.LoadInt32(&runs1), atomic.LoadInt32(&runs2))
	}
}

// TestRefreshAllAsync_StopWaitsForBlockingOnDone verifies Stop waits for a
// launched onDone that is still running: Stop must NOT return while the
// callback is blocked, and the callback's final "models.json write" (a
// marker file, standing in for the SyncTools write) is complete before Stop
// returns — after Stop no callback work can be in flight.
func TestRefreshAllAsync_StopWaitsForBlockingOnDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	marker := filepath.Join(dir, "synced-marker")
	mc := &ModelCache{
		dir:    dir,
		caches: map[string]*providerCache{},
		cfg: &config.Config{
			Accounts: []config.AccountConfig{
				{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
			},
			MaxConcurrentPerAccount: map[string]int{"*": 1},
		},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	cbEntered := make(chan struct{})
	releaseCb := make(chan struct{})
	cbFinished := make(chan struct{})
	mc.RefreshAllAsync(func() {
		close(cbEntered)
		<-releaseCb
		// The last thing the callback does is its write (standing in for
		// the SyncTools models.json write).
		if err := os.WriteFile(marker, []byte("synced"), 0644); err != nil {
			t.Errorf("callback marker write: %v", err)
		}
		close(cbFinished)
	})
	select {
	case <-cbEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("onDone never started")
	}

	stopDone := make(chan struct{})
	go func() {
		mc.Stop()
		close(stopDone)
	}()
	// Stop must NOT return while the callback is still blocked.
	select {
	case <-stopDone:
		t.Fatal("Stop returned while a launched onDone was still running")
	case <-time.After(300 * time.Millisecond):
	}

	close(releaseCb)
	select {
	case <-cbFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("onDone never finished after release")
	}
	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never returned after the onDone finished")
	}
	// The callback finished (and its marker write completed) BEFORE Stop
	// returned: no callback work, hence no models.json write, can follow
	// Stop.
	if data, err := os.ReadFile(marker); err != nil || string(data) != "synced" {
		t.Errorf("callback marker = %q, err=%v; want complete write before Stop returns", data, err)
	}
	// Stop is idempotent.
	mc.Stop()
}

// TestRefreshAllAsync_OnDoneAsyncStopNoDeadlock pins the callback contract:
// onDone must NOT call Stop synchronously — Stop waits for every launched
// callback (doneWG), so a synchronous Stop from inside a callback would
// deadlock Stop on itself. The supported shape is an ASYNC stop triggered
// from the callback: Stop runs on its own goroutine, waits for the in-flight
// callback to finish, and returns only after it has. Production callbacks
// are pure SyncTools (the three call sites in cmd/prism) and never call
// Stop at all; this test pins the async shape as the only deadlock-free
// self-stop.
func TestRefreshAllAsync_OnDoneAsyncStopNoDeadlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg: &config.Config{
			Accounts: []config.AccountConfig{
				{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
			},
			MaxConcurrentPerAccount: map[string]int{"*": 1},
		},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	cbEntered := make(chan struct{})
	releaseCb := make(chan struct{})
	cbDone := make(chan struct{})
	stopReturned := make(chan struct{})
	mc.RefreshAllAsync(func() {
		close(cbEntered)
		// Async stop from inside the callback: this is the supported shape
		// (e.g. a shutdown signal arriving mid-callback). It must not
		// deadlock — Stop waits for THIS callback, which then completes.
		go func() {
			mc.Stop()
			close(stopReturned)
		}()
		<-releaseCb
		close(cbDone)
	})
	select {
	case <-cbEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("onDone never started")
	}
	select {
	case <-stopReturned:
		t.Fatal("Stop returned while the in-flight onDone was still blocked")
	case <-time.After(300 * time.Millisecond):
	}
	close(releaseCb)
	select {
	case <-cbDone:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: onDone never completed after async Stop")
	}
	select {
	case <-stopReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("async Stop never returned after the onDone completed")
	}
	// Stop is idempotent and the state is clean.
	mc.Stop()
	mc.refreshMu.Lock()
	refreshing, stopped := mc.refreshing, mc.stopped
	mc.refreshMu.Unlock()
	if refreshing {
		t.Error("Stop returned with refreshing=true")
	}
	if !stopped {
		t.Error("Stop returned with stopped=false")
	}
}

// TestRefreshLoop_StopWaitsForLoopExit: the StartRefreshLoop goroutine is
// tracked in loopWG; Stop returns only after the loop has exited. After
// Stop no further stale ticks can fire (a leaked loop would keep requesting
// upstream). The upstream always fails (500), so the cache stays stale and
// every tick provably reaches the server while the loop is alive.
func TestRefreshLoop_StopWaitsForLoopExit(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg: &config.Config{
			Accounts: []config.AccountConfig{
				{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
			},
			MaxConcurrentPerAccount: map[string]int{"*": 1},
		},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}
	mc.SetBackoffInitialForTest(time.Millisecond)
	mc.SetStaggerForTest(0)

	mc.StartRefreshLoop(5*time.Millisecond, nil)
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&hits) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("stale ticker never produced upstream hits")
		}
		time.Sleep(2 * time.Millisecond)
	}

	start := time.Now()
	mc.Stop()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Stop took %v, want prompt return after loop exit", elapsed)
	}
	before := atomic.LoadInt32(&hits)
	time.Sleep(50 * time.Millisecond)
	if after := atomic.LoadInt32(&hits); after != before {
		t.Errorf("upstream hits after Stop = %d, want frozen at %d (loop still alive)", after, before)
	}
}

// ---------------------------------------------------------------------------
// Round-3 audit: multi-account failover, same-provider fetch merging,
// RefreshAll scheduler, safe connection errors
// ---------------------------------------------------------------------------

// TestFetch_FailoverFirstSaturatedSecondSucceeds is the item-1 core: with
// two healthy accounts for the provider, the FIRST account saturated (its
// concurrency slot occupied by a business request), the fetch must move to
// the second account instead of failing with ErrFetchSaturated. The first
// account's upstream must never be touched, and every acquired slot is
// released exactly once (both accounts back to in-flight 0).
func TestFetch_FailoverFirstSaturatedSecondSucceeds(t *testing.T) {
	hits1 := make(chan struct{}, 1)
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits1 <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv2.Close()

	cfg := &config.Config{
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv1.URL, Key: "k"},
			{Name: "a2", Provider: "p", BaseURL: srv2.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	accs := mc.pool.AllAccounts()
	// Occupy the first account's single slot: the fetch must fail over to a2.
	slot0 := accs[0].TryAcquire("", 1)
	if slot0 == nil {
		t.Fatal(`test setup: TryAcquire("", 1) on a1 failed`)
	}

	err := mc.Fetch("p")
	accs[0].Release(slot0)

	if err != nil {
		t.Fatalf("Fetch must fail over to the second account, got: %v", err)
	}
	models := mc.GetModels("p")
	if len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("fetched models = %+v, want [m1] (from account 2)", models)
	}
	select {
	case <-hits1:
		t.Error("saturated first account must never be touched by the fetch")
	default:
	}
	if got := accs[0].InFlightCount(); got != 0 {
		t.Errorf("a1 in-flight after fetch = %d, want 0", got)
	}
	if got := accs[1].InFlightCount(); got != 0 {
		t.Errorf("a2 in-flight after fetch = %d, want 0 (exactly one release)", got)
	}
}

// TestFetch_FailoverAllSaturated: when EVERY healthy account is at its
// concurrency cap the fetch returns ErrFetchSaturated (mapped by
// internal/proxy to 503 model_fetch_saturated) and no upstream is touched.
func TestFetch_FailoverAllSaturated(t *testing.T) {
	hits := make(chan struct{}, 2)
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv2.Close()

	cfg := &config.Config{
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv1.URL, Key: "k"},
			{Name: "a2", Provider: "p", BaseURL: srv2.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	accs := mc.pool.AllAccounts()
	s0 := accs[0].TryAcquire("", 1)
	s1 := accs[1].TryAcquire("", 1)
	if s0 == nil || s1 == nil {
		t.Fatal(`test setup: TryAcquire("", 1) failed`)
	}

	err := mc.Fetch("p")
	accs[0].Release(s0)
	accs[1].Release(s1)

	if !errors.Is(err, ErrFetchSaturated) {
		t.Fatalf("Fetch error = %v, want ErrFetchSaturated", err)
	}
	select {
	case <-hits:
		t.Error("all-saturated fetch must not send any upstream request")
	default:
	}
	if got := accs[0].InFlightCount(); got != 0 {
		t.Errorf("a1 in-flight = %d, want 0", got)
	}
	if got := accs[1].InFlightCount(); got != 0 {
		t.Errorf("a2 in-flight = %d, want 0", got)
	}
}

// TestFetch_FailoverAcquiredThenSaturated pins the final-review error
// classification: account A is ACQUIRED and its upstream fetch fails (500),
// account B is saturated (slot occupied). The fetch must return A's real
// upstream error — NEVER ErrFetchSaturated — because a fetch that actually
// reached an upstream and failed must surface as 502 model_fetch_failed,
// not as a misleading "saturated" (503). Saturated is reserved for the case
// where NO account could be acquired at all. A's slot must still be
// released exactly once.
func TestFetch_FailoverAcquiredThenSaturated(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv1.Close()

	cfg := &config.Config{
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv1.URL, Key: "k"},
			{Name: "a2", Provider: "p", BaseURL: "http://127.0.0.1:1", Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	accs := mc.pool.AllAccounts()
	// Occupy a2's single slot: a1 will be acquired and fail, a2 stays
	// saturated — the exact mixed-failure shape that must NOT report
	// saturated.
	slot1 := accs[1].TryAcquire("", 1)
	if slot1 == nil {
		t.Fatal(`test setup: TryAcquire("", 1) on a2 failed`)
	}

	err := mc.Fetch("p")
	accs[1].Release(slot1)

	if err == nil {
		t.Fatal("Fetch must fail (a1 upstream 500, a2 saturated)")
	}
	if errors.Is(err, ErrFetchSaturated) {
		t.Fatalf("Fetch error = %v: an acquired-then-failed fetch must NEVER report saturated", err)
	}
	if errors.Is(err, ErrNoHealthyAccount) {
		t.Fatalf("Fetch error = %v: accounts are healthy, must not report no_healthy", err)
	}
	if !strings.Contains(err.Error(), "upstream returned 500") {
		t.Errorf("Fetch error = %v, want a1's real upstream error (upstream returned 500)", err)
	}
	// a1's acquired slot must be released exactly once.
	if got := accs[0].InFlightCount(); got != 0 {
		t.Errorf("a1 in-flight after fetch = %d, want 0 (acquired slot must be released)", got)
	}
	if got := accs[1].InFlightCount(); got != 0 {
		t.Errorf("a2 in-flight after fetch = %d, want 0", got)
	}
}

// TestFetch_FailoverFirstUpstreamFailsSecondSucceeds: a temporary upstream
// failure (500) on the first account must fail over to the second account,
// which succeeds — the fetch as a whole returns nil and the cache is
// populated. Both accounts are attempted exactly once and every slot is
// released.
func TestFetch_FailoverFirstUpstreamFailsSecondSucceeds(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m2","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv2.Close()

	cfg := &config.Config{
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv1.URL, Key: "k"},
			{Name: "a2", Provider: "p", BaseURL: srv2.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	if err := mc.Fetch("p"); err != nil {
		t.Fatalf("Fetch must fail over after the first account's 500, got: %v", err)
	}
	models := mc.GetModels("p")
	if len(models) != 1 || models[0].ID != "m2" {
		t.Errorf("fetched models = %+v, want [m2] (from account 2)", models)
	}
	for i, acc := range mc.pool.AllAccounts() {
		if got := acc.InFlightCount(); got != 0 {
			t.Errorf("account %d in-flight after fetch = %d, want 0", i, got)
		}
	}
}

// TestFetch_FailoverNoHealthyAccount: no healthy account at all still maps
// to ErrNoHealthyAccount (not a confusing aggregate error).
func TestFetch_FailoverNoHealthyAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    &config.Config{MaxConcurrentPerAccount: map[string]int{"*": 1}},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	acc := mc.pool.AllAccounts()[0]
	acc.MarkExhausted()

	err := mc.Fetch("p")
	if !errors.Is(err, ErrNoHealthyAccount) {
		t.Fatalf("Fetch error = %v, want ErrNoHealthyAccount", err)
	}
}

// TestFetch_ConcurrentSameProviderSingleUpstream is the item-2 core: 20
// concurrent Fetch calls for the SAME provider collapse into exactly ONE
// upstream request. The leader blocks in the upstream handler while all 20
// goroutines are provably launched, so every one of them either leads or
// joins the in-flight fetch — the hit count proves the merge.
func TestFetch_ConcurrentSameProviderSingleUpstream(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	const n = 20
	var started int32
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			atomic.AddInt32(&started, 1)
			errs[idx] = mc.Fetch("p")
		}(i)
	}

	// The leader is provably blocked at the upstream; wait until all 20
	// goroutines are launched (they can only lead or join — the leader
	// cannot finish before release), then release the upstream.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader never reached the upstream")
	}
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&started) < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d goroutines launched", atomic.LoadInt32(&started), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(release)

	wg.Wait()
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hits = %d, want exactly 1 (20 same-provider fetches merged)", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("fetch %d failed: %v", i, err)
		}
	}
	models := mc.GetModels("p")
	if len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("fetched models = %+v, want [m1]", models)
	}
}

// TestFetch_DifferentProvidersParallel: fetches for DIFFERENT providers
// must run concurrently — each upstream is entered while the other fetch is
// still blocked, which a global (or per-provider) serialization would make
// impossible.
func TestFetch_DifferentProvidersParallel(t *testing.T) {
	enteredA := make(chan struct{})
	enteredB := make(chan struct{})
	release := make(chan struct{})
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-enteredA:
		default:
			close(enteredA)
		}
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"mA","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-enteredB:
		default:
			close(enteredB)
		}
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"mB","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srvB.Close()

	cfg := &config.Config{
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "aA", Provider: "pA", BaseURL: srvA.URL, Key: "k"},
			{Name: "aB", Provider: "pB", BaseURL: srvB.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	errCh := make(chan error, 2)
	go func() { errCh <- mc.Fetch("pA") }()
	go func() { errCh <- mc.Fetch("pB") }()

	select {
	case <-enteredA:
	case <-time.After(5 * time.Second):
		t.Fatal("pA fetch never reached its upstream")
	}
	select {
	case <-enteredB:
	case <-time.After(5 * time.Second):
		t.Fatal("pB fetch never reached its upstream while pA was still blocked (different providers must run in parallel)")
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("parallel fetch failed: %v", err)
		}
	}
}

// TestFetch_FollowerCancelIndependent: a follower whose own context is
// cancelled returns its cancellation immediately WITHOUT cancelling the
// leader — the leader stays blocked at the upstream, completes, and
// publishes the cache. Exactly one upstream request is made.
func TestFetch_FollowerCancelIndependent(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	leaderDone := make(chan error, 1)
	go func() { leaderDone <- mc.fetchWithContext(context.Background(), "p") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader never reached the upstream")
	}

	// Follower with an ALREADY-cancelled context: must return immediately
	// with context.Canceled and must NOT cancel the leader.
	fctx, fcancel := context.WithCancel(context.Background())
	fcancel()
	fstart := time.Now()
	err := mc.fetchWithContext(fctx, "p")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("follower error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(fstart); elapsed > 2*time.Second {
		t.Errorf("follower cancellation took %v, want immediate return", elapsed)
	}

	// The leader must still be alive (its own context is untouched).
	select {
	case <-leaderDone:
		t.Fatal("leader finished when only the follower was cancelled")
	default:
	}
	close(release)
	select {
	case lerr := <-leaderDone:
		if lerr != nil {
			t.Errorf("leader failed: %v", lerr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("leader never completed after release")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1", got)
	}
	models := mc.GetModels("p")
	if len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("fetched models = %+v, want [m1]", models)
	}
}

// TestFetch_LeaderPanicCleansInflightEntry pins the final-review item 1: a
// same-provider fetch leader that dies abnormally (panic inside
// fetchLeader, forced via the test-only fetchLeaderHook) must clean up its
// in-flight entry on the way out:
//   - followers parked on the leader's done channel finish with a SAFE
//     generic error (ErrFetchAborted) — never the panic value — instead of
//     waiting forever;
//   - the fetches map entry is removed, so a later same-provider Fetch
//     becomes the leader again and completes normally;
//   - the panic itself keeps propagating (the cleanup is a defer state
//     machine, not a recover that swallows the panic).
//
// The follower outcome has two defined shapes: it either parks on done and
// receives ErrFetchAborted (the regression this pins) or — if it arrives
// after the cleanup removed the entry — it becomes the leader itself and
// completes the upstream round. Both finish; the assertion set accepts
// both and the permanent-wait bug fails the deadline either way.
func TestFetch_LeaderPanicCleansInflightEntry(t *testing.T) {
	entered := make(chan struct{})
	goPanic := make(chan struct{})
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv.Close()

	// Fake "credential-looking" panic value: it must never reach the
	// follower error or any log line.
	const panicSecret = "test-panic-secret-7f3a"
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg: &config.Config{
			MaxConcurrentPerAccount: map[string]int{"*": 1},
		},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop: make(chan struct{}),
		fetchLeaderHook: func() {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-goPanic
			panic(panicSecret)
		},
	}

	// Leader: registers the in-flight entry, then panics inside fetchLeader
	// (the hook is called at the top of fetchLeader, before any upstream
	// work). The wrapper recovers the propagated panic for the test.
	leaderPanic := make(chan any, 1)
	leaderDone := make(chan error, 1)
	go func() {
		defer func() { leaderPanic <- recover() }()
		leaderDone <- mc.fetchWithContext(context.Background(), "p")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader never entered fetchLeader")
	}

	// Follower joins while the leader is still parked in the hook: the
	// entry exists, so it MUST join (it cannot become the leader yet) and
	// park on done.
	followerStarted := make(chan struct{})
	followerErrCh := make(chan error, 1)
	go func() {
		close(followerStarted)
		followerErrCh <- mc.fetchWithContext(context.Background(), "p")
	}()

	select {
	case <-followerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("follower never started")
	}
	close(goPanic)

	// The panic must propagate out of fetchWithContext (cleanup is not a
	// recover) and reach the test wrapper's recover with its original value.
	select {
	case v := <-leaderPanic:
		if v != panicSecret {
			t.Fatalf("leader panic value = %v, want the injected panic (must propagate, not be swallowed)", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leader panic never propagated to the caller")
	}
	select {
	case <-leaderDone:
		t.Fatal("panicking leader returned a value")
	default:
	}

	// The follower must finish: parked followers must never wait forever
	// after the leader dies. The error must be the safe sentinel — and even
	// the "arrived after cleanup and became leader" shape must not leak the
	// panic value.
	select {
	case err := <-followerErrCh:
		if err != nil {
			if !errors.Is(err, ErrFetchAborted) {
				t.Fatalf("follower error = %v, want ErrFetchAborted", err)
			}
			if strings.Contains(err.Error(), panicSecret) {
				t.Fatalf("follower error leaks the panic value: %v", err)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follower never finished after the leader panicked (permanent wait)")
	}

	// No dead entry may be left behind for the provider.
	mc.fetchMu.Lock()
	left := len(mc.fetches)
	mc.fetchMu.Unlock()
	if left != 0 {
		t.Fatalf("fetches map has %d leftover entries after leader panic, want 0", left)
	}

	// A subsequent same-provider Fetch must become the leader again and
	// complete normally with a fresh upstream round. The test hook must be
	// removed first: it is a fetchLeader injection, so a still-installed
	// hook would panic the new leader too.
	mc.fetchLeaderHook = nil
	if err := mc.Fetch("p"); err != nil {
		t.Fatalf("subsequent Fetch after leader panic failed: %v", err)
	}
	models := mc.GetModels("p")
	if len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("fetched models = %+v, want [m1]", models)
	}
	if got := atomic.LoadInt32(&hits); got == 0 {
		t.Error("subsequent fetch never reached the upstream (must have become the leader)")
	}
}

// TestFetch_LeaderErrorSharedWithFollower pins the final-review must-fix: a
// same-provider follower that joined an in-flight leader fetch must receive
// the leader's REAL failure (non-nil, identical, recognizable) on the
// normal-failure paths — upstream 5xx and parse failure — never the
// zero-value nil (a nil follower error makes the proxy answer 200 with an
// empty model list) and never ErrFetchAborted (the proxy classifies
// 502/503 from the real error).
//
// Determinism: the leader is provably blocked at the upstream (entered)
// while the follower starts, so the in-flight entry is still registered and
// the follower MUST join it. Release happens only after the follower is
// launched, and the upstream adds a slow tail after release (200ms before
// the failure response), so the leader cannot close done before the
// follower has parked — the hits == 1 assertion proves the join: a follower
// that missed the entry would have become a second leader and issued a
// second upstream request.
func TestFetch_LeaderErrorSharedWithFollower(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		want string // recognizable fragment of the real upstream error
	}{
		{name: "upstream500", code: http.StatusInternalServerError, body: `boom`, want: "500"},
		{name: "parseFailure", code: http.StatusOK, body: `not json at all`, want: "decode response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				select {
				case <-entered:
				default:
					close(entered)
				}
				<-release
				time.Sleep(200 * time.Millisecond) // slow tail: see the comment above
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			mc := &ModelCache{
				dir:    t.TempDir(),
				caches: map[string]*providerCache{},
				cfg: &config.Config{
					MaxConcurrentPerAccount: map[string]int{"*": 1},
				},
				pool: pool.NewPool([]config.AccountConfig{
					{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
				}),
				stop: make(chan struct{}),
			}

			// Leader: registers the in-flight entry and blocks at the
			// upstream until release.
			leaderErrCh := make(chan error, 1)
			go func() { leaderErrCh <- mc.fetchWithContext(context.Background(), "p") }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("leader never reached the upstream")
			}

			// Follower: the leader's entry is still registered (it cannot
			// finish before release), so the follower MUST join it.
			followerStarted := make(chan struct{})
			followerErrCh := make(chan error, 1)
			go func() {
				close(followerStarted)
				followerErrCh <- mc.fetchWithContext(context.Background(), "p")
			}()
			select {
			case <-followerStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("follower never started")
			}
			close(release)

			var leaderErr, followerErr error
			select {
			case leaderErr = <-leaderErrCh:
			case <-time.After(10 * time.Second):
				t.Fatal("leader never finished")
			}
			select {
			case followerErr = <-followerErrCh:
			case <-time.After(10 * time.Second):
				t.Fatal("follower never finished")
			}

			// The regression: the follower must fail with the leader's real
			// error — never nil.
			if leaderErr == nil {
				t.Fatal("leader error = nil, want a non-nil upstream failure")
			}
			if followerErr == nil {
				t.Fatalf("follower error = nil, want the shared leader error %v", leaderErr)
			}
			if !strings.Contains(leaderErr.Error(), tc.want) {
				t.Errorf("leader error = %v, want a recognizable upstream error containing %q", leaderErr, tc.want)
			}
			if followerErr.Error() != leaderErr.Error() {
				t.Errorf("follower error = %v, want the leader's identical error %v", followerErr, leaderErr)
			}
			if errors.Is(followerErr, ErrFetchAborted) {
				t.Errorf("follower error = %v, want the real upstream error, not ErrFetchAborted (proxy 502/503 classification depends on it)", followerErr)
			}
			// Exactly one upstream request: the follower must have joined,
			// not become a second leader.
			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Errorf("upstream hits = %d, want 1 (follower must join the leader, not refetch)", got)
			}
		})
	}
}

// TestFetch_FailoverSharedBudget pins the final-review item 2: N accounts
// that fail slowly in sequence must respect ONE total budget for the whole
// failover round — never N × budget. The test injects a short fetchBudget
// (no real 30s wait) and controlled per-attempt upstream delays, then
// proves the round stops when the shared budget expires mid-attempt:
// account 1 and 2 each burn a full delay, account 3's attempt is cut by
// the budget, and the total stays ~one budget instead of three.
func TestFetch_FailoverSharedBudget(t *testing.T) {
	const perAttempt = 100 * time.Millisecond // controlled slow failure per account
	const budget = 280 * time.Millisecond     // one shared round budget (injected)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(perAttempt) // consume budget, then fail
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"slow failure"}}`))
	}))
	defer srv.Close()

	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg: &config.Config{
			MaxConcurrentPerAccount: map[string]int{"*": 1},
		},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
			{Name: "a2", Provider: "p", BaseURL: srv.URL, Key: "k"},
			{Name: "a3", Provider: "p", BaseURL: srv.URL, Key: "k"},
		}),
		stop:        make(chan struct{}),
		fetchBudget: budget,
	}

	start := time.Now()
	err := mc.Fetch("p")
	elapsed := time.Since(start)

	// Every account must have been attempted (the failover really walked
	// the list) before the shared budget cut the round short. Attempt 3's
	// request is sent ~200ms in and the budget expires ~80ms later, so the
	// delivery window is ample.
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("upstream hits = %d, want 3 (every account attempted before the budget expired)", got)
	}
	// The round must end at ~ONE budget, never N × budget: a per-account
	// timeout would take >= 3×budget = 840ms, far above the threshold.
	if elapsed >= 2*budget {
		t.Errorf("failover took %v, want < %v (one shared budget, not per-account)", elapsed, 2*budget)
	}
	// The two full attempts put a real floor under the elapsed time: a
	// result far below means the accounts were not exercised sequentially
	// (or the budget fired before any attempt, i.e. the loop was skipped).
	if elapsed < 2*perAttempt-50*time.Millisecond {
		t.Errorf("failover took %v, want >= ~%v (sequential attempts consumed real time)", elapsed, 2*perAttempt-50*time.Millisecond)
	}
	// The budget firing mid-attempt surfaces as the classified timeout.
	if err == nil || !strings.Contains(err.Error(), "upstream_timeout") {
		t.Fatalf("Fetch error = %v, want the shared-budget upstream_timeout", err)
	}
	// Every acquired slot is released exactly once.
	for i, acc := range mc.pool.AllAccounts() {
		if got := acc.InFlightCount(); got != 0 {
			t.Errorf("account %d in-flight after fetch = %d, want 0", i, got)
		}
	}
}

// TestRefreshAll_SchedulerInterleave pins item 9: RefreshAll runs as a
// SYNCHRONOUS manual round through the shared scheduler. While it is in
// flight, a second RefreshAll is REFUSED (returns immediately — the running
// manual round already subsumes its work), and a RefreshAllAsync request is
// coalesced into exactly one pending round that runs when the first
// finishes.
func TestRefreshAll_SchedulerInterleave(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			select {
			case <-entered:
			default:
				close(entered)
			}
			select {
			case <-release:
			case <-done:
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()
	defer close(done) // runs before srv.Close: unblocks the handler if the test fails early

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool:   pool.NewPool(cfg.Accounts),
		stop:   make(chan struct{}),
	}
	defer mc.Stop()

	refreshAllDone := make(chan struct{})
	go func() {
		mc.RefreshAll()
		close(refreshAllDone)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("RefreshAll round never reached the upstream")
	}

	// A second synchronous RefreshAll while a round is live must be refused
	// immediately (the running manual round subsumes it).
	refusedStart := time.Now()
	mc.RefreshAll()
	if elapsed := time.Since(refusedStart); elapsed > time.Second {
		t.Errorf("second RefreshAll took %v, want immediate refusal while a round is in flight", elapsed)
	}

	// An Async request during the round is coalesced into one pending round.
	onDoneCh := make(chan struct{}, 1)
	mc.RefreshAllAsync(func() { onDoneCh <- struct{}{} })

	close(release)
	select {
	case <-refreshAllDone:
	case <-time.After(10 * time.Second):
		t.Fatal("first RefreshAll round never completed")
	}
	// The pending async round runs exactly once more (hits == 2), then the
	// onDone callback fires.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&hits) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("pending async round never ran (hits = %d)", atomic.LoadInt32(&hits))
		}
		time.Sleep(2 * time.Millisecond)
	}
	select {
	case <-onDoneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("pending round's onDone never fired")
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("upstream hits = %d, want 2 (manual round + one coalesced pending round)", got)
	}
}

// TestRefreshAll_StopCancels pins item 9's Stop semantics: an in-flight
// synchronous RefreshAll is aborted by Stop (the shared lifecycle context
// is cancelled) and both RefreshAll and Stop return — no deadlock, no cache
// file published after shutdown began.
func TestRefreshAll_StopCancels(t *testing.T) {
	entered := make(chan struct{})
	blockForever := make(chan struct{})
	done := make(chan struct{})
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case <-entered:
		default:
			close(entered)
		}
		select {
		case <-blockForever:
		case <-done:
			return
		}
	}))
	defer srv.Close()
	defer close(done) // runs before srv.Close: unblocks the handler if the test fails early

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: srv.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool:   pool.NewPool(cfg.Accounts),
		stop:   make(chan struct{}),
	}

	refreshAllDone := make(chan struct{})
	go func() {
		mc.RefreshAll()
		close(refreshAllDone)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("RefreshAll round never reached the upstream")
	}

	stopDone := make(chan struct{})
	go func() {
		mc.Stop()
		close(stopDone)
	}()

	select {
	case <-refreshAllDone:
	case <-time.After(10 * time.Second):
		t.Fatal("RefreshAll did not return after Stop cancelled the round")
	}
	select {
	case <-stopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after the round exited")
	}
	// The cancelled round must not have published a cache file.
	if fp, err := mc.filePath("p"); err == nil {
		if _, statErr := os.Stat(fp); statErr == nil {
			t.Error("cache file published after Stop cancelled the round")
		}
	}
}

// TestFetch_ConnErrorNoURLLeak pins item 10 for the cache path: a
// connection error (url.Error) must never leak the upstream URL — including
// a base URL that embeds credentials — or any query text into the returned
// error (which runRefresh / RefreshAll / proxyModels log verbatim). Only the
// safe classification survives.
func TestFetch_ConnErrorNoURLLeak(t *testing.T) {
	// Grab a port, then close the listener so the fetch hits "connection
	// refused" with a real *url.Error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://user:sekrit-query-value@" + ln.Addr().String()
	ln.Close()

	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    &config.Config{MaxConcurrentPerAccount: map[string]int{"*": 1}},
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "a1", Provider: "p", BaseURL: deadURL, Key: "k"},
		}),
		stop: make(chan struct{}),
	}

	err = mc.Fetch("p")
	if err == nil {
		t.Fatal("Fetch over a refused connection must return an error")
	}
	msg := err.Error()
	for _, leak := range []string{deadURL, ln.Addr().String(), "sekrit-query-value", "user@"} {
		if strings.Contains(msg, leak) {
			t.Errorf("fetch error leaks %q: %v", leak, msg)
		}
	}
	if !strings.Contains(msg, "upstream_refused") {
		t.Errorf("fetch error = %q, want the safe upstream_refused classification", msg)
	}
}

// TestNew_CacheDirMode0700 pins the cache directory as owner-only 0700
// (it may hold upstream metadata). MkdirAll+Chmod so a wide umask cannot
// leave the dir world-readable.
func TestNew_CacheDirMode0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "models-cache")
	mc, err := New(dir, pool.NewPool(nil), &config.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if mc == nil {
		t.Fatal("New returned nil cache")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat cache dir: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0700 {
		t.Errorf("cache dir mode = %o, want 0700", perm)
	}
}

// TestSyncPIModelsJSON_ParseErrorAborts: a models.json that does not
// unmarshal must abort the sync and leave the file byte-for-byte (and
// inode) untouched — never treat parse failure as empty Providers and
// wipe a non-Prism entry.
func TestSyncPIModelsJSON_ParseErrorAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	old := []byte(`{"providers":{"external":{"baseUrl":"https://keep.example","apiKey":"secret-token","models":[{"id":"x"}]}}`)
	if err := os.WriteFile(path, old, 0644); err != nil {
		t.Fatal(err)
	}
	fi0, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ino0 := fi0.Sys().(*syscall.Stat_t).Ino

	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "a", Provider: "p", BaseURL: "http://127.0.0.1:1/v1"}},
	}
	mc := NewForTest(map[string]*providerCache{
		"p": {Models: []ModelEntry{{ID: "m1", Object: "model", Created: 1, OwnedBy: "x"}}},
	})
	if err := mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg); err == nil {
		t.Fatal("syncPIModelsJSON must return an error when models.json cannot be parsed")
	}
	assertFileUntouched(t, path, old, ino0)
}

// TestSyncPIModelsJSON_PreservesExistingAPIKey: a hand-edited non-empty
// apiKey (a real token) must survive rebuild. An entry with no apiKey
// still gets the prism-dummy-key placeholder.
func TestSyncPIModelsJSON_PreservesExistingAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	writePIModelsJSON(t, path, map[string]any{
		"p": map[string]any{
			"baseUrl": "http://127.0.0.1:18790/v1",
			"api":     "openai-completions",
			"apiKey":  "sk-user-real-token",
			"models":  []map[string]any{{"id": "m1"}},
		},
	})
	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a", Provider: "p", BaseURL: "http://127.0.0.1:1/v1"},
			{Name: "b", Provider: "q", BaseURL: "http://127.0.0.1:1/v1"},
		},
	}
	mc := NewForTest(map[string]*providerCache{
		"p": {Models: []ModelEntry{{ID: "m1", Object: "model", Created: 1, OwnedBy: "x"}}},
		"q": {Models: []ModelEntry{{ID: "m2", Object: "model", Created: 1, OwnedBy: "x"}}},
	})
	if err := mc.syncPIModelsJSON(path, "http://127.0.0.1:18790/v1", cfg); err != nil {
		t.Fatalf("syncPIModelsJSON: %v", err)
	}
	got := readPIProvider(t, path)
	if got["p"].APIKey != "sk-user-real-token" {
		t.Errorf("provider p apiKey = %q, want the hand-edited token (must not be overwritten with prism-dummy-key)", got["p"].APIKey)
	}
	if got["q"].APIKey != "prism-dummy-key" {
		t.Errorf("provider q apiKey = %q, want prism-dummy-key when no previous key exists", got["q"].APIKey)
	}
}

func readPIProvider(t *testing.T, path string) map[string]struct {
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey"`
} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read synced file: %v", err)
	}
	var pc struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &pc); err != nil {
		t.Fatalf("unmarshal synced file: %v", err)
	}
	return pc.Providers
}

// TestSyncTools_TLSUsesHTTPS writes https:// when both TLS cert and key
// paths are set, and http:// when they are not. Port still comes from
// cfg.Listen.
func TestSyncTools_TLSUsesHTTPS(t *testing.T) {
	dir := t.TempDir()
	httpsPath := filepath.Join(dir, "https.json")
	httpPath := filepath.Join(dir, "http.json")
	mc := &ModelCache{
		caches: map[string]*providerCache{
			"p": {Models: []ModelEntry{{ID: "m1", Object: "model", Created: 1, OwnedBy: "x"}}},
		},
	}
	base := config.Config{
		Listen:   "127.0.0.1:9443",
		Accounts: []config.AccountConfig{{Name: "a", Provider: "p", BaseURL: "http://x/v1"}},
	}

	cfgTLS := base
	cfgTLS.TLSCertFile = "/tmp/cert.pem"
	cfgTLS.TLSKeyFile = "/tmp/key.pem"
	cfgTLS.Tools = map[string]string{"pi": httpsPath}
	mc.SyncTools(&cfgTLS)
	if got := readPIProvider(t, httpsPath)["p"].BaseURL; got != "https://127.0.0.1:9443/v1" {
		t.Errorf("TLS baseUrl = %q, want https://127.0.0.1:9443/v1", got)
	}

	cfgPlain := base
	cfgPlain.Tools = map[string]string{"pi": httpPath}
	mc.SyncTools(&cfgPlain)
	if got := readPIProvider(t, httpPath)["p"].BaseURL; got != "http://127.0.0.1:9443/v1" {
		t.Errorf("plain baseUrl = %q, want http://127.0.0.1:9443/v1", got)
	}

	cfgOne := base
	cfgOne.TLSCertFile = "/tmp/cert.pem"
	cfgOne.Tools = map[string]string{"pi": filepath.Join(dir, "one.json")}
	mc.SyncTools(&cfgOne)
	if got := readPIProvider(t, cfgOne.Tools["pi"])["p"].BaseURL; got != "http://127.0.0.1:9443/v1" {
		t.Errorf("cert-only baseUrl = %q, want http (both cert and key required)", got)
	}
}

// TestOllamaShowBudgetShorterThanRemaining: the independent show budget
// is capped below the parent fetch remaining time (and at ollamaShowTotalMax).
func TestOllamaShowBudgetShorterThanRemaining(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got := ollamaShowBudget(ctx)
	if got != ollamaShowTotalMax {
		t.Errorf("ollamaShowBudget(30s parent) = %v, want cap %v", got, ollamaShowTotalMax)
	}
	if got >= 30*time.Second {
		t.Errorf("show budget %v must be shorter than the 30s parent fetch", got)
	}

	short, shortCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer shortCancel()
	got = ollamaShowBudget(short)
	if got >= 200*time.Millisecond {
		t.Errorf("ollamaShowBudget(200ms parent) = %v, want strictly shorter than remaining", got)
	}
	if got <= 0 {
		t.Errorf("ollamaShowBudget(200ms parent) = %v, want a positive independent budget", got)
	}
}

// TestFetch_ShowTimeoutStillPersistsModels: after /v1/models succeeds, a
// hung /api/show that burns the (short) fetch budget must still persist
// the model list. Only Stop/cancel discards the whole result.
func TestFetch_ShowTimeoutStillPersistsModels(t *testing.T) {
	modelsHit := make(chan struct{})
	showHit := make(chan struct{})
	releaseShow := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			close(modelsHit)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data": []map[string]any{
					{"id": "m1", "object": "model", "created": 1, "owned_by": "ollama"},
					{"id": "m2", "object": "model", "created": 1, "owned_by": "ollama"},
				},
			})
		case "/api/show":
			select {
			case <-showHit:
			default:
				close(showHit)
			}
			select {
			case <-releaseShow:
				_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"x.context_length": 64000}})
			case <-r.Context().Done():
				return
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	mc := &ModelCache{
		dir:    dir,
		caches: map[string]*providerCache{},
		cfg:    loadOllamaSchemaCfg(t),
		pool: pool.NewPool([]config.AccountConfig{
			{Name: "ollama-acc", Provider: "ollama-cloud", BaseURL: srv.URL + "/v1", Key: "k"},
		}),
		stop:        make(chan struct{}),
		fetchBudget: 200 * time.Millisecond,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- mc.Fetch("ollama-cloud") }()

	select {
	case <-modelsHit:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never reached /v1/models")
	}
	select {
	case <-showHit:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never reached /api/show")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("show-budget timeout must still persist /v1/models, Fetch err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Fetch did not return after the independent show budget")
	}

	models := mc.GetModels("ollama-cloud")
	if len(models) != 2 || models[0].ID != "m1" || models[1].ID != "m2" {
		t.Errorf("cached models = %+v, want [m1 m2] (timeout during show must not drop the list)", models)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "ollama-cloud.json" {
		t.Errorf("cache dir after show timeout = %v, want [ollama-cloud.json]", entries)
	}
	close(releaseShow) // let /api/show handlers return so the test server can shut down
}

func TestRefreshOneAsync_PendingFillUpgradesToFullWhenOneManualArrives(t *testing.T) {
	var p1Hits, p2Hits int32
	entered1 := make(chan struct{})
	release1 := make(chan struct{})

	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := atomic.AddInt32(&p1Hits, 1)
		if h == 1 {
			close(entered1)
			<-release1
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m2","object":"model","created":1,"owned_by":"y"}]}`))
	}))
	defer srv2.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k"},
			{Name: "a2", Provider: "p2", BaseURL: srv2.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool:   pool.NewPool(cfg.Accounts),
		stop:   make(chan struct{}),
	}
	defer mc.Stop()

	// Round 1: manual refresh, blocks on p1
	mc.RefreshAllAsync(nil)
	select {
	case <-entered1:
	case <-time.After(5 * time.Second):
		t.Fatal("round 1 never reached p1")
	}

	// Queue pending fill
	fillDone := make(chan struct{})
	mc.FetchAllAsync(func() { close(fillDone) })

	mc.refreshMu.Lock()
	if !mc.pending || mc.pendingKind != roundFill || mc.pendingTarget != "" {
		mc.refreshMu.Unlock()
		t.Fatalf("expected pending fill round, got pending=%v kind=%v target=%q", mc.pending, mc.pendingKind, mc.pendingTarget)
	}
	mc.refreshMu.Unlock()

	// Now single-provider manual arrives (RefreshOneAsync for p1):
	// Must NOT downgrade pending to single-provider manual targeting only p1;
	// it must upgrade to full manual (target="") and merge onDone callbacks!
	manualDone := make(chan struct{})
	mc.RefreshOneAsync("p1", func() { close(manualDone) })

	mc.refreshMu.Lock()
	if !mc.pending || mc.pendingKind != roundManual || mc.pendingTarget != "" {
		mc.refreshMu.Unlock()
		t.Fatalf("expected pending fill upgraded to full manual (target=\"\"), got pending=%v kind=%v target=%q", mc.pending, mc.pendingKind, mc.pendingTarget)
	}
	mc.refreshMu.Unlock()

	close(release1)

	select {
	case <-manualDone:
	case <-time.After(10 * time.Second):
		t.Fatal("manual onDone never ran")
	}

	select {
	case <-fillDone:
	case <-time.After(10 * time.Second):
		t.Fatal("fill onDone never ran")
	}

	// Round 1 fetched p1 and p2 (1 each).
	// Pending round was upgraded to full manual, so it fetched p1 and p2 again (2 hits each total).
	if got := atomic.LoadInt32(&p1Hits); got != 2 {
		t.Errorf("p1 hits = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&p2Hits); got != 2 {
		t.Errorf("p2 hits = %d, want 2 (upgraded full manual must fetch both p1 and p2)", got)
	}
}

func TestRefreshOneAsync_TwoDifferentSingleProvidersUpgradeToFullAndMergeOnDone(t *testing.T) {
	var p1Hits, p2Hits int32
	entered1 := make(chan struct{})
	release1 := make(chan struct{})

	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := atomic.AddInt32(&p1Hits, 1)
		if h == 1 {
			close(entered1)
			<-release1
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m2","object":"model","created":1,"owned_by":"y"}]}`))
	}))
	defer srv2.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: srv1.URL, Key: "k"},
			{Name: "a2", Provider: "p2", BaseURL: srv2.URL, Key: "k"},
		},
		MaxConcurrentPerAccount: map[string]int{"*": 1},
	}
	mc := &ModelCache{
		dir:    t.TempDir(),
		caches: map[string]*providerCache{},
		cfg:    cfg,
		pool:   pool.NewPool(cfg.Accounts),
		stop:   make(chan struct{}),
	}
	defer mc.Stop()

	// Round 1: blocks on p1
	mc.RefreshAllAsync(nil)
	select {
	case <-entered1:
	case <-time.After(5 * time.Second):
		t.Fatal("round 1 never reached p1")
	}

	done1 := make(chan struct{})
	mc.RefreshOneAsync("p1", func() { close(done1) })

	done2 := make(chan struct{})
	mc.RefreshOneAsync("p2", func() { close(done2) })

	mc.refreshMu.Lock()
	if !mc.pending || mc.pendingKind != roundManual || mc.pendingTarget != "" {
		mc.refreshMu.Unlock()
		t.Fatalf("expected pending upgraded to full manual (target=\"\"), got pending=%v kind=%v target=%q", mc.pending, mc.pendingKind, mc.pendingTarget)
	}
	mc.refreshMu.Unlock()

	close(release1)

	select {
	case <-done1:
	case <-time.After(10 * time.Second):
		t.Fatal("done1 never ran")
	}

	select {
	case <-done2:
	case <-time.After(10 * time.Second):
		t.Fatal("done2 never ran")
	}

	if got := atomic.LoadInt32(&p1Hits); got != 2 {
		t.Errorf("p1 hits = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&p2Hits); got != 2 {
		t.Errorf("p2 hits = %d, want 2", got)
	}
}

func TestModelCache_New_StaggerDefaultAndCancellable(t *testing.T) {
	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: "http://127.0.0.1:1", Key: "k"},
			{Name: "a2", Provider: "p2", BaseURL: "http://127.0.0.1:1", Key: "k"},
			{Name: "a3", Provider: "p3", BaseURL: "http://127.0.0.1:1", Key: "k"},
		},
	}
	mc, err := New(t.TempDir(), pool.NewPool(cfg.Accounts), cfg)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if mc.staggerMax != defaultStaggerMax {
		t.Fatalf("staggerMax in New() = %v, want default %v", mc.staggerMax, defaultStaggerMax)
	}

	// Set a long stagger duration and verify Stop aborts immediately without hanging
	mc.SetStaggerForTest(10 * time.Second)

	start := time.Now()
	mc.RefreshAllAsync(nil)

	// Wait briefly so round starts and enters stagger between providers
	time.Sleep(50 * time.Millisecond)

	stopDone := make(chan struct{})
	go func() {
		mc.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("mc.Stop() hung or waited for full stagger duration")
	}

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("mc.Stop() took %v, expected prompt exit (< 3s)", elapsed)
	}
}

func TestModelCache_Snapshot_ConcurrentStress(t *testing.T) {
	cfg1 := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: "http://127.0.0.1:1", Key: "k"},
		},
	}
	cfg2 := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: "http://127.0.0.1:1", Key: "k"},
			{Name: "a2", Provider: "p2", BaseURL: "http://127.0.0.1:1", Key: "k"},
		},
	}
	mc, err := New(t.TempDir(), pool.NewPool(cfg1.Accounts), cfg1)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer mc.Stop()

	stopStress := make(chan struct{})
	var wg sync.WaitGroup

	// Readers running Snapshot
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopStress:
					return
				default:
					_ = mc.Snapshot()
				}
			}
		}()
	}

	// Writers alternating UpdateConfig and requestPeriodicRefresh
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				select {
				case <-stopStress:
					return
				default:
					if j%2 == 0 {
						mc.UpdateConfig(cfg2)
					} else {
						mc.UpdateConfig(cfg1)
					}
					mc.requestPeriodicRefresh(nil)
				}
			}
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	close(stopStress)
	wg.Wait()
}
