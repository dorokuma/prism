package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

// ModelCache manages per-provider model list caches persisted to disk.
type ModelCache struct {
	dir    string
	caches map[string]*providerCache
	mu     sync.RWMutex
	pool   *pool.Pool
	cfg    *config.Config
	stop   chan struct{}
}

type providerCache struct {
	Models    []ModelEntry         `json:"models"`
	Meta      map[string]ModelMeta `json:"meta,omitempty"`
	UpdatedAt time.Time            `json:"updated_at"`
}

// ModelMeta holds metadata pulled from a provider's upstream endpoint
// (e.g. ollama /api/show). It is persisted in the on-disk cache under the
// "meta" key (omitempty + nil keeps the cache backward compatible with the
// old format that had no such key).
type ModelMeta struct {
	ContextWindow *int     `json:"context_window,omitempty"`
	MaxTokens     *int     `json:"max_tokens,omitempty"`
	Reasoning     *bool    `json:"reasoning,omitempty"`
	Input         []string `json:"input,omitempty"`
}

// ModelEntry represents a single model from /v1/models response.
type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// upstreamModelsResponse is the raw response from GET /v1/models.
type upstreamModelsResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

// New creates a ModelCache with the given cache directory, pool, and config.
// The cache directory is created if it doesn't exist.
func New(dir string, p *pool.Pool, cfg *config.Config) (*ModelCache, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &ModelCache{
		dir:    dir,
		caches: make(map[string]*providerCache),
		pool:   p,
		cfg:    cfg,
		stop:   make(chan struct{}),
	}, nil
}

// filePath returns the path to the cache file for a provider.
func (mc *ModelCache) filePath(provider string) string {
	return filepath.Join(mc.dir, provider+".json")
}

// LoadFromDisk reads all cached provider model lists from disk.
// Missing files are silently skipped.
func (mc *ModelCache) LoadFromDisk() {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	for _, acc := range mc.cfg.Accounts {
		provider := acc.Provider
		if provider == "" {
			continue
		}
		fp := mc.filePath(provider)
		data, err := os.ReadFile(fp)
		if err != nil {
			slog.Debug("cache file not found, will fetch async", "provider", provider, "path", fp)
			continue
		}
		var pc providerCache
		if err := json.Unmarshal(data, &pc); err != nil {
			slog.Warn("cache file corrupt, will fetch async", "provider", provider, "error", err)
			continue
		}
		mc.caches[provider] = &pc
		slog.Info("model cache loaded from disk", "provider", provider, "models", len(pc.Models), "updated", pc.UpdatedAt.Format(time.RFC3339))
	}
}

// GetModels returns the cached model list for a provider.
// Returns nil if not cached.
func (mc *ModelCache) GetModels(provider string) []ModelEntry {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	pc := mc.caches[provider]
	if pc == nil {
		return nil
	}
	return pc.Models
}

// FetchAllAsync fetches model lists from upstream for any providers missing cache.
// Runs each fetch in its own goroutine. After all fetches complete,
// onDone is called exactly once (may be nil).
func (mc *ModelCache) FetchAllAsync(onDone func()) {
	var wg sync.WaitGroup
	for _, acc := range mc.cfg.Accounts {
		provider := acc.Provider
		if provider == "" {
			continue
		}
		mc.mu.RLock()
		pc, exists := mc.caches[provider]
		mc.mu.RUnlock()
		needsFetch := !exists
		if exists && mc.cfg != nil && mc.cfg.EffortSchema(provider) == "ollama" && len(pc.Models) > 0 {
			// Self-heal: old cache files (pre-Meta) have Models but nil/empty Meta.
			// Force a Fetch so /api/show runs and populates Meta, without needing
			// a manual SIGHUP. New caches (Meta present) are left untouched.
			if pc.Meta == nil || len(pc.Meta) == 0 {
				needsFetch = true
			}
		}
		if !needsFetch {
			continue
		}
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if err := mc.Fetch(p); err != nil {
				slog.Warn("async fetch failed", "provider", p, "error", err)
			}
		}(provider)
	}
	if onDone != nil {
		go func() {
			wg.Wait()
			onDone()
		}()
	}
}

