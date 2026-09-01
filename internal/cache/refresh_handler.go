package cache

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/dorokuma/prism/internal/adminauth"
	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/util"
)

// refreshRateLimiter provides dedicated rate limiting for the models refresh endpoint:
// 1 request per 10 seconds (0.1/s), burst 2.
type refreshRateLimiter struct {
	mu        sync.Mutex
	tokens    float64
	lastCheck time.Time
}

func newRefreshRateLimiter() *refreshRateLimiter {
	return &refreshRateLimiter{
		tokens:    2.0,
		lastCheck: time.Now(),
	}
}

func (rl *refreshRateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	if rl.lastCheck.IsZero() {
		rl.tokens = 2.0
		rl.lastCheck = now
	}
	elapsed := now.Sub(rl.lastCheck).Seconds()
	rl.tokens += elapsed * 0.1 // 1 token per 10 seconds
	if rl.tokens > 2.0 {
		rl.tokens = 2.0
	}
	rl.lastCheck = now
	if rl.tokens >= 1.0 {
		rl.tokens -= 1.0
		return true
	}
	return false
}

// RefreshHandler handles GET and POST /prism/v1/models/refresh requests.
type RefreshHandler struct {
	mc      *ModelCache
	holder  *config.ConfigHolder
	limiter *refreshRateLimiter
}

// NewRefreshHandler creates a new handler for the models refresh endpoint.
func NewRefreshHandler(mc *ModelCache, holder *config.ConfigHolder) *RefreshHandler {
	return &RefreshHandler{
		mc:      mc,
		holder:  holder,
		limiter: newRefreshRateLimiter(),
	}
}

// RefreshResponse is the JSON response body for /prism/v1/models/refresh.
type RefreshResponse struct {
	Status    string                      `json:"status"`
	Provider  string                      `json:"provider,omitempty"`
	Providers map[string]ProviderSnapshot `json:"providers"`
}

func (h *RefreshHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := util.RequestIDFromCtx(r.Context())

	// Authenticate via admin token or loopback
	if !adminauth.Authorized(r) {
		slog.Warn("admin.models_refresh.unauthorized",
			"remote_addr", r.RemoteAddr,
			"path", r.URL.Path,
			"method", r.Method,
			"req", reqID,
		)
		util.WriteJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{
				"message": "unauthorized: PRISM_ADMIN_TOKEN required",
				"code":    "unauthorized",
			},
		})
		return
	}

	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		slog.Warn("admin.models_refresh.method_not_allowed",
			"remote_addr", r.RemoteAddr,
			"method", r.Method,
			"req", reqID,
		)
		util.WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": map[string]any{
				"message": "method not allowed: use GET or POST",
				"code":    "method_not_allowed",
			},
		})
		return
	}

	if h.mc == nil {
		slog.Warn("admin.models_refresh.unavailable",
			"remote_addr", r.RemoteAddr,
			"method", r.Method,
			"req", reqID,
		)
		util.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"message": "模型缓存未初始化",
				"code":    "cache_unavailable",
			},
		})
		return
	}

	provider := r.URL.Query().Get("provider")

	// Read-only status query via GET
	if r.Method == http.MethodGet {
		if provider != "" && h.holder != nil {
			if cfg := h.holder.Load(); cfg != nil && !cfg.HasProvider(provider) {
				slog.Warn("admin.models_refresh.unknown_provider",
					"remote_addr", r.RemoteAddr,
					"provider", provider,
					"req", reqID,
				)
				util.WriteJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{
						"message": "provider \"" + provider + "\" not found",
						"code":    "unknown_provider",
					},
				})
				return
			}
		}

		snapshots := h.mc.Snapshot()
		if provider != "" {
			if snap, ok := snapshots[provider]; ok {
				snapshots = map[string]ProviderSnapshot{provider: snap}
			} else {
				snapshots = map[string]ProviderSnapshot{}
			}
		}

		slog.Debug("admin.models_status",
			"remote_addr", r.RemoteAddr,
			"provider", provider,
			"status", "ok",
			"req", reqID,
		)

		resp := RefreshResponse{
			Status:    "ok",
			Provider:  provider,
			Providers: snapshots,
		}
		util.WriteJSON(w, http.StatusOK, resp)
		return
	}

	// Apply dedicated rate limit for POST refresh
	if h.limiter != nil && !h.limiter.Allow() {
		w.Header().Set("Retry-After", "10")
		slog.Warn("admin.models_refresh.rate_limited",
			"remote_addr", r.RemoteAddr,
			"provider", provider,
			"req", reqID,
		)
		util.WriteJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{
				"message": "限流：10 秒内最多刷新一次",
				"code":    "rate_limited",
			},
		})
		return
	}

	var onDone func()
	if h.holder != nil {
		onDone = func() {
			if cfg := h.holder.Load(); cfg != nil {
				h.mc.SyncTools(cfg)
			}
		}
	}

	snapshots, err := h.mc.AcceptRefresh(provider, onDone)
	if err != nil {
		slog.Warn("admin.models_refresh.bad_request",
			"remote_addr", r.RemoteAddr,
			"provider", provider,
			"error", err.Error(),
			"req", reqID,
		)
		util.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": err.Error(),
				"code":    "unknown_provider",
			},
		})
		return
	}

	util.MetricsModelCacheRefreshesTotal.Add(1)

	slog.Info("admin.models_refresh",
		"remote_addr", r.RemoteAddr,
		"provider", provider,
		"status", "accepted",
		"req", reqID,
	)

	resp := RefreshResponse{
		Status:    "accepted",
		Provider:  provider,
		Providers: snapshots,
	}
	util.WriteJSON(w, http.StatusAccepted, resp)
}
