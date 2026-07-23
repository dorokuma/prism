package proxy

import (
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
			data[i] = map[string]any{
				"id": id, "object": "model", "created": 1700000000, "owned_by": "prism",
			}
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
			util.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
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
		data[i] = map[string]any{
			"id": m.ID, "object": m.Object, "created": m.Created, "owned_by": m.OwnedBy,
		}
	}
	util.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	slog.Debug("models returning", "provider", provider, "count", len(models), "req", requestID, "duration_ms", time.Since(start).Milliseconds())
}

// getTenantID returns the tenant identifier for the request.
// Currently always returns "default" as multi-tenancy is not yet implemented.
// TODO: implement per-tenant isolation when multi-tenant support is needed.
func getTenantID(r *http.Request) string {
	return "default"
}