// Fetch calls the upstream /v1/models endpoint for a provider, caches
// the result to disk, and updates the in-memory cache.
func (mc *ModelCache) Fetch(provider string) error {
	// Find the first healthy account for this provider
	account := mc.selectAccount(provider)
	if account == nil {
		return fmt.Errorf("no healthy account for provider %q", provider)
	}

	url := util.JoinURLPath(account.BaseURL(), "/v1/models")
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+account.Key())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := account.Client().Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upstream returned %d: %s", resp.StatusCode, string(body))
	}

	var upstream upstreamModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&upstream); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	pc := &providerCache{
		Models:    upstream.Data,
		Meta:      mc.fetchOllamaMeta(account, upstream.Data), // ollama only; non-ollama returns nil
		UpdatedAt: time.Now(),
	}

	data, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	if err := os.WriteFile(mc.filePath(provider), data, 0644); err != nil {
		return fmt.Errorf("write cache file: %w", err)
	}

	mc.mu.Lock()
	mc.caches[provider] = pc
	mc.mu.Unlock()

	slog.Info("model cache fetched", "provider", provider, "models", len(pc.Models))
	return nil
}

// RefreshStale fetches model lists for any providers whose cache is older than 24h.
func (mc *ModelCache) RefreshStale() {
	threshold := time.Now().Add(-24 * time.Hour)
	for _, acc := range mc.cfg.Accounts {
		provider := acc.Provider
		if provider == "" {
			continue
		}
		mc.mu.RLock()
		pc := mc.caches[provider]
		mc.mu.RUnlock()
		if pc != nil && pc.UpdatedAt.After(threshold) {
			continue
		}
		if err := mc.Fetch(provider); err != nil {
			slog.Warn("refresh stale failed", "provider", provider, "error", err)
		}
	}
}

// RefreshAll fetches model lists for all providers, ignoring cache state.
func (mc *ModelCache) RefreshAll() {
	for _, acc := range mc.cfg.Accounts {
		provider := acc.Provider
		if provider == "" {
			continue
		}
		if err := mc.Fetch(provider); err != nil {
			slog.Warn("refresh all failed", "provider", provider, "error", err)
		}
	}
}

// StartRefreshLoop runs a background goroutine that checks for stale caches
// every checkInterval and refreshes them. Call Stop() to shut down.
func (mc *ModelCache) StartRefreshLoop(checkInterval time.Duration, onRefresh func()) {
	go func() {
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-mc.stop:
				slog.Info("model cache refresh loop stopped")
				return
			case <-ticker.C:
				mc.RefreshStale()
				if onRefresh != nil {
					onRefresh()
				}
			}
		}
	}()
}

// Stop shuts down the refresh loop.
func (mc *ModelCache) Stop() {
	close(mc.stop)
}

// UpdateConfig replaces the stored config reference. Call after SIGHUP reloads config.
func (mc *ModelCache) UpdateConfig(cfg *config.Config) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.cfg = cfg
}

// selectAccount finds any healthy account for the given provider.
// Must not hold mc.mu when calling (calls pool methods).
func (mc *ModelCache) selectAccount(provider string) *pool.Account {
	for _, acc := range mc.pool.AllAccounts() {
		if acc.Provider() == provider && acc.IsHealthy() && !acc.IsInCooldown() {
			return acc
		}
	}
	return nil
}

