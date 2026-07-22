package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/convert"
	"github.com/dorokuma/prism/internal/mcp"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/stream"
	"github.com/dorokuma/prism/internal/util"
)

var upstreamHeaderAllowlist = map[string]bool{
	"Content-Type":        true,
	"Content-Disposition": true,
	"Content-Language":    true,
	"Retry-After":         true,
}

var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Transfer-Encoding":   true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Upgrade":             true,
}

var sensitiveClientHeaders = map[string]bool{
	http.CanonicalHeaderKey("Cookie"):       true,
	http.CanonicalHeaderKey("X-Api-Key"):    true,
	http.CanonicalHeaderKey("X-Auth-Token"): true,
}

func isHopByHop(key string) bool {
	return hopByHopHeaders[http.CanonicalHeaderKey(key)]
}

// OpenAIErrorResponse represents an OpenAI API error response body.
type OpenAIErrorResponse struct {
	Error struct {
		Message string      `json:"message"`
		Type    string      `json:"type"`
		Param   interface{} `json:"param"`
		Code    string      `json:"code"`
	} `json:"error"`
}

// IsPermanentCredentialError checks if the response body indicates a permanent
// credential error (invalid_api_key, revoked, account_deactivated).
func IsPermanentCredentialError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var errResp OpenAIErrorResponse
	err := json.Unmarshal(body, &errResp)

	if err == nil && errResp.Error.Code != "" {
		code := strings.ToLower(errResp.Error.Code)
		if code == "invalid_api_key" || code == "revoked" || code == "account_deactivated" {
			return true
		}
	}
	return false
}

// IsQuotaError checks if the response body indicates a quota/rate-limit error.
func IsQuotaError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var errResp OpenAIErrorResponse
	_ = json.Unmarshal(body, &errResp)

	if errResp.Error.Type != "" {
		typ := strings.ToLower(errResp.Error.Type)
		if typ == "gousagelimiterror" {
			return true
		}
	}
	if errResp.Error.Code != "" {
		code := strings.ToLower(errResp.Error.Code)
		if code == "insufficient_quota" {
			return true
		}
	}
	bodyLower := strings.ToLower(string(body))
	if strings.Contains(bodyLower, "quota exceeded") ||
		strings.Contains(bodyLower, "usage limit") ||
		strings.Contains(bodyLower, "monthly usage limit") {
		return true
	}
	return false
}

func parseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 0
	}
	if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

func handleUpstreamError(acc *pool.Account, resp *http.Response, requestID string, model string) {
	if resp == nil || resp.Body == nil {
		return
	}
	limitReader := io.LimitReader(resp.Body, 4096)
	bodyBytes, err := io.ReadAll(limitReader)
	if err != nil {
		slog.Error("handleUpstreamError read body", "req", requestID, "error", err)
	}

	baseAttrs := []any{"req", requestID, "model", model, "account", acc.Name(), "status", resp.StatusCode}

	if resp.StatusCode == 401 || resp.StatusCode == 402 {
		acc.MarkExhausted()
		slog.Error("upstream permanent error, marking exhausted", append(baseAttrs, "body", util.RedactBody(bodyBytes), "error_type", "auth_failed")...)
		return
	}

	if IsPermanentCredentialError(bodyBytes) {
		acc.MarkExhausted()
		slog.Error("upstream permanent credential error, marking exhausted", append(baseAttrs, "body", util.RedactBody(bodyBytes), "error_type", "auth_failed")...)
		return
	}

	if resp.StatusCode == 429 {
		if IsQuotaError(bodyBytes) {
			acc.MarkExhausted()
			slog.Error("upstream 429+quota exhaustion, marking exhausted", append(baseAttrs, "body", util.RedactBody(bodyBytes), "error_type", "upstream_ratelimited")...)
			return
		}
		cd := parseRetryAfter(resp)
		if cd <= 0 {
			cd = 30 * time.Second
		}
		if cd > 5*time.Minute {
			cd = 5 * time.Minute
		}
		acc.SetCooldown(cd)
		slog.Warn("upstream rate-limited 429", append(baseAttrs, "cooldown", cd.String(), "body", util.RedactBody(bodyBytes), "error_type", "upstream_ratelimited")...)
		return
	}

	if IsQuotaError(bodyBytes) {
		acc.MarkExhausted()
		slog.Error("upstream insufficient_quota, marking exhausted", append(baseAttrs, "body", util.RedactBody(bodyBytes), "error_type", "upstream_ratelimited")...)
		return
	}

	acc.SetCooldown(30 * time.Second)
	slog.Warn("upstream temporary error, cooling down", append(baseAttrs, "body", util.RedactBody(bodyBytes), "error_type", "upstream_5xx")...)
}

