package proxy

import (
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/dorokuma/prism/internal/cache"
	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/mcp"
	"github.com/dorokuma/prism/internal/middleware"
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
	if provider == "" && cfg != nil {
		// Same fallback as chat/completions: a configured default_provider
		// is the operator's "header optional" switch. Returning an empty
		// list here while chat already routes would make clients that
		// list models first (Codex / OpenCode / Cursor) show no models.
		provider = cfg.DefaultProvider
	}
	if provider == "" {
		// No header and no default_provider → empty list (do not leak
		// every provider's catalog).
		util.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		slog.Debug("models returning empty (no provider header)", "req", requestID)
		return
	}

	// 读缓存，没有就现场拉
	models := mc.GetModels(provider)
	if models == nil {
		slog.Info("models cache miss, fetching", "provider", provider, "req", requestID)
		// The request's context bounds only the WAIT for the fetch
		// (FetchWithContext): a client that disconnects mid-fetch stops
		// waiting immediately, while the shared fetch work itself keeps
		// running on the cache's own work context (a concurrent request or
		// a background fill still gets the result). The two checks below
		// turn the cancelled wait into "answer nothing": a client that is
		// gone must never receive a 200 empty list (it would cache "no
		// models" forever) or an error JSON it can no longer read.
		if err := mc.FetchWithContext(r.Context(), provider); err != nil {
			if r.Context().Err() != nil {
				slog.Debug("models fetch wait aborted by request cancellation", "provider", provider, "req", requestID)
				return
			}
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
			// The fetch succeeded but published no models; the client may
			// have disconnected while we waited — never answer a cancelled
			// request (an empty list would be cached as "no models").
			if r.Context().Err() != nil {
				return
			}
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

// getTenantID returns the MCP tool-cache identity for the request:
//
//   - authenticated requests (a real api_keys credential was presented) get
//     the authenticated API key NAME from the auth middleware context
//     (stable and non-secret — never the raw token), so tools cached from
//     one key's namespace bundles are never visible to another key
//     (dual-tenant isolation);
//   - requests that did NOT go through a real credential check (auth
//     disabled, or direct handler tests) get the fixed, read-only
//     mcp.UnauthenticatedIdentity — with auth disabled there is no
//     per-client identity, and a shared writable bucket would let different
//     local clients pollute each other's cached MCP tools. The
//     unauthenticated bucket is never written (mcp.cacheMCPTool refuses)
//     and sees only the shared admin-injected bucket.
//
// The "default" fallback only applies to the (authenticated) case where the
// auth middleware installed no key name at all — a plain per-client bucket,
// NOT the admin bucket: the shared admin-injected bucket (mcp_tools.json)
// uses an internal reserved key (config.McpAdminIdentity) that config
// validation forbids for client keys, and the request path can never write
// to it (see mcp.cacheMCPTool / cacheAdminTool). It stays visible to every
// identity (see mcp.getTenantMCPTools).
func getTenantID(r *http.Request) string {
	if !middleware.IsAuthenticated(r.Context()) {
		return mcp.UnauthenticatedIdentity
	}
	if id := middleware.APIKeyFromContext(r.Context()); id != "" {
		return id
	}
	return "default"
}