// SyncTools writes model configuration to each tool's config file
// based on current cache state. Only updates providers managed by Prism;
// leaves other providers in the file untouched.
func (mc *ModelCache) SyncTools(cfg *config.Config) {
	if cfg.Tools == nil {
		return
	}

	// Resolve the base URL: always 127.0.0.1 with the port from config.Listen.
	port := "18790"
	if hostPort := cfg.Listen; hostPort != "" {
		if _, p, err := net.SplitHostPort(hostPort); err == nil {
			port = p
		}
	}
	baseURL := "http://127.0.0.1:" + port + "/v1"

	for toolName, toolPath := range cfg.Tools {
		switch toolName {
		case "pi":
			if err := mc.syncPIModelsJSON(toolPath, baseURL, cfg); err != nil {
				slog.Error("sync tool config failed", "tool", toolName, "path", toolPath, "error", err)
			}
		default:
			slog.Warn("unsupported tool, skipping sync", "tool", toolName)
		}
	}
}

// syncPIModelsJSON merges upstream model IDs into pi's models.json.
//
// For each Prism-managed provider:
//   - Existing models → rebuilt entry: prism-managed fields (contextWindow/
//     maxTokens/reasoning/input/cost/thinkingLevelMap + config.Extra keys)
//     are overwritten from upstream meta + config metadata; any other keys
//     on the previous entry (e.g. a hand-edited "name") are preserved.
//     This guarantees already-existing models also receive metadata written
//     by config/upstream (fixes e.g. glm-5.2 showing context=128k).
//   - New models      → entry created with { "id": "..." } + metadata from
//     config.ModelMetadata when available
//
// Non-Prism providers in the file are untouched.
func (mc *ModelCache) syncPIModelsJSON(path string, baseURL string, cfg *config.Config) error {
	type piProvider struct {
		BaseURL string            `json:"baseUrl"`
		API     string            `json:"api"`
		APIKey  string            `json:"apiKey"`
		Headers map[string]string `json:"headers,omitempty"`
		Models  []map[string]any  `json:"models"`
	}
	type piConfig struct {
		Providers map[string]piProvider `json:"providers"`
	}

	// Read existing file (or start fresh)
	pc := piConfig{Providers: make(map[string]piProvider)}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &pc); err != nil {
			slog.Warn("pi models.json parse error, overwriting", "path", path, "error", err)
			pc = piConfig{Providers: make(map[string]piProvider)}
		}
	}

	// Create directory if needed
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	for _, provider := range cfg.ProviderNames() {
		models := mc.GetModels(provider)
		if models == nil {
			slog.Warn("no cache for provider, skipping sync", "provider", provider)
			continue
		}

		// Build set of upstream model IDs
		upstreamIDs := make(map[string]bool, len(models))
		for _, m := range models {
			upstreamIDs[m.ID] = true
		}

		// Filter existing entries: keep only those still in upstream
		existingByID := make(map[string]map[string]any)
		if oldProvider, ok := pc.Providers[provider]; ok {
			for _, entry := range oldProvider.Models {
				id, _ := entry["id"].(string)
				if id != "" && upstreamIDs[id] {
					existingByID[id] = entry
				}
			}
		}

		// Build new model list (unified rebuild).
		//
		// Every upstream model gets a freshly-built entry. Prism-managed
		// fields (contextWindow/maxTokens/reasoning/input/cost/
		// thinkingLevelMap + config.Extra keys) are always overwritten from
		// upstream meta + config metadata; any other keys present on a
		// previous entry (e.g. a hand-edited "name") are preserved. This
		// guarantees already-existing models also receive metadata written
		// by config/upstream (fixes e.g. glm-5.2 showing context=128k).
		entries := make([]map[string]any, 0, len(models))

		// Keys that prism fully controls and always overwrites.
		prismManaged := map[string]bool{
			"contextWindow":    true,
			"maxTokens":        true,
			"reasoning":        true,
			"input":            true,
			"cost":             true,
			"thinkingLevelMap": true,
		}
		// Config-declared extra keys are also prism-managed.
		for _, cm := range cfg.ModelMetadata {
			for k := range cm.Extra {
				prismManaged[k] = true
			}
		}
		// Per-provider override entries may also declare extra keys.
		for _, pp := range cfg.ModelMetadataPerProvider {
			for _, cm := range pp {
				for k := range cm.Extra {
					prismManaged[k] = true
				}
			}
		}

		for _, m := range models {
			existing := existingByID[m.ID] // may be nil
			upMeta, _ := mc.GetModelMeta(provider, m.ID)
			cfgMeta, _ := cfg.LookupModelMetadata(provider, m.ID)
			merged := mergeMeta(upMeta, cfgMeta)

			entry := map[string]any{"id": m.ID}
			if existing != nil {
				// Preserve non-managed keys from the old entry
				// (e.g. hand-edited name), dropping managed ones that
				// will be re-derived below.
				for k, v := range existing {
					if k != "id" && !prismManaged[k] {
						entry[k] = v
					}
				}
			}
			applyMergedCamel(entry, merged, cfgMeta)
			entries = append(entries, entry)
		}

		pc.Providers[provider] = piProvider{
			BaseURL: baseURL,
			API:     "openai-completions",
			APIKey:  "prism-dummy-key",
			Headers: map[string]string{"X-Prism-Provider": provider},
			Models:  entries,
		}
	}

	data, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	// Direct overwrite (not tmp+rename): pi's models.json lives in /root/.pi/agent/
	// (root-owned dir; prism user has file write via chown but not dir write),
	// so atomic tmp+rename (needs dir write) is impossible here. models.json is
	// fully regenerable by sync, so non-atomic overwrite is acceptable.
	if err := os.WriteFile(path, data, 0664); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	slog.Info("pi models.json synced", "path", path, "providers", len(pc.Providers))
	return nil
}

