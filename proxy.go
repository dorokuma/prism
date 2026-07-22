package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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

func isPermanentCredentialError(body []byte) bool {
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

func isQuotaError(body []byte) bool {
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

func handleUpstreamError(acc *Account, resp *http.Response, requestID string, model string) {
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
		slog.Error("upstream permanent error, marking exhausted", append(baseAttrs, "body", redactBody(bodyBytes), "error_type", "auth_failed")...)
		return
	}

	if isPermanentCredentialError(bodyBytes) {
		acc.MarkExhausted()
		slog.Error("upstream permanent credential error, marking exhausted", append(baseAttrs, "body", redactBody(bodyBytes), "error_type", "auth_failed")...)
		return
	}

	if resp.StatusCode == 429 {
		if isQuotaError(bodyBytes) {
			acc.MarkExhausted()
			slog.Error("upstream 429+quota exhaustion, marking exhausted", append(baseAttrs, "body", redactBody(bodyBytes), "error_type", "upstream_ratelimited")...)
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
		slog.Warn("upstream rate-limited 429", append(baseAttrs, "cooldown", cd.String(), "body", redactBody(bodyBytes), "error_type", "upstream_ratelimited")...)
		return
	}

	if isQuotaError(bodyBytes) {
		acc.MarkExhausted()
		slog.Error("upstream insufficient_quota, marking exhausted", append(baseAttrs, "body", redactBody(bodyBytes), "error_type", "upstream_ratelimited")...)
		return
	}

	acc.SetCooldown(30 * time.Second)
	slog.Warn("upstream temporary error, cooling down", append(baseAttrs, "body", redactBody(bodyBytes), "error_type", "upstream_5xx")...)
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
func upstreamContext(r *http.Request, stream bool) (context.Context, context.CancelFunc) {
	if stream {
		return context.WithTimeout(r.Context(), streamMaxDuration)
	}
	return context.WithTimeout(r.Context(), upstreamTimeout)
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

func NewProxyHandler(pool *Pool, wire WireAPIMode, holder *ConfigHolder) http.Handler {
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
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
					"error": map[string]any{"message": "method not allowed", "code": "method_not_allowed"},
				})
				return
			}
			proxyModels(pool, w, r, cfg)
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			if !wire.allowsLegacy() {
				writeJSON(w, http.StatusNotFound, map[string]any{
					"error": map[string]any{"message": "wire_api=responses: /v1/chat/completions disabled", "code": "disabled"},
				})
				return
			}
			proxyChat(pool, w, r, cfg)
			return
		}
		if r.URL.Path == "/v1/responses" {
			if !wire.allowsResponses() {
				writeJSON(w, http.StatusNotFound, map[string]any{
					"error": map[string]any{"message": "wire_api=legacy: /v1/responses disabled", "code": "disabled"},
				})
				return
			}
			proxyResponses(pool, w, r, cfg)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"message": "not found", "code": "not_found"},
		})
		slog.Debug("not_found", "path", r.URL.Path)
	})
}

