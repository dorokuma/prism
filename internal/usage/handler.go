package usage

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/middleware"
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
	if !h.authorized(r) {
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
// table shares RenderUsageReport with the CLI, so both outputs are produced
// by the same code.
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

// adminTokenPadLen is the fixed pad length for the admin-token comparison,
// mirroring the business-key auth pad (middleware.authPadLen = 256) so both
// auth paths use the same constant-time discipline. A token longer than the
// pad cannot be compared without truncation (a prefix match could pass), so
// it is rejected outright.
const adminTokenPadLen = 256

// authorized decides admin access. Fail-closed: a configured
// PRISM_ADMIN_TOKEN means loopback is NOT trusted and every request must
// present the correct Bearer token. The token is re-read from the
// environment on every call, so a rotation takes effect without a restart
// (identical to the /metrics METRICS_TOKEN behavior). Loopback is decided by
// middleware.IsLocalhost — the same single implementation the business auth
// and /metrics paths use (any 127.0.0.0/8 or ::1 address); there is no
// second, divergent loopback check here. Only when the token is unset does
// the loopback shortcut apply, and even then only for DIRECT local
// requests: a same-machine reverse proxy also presents a loopback RemoteAddr
// but adds X-Forwarded-For / X-Real-IP / Forwarded, so a loopback request
// carrying a forwarding header is denied (mirrors the /metrics rule). An
// unset PRISM_ADMIN_TOKEN means remote access is denied entirely.
func (h *SummaryHandler) authorized(r *http.Request) bool {
	token := os.Getenv("PRISM_ADMIN_TOKEN")
	if token != "" {
		// Shared Bearer semantics with the business auth path
		// (middleware.SplitBearerToken): case-insensitive scheme, token bytes
		// returned verbatim (never trimmed or folded), and an empty or
		// whitespace-only credential rejected outright — "Bearer  token"
		// (double space) is a different token, not a trimmed "token".
		got, ok := middleware.SplitBearerToken(r.Header.Get("Authorization"))
		if !ok {
			return false
		}
		// Fixed-length pad comparison, identical to middleware.Authenticate:
		// unequal lengths must not short-circuit the comparison and leak the
		// expected length via timing. Length-based rejection leaks only the
		// input's own length class, never anything about the configured token.
		if len(got) > adminTokenPadLen || len(token) > adminTokenPadLen {
			return false
		}
		pb := make([]byte, adminTokenPadLen)
		eb := make([]byte, adminTokenPadLen)
		copy(pb, got)
		copy(eb, token)
		return subtle.ConstantTimeCompare(pb, eb) == 1
	}
	return middleware.IsLocalhost(r) && !middleware.HasForwardedHeaders(r)
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
