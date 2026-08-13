package proxy

import (
	"log/slog"
	"net/http"

	"github.com/dorokuma/prism/internal/cache"
	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

// NewProxyHandler creates the main HTTP handler that routes requests to the
// appropriate handler based on the request path.
func NewProxyHandler(pp *pool.Pool, wire config.WireAPIMode, holder *config.ConfigHolder, mc *cache.ModelCache) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := holder.Load()
		if r.URL.Path == "/health" {
			slog.Debug("health")
			w.WriteHeader(200)
			w.Write([]byte("ok"))
			return
		}
		if r.URL.Path == "/ready" {
			// Readiness (deploy.sh / load balancers): 200 only when at least
			// one account is healthy AND out of cooldown — the process may be
			// up (liveness) while every account is exhausted or cooling down.
			// Deliberately distinct from /health (liveness: the process is
			// serving). A nil pool (never wired) is not ready. The 503 is
			// logged at DEBUG: deploy.sh and load balancers poll /ready every
			// second while the pool is down, and an INFO/WARN line per poll
			// would flood the log during every all-accounts-down window; the
			// readiness STATE stays fully observable via the 503 itself and
			// the per-account cooldown/exhaustion logs.
			if pp != nil && pp.Ready() {
				w.WriteHeader(200)
				w.Write([]byte("ok"))
				return
			}
			slog.Debug("not ready: no healthy account available")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
			return
		}
		if r.URL.Path == "/v1/models" {
			if r.Method != http.MethodGet {
				util.WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{
					"error": map[string]any{"message": "method not allowed", "code": "method_not_allowed"},
				})
				return
			}
			proxyModels(mc, w, r, cfg)
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			if r.Method != http.MethodPost {
				util.WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{
					"error": map[string]any{"message": "method not allowed", "code": "method_not_allowed"},
				})
				return
			}
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
			if r.Method != http.MethodPost {
				util.WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{
					"error": map[string]any{"message": "method not allowed", "code": "method_not_allowed"},
				})
				return
			}
			if !wire.AllowsResponses() {
				util.WriteJSON(w, http.StatusNotFound, map[string]any{
					"error": map[string]any{"message": "wire_api=legacy: /v1/responses disabled", "code": "disabled"},
				})
				return
			}
			proxyResponses(pp, w, r, cfg)
			return
		}
		if r.URL.Path == "/v1/messages" {
			if r.Method != http.MethodPost {
				util.WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{
					"error": map[string]any{"message": "method not allowed", "code": "method_not_allowed"},
				})
				return
			}
			// /v1/messages (anthropic messages) is an independent third surface
			// and is always enabled: wire_api only governs OpenAI legacy chat
			// vs responses and is not a subset relationship with anthropic
			// messages (a responses-only deployment must still reach it).
			proxyMessages(pp, w, r, cfg)
			return
		}
		util.WriteJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": "not found", "code": "not_found"},
		})
		slog.Debug("not_found", "path", r.URL.Path)
	})
}