// upstreamErrorType classifies an upstream connection error into a short category
// string for structured logging and future metrics/audit.
func upstreamErrorType(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if strings.Contains(s, "context deadline exceeded") {
		return "upstream_timeout"
	}
	if strings.Contains(s, "connection refused") {
		return "upstream_refused"
	}
	if strings.Contains(s, "EOF") {
		return "upstream_refused"
	}
	if strings.Contains(s, "context canceled") {
		return "client_disconnect"
	}
	return "upstream_refused"
}

// upstreamContext creates a context for upstream requests.
// For streaming requests, it applies a wide streamMaxDuration timeout on top of
// r.Context() so that long-lived inference connections propagate client
// disconnection but are guarded against hanging indefinitely.
// For non-streaming requests, it applies upstreamTimeout.
func upstreamContext(r *http.Request, isStream bool) (context.Context, context.CancelFunc) {
	if isStream {
		return context.WithTimeout(r.Context(), config.StreamMaxDuration)
	}
	return context.WithTimeout(r.Context(), config.UpstreamTimeout)
}

func copyClientHeaders(dst http.Header, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		ck := http.CanonicalHeaderKey(k)
		if ck == "Authorization" {
			continue
		}
		if ck == "Accept-Encoding" {
			continue
		}
		if sensitiveClientHeaders[ck] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// copyUpstreamHeaders copies only allowed response headers from the upstream
// to the client. Only headers in upstreamHeaderAllowlist are forwarded;
// headers that expose upstream identity (Server, Via, X-RateLimit-*,
// X-Request-ID) are excluded.
func copyUpstreamHeaders(dst http.ResponseWriter, src http.Header) {
	for k, vs := range src {
		ck := http.CanonicalHeaderKey(k)
		if !upstreamHeaderAllowlist[ck] {
			continue
		}
		for _, v := range vs {
			dst.Header().Add(k, v)
		}
	}
}

// doUpstreamResult bundles the result of an upstream request attempt.
// resp is non-nil when the upstream returned an HTTP response (success or not).
// ctx/cancel are valid only when resp is non-nil and are passed to
// handleUpstreamResponse which owns their lifecycle.
// When resp is nil, retry indicates whether the caller should retry
// (retry=true) or treat this as a fatal error (retry=false with fatalErr).
type doUpstreamResult struct {
	resp     *http.Response
	ctx      context.Context
	cancel   context.CancelFunc
	retry    bool
	fatalErr error
}

// doUpstreamRequest builds and sends the upstream HTTP request.
// On success it returns the response plus ctx/cancel for the caller (segment 3)
// to manage. On any error it explicitly cancels the upstream context and returns
// a result describing whether the caller should retry.
func doUpstreamRequest(acc *pool.Account, r *http.Request, bodyBytes []byte, opts ChatForwardOpts, requestID string) doUpstreamResult {
	ctx, cancel := upstreamContext(r, opts.Stream)

	targetURL := acc.BaseURL() + "/chat/completions"
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		cancel()
		slog.Error("failed to create upstream request", "req", requestID, "model", opts.Model, "account", acc.Name(), "error", err)
		util.RecordUpstreamRetry()
		return doUpstreamResult{retry: true}
	}
	copyClientHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+acc.Key())
	req.Header.Set("Content-Type", "application/json")

	resp, err := acc.Client().Do(req)
	if err != nil {
		cancel()
		// If client disconnected, don't retry
		if r.Context().Err() != nil {
			slog.Warn("client disconnected, aborting retry", "req", requestID, "model", opts.Model, "error_type", "client_disconnect")
			util.RecordError()
			return doUpstreamResult{retry: false, fatalErr: fmt.Errorf("client disconnected: %w", r.Context().Err())}
		}
		acc.SetCooldown(30 * time.Second)
		slog.Warn("chat retry, upstream connection error", "req", requestID, "model", opts.Model, "account", acc.Name(), "error", err, "error_type", upstreamErrorType(err))
		util.RecordUpstreamRetry()
		return doUpstreamResult{retry: true}
	}

	return doUpstreamResult{resp: resp, ctx: ctx, cancel: cancel}
}

