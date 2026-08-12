package proxy

import (
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/dorokuma/prism/internal/cache"
	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/util"
)

func proxyModels(mc *cache.ModelCache, w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	start := time.Now()
	requestID := util.RequestIDFromCtx(r.Context())
	slog.Debug("models request", "remote_addr", r.RemoteAddr, "req", requestID)

	// Codex 兼容：model_remap_enabled 时走旧逻辑
	if cfg.ModelRemapEnabled {
		modelIDs := cfg.AllModels()
		sort.Strings(modelIDs)
		data := make([]map[string]any, len(modelIDs))
		for i, id := range modelIDs {
			data[i] = enrichModel(map[string]any{
				"id": id, "object": "model", "created": 1700000000, "owned_by": "prism",
			}, "", id, cfg)
		}
		util.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
		slog.Debug("models returning (remap)", "count", len(modelIDs), "req", requestID, "duration_ms", time.Since(start).Milliseconds())
		return
	}

	provider := r.Header.Get("X-Prism-Provider")
	if provider == "" {
		// 不传 header → 返回空列表
		util.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		slog.Debug("models returning empty (no provider header)", "req", requestID)
		return
	}

	// 读缓存，没有就现场拉
	models := mc.GetModels(provider)
	if models == nil {
		slog.Info("models cache miss, fetching", "provider", provider, "req", requestID)
		if err := mc.Fetch(provider); err != nil {
			slog.Error("models fetch failed", "provider", provider, "error", err, "req", requestID)
			// A cache-miss fetch failure must never answer 200 with an empty
			// list (a client would cache "no models" forever). Failures are
			// classified from the Fetch sentinel errors:
			//   - no healthy account / saturated → 503 (the provider has no
			//     capacity right now; no_healthy / model_fetch_saturated)
			//   - everything else (upstream error, parse error, response
			//     over the size cap) → 502 model_fetch_failed
			// The no-provider-header and cache-hit paths above are unchanged.
			var code, msg string
			status := http.StatusBadGateway
			switch {
			case errors.Is(err, cache.ErrNoHealthyAccount):
				status, code, msg = http.StatusServiceUnavailable, "no_healthy", "no healthy account"
			case errors.Is(err, cache.ErrFetchSaturated):
				status, code, msg = http.StatusServiceUnavailable, "model_fetch_saturated", "model fetch saturated"
			default:
				code, msg = "model_fetch_failed", "model fetch failed"
			}
			util.WriteJSON(w, status, map[string]any{
				"error": map[string]any{"message": msg, "code": code},
			})
			return
		}
		models = mc.GetModels(provider)
		if models == nil {
			util.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
			return
		}
	}

	data := make([]map[string]any, len(models))
	for i, m := range models {
		entry := map[string]any{
			"id": m.ID, "object": m.Object, "created": m.Created, "owned_by": m.OwnedBy,
		}
		// Layer upstream metadata (snake_case, /v1/models convention) beneath
		// config metadata. enrichModel below overwrites the same snake_case
		// keys with config values, so config always wins over upstream.
		if meta, ok := mc.GetModelMeta(provider, m.ID); ok {
			applyUpstreamMetaSnake(entry, meta)
		}
		data[i] = enrichModel(entry, provider, m.ID, cfg)
	}
	util.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	slog.Debug("models returning", "provider", provider, "count", len(models), "req", requestID, "duration_ms", time.Since(start).Milliseconds())
}

// applyUpstreamMetaSnake writes upstream model metadata into a /v1/models
// response entry using snake_case keys. It must stay strictly separate from
// applyMergedCamel (camelCase, for pi's models.json). Config metadata is
// applied later by enrichModel, overwriting these same keys when present.
func applyUpstreamMetaSnake(entry map[string]any, meta cache.ModelMeta) {
	if meta.ContextWindow != nil {
		entry["context_window"] = *meta.ContextWindow
	}
	if meta.MaxTokens != nil {
		entry["max_tokens"] = *meta.MaxTokens
	}
	if meta.Reasoning != nil {
		entry["reasoning"] = *meta.Reasoning
	}
	if len(meta.Input) > 0 {
		entry["input"] = meta.Input
	}
}

// enrichModel merges optional model_metadata from config into the response entry.
// Extra fields are appended to the map; tools that don't understand them ignore them.
func enrichModel(entry map[string]any, provider, modelID string, cfg *config.Config) map[string]any {
	meta, ok := cfg.LookupModelMetadata(provider, modelID)
	if !ok {
		return entry
	}
	if meta.ContextWindow != nil {
		entry["context_window"] = *meta.ContextWindow
	}
	if meta.MaxTokens != nil {
		entry["max_tokens"] = *meta.MaxTokens
	}
	if meta.Reasoning != nil {
		entry["reasoning"] = *meta.Reasoning
	}
	if len(meta.Input) > 0 {
		entry["input"] = meta.Input
	}
	if meta.Cost != nil {
		entry["cost"] = map[string]float64{
			"input":       meta.Cost.Input,
			"output":      meta.Cost.Output,
			"cache_read":  meta.Cost.CacheRead,
			"cache_write": meta.Cost.CacheWrite,
		}
	}
	if len(meta.ThinkingLevelMap) > 0 {
		entry["thinking_level_map"] = meta.ThinkingLevelMap
	}
	if len(meta.Extra) > 0 {
		for k, v := range meta.Extra {
			entry[k] = v
		}
	}
	return entry
}

// getTenantID returns the tenant identifier for the request.
// Currently always returns "default" as multi-tenancy is not yet implemented.
// TODO: implement per-tenant isolation when multi-tenant support is needed.
func getTenantID(r *http.Request) string {
	return "default"
}