func proxyResponses(pool *Pool, w http.ResponseWriter, r *http.Request, cfg *Config) {
	start := time.Now()
	defer r.Body.Close()
	const maxBodySize = 10 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("responses body read error", "error", err)
		http.Error(w, "failed to read body", 500)
		return
	}
	tenantID := getTenantID(r)

	// Extract model early for logging (before conversion in case it fails).
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(bodyBytes, &raw)
	virtualModel, _ := rawStringField(raw, "model")
	requestID := requestIDFromCtx(r.Context())

	chatBody, stream, reqTools, err := responsesToChatCompletions(bodyBytes, tenantID)
	dumpDebugResponsesBody(bodyBytes)
	dumpDebugChatBody(chatBody)
	if err != nil {
		slog.Error("responses convert error", "error", err)
		// Rejection logging: classify known rejection reasons.
		errStr := err.Error()
		if strings.Contains(errStr, "image") ||
			strings.Contains(errStr, "previous_response_id") ||
			strings.Contains(errStr, "not supported") {
			reason := "unsupported_input"
			if strings.Contains(errStr, "image") {
				reason = "multimodal_input"
			} else if strings.Contains(errStr, "previous_response_id") {
				reason = "previous_response_id"
			}
			slog.Warn("request_rejected", "req", requestID, "reason", reason, "model", virtualModel)
		}
		errBody, marshalErr := json.Marshal(map[string]any{
			"error": map[string]any{"message": err.Error(), "code": "invalid_request"},
		})
		if marshalErr != nil {
			errBody = []byte(`{"error":{"message":"internal error","code":"internal"}}`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write(errBody)
		return
	}

	slog.Debug("responses request", "remote_addr", r.RemoteAddr, "model", virtualModel, "stream", stream, "chat_body_bytes", len(chatBody))
	proxyChatWithBody(pool, w, r, chatBody, start, chatForwardOpts{
		responsesOut: true,
		stream:       stream,
		model:        virtualModel,
		reqTools:     reqTools,
		tenantID:     tenantID,
	}, cfg)
}

type chatForwardOpts struct {
	responsesOut bool
	stream       bool
	model        string
	reqTools     json.RawMessage
	tenantID     string
}

func proxyChat(pool *Pool, w http.ResponseWriter, r *http.Request, cfg *Config) {
	start := time.Now()
	defer r.Body.Close()
	const maxBodySize = 10 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("chat body read error", "error", err)
		http.Error(w, "failed to read body", 500)
		return
	}
	tenantID := getTenantID(r)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(bodyBytes, &raw)
	stream := rawBoolField(raw, "stream")
	model, _ := rawStringField(raw, "model")
	proxyChatWithBody(pool, w, r, bodyBytes, start, chatForwardOpts{
		stream:   stream,
		model:    model,
		tenantID: tenantID,
	}, cfg)
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
func doUpstreamRequest(acc *Account, r *http.Request, bodyBytes []byte, opts chatForwardOpts, requestID string) doUpstreamResult {
	ctx, cancel := upstreamContext(r, opts.stream)

	targetURL := acc.BaseURL() + "/chat/completions"
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		cancel()
		slog.Error("failed to create upstream request", "req", requestID, "model", opts.model, "account", acc.Name(), "error", err)
		recordUpstreamRetry()
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
			slog.Warn("client disconnected, aborting retry", "req", requestID, "model", opts.model, "error_type", "client_disconnect")
			recordError()
			return doUpstreamResult{retry: false, fatalErr: fmt.Errorf("client disconnected: %w", r.Context().Err())}
		}
		acc.SetCooldown(30 * time.Second)
		slog.Warn("chat retry, upstream connection error", "req", requestID, "model", opts.model, "account", acc.Name(), "error", err, "error_type", upstreamErrorType(err))
		recordUpstreamRetry()
		return doUpstreamResult{retry: true}
	}

	return doUpstreamResult{resp: resp, ctx: ctx, cancel: cancel}
}

