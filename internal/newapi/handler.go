package newapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/usage"
	"github.com/dorokuma/prism/internal/util"
)

// Store defines the usage persistence interface needed by the NewAPI compatibility endpoints.
type Store interface {
	UserSelf(ctx context.Context, keyID string) (*usage.UserSelfData, error)
	LogSelf(ctx context.Context, q usage.LogSelfQuery) (*usage.LogSelfResult, error)
}

// Handler serves GET /api/user/self and GET /api/log/self.
type Handler struct {
	Store   Store
	Holder  *config.ConfigHolder
	Flusher func(ctx context.Context) error
}

// NewHandler creates a new NewAPI compatibility handler.
func NewHandler(store Store, holder *config.ConfigHolder, flusher func(ctx context.Context) error) *Handler {
	return &Handler{
		Store:   store,
		Holder:  holder,
		Flusher: flusher,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		util.WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"success": false,
			"message": "method not allowed",
		})
		return
	}

	keyName := h.authenticate(r)
	if keyName == "" {
		util.WriteJSON(w, http.StatusUnauthorized, map[string]any{
			"success": false,
			"message": "unauthorized",
		})
		return
	}

	switch r.URL.Path {
	case "/api/user/self":
		h.handleUserSelf(w, r, keyName)
	case "/api/log/self":
		h.handleLogSelf(w, r, keyName)
	default:
		util.WriteJSON(w, http.StatusNotFound, map[string]any{
			"success": false,
			"message": "not found",
		})
	}
}

func (h *Handler) authenticate(r *http.Request) string {
	if keyName := middleware.APIKeyFromContext(r.Context()); keyName != "" {
		return keyName
	}
	if h.Holder == nil {
		return ""
	}
	cfg := h.Holder.Load()
	if cfg == nil {
		return ""
	}
	if len(cfg.APIKeys) == 0 {
		if cfg.Usage.DefaultKeyID != "" {
			return cfg.Usage.DefaultKeyID
		}
		return "anonymous"
	}
	keyName, ok := middleware.Authenticate(r, cfg.APIKeys)
	if !ok {
		return ""
	}
	return keyName
}

type UserSelfData struct {
	ID           int    `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"display_name"`
	Role         int    `json:"role"`
	Status       int    `json:"status"`
	Email        string `json:"email"`
	Quota        int64  `json:"quota"`
	UsedQuota    int64  `json:"used_quota"`
	RequestCount int64  `json:"request_count"`
}

type UserSelfResponse struct {
	Success bool         `json:"success"`
	Message string       `json:"message"`
	Data    UserSelfData `json:"data"`
}

func (h *Handler) handleUserSelf(w http.ResponseWriter, r *http.Request, keyName string) {
	if h.Store == nil {
		util.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false,
			"message": "usage store unavailable",
		})
		return
	}

	data, err := h.Store.UserSelf(r.Context(), keyName)
	if err != nil {
		util.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	costUSD := data.CostUSD
	usedQuota := int64(math.Round(costUSD * 500000))

	// Resolve remaining quota
	quota := int64(5000000000) // Default $10,000 in quota units
	if h.Holder != nil {
		if cfg := h.Holder.Load(); cfg != nil {
			for _, k := range cfg.APIKeys {
				if k.Name == keyName && k.QuotaUSD != nil {
					remUSD := *k.QuotaUSD - costUSD
					q := int64(math.Round(remUSD * 500000))
					if q < 0 {
						q = 0
					}
					quota = q
					break
				}
			}
		}
	}

	resp := UserSelfResponse{
		Success: true,
		Message: "",
		Data: UserSelfData{
			ID:           1,
			Username:     keyName,
			DisplayName:  keyName,
			Role:         1,
			Status:       1,
			Email:        "",
			Quota:        quota,
			UsedQuota:    usedQuota,
			RequestCount: data.RequestCount,
		},
	}
	util.WriteJSON(w, http.StatusOK, resp)
}

