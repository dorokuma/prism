package proxy

import (
	"log/slog"
	"net/http"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

// NewProxyHandler creates the main HTTP handler that routes requests to the
// appropriate handler based on the request path.
func NewProxyHandler(pp *pool.Pool, wire config.WireAPIMode, holder *config.ConfigHolder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := holder.Load()
		if r.URL.Path == "/health" {
			slog.Debug("health")
			w.WriteHeader(200)
			w.Write([]byte("ok"))
			return
		}
		if r.URL.Path == "/v1/models" {
			if r.Method != http.MethodGet {
				util.WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{
					"error": map[string]any{"message": "method not allowed", "code": "method_not_allowed"},
				})
				return
			}
			proxyModels(pp, w, r, cfg)
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			if !wire.AllowsLegacy() {
				util.WriteJSON(w, http.StatusNotFound, map[string]any{
					"error": map[string]any{"message": "wire_api=responses: /v1/chat/completions disabled", "code": "disabled"},
				})
				return
			}
			proxyChat(pp, w, r, cfg)
			return
		}
		if r.URL.Path == "/v1/responses" {
			if !wire.AllowsResponses() {
				util.WriteJSON(w, http.StatusNotFound, map[string]any{
					"error": map[string]any{"message": "wire_api=legacy: /v1/responses disabled", "code": "disabled"},
				})
				return
			}
			proxyResponses(pp, w, r, cfg)
			return
		}
		util.WriteJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": "not found", "code": "not_found"},
		})
		slog.Debug("not_found", "path", r.URL.Path)
	})
}