// handleUpstreamResponse processes the upstream HTTP response and writes the
// result to the client. It owns the lifecycle of ctx (via the provided cancel)
// and resp.Body.
func handleUpstreamResponse(acc *Account, w http.ResponseWriter, r *http.Request, resp *http.Response, bodyBytes []byte, start time.Time, opts chatForwardOpts, requestID string, ctx context.Context, cancel context.CancelFunc) (done bool, fatalErr error) {
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode == 429 || resp.StatusCode == 402 || resp.StatusCode == 401 {
		handleUpstreamError(acc, resp, requestID, opts.model)
		recordUpstreamRetry()
		return false, nil
	}

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// 4xx client error (other than 401/402/429 handled above, plus
		// 403 which is a permission error not helped by retry).
		// Pass through with redacted body, no cooldown, no retry.
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		if readErr != nil {
			slog.Warn("failed to read upstream 4xx body", "req", requestID, "error", readErr)
		}
		errStr := string(redactBody(errBody))
		slog.Warn("upstream 4xx", "req", requestID, "model", opts.model, "account", acc.Name(), "status", resp.StatusCode, "body", errStr, "error_type", "upstream_4xx")
		recordError()
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
		w.Write(redactBodyBytesWithKeys(errBody, []string{acc.Key()}))
		// Audit: terminal 4xx
		if a := auditFromCtx(r.Context()); a != nil {
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
		slog.Warn("upstream 5xx error, cooling down", "req", requestID, "model", opts.model, "account", acc.Name(), "status", resp.StatusCode, "body", redactBody(errBody), "error_type", "upstream_5xx")
		recordUpstreamRetry()
		return false, nil
	}

	if opts.responsesOut && opts.stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		translateStart := time.Now()
		slog.Debug("responses_stream translate start", "request_id", requestID, "account", acc.Name(), "translate_start", translateStart.Format(time.RFC3339Nano))
		err := translateChatStreamToResponses(w, resp.Body, opts.model, opts.reqTools, getSearchToolCache(opts.tenantID), ctx)
		translateElapsed := time.Since(translateStart).Milliseconds()
		if err != nil {
			slog.Error("responses_stream translate error", "req", requestID, "model", opts.model, "account", acc.Name(), "error", err, "translate_ms", translateElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
			recordError()
			if a := auditFromCtx(r.Context()); a != nil {
				a.Status = http.StatusOK
				a.Account = acc.Name()
				a.ErrorType = upstreamErrorType(err)
				a.Error = err.Error()
			}
			return true, err
		}
		slog.Debug("responses_stream translate done", "request_id", requestID, "account", acc.Name(), "translate_ms", translateElapsed, "elapsed", time.Since(start))
		recordRequest(time.Since(start))
		if a := auditFromCtx(r.Context()); a != nil {
			a.Status = http.StatusOK
			a.Account = acc.Name()
		}
		return true, nil
	}

	if opts.responsesOut && !opts.stream {
		bodyReadStart := time.Now()
		rawBody, err := io.ReadAll(resp.Body)
		bodyReadElapsed := time.Since(bodyReadStart).Milliseconds()
		if err != nil {
			slog.Warn("responses_json body read error", "request_id", requestID, "model", opts.model, "account", acc.Name(), "body_ms", bodyReadElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
			if a := auditFromCtx(r.Context()); a != nil {
				a.Account = acc.Name()
				a.ErrorType = upstreamErrorType(err)
				a.Error = err.Error()
			}
			return true, err
		}
		dumpDebugUpstreamResponse(rawBody)
		translateStart := time.Now()
		out, err := chatCompletionToResponse(rawBody, opts.model, opts.reqTools)
		translateElapsed := time.Since(translateStart).Milliseconds()
		if err != nil {
			slog.Error("responses_json translate error", "req", requestID, "model", opts.model, "account", acc.Name(), "error", err, "translate_ms", translateElapsed, "body_ms", bodyReadElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
			recordError()
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]any{"message": "upstream response translation failed", "code": "upstream_error"},
			})
			if a := auditFromCtx(r.Context()); a != nil {
				a.Status = http.StatusBadGateway
				a.Account = acc.Name()
				a.ErrorType = "upstream_5xx"
				a.Error = err.Error()
			}
			return true, nil
		}
		// Capture token usage from the response body for non-streaming audit.
		if a := auditFromCtx(r.Context()); a != nil {
			if tokensIn, tokensOut := parseUsageFromChatCompletion(rawBody); tokensIn > 0 || tokensOut > 0 {
				a.TokensIn = tokensIn
				a.TokensOut = tokensOut
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		n, _ := w.Write(out)
		slog.Debug("responses_json done", "request_id", requestID, "account", acc.Name(), "written", n, "body_ms", bodyReadElapsed, "translate_ms", translateElapsed, "elapsed", time.Since(start))
		recordRequest(time.Since(start))
		if a := auditFromCtx(r.Context()); a != nil {
			a.Status = http.StatusOK
			a.Account = acc.Name()
		}
		return true, nil
	}

	// Legacy chat path (no responses translation).
	if opts.stream {
		// Streaming: pass through SSE chunks without token capture.
		// Streaming token interception is complex and risks breaking
		// the SSE stream; tokens_in/tokens_out remain 0 (acceptable).
		copyUpstreamHeaders(w, resp.Header)
		w.WriteHeader(resp.StatusCode)
		bodyReadStart := time.Now()
		n, err := streamResponseBody(w, resp.Body, r, acc.Name())
		bodyReadElapsed := time.Since(bodyReadStart).Milliseconds()
		if err != nil {
			slog.Warn("legacy_stream body read error", "request_id", requestID, "model", opts.model, "account", acc.Name(), "body_ms", bodyReadElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
			recordError()
			if a := auditFromCtx(r.Context()); a != nil {
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
		recordRequest(time.Since(start))
		if a := auditFromCtx(r.Context()); a != nil {
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
		slog.Warn("legacy_nonstream body read error", "request_id", requestID, "model", opts.model, "account", acc.Name(), "body_ms", bodyReadElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
		recordError()
		if a := auditFromCtx(r.Context()); a != nil {
			a.Status = resp.StatusCode
			a.Account = acc.Name()
			a.ErrorType = upstreamErrorType(err)
			a.Error = err.Error()
		}
		return true, err
	}
	dumpDebugUpstreamResponse(rawBody)
	// Capture token usage from the response body for non-streaming audit.
	if a := auditFromCtx(r.Context()); a != nil {
		if tokensIn, tokensOut := parseUsageFromChatCompletion(rawBody); tokensIn > 0 || tokensOut > 0 {
			a.TokensIn = tokensIn
			a.TokensOut = tokensOut
		}
	}
	copyUpstreamHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	n, _ := w.Write(rawBody)
	slog.Debug("legacy_nonstream done", "request_id", requestID, "account", acc.Name(), "status", resp.StatusCode, "written", n, "body_ms", bodyReadElapsed, "elapsed", time.Since(start))
	recordRequest(time.Since(start))
	if a := auditFromCtx(r.Context()); a != nil {
		a.Status = resp.StatusCode
		a.Account = acc.Name()
	}
	return true, nil
}

// resolveMaxConcurrent returns the maximum concurrent requests per account
// for the given model. Resolution order:
//  1. cfg.MaxConcurrentPerAccount[model]  (exact match)
//  2. cfg.MaxConcurrentPerAccount["*"]   (wildcard default)
//  3. model name contains "flash" → deepseekV4FlashConcurrency * defaultConcurrencyRatio / 100
//  4. model name contains "pro"   → deepseekV4ProConcurrency * defaultConcurrencyRatio / 100
//  5. fallback: deepseekV4ProConcurrency * defaultConcurrencyRatio / 100 + warn
func resolveMaxConcurrent(model string, cfg *Config) int {
	if cfg.MaxConcurrentPerAccount != nil {
		if v, ok := cfg.MaxConcurrentPerAccount[model]; ok && v > 0 {
			return v
		}
		if v, ok := cfg.MaxConcurrentPerAccount["*"]; ok && v > 0 {
			return v
		}
	}
	modelLower := strings.ToLower(model)
	if strings.Contains(modelLower, "flash") {
		return deepseekV4FlashConcurrency * defaultConcurrencyRatio / 100
	}
	if strings.Contains(modelLower, "pro") {
		return deepseekV4ProConcurrency * defaultConcurrencyRatio / 100
	}
	// Default for unknown models
	slog.Warn("unknown model, using default concurrency", "model", model)
	return deepseekV4ProConcurrency * defaultConcurrencyRatio / 100
}

func proxyChatWithBody(pool *Pool, w http.ResponseWriter, r *http.Request, bodyBytes []byte, start time.Time, opts chatForwardOpts, cfg *Config) {
	requestID := requestIDFromCtx(r.Context())
	aud := &requestAudit{
		Req:    requestID,
		Method: r.Method,
		Path:   r.URL.Path,
		Model:  opts.model,
	}
	r = r.WithContext(context.WithValue(r.Context(), auditKey{}, aud))
	sc := &statusCapture{ResponseWriter: w}

	defer func() {
		aud.DurationMs = float64(time.Since(start).Milliseconds())
		aud.Status = sc.Code
		emitAudit(aud)
	}()

	bodyBytes = transformRequestBody(bodyBytes, cfg)
	if pool.AccountCount() == 0 {
		aud.Error = "no accounts configured"
		aud.ErrorType = "config_error"
		writeJSON(sc, 503, map[string]any{
			"error": map[string]any{"message": "No accounts configured", "code": "no_accounts"},
		})
		return
	}
	maxAttempts := pool.AccountCount() * 2
	maxConcurrent := resolveMaxConcurrent(opts.model, cfg)
	slog.Debug("proxy request start", "request_id", requestID, "path", r.URL.Path, "stream", opts.stream, "responses_out", opts.responsesOut, "start", start.Format(time.RFC3339Nano), "max_concurrent", maxConcurrent)

	for attempts := 0; attempts < maxAttempts; attempts++ {
		if attempts > 0 {
			time.Sleep(upstreamRetryDelay)
		}

		selectCtx, cancel := context.WithTimeout(context.Background(), accountSelectTimeout)
		selectStart := time.Now()
		acc, err := pool.Select(selectCtx, maxConcurrent)
		selectDuration := time.Since(selectStart).Milliseconds()
		cancel()
		accName := "nil"
		if acc != nil {
			accName = acc.Name()
		}
		slog.Debug("pool select done", "request_id", requestID, "attempt", attempts, "select_ms", selectDuration, "account", accName, "error", err)
		if err != nil {
			slog.Error("select account failed", "error", err)
			recordError()
			aud.Error = err.Error()
			if err == ErrNoHealthyAccounts {
				aud.ErrorType = "no_healthy"
			} else if err == ErrSelectTimeout {
				aud.ErrorType = "select_timeout"
			} else {
				aud.ErrorType = "select_failed"
			}
			writeJSON(sc, 503, map[string]any{
				"error": map[string]any{"message": "No healthy accounts available", "code": "no_accounts"},
			})
			return
		}

		// Record the last attempted account for audit, even if the upstream
		// request later fails.
		aud.Account = acc.Name()
		aud.Concurrency = acc.InFlightCount()

		var terminalDone bool
		var terminalFatalErr error

		func() {
			defer pool.Release(acc)
			res := doUpstreamRequest(acc, r, bodyBytes, opts, requestID)
			if res.resp != nil {
				done, fatalErr := handleUpstreamResponse(acc, sc, r, res.resp, bodyBytes, start, opts, requestID, res.ctx, res.cancel)
				if done {
					terminalDone = true
					terminalFatalErr = fatalErr
					return
				}
				return
			}
			if res.retry {
				return
			}
			terminalDone = true
			terminalFatalErr = res.fatalErr
		}()

		if terminalDone {
			if terminalFatalErr != nil {
				return
			}
			return
		}
	}
	slog.Error("all accounts exhausted after retries", "request_id", requestID, "attempts", maxAttempts, "elapsed", time.Since(start))
	recordError()
	aud.Error = "all accounts exhausted after retries"
	aud.ErrorType = "all_exhausted"
	writeJSON(sc, 503, map[string]any{
		"error": map[string]any{"message": "All accounts exhausted after retries", "code": "all_exhausted"},
	})
}

func proxyModels(pool *Pool, w http.ResponseWriter, r *http.Request, cfg *Config) {
	start := time.Now()
	slog.Debug("models request", "remote_addr", r.RemoteAddr, "req", requestIDFromCtx(r.Context()))

	modelIDs := cfg.AllModels()
	if len(modelIDs) == 0 {
		writeJSON(w, 503, map[string]any{
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
	writeJSON(w, http.StatusOK, resp)
	slog.Debug("models returning", "count", len(modelIDs), "req", requestIDFromCtx(r.Context()), "duration_ms", time.Since(start).Milliseconds())
}

var redactBody = util.RedactBody
var redactBodyBytes = util.RedactBodyBytes
var redactBodyRegex = util.RedactBodyRegex
var redactJSON = util.RedactJSON
var redactBodyBytesWithKeys = util.RedactBodyBytesWithKeys
var redactJSONWithKeys = util.RedactJSONWithKeys
var redactStringWithKeys = util.RedactStringWithKeys

// parseUsageFromChatCompletion extracts input/output token counts from a raw
// chat completion response body (non-streaming).  Returns 0, 0 when the body
// cannot be parsed or the usage field is absent.
func parseUsageFromChatCompletion(body []byte) (tokensIn, tokensOut int) {
	var comp chatCompletionResponse
	if err := json.Unmarshal(body, &comp); err != nil || comp.Usage == nil {
		return 0, 0
	}
	return comp.Usage.PromptTokens, comp.Usage.CompletionTokens
}

// transformRequestBody applies model remap, thinking field remap (for DeepSeek),
// and strips unsupported fields (per config) in a single JSON parse/marshal pass.
// Returns the original body unchanged if no transformation was needed.
func transformRequestBody(body []byte, cfg *Config) []byte {
	if cfg == nil {
		return body
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	changed := false

	// Step 1: Model name remap
	model, ok := rawStringField(raw, "model")
	if ok && model != "" {
		remapped := cfg.RemapModel(model)
		if remapped != model {
			rawBytes, _ := json.Marshal(remapped)
			raw["model"] = json.RawMessage(rawBytes)
			changed = true
			slog.Debug("model remap", "from", model, "to", remapped)
			model = remapped // use remapped name for downstream steps
		}
	}

	// Step 2: Thinking field remap for DeepSeek models
	if isDeepSeekModel(model) {
		if thinkRaw, ok := raw["thinking"]; ok && len(thinkRaw) > 0 && string(thinkRaw) != "null" {
			var thinking map[string]any
			if err := json.Unmarshal(thinkRaw, &thinking); err == nil {
				if level, ok := thinking["level"].(string); ok {
					mapped := mapThoughtLevel(level)
					if mapped != level {
						slog.Debug("thinking level remap", "model", model, "from", level, "to", mapped)
						thinking["level"] = mapped
						if b, err := json.Marshal(thinking); err == nil {
							raw["thinking"] = json.RawMessage(b)
							changed = true
						}
					}
				}
			}
		}
		if effortRaw, ok := raw["reasoning_effort"]; ok && len(effortRaw) > 0 && string(effortRaw) != "null" {
			var effort string
			if err := json.Unmarshal(effortRaw, &effort); err == nil {
				mapped := mapThoughtLevel(effort)
				if mapped != effort {
					slog.Debug("reasoning_effort remap", "model", model, "from", effort, "to", mapped)
					if b, err := json.Marshal(mapped); err == nil {
						raw["reasoning_effort"] = json.RawMessage(b)
						changed = true
					}
				}
			}
		}
	}

	// Step 3: Strip unsupported fields per tier config
	// Aggregate StripFields across all tiers whose upstream matches the model.
	if len(cfg.StripFields) > 0 && model != "" {
		var matchedTiers []string
		seenFields := make(map[string]bool)
		var mergedFields []string
		for t, upstream := range cfg.ModelTiers {
			if upstream == model {
				matchedTiers = append(matchedTiers, t)
				if fields, ok := cfg.StripFields[t]; ok {
					for _, f := range fields {
						if !seenFields[f] {
							seenFields[f] = true
							mergedFields = append(mergedFields, f)
						}
					}
				}
			}
		}
		if len(mergedFields) > 0 {
			sort.Strings(matchedTiers)
			for _, field := range mergedFields {
				if _, exists := raw[field]; exists {
					delete(raw, field)
					changed = true
					slog.Debug("stripped field", "field", field, "model", model, "tiers", matchedTiers)
				}
			}
		}
	}

	if !changed {
		return body
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// getTenantID returns the tenant identifier for the request.
// Currently always returns "default" as multi-tenancy is not yet implemented.
// TODO: implement per-tenant isolation when multi-tenant support is needed.
func getTenantID(r *http.Request) string {
	return "default"
}


