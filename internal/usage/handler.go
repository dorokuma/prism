package usage

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/adminauth"
	"github.com/dorokuma/prism/internal/util"
)

// SummaryHandler serves GET /admin/usage/summary. It is defined here but not
// registered; the wiring stage registers it on the admin mux.
//
// Auth (fail-closed): when PRISM_ADMIN_TOKEN is configured, EVERY request —
// loopback included — must present it as a Bearer token (compared in
// constant time). Only when NO token is configured is a direct local request
// (loopback RemoteAddr without X-Forwarded-For / X-Real-IP / Forwarded)
// allowed without one; a loopback request carrying a forwarding header
// (same-machine reverse proxy) and all other clients are denied.
// METRICS_TOKEN and business API keys are deliberately not accepted.
//
// The token is read from the environment on EVERY request (like the
// /metrics METRICS_TOKEN path), so a token set or rotated while the process
// runs takes effect immediately — no restart, no stale-token window.
type SummaryHandler struct {
	Store Store
}

// NewSummaryHandler creates a summary handler. The admin token is NOT read
// here: authorized() reads PRISM_ADMIN_TOKEN per request (hot-reload of the
// env var without a restart).
func NewSummaryHandler(store Store) *SummaryHandler {
	return &SummaryHandler{Store: store}
}

func (h *SummaryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if h.Store == nil {
		util.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "usage store unavailable", "code": "store_unavailable"},
		})
		return
	}
	q, err := parseSummaryQuery(r)
	if err != nil {
		writeSummaryError(w, err)
		return
	}
	// format=table renders the same report as the prism usage CLI (shared
	// render code); format=json (default) is the existing behavior.
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	switch format {
	case "", "json":
		// existing JSON behavior, unchanged
	case "table":
		h.serveTable(w, r, q)
		return
	default:
		writeSummaryError(w, &QueryError{Msg: "invalid format"})
		return
	}
	rows, err := h.Store.Summary(r.Context(), q)
	if err != nil {
		var qe *QueryError
		if errors.As(err, &qe) {
			writeSummaryError(w, err)
			return
		}
		slog.Error("usage: summary query failed", "error", err)
		util.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "usage store unavailable", "code": "store_unavailable"},
		})
		return
	}
	if rows == nil {
		rows = []SummaryRow{}
	}
	util.WriteJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

// serveTable renders the format=table response: the summary header comes
// from Overview (never from summing the LIMIT-truncated rows) and the detail
// section shares RenderUsageReport with the CLI, so both outputs are
// produced by the same code. The compact single-line table is the only
// layout — there are no layout/width params and no terminal-width
// dependency.
func (h *SummaryHandler) serveTable(w http.ResponseWriter, r *http.Request, q SummaryQuery) {
	ov, err := h.Store.Overview(r.Context(), q)
	if err != nil {
		slog.Error("usage: overview query failed", "error", err)
		util.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "usage store unavailable", "code": "store_unavailable"},
		})
		return
	}
	rows, err := h.Store.Summary(r.Context(), q)
	if err != nil {
		var qe *QueryError
		if errors.As(err, &qe) {
			writeSummaryError(w, err)
			return
		}
		slog.Error("usage: summary query failed", "error", err)
		util.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "usage store unavailable", "code": "store_unavailable"},
		})
		return
	}
	body := RenderUsageReport(ov, rows, q.GroupBy, ReportOptions{
		Period: DescribePeriod(q.From, q.To, time.Now().Unix()),
	})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}

func writeSummaryError(w http.ResponseWriter, err error) {
	util.WriteJSON(w, http.StatusBadRequest, map[string]any{
		"error": map[string]any{"message": err.Error(), "code": "bad_request"},
	})
}

// parseSummaryQuery reads and validates the query parameters. All validation
// failures are returned as *QueryError (HTTP 400).
func parseSummaryQuery(r *http.Request) (SummaryQuery, error) {
	qp := r.URL.Query()
	var q SummaryQuery
	var err error
	if v := qp.Get("from"); v != "" {
		if q.From, err = strconv.ParseInt(v, 10, 64); err != nil {
			return q, &QueryError{Msg: "invalid from"}
		}
	}
	if v := qp.Get("to"); v != "" {
		if q.To, err = strconv.ParseInt(v, 10, 64); err != nil {
			return q, &QueryError{Msg: "invalid to"}
		}
	}
	if v := qp.Get("group_by"); v != "" {
		for _, g := range strings.Split(v, ",") {
			if g = strings.TrimSpace(g); g != "" {
				q.GroupBy = append(q.GroupBy, g)
			}
		}
	}
	q.Model = qp.Get("model")
	q.Provider = qp.Get("provider")
	q.Account = qp.Get("account")
	q.KeyID = qp.Get("key_id")
	if v := qp.Get("stream"); v != "" {
		var b bool
		if b, err = parseBoolParam(v); err != nil {
			return q, &QueryError{Msg: "invalid stream"}
		}
		q.Stream = &b
	}
	if v := qp.Get("success"); v != "" {
		var b bool
		if b, err = parseBoolParam(v); err != nil {
			return q, &QueryError{Msg: "invalid success"}
		}
		q.Success = &b
	}
	if v := qp.Get("limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 {
			return q, &QueryError{Msg: "invalid limit"}
		}
		q.Limit = n
	}
	return q, nil
}

func parseBoolParam(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid bool %q", v)
}
