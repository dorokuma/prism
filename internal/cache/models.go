package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

// ModelCache manages per-provider model list caches persisted to disk.
type ModelCache struct {
	dir      string
	caches   map[string]*providerCache
	mu       sync.RWMutex
	pool     *pool.Pool
	cfg      *config.Config
	stop     chan struct{}
}

type providerCache struct {
	Models    []ModelEntry `json:"models"`
	UpdatedAt time.Time    `json:"updated_at"`
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
		_, exists := mc.caches[provider]
		mc.mu.RUnlock()
		if exists {
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

// syncPIModelsJSON reads/creates a PI models.json file and updates
// the Prism-managed provider entries with current cache data.
func (mc *ModelCache) syncPIModelsJSON(path string, baseURL string, cfg *config.Config) error {
	type piModel struct {
		ID string `json:"id"`
	}
	type piProvider struct {
		BaseURL string            `json:"baseUrl"`
		API     string            `json:"api"`
		APIKey  string            `json:"apiKey"`
		Headers map[string]string `json:"headers,omitempty"`
		Models  []piModel         `json:"models"`
	}
	type piConfig struct {
		Providers map[string]piProvider `json:"providers"`
	}

	// Read existing file (or start fresh)
	pc := piConfig{Providers: make(map[string]piProvider)}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &pc); err != nil {
			slog.Warn("pi models.json parse error, overwriting", "path", path, "error", err)
		}
	}

	// Create directory if needed
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	// Update provider entries for each configured provider
	providers := cfg.ProviderNames()
	for _, provider := range providers {
		models := mc.GetModels(provider)
		if models == nil {
			slog.Warn("no cache for provider, skipping sync", "provider", provider)
			continue
		}
		entries := make([]piModel, len(models))
		for i, m := range models {
			entries[i] = piModel{ID: m.ID}
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
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	slog.Info("pi models.json synced", "path", path, "providers", len(pc.Providers))
	return nil
}
