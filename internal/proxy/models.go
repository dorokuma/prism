package proxy

import (
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

func proxyModels(p *pool.Pool, w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	start := time.Now()
	slog.Debug("models request", "remote_addr", r.RemoteAddr, "req", util.RequestIDFromCtx(r.Context()))

	modelIDs := cfg.AllModels()
	if len(modelIDs) == 0 {
		util.WriteJSON(w, 503, map[string]any{
			"error": map[string]any{"message": "No models configured", "code": "no_models"},
		})
		return
	}
	sort.Strings(modelIDs)

	data := make([]map[string]any, len(modelIDs))
	for i, id := range modelIDs {
		data[i] = map[string]any{
			"id":       id,
			"object":   "model",
			"created":  1700000000,
			"owned_by": "prism",
		}
	}
	resp := map[string]any{
		"object": "list",
		"data":   data,
	}
	util.WriteJSON(w, http.StatusOK, resp)
	slog.Debug("models returning", "count", len(modelIDs), "req", util.RequestIDFromCtx(r.Context()), "duration_ms", time.Since(start).Milliseconds())
}

// getTenantID returns the tenant identifier for the request.
// Currently always returns "default" as multi-tenancy is not yet implemented.
// TODO: implement per-tenant isolation when multi-tenant support is needed.
func getTenantID(r *http.Request) string {
	return "default"
}