type LogItem struct {
	ID               int64   `json:"id"`
	CreatedAt        int64   `json:"created_at"`
	UserID           int     `json:"user_id"`
	Username         string  `json:"username"`
	TokenName        string  `json:"token_name"`
	ModelName        string  `json:"model_name"`
	Type             int     `json:"type"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Quota            int64   `json:"quota"`
	ActualCost       float64 `json:"actual_cost"`
	Duration         int64   `json:"duration"`
	RequestTime      int64   `json:"request_time"`
	IsStream         bool    `json:"is_stream"`
	Other            string  `json:"other"`
}

type LogData struct {
	Items []LogItem `json:"items"`
	Total int64     `json:"total"`
}

type LogSelfResponse struct {
	Success bool    `json:"success"`
	Message string  `json:"message"`
	Data    LogData `json:"data"`
}

func (h *Handler) handleLogSelf(w http.ResponseWriter, r *http.Request, keyName string) {
	if h.Store == nil {
		util.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false,
			"message": "usage store unavailable",
		})
		return
	}

	if h.Flusher != nil {
		_ = h.Flusher(r.Context())
	}

	q := r.URL.Query()
	page := 1
	if pStr := q.Get("p"); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			page = p
		}
	} else if pageStr := q.Get("page"); pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
			page = p
		}
	}

	size := 20
	if sStr := q.Get("size"); sStr != "" {
		if s, err := strconv.Atoi(sStr); err == nil && s > 0 {
			size = s
		}
	} else if sStr := q.Get("page_size"); sStr != "" {
		if s, err := strconv.Atoi(sStr); err == nil && s > 0 {
			size = s
		}
	}
	if size > 100 {
		size = 100
	}

	model := q.Get("model")
	if model == "" {
		model = q.Get("model_name")
	}

	var startTs, endTs int64
	if sStr := q.Get("start_timestamp"); sStr != "" {
		if s, err := strconv.ParseInt(sStr, 10, 64); err == nil {
			startTs = s
		}
	} else if sStr := q.Get("start_time"); sStr != "" {
		if s, err := strconv.ParseInt(sStr, 10, 64); err == nil {
			startTs = s
		}
	}
	if eStr := q.Get("end_timestamp"); eStr != "" {
		if e, err := strconv.ParseInt(eStr, 10, 64); err == nil {
			endTs = e
		}
	} else if eStr := q.Get("end_time"); eStr != "" {
		if e, err := strconv.ParseInt(eStr, 10, 64); err == nil {
			endTs = e
		}
	}
	if startTs > 1e11 {
		startTs /= 1000
	}
	if endTs > 1e11 {
		endTs /= 1000
	}

	result, err := h.Store.LogSelf(r.Context(), usage.LogSelfQuery{
		KeyID:          keyName,
		Model:          strings.TrimSpace(model),
		StartTimestamp: startTs,
		EndTimestamp:   endTs,
		Page:           page,
		Size:           size,
	})
	if err != nil {
		util.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	items := make([]LogItem, 0, len(result.Items))
	for _, row := range result.Items {
		var actualCost float64
		var quota int64
		if row.CostUSD != nil {
			actualCost = *row.CostUSD
			quota = int64(math.Round(actualCost * 500000))
		}
		duration := int64(math.Round(row.DurationMS))
		other := "{}"
		if row.CachedTokens > 0 || row.CacheWriteTokens > 0 {
			other = fmt.Sprintf(`{"model_ratio":1.0,"completion_ratio":1.0,"cache_ratio":0.1,"cache_creation_ratio":1.25,"cache_tokens":%d,"cache_creation_tokens":%d}`, row.CachedTokens, row.CacheWriteTokens)
		}

		items = append(items, LogItem{
			ID:               row.ID,
			CreatedAt:        row.TsUnix,
			UserID:           1,
			Username:         keyName,
			TokenName:        keyName,
			ModelName:        row.Model,
			Type:             1,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			TotalTokens:      row.TotalTokens,
			Quota:            quota,
			ActualCost:       actualCost,
			Duration:         duration,
			RequestTime:      duration,
			IsStream:         row.Stream == 1,
			Other:            other,
		})
	}

	resp := LogSelfResponse{
		Success: true,
		Message: "",
		Data: LogData{
			Items: items,
			Total: result.Total,
		},
	}
	util.WriteJSON(w, http.StatusOK, resp)
}
