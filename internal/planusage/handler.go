package planusage

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/adminauth"
	"github.com/dorokuma/prism/internal/util"
)

// Handler serves GET /admin/quota.
type Handler struct {
	Cache   *Cache
	Enabled func() bool
}

func NewHandler(cache *Cache, enabled func() bool) *Handler {
	if enabled == nil {
		enabled = func() bool { return cache != nil }
	}
	return &Handler{Cache: cache, Enabled: enabled}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		util.WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": map[string]any{"message": "method not allowed", "code": "method_not_allowed"},
		})
		return
	}
	if !adminauth.Authorized(r) {
		util.WriteJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"message": "unauthorized", "code": "unauthorized"},
		})
		return
	}
	if h.Cache == nil || (h.Enabled != nil && !h.Enabled()) {
		util.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "quota unavailable", "code": "quota_unavailable"},
		})
		return
	}
	snaps := h.Cache.List()
	if snaps == nil {
		snaps = []Snapshot{}
	}
	resp := Response{FetchedAt: oldestFetchedAt(snaps), Providers: snaps}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	switch format {
	case "", "json":
		util.WriteJSON(w, http.StatusOK, resp)
	case "table":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, RenderTable(snaps))
	default:
		util.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "invalid format", "code": "bad_request"},
		})
	}
}

func oldestFetchedAt(snaps []Snapshot) time.Time {
	var oldest time.Time
	for i := range snaps {
		ft := snaps[i].FetchedAt
		if ft.IsZero() {
			continue
		}
		if oldest.IsZero() || ft.Before(oldest) {
			oldest = ft
		}
	}
	return oldest
}

// WriteJSON is a small helper for the CLI --json path.
func WriteJSON(w io.Writer, snaps []Snapshot) error {
	resp := Response{FetchedAt: oldestFetchedAt(snaps), Providers: snaps}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(resp)
}