// rootURL strips the trailing "/v1" (and any trailing slash) from a base URL
// so it can be joined with ollama's "/api/show" endpoint. JoinURLPath is not
// suitable because /api/show is not under /v1.
func rootURL(base string) string {
	base = strings.TrimSuffix(base, "/")
	return strings.TrimSuffix(base, "/v1")
}

// deriveContextLength scans an ollama model_info map for a key ending in
// ".context_length" and returns its (int) value. The exact key is
// architecture-dependent (e.g. "llama.context_length"), so we match by suffix
// rather than hard-coding it.
func deriveContextLength(modelInfo map[string]any) *int {
	if modelInfo == nil {
		return nil
	}
	for k, v := range modelInfo {
		if !strings.HasSuffix(k, ".context_length") {
			continue
		}
		switch n := v.(type) {
		case float64:
			i := int(n)
			return &i
		case int:
			return &n
		case json.Number:
			if i, err := n.Int64(); err == nil {
				i2 := int(i)
				return &i2
			}
		}
	}
	return nil
}

// fetchOllamaShow queries ollama's /api/show endpoint for a single model and
// extracts its metadata (currently just context_window from model_info).
// A non-200, timeout, or parse error is returned as an error so the caller can
// skip the model without failing the whole fetch.
func (mc *ModelCache) fetchOllamaShow(acc *pool.Account, id string) (ModelMeta, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	body, err := json.Marshal(map[string]string{"name": id})
	if err != nil {
		return ModelMeta{}, fmt.Errorf("marshal show body: %w", err)
	}
	url := rootURL(acc.BaseURL()) + "/api/show"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ModelMeta{}, fmt.Errorf("create show request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key := acc.Key(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := acc.Client().Do(req)
	if err != nil {
		return ModelMeta{}, fmt.Errorf("show request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ModelMeta{}, fmt.Errorf("api/show returned %d: %s", resp.StatusCode, string(b))
	}

	var show struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return ModelMeta{}, fmt.Errorf("decode show response: %w", err)
	}
	return ModelMeta{ContextWindow: deriveContextLength(show.ModelInfo)}, nil
}

// fetchOllamaMeta fetches metadata for every model from ollama's /api/show
// endpoint. Only ollama providers (per cfg.EffortSchema) are queried; all other
// providers return nil. A single model's failure is logged and skipped — it
// never fails the enclosing Fetch. Up to 4 requests run concurrently.
func (mc *ModelCache) fetchOllamaMeta(acc *pool.Account, models []ModelEntry) map[string]ModelMeta {
	if acc == nil || mc.cfg == nil || mc.cfg.EffortSchema(acc.Provider()) != "ollama" {
		return nil
	}
	return mc.collectOllamaMeta(acc, models)
}

// collectOllamaMeta performs the concurrent /api/show fan-out. It is the
// testable core of fetchOllamaMeta (which adds the ollama-only gate).
func (mc *ModelCache) collectOllamaMeta(acc *pool.Account, models []ModelEntry) map[string]ModelMeta {
	if len(models) == 0 {
		return nil
	}
	result := make(map[string]ModelMeta)
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, m := range models {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			meta, err := mc.fetchOllamaShow(acc, id)
			if err != nil {
				slog.Warn("fetch ollama /api/show failed, skipping model",
					"provider", acc.Provider(), "model", id, "error", err)
				return
			}
			mu.Lock()
			result[id] = meta
			mu.Unlock()
		}(m.ID)
	}
	wg.Wait()
	if len(result) == 0 {
		return nil
	}
	return result
}