// handleUpstreamResponse processes the upstream HTTP response and writes the
// result to the client. It owns the lifecycle of ctx (via the provided cancel)
// and resp.Body.
func handleUpstreamResponse(acc *pool.Account, w http.ResponseWriter, r *http.Request, resp *http.Response, bodyBytes []byte, start time.Time, opts ChatForwardOpts, requestID string, ctx context.Context, cancel context.CancelFunc) (done bool, fatalErr error) {
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode == 429 || resp.StatusCode == 402 || resp.StatusCode == 401 {
		handleUpstreamError(acc, resp, requestID, opts.Model)
		util.RecordUpstreamRetry()
		return false, nil
	}

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// 4xx client error (other than 401/402/429 handled above, plus
		// 403 which is a permission error not helped by retry).
		// Pass through with redacted body, no cooldown, no retry.
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, config.MaxErrorBodyBytes))
		if readErr != nil {
			slog.Warn("failed to read upstream 4xx body", "req", requestID, "error", readErr)
		}
		errStr := string(util.RedactBody(errBody))
		slog.Warn("upstream 4xx", "req", requestID, "model", opts.Model, "account", acc.Name(), "status", resp.StatusCode, "body", errStr, "error_type", "upstream_4xx")
		util.RecordError()
		// Transparent proxy: forward all non-hop-by-hop upstream headers
		// (see copyUpstreamHeaders godoc for design rationale), then remove
		// headers that become invalid after body redaction.
		copyUpstreamHeaders(w, resp.Header)
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Encoding")
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(util.RedactBodyBytesWithKeys(errBody, []string{acc.Key()}))
		// Audit: terminal 4xx
		if a := middleware.AuditFromCtx(r.Context()); a != nil {
			a.Status = resp.StatusCode
			a.Account = acc.Name()
			a.ErrorType = "upstream_4xx"
			a.Error = errStr
		}
		return true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 5xx server error or other non-2xx: cooldown and retry.
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			slog.Warn("failed to read upstream 5xx body", "req", requestID, "error", readErr)
		}
		acc.SetCooldown(30 * time.Second)
		slog.Warn("upstream 5xx error, cooling down", "req", requestID, "model", opts.Model, "account", acc.Name(), "status", resp.StatusCode, "body", util.RedactBody(errBody), "error_type", "upstream_5xx")
		util.RecordUpstreamRetry()
		return false, nil
	}

	if opts.ResponsesOut && opts.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		translateStart := time.Now()
		slog.Debug("responses_stream translate start", "request_id", requestID, "account", acc.Name(), "translate_start", translateStart.Format(time.RFC3339Nano))
		err := stream.TranslateChatStreamToResponses(w, resp.Body, opts.Model, opts.ReqTools, mcp.GetSearchToolCache(opts.TenantID), ctx)
		translateElapsed := time.Since(translateStart).Milliseconds()
		if err != nil {
			slog.Error("responses_stream translate error", "req", requestID, "model", opts.Model, "account", acc.Name(), "error", err, "translate_ms", translateElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
			util.RecordError()
			if a := middleware.AuditFromCtx(r.Context()); a != nil {
				a.Status = http.StatusOK
				a.Account = acc.Name()
				a.ErrorType = upstreamErrorType(err)
				a.Error = err.Error()
			}
			return true, err
		}
		slog.Debug("responses_stream translate done", "request_id", requestID, "account", acc.Name(), "translate_ms", translateElapsed, "elapsed", time.Since(start))
		util.RecordRequest(time.Since(start))
		if a := middleware.AuditFromCtx(r.Context()); a != nil {
			a.Status = http.StatusOK
			a.Account = acc.Name()
		}
		return true, nil
	}

	if opts.ResponsesOut && !opts.Stream {
		bodyReadStart := time.Now()
		rawBody, err := io.ReadAll(resp.Body)
		bodyReadElapsed := time.Since(bodyReadStart).Milliseconds()
		if err != nil {
			slog.Warn("responses_json body read error", "request_id", requestID, "model", opts.Model, "account", acc.Name(), "body_ms", bodyReadElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
			if a := middleware.AuditFromCtx(r.Context()); a != nil {
				a.Account = acc.Name()
				a.ErrorType = upstreamErrorType(err)
				a.Error = err.Error()
			}
			return true, err
		}
		util.DumpDebugUpstreamResponse(rawBody)
		translateStart := time.Now()
		out, err := convert.ChatCompletionToResponse(rawBody, opts.Model, opts.ReqTools)
		translateElapsed := time.Since(translateStart).Milliseconds()
		if err != nil {
			slog.Error("responses_json translate error", "req", requestID, "model", opts.Model, "account", acc.Name(), "error", err, "translate_ms", translateElapsed, "body_ms", bodyReadElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
			util.RecordError()
			util.WriteJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]any{"message": "upstream response translation failed", "code": "upstream_error"},
			})
			if a := middleware.AuditFromCtx(r.Context()); a != nil {
				a.Status = http.StatusBadGateway
				a.Account = acc.Name()
				a.ErrorType = "upstream_5xx"
				a.Error = err.Error()
			}
			return true, nil
		}
		// Capture token usage from the response body for non-streaming audit.
		if a := middleware.AuditFromCtx(r.Context()); a != nil {
			if tokensIn, tokensOut := parseUsageFromChatCompletion(rawBody); tokensIn > 0 || tokensOut > 0 {
				a.TokensIn = tokensIn
				a.TokensOut = tokensOut
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		n, _ := w.Write(out)
		slog.Debug("responses_json done", "request_id", requestID, "account", acc.Name(), "written", n, "body_ms", bodyReadElapsed, "translate_ms", translateElapsed, "elapsed", time.Since(start))
		util.RecordRequest(time.Since(start))
		if a := middleware.AuditFromCtx(r.Context()); a != nil {
			a.Status = http.StatusOK
			a.Account = acc.Name()
		}
		return true, nil
	}

	// Legacy chat path (no responses translation).
	if opts.Stream {
		// Streaming: pass through SSE chunks without token capture.
		// Streaming token interception is complex and risks breaking
		// the SSE stream; tokens_in/tokens_out remain 0 (acceptable).
		copyUpstreamHeaders(w, resp.Header)
		w.WriteHeader(resp.StatusCode)
		bodyReadStart := time.Now()
		n, err := stream.StreamResponseBody(w, resp.Body, r, acc.Name())
		bodyReadElapsed := time.Since(bodyReadStart).Milliseconds()
		if err != nil {
			slog.Warn("legacy_stream body read error", "request_id", requestID, "model", opts.Model, "account", acc.Name(), "body_ms", bodyReadElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
			util.RecordError()
			if a := middleware.AuditFromCtx(r.Context()); a != nil {
				a.Status = resp.StatusCode
				a.Account = acc.Name()
				a.ErrorType = upstreamErrorType(err)
				a.Error = err.Error()
			}
			return true, err
		}
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			slog.Debug("legacy_stream done", "request_id", requestID, "account", acc.Name(), "status", resp.StatusCode, "written", n, "content_length", cl, "body_ms", bodyReadElapsed, "elapsed", time.Since(start))
		} else {
			slog.Debug("legacy_stream done", "request_id", requestID, "account", acc.Name(), "status", resp.StatusCode, "written", n, "body_ms", bodyReadElapsed, "elapsed", time.Since(start))
		}
		util.RecordRequest(time.Since(start))
		if a := middleware.AuditFromCtx(r.Context()); a != nil {
			a.Status = resp.StatusCode
			a.Account = acc.Name()
		}
		return true, nil
	}

	// Non-streaming legacy: read full body, capture token usage, write to client.
	bodyReadStart := time.Now()
	rawBody, err := io.ReadAll(resp.Body)
	bodyReadElapsed := time.Since(bodyReadStart).Milliseconds()
	if err != nil {
		slog.Warn("legacy_nonstream body read error", "request_id", requestID, "model", opts.Model, "account", acc.Name(), "body_ms", bodyReadElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
		util.RecordError()
		if a := middleware.AuditFromCtx(r.Context()); a != nil {
			a.Status = resp.StatusCode
			a.Account = acc.Name()
			a.ErrorType = upstreamErrorType(err)
			a.Error = err.Error()
		}
		return true, err
	}
	util.DumpDebugUpstreamResponse(rawBody)
	// Capture token usage from the response body for non-streaming audit.
	if a := middleware.AuditFromCtx(r.Context()); a != nil {
		if tokensIn, tokensOut := parseUsageFromChatCompletion(rawBody); tokensIn > 0 || tokensOut > 0 {
			a.TokensIn = tokensIn
			a.TokensOut = tokensOut
		}
	}
	copyUpstreamHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	n, _ := w.Write(rawBody)
	slog.Debug("legacy_nonstream done", "request_id", requestID, "account", acc.Name(), "status", resp.StatusCode, "written", n, "body_ms", bodyReadElapsed, "elapsed", time.Since(start))
	util.RecordRequest(time.Since(start))
	if a := middleware.AuditFromCtx(r.Context()); a != nil {
		a.Status = resp.StatusCode
		a.Account = acc.Name()
	}
	return true, nil
}