// GetModelMeta returns the upstream metadata for a single model, if known.
func (mc *ModelCache) GetModelMeta(provider, id string) (ModelMeta, bool) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	pc := mc.caches[provider]
	if pc == nil || pc.Meta == nil {
		return ModelMeta{}, false
	}
	meta, ok := pc.Meta[id]
	return meta, ok
}

// GetMeta returns the upstream metadata map for a provider.
func (mc *ModelCache) GetMeta(provider string) map[string]ModelMeta {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	pc := mc.caches[provider]
	if pc == nil {
		return nil
	}
	return pc.Meta
}

// mergeMeta combines upstream metadata with config metadata. Config fields
// that are set (non-nil/non-empty) override the upstream value of the same
// field. cost/thinkingLevelMap/Extra come only from config and are not part
// of ModelMeta, so they are handled separately by applyMergedCamel.
func mergeMeta(up ModelMeta, cfg config.ModelMetadata) ModelMeta {
	merged := up
	if cfg.ContextWindow != nil {
		merged.ContextWindow = cfg.ContextWindow
	}
	if cfg.MaxTokens != nil {
		merged.MaxTokens = cfg.MaxTokens
	}
	if cfg.Reasoning != nil {
		merged.Reasoning = cfg.Reasoning
	}
	if len(cfg.Input) > 0 {
		merged.Input = cfg.Input
	}
	return merged
}

// applyMergedCamel writes the merged metadata into a pi models.json entry using
// camelCase keys (models.json convention). ContextWindow/MaxTokens/Reasoning/
// Input come from merged (upstream + config override); cost/thinkingLevelMap/
// extra come only from cfgMeta (config-only). Prism-managed fields already on
// the entry are overwritten; this is the single place models.json metadata is
// written.
func applyMergedCamel(entry map[string]any, merged ModelMeta, cfgMeta config.ModelMetadata) {
	if merged.ContextWindow != nil {
		entry["contextWindow"] = *merged.ContextWindow
	}
	if merged.MaxTokens != nil {
		entry["maxTokens"] = *merged.MaxTokens
	}
	if merged.Reasoning != nil {
		entry["reasoning"] = *merged.Reasoning
	}
	if len(merged.Input) > 0 {
		entry["input"] = merged.Input
	}
	if cfgMeta.Cost != nil {
		entry["cost"] = map[string]float64{
			"input":      cfgMeta.Cost.Input,
			"output":     cfgMeta.Cost.Output,
			"cacheRead":  cfgMeta.Cost.CacheRead,
			"cacheWrite": cfgMeta.Cost.CacheWrite,
		}
	}
	if len(cfgMeta.ThinkingLevelMap) > 0 {
		entry["thinkingLevelMap"] = cfgMeta.ThinkingLevelMap
	}
	if len(cfgMeta.Extra) > 0 {
		for k, v := range cfgMeta.Extra {
			entry[k] = v
		}
	}
}
