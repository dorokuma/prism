package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

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

var (
	reAPIKey = regexp.MustCompile(`sk-[A-Za-z0-9_-]{10,}`)
	reBearer = regexp.MustCompile(`Bearer [A-Za-z0-9_-]{10,}`)
)

// sensitiveJSONKeys names JSON object keys whose values should be redacted
// in debug logs and error responses. Compared after strings.ToLower.
var sensitiveJSONKeys = map[string]bool{
	"api_key":        true,
	"apikey":         true,
	"token":          true,
	"access_token":   true,
	"refresh_token":  true,
	"password":       true,
	"passwd":         true,
	"secret":         true,
	"authorization":  true,
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

func handleUpstreamError(acc *Account, resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	limitReader := io.LimitReader(resp.Body, 4096)
	bodyBytes, err := io.ReadAll(limitReader)
	if err != nil {
		log.Printf("proxy: handleUpstreamError read body: %v", err)
	}

	if resp.StatusCode == 401 || resp.StatusCode == 402 {
		acc.MarkExhausted()
		log.Printf("proxy: %s status %d, marking exhausted. body: %s",
			acc.Name(), resp.StatusCode, redactBody(bodyBytes))
		return
	}

	if isPermanentCredentialError(bodyBytes) {
		acc.MarkExhausted()
		log.Printf("proxy: %s permanent credential error (status=%d), marking exhausted. body: %s",
			acc.Name(), resp.StatusCode, redactBody(bodyBytes))
		return
	}

	if resp.StatusCode == 429 {
		if isQuotaError(bodyBytes) {
			acc.MarkExhausted()
			log.Printf("proxy: %s 429+quota exhaustion, marking exhausted. body: %s",
				acc.Name(), redactBody(bodyBytes))
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
		log.Printf("proxy: %s rate-limited 429, cooling down %v. body: %s",
			acc.Name(), cd, redactBody(bodyBytes))
		return
	}

	if isQuotaError(bodyBytes) {
		acc.MarkExhausted()
		log.Printf("proxy: %s insufficient_quota (status=%d), marking exhausted. body: %s",
			acc.Name(), resp.StatusCode, redactBody(bodyBytes))
		return
	}

	acc.SetCooldown(30 * time.Second)
	log.Printf("proxy: %s temporary error (status=%d), cooling down 30s. body: %s",
		acc.Name(), resp.StatusCode, redactBody(bodyBytes))
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
		if sensitiveClientHeaders[ck] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// copyUpstreamHeaders copies all non-hop-by-hop headers from the upstream
// response to the client. This is a transparent proxy by design: all upstream
// headers (including Server, Via, X-RateLimit-* and other upstream fingerprints)
// are forwarded as-is. This is intentional for full transparency; if hiding
// upstream implementation details becomes important in the future, switch to
// an allowlist approach.
func copyUpstreamHeaders(dst http.ResponseWriter, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			dst.Header().Add(k, v)
		}
	}
}

func NewProxyHandler(pool *Pool, wire WireAPIMode, cfg *Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
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
	})
}

func proxyResponses(pool *Pool, w http.ResponseWriter, r *http.Request, cfg *Config) {
	start := time.Now()
	defer r.Body.Close()
	const maxBodySize = 10 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("proxy: responses body read error: %v", err)
		http.Error(w, "failed to read body", 500)
		return
	}
	tenantID := getTenantID(r)
	chatBody, stream, reqTools, err := responsesToChatCompletions(bodyBytes, tenantID)
	dumpDebugResponsesBody(bodyBytes)
	dumpDebugChatBody(chatBody)
	if err != nil {
		log.Printf("proxy: responses convert error: %v", err)
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
	var virtualModel string
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(bodyBytes, &raw)
	virtualModel, _ = rawStringField(raw, "model")

	log.Printf("proxy: responses request from %s model=%s stream=%v chat_body=%d bytes", r.RemoteAddr, virtualModel, stream, len(chatBody))
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
		log.Printf("proxy: chat body read error: %v", err)
		http.Error(w, "failed to read body", 500)
		return
	}
	tenantID := getTenantID(r)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(bodyBytes, &raw)
	stream := rawBoolField(raw, "stream")
	proxyChatWithBody(pool, w, r, bodyBytes, start, chatForwardOpts{
		stream:   stream,
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
		log.Printf("proxy: failed to create request for %s: %v", acc.Name(), err)
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
			log.Printf("proxy: req=%s client disconnected, aborting retry", requestID)
			recordError()
			return doUpstreamResult{retry: false, fatalErr: fmt.Errorf("client disconnected: %w", r.Context().Err())}
		}
		acc.SetCooldown(30 * time.Second)
		log.Printf("proxy: chat retry via %s (upstream connection error), cooling down 30s: %v", acc.Name(), err)
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
		handleUpstreamError(acc, resp)
		recordUpstreamRetry()
		return false, nil
	}

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// 4xx client error (other than 401/402/429 handled above, plus
		// 403 which is a permission error not helped by retry).
		// Pass through with redacted body, no cooldown, no retry.
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		if readErr != nil {
			log.Printf("proxy: failed to read upstream 4xx body: %v", readErr)
		}
		log.Printf("proxy: %s upstream 4xx status=%d body=%s", acc.Name(), resp.StatusCode, redactBody(errBody))
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
		w.Write(redactBodyBytes(errBody))
		return true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 5xx server error or other non-2xx: cooldown and retry.
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			log.Printf("proxy: failed to read upstream 5xx body: %v", readErr)
		}
		acc.SetCooldown(30 * time.Second)
		log.Printf("proxy: %s 5xx error (status=%d), cooling down 30s. body: %s", acc.Name(), resp.StatusCode, redactBody(errBody))
		recordUpstreamRetry()
		return false, nil
	}

	if opts.responsesOut && opts.stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		translateStart := time.Now()
		log.Printf("proxy: req=%s account=%s mode=responses_stream translate.start=%s", requestID, acc.Name(), translateStart.Format(time.RFC3339Nano))
		err := translateChatStreamToResponses(w, resp.Body, opts.model, opts.reqTools, getSearchToolCache(opts.tenantID), ctx)
		translateElapsed := time.Since(translateStart).Milliseconds()
		if err != nil {
			log.Printf("proxy: req=%s account=%s mode=responses_stream translate.error=%v translate_ms=%d elapsed=%v", requestID, acc.Name(), err, translateElapsed, time.Since(start))
			recordError()
			return true, err
		}
		log.Printf("proxy: req=%s account=%s mode=responses_stream translate.done translate_ms=%d elapsed=%v", requestID, acc.Name(), translateElapsed, time.Since(start))
		recordRequest(time.Since(start))
		return true, nil
	}

	if opts.responsesOut && !opts.stream {
		bodyReadStart := time.Now()
		rawBody, err := io.ReadAll(resp.Body)
		bodyReadElapsed := time.Since(bodyReadStart).Milliseconds()
		if err != nil {
			log.Printf("proxy: req=%s account=%s mode=responses_json body_read_error body_ms=%d elapsed=%v", requestID, acc.Name(), bodyReadElapsed, time.Since(start))
			return true, err
		}
		dumpDebugUpstreamResponse(rawBody)
		translateStart := time.Now()
		out, err := chatCompletionToResponse(rawBody, opts.model, opts.reqTools)
		translateElapsed := time.Since(translateStart).Milliseconds()
		if err != nil {
			log.Printf("proxy: req=%s account=%s mode=responses_json translate.error=%v translate_ms=%d body_ms=%d elapsed=%v", requestID, acc.Name(), err, translateElapsed, bodyReadElapsed, time.Since(start))
			recordError()
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]any{"message": "upstream response translation failed", "code": "upstream_error"},
			})
			return true, nil
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		n, _ := w.Write(out)
		log.Printf("proxy: req=%s account=%s mode=responses_json done written=%d body_ms=%d translate_ms=%d elapsed=%v", requestID, acc.Name(), n, bodyReadElapsed, translateElapsed, time.Since(start))
		recordRequest(time.Since(start))
		return true, nil
	}

	copyUpstreamHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)
	bodyReadStart := time.Now()
	n, err := streamResponseBody(w, resp.Body, r, acc.Name())
	bodyReadElapsed := time.Since(bodyReadStart).Milliseconds()
	if err != nil {
		log.Printf("proxy: req=%s account=%s mode=legacy_stream body_read_error body_ms=%d elapsed=%v", requestID, acc.Name(), bodyReadElapsed, time.Since(start))
		recordError()
		return true, err
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		log.Printf("proxy: req=%s account=%s mode=legacy_stream done status=%d written=%d content-length=%s body_read_ms=%d elapsed=%v", requestID, acc.Name(), resp.StatusCode, n, cl, bodyReadElapsed, time.Since(start))
	} else {
		log.Printf("proxy: req=%s account=%s mode=legacy_stream done status=%d written=%d body_read_ms=%d elapsed=%v", requestID, acc.Name(), resp.StatusCode, n, bodyReadElapsed, time.Since(start))
	}
	recordRequest(time.Since(start))
	return true, nil
}

func proxyChatWithBody(pool *Pool, w http.ResponseWriter, r *http.Request, bodyBytes []byte, start time.Time, opts chatForwardOpts, cfg *Config) {
	bodyBytes = transformRequestBody(bodyBytes, cfg)
	if len(pool.accounts) == 0 {
		writeJSON(w, 503, map[string]any{
			"error": map[string]any{"message": "No accounts configured", "code": "no_accounts"},
		})
		return
	}
	maxAttempts := len(pool.accounts) * 2
	requestID := randomID()
	log.Printf("proxy: req=%s path=%s stream=%v responsesOut=%v totalStart=%s", requestID, r.URL.Path, opts.stream, opts.responsesOut, start.Format(time.RFC3339Nano))

	for attempts := 0; attempts < maxAttempts; attempts++ {
		if attempts > 0 {
			time.Sleep(upstreamRetryDelay)
		}

		selectCtx, cancel := context.WithTimeout(context.Background(), accountSelectTimeout)
		selectStart := time.Now()
		acc, err := pool.Select(selectCtx)
		selectDuration := time.Since(selectStart).Milliseconds()
		cancel()
		accName := "nil"
		if acc != nil {
			accName = acc.Name()
		}
		log.Printf("proxy: req=%s attempt=%d pool.select_done=%dms account=%s err=%v", requestID, attempts, selectDuration, accName, err)
		if err != nil {
			log.Printf("proxy: select account failed: %v", err)
			recordError()
			writeJSON(w, 503, map[string]any{
				"error": map[string]any{"message": "No healthy accounts available", "code": "no_accounts"},
			})
			return
		}

		var terminalDone bool
		var terminalFatalErr error

		func() {
			defer pool.Release(acc)
			res := doUpstreamRequest(acc, r, bodyBytes, opts, requestID)
			if res.resp != nil {
				done, fatalErr := handleUpstreamResponse(acc, w, r, res.resp, bodyBytes, start, opts, requestID, res.ctx, res.cancel)
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
	log.Printf("proxy: req=%s channel=all_exhausted attempts=%d elapsed=%v", requestID, maxAttempts, time.Since(start))
	recordError()
	writeJSON(w, 503, map[string]any{
		"error": map[string]any{"message": "All accounts exhausted after retries", "code": "all_exhausted"},
	})
}

func proxyModels(pool *Pool, w http.ResponseWriter, r *http.Request, cfg *Config) {
	log.Printf("proxy: models request from %s", r.RemoteAddr)

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
	log.Printf("proxy: models returning %d models", len(modelIDs))
}

// redactBody masks common sensitive patterns in error/response bodies for safe
// logging. It first tries JSON-aware redaction (walk the object tree and replace
// sensitive-key values with "***"); if the body is not valid JSON it falls back
// to regex-based redaction of sk-* and Bearer tokens.
func redactBody(body []byte) string {
	return string(redactBodyBytes(body))
}

// redactBodyBytes is the []byte version of redactBody, for direct use in
// response writing without an extra string allocation.
func redactBodyBytes(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	// Try JSON-aware redaction first.
	var parsed any
	if err := json.Unmarshal(body, &parsed); err == nil && parsed != nil {
		parsed = redactJSON(parsed, 0)
		if out, err := json.Marshal(parsed); err == nil {
			return out
		}
	}
	// Fall back to regex-based redaction.
	return []byte(redactBodyRegex(string(body)))
}

// redactBodyRegex applies regex-based redaction: sk-* keys and Bearer tokens.
func redactBodyRegex(s string) string {
	result := reAPIKey.ReplaceAllString(s, "sk-***")
	result = reBearer.ReplaceAllString(result, "Bearer ***")
	return result
}

// redactJSON recursively walks a JSON value and replaces sensitive string
// values with "***". Arrays and nested objects are recursed into.  String
// leaf values also get regex redaction for embedded sk-*/Bearer patterns.
// depth is capped at 20 to prevent stack overflow from malicious nesting.
// Returns the redacted value; when depth exceeds 20 the subtree is replaced
// with "<redacted:too deep>" instead of being silently passed through.
func redactJSON(v any, depth int) any {
	if depth > redactJSONMaxDepth {
		return "<redacted:too deep>"
	}
	switch val := v.(type) {
	case map[string]any:
		for k, vv := range val {
			if sensitiveJSONKeys[strings.ToLower(k)] {
				val[k] = "***"
			} else if s, ok := vv.(string); ok {
				val[k] = redactBodyRegex(s)
			} else {
				val[k] = redactJSON(vv, depth+1)
			}
		}
		return val
	case []any:
		for i, item := range val {
			if s, ok := item.(string); ok {
				val[i] = redactBodyRegex(s)
			} else {
				val[i] = redactJSON(item, depth+1)
			}
		}
		return val
	}
	return v
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
			log.Printf("proxy: model remap %s -> %s", model, remapped)
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
						log.Printf("proxy: thinking level remap model=%s level=%s -> %s", model, level, mapped)
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
					log.Printf("proxy: reasoning_effort remap model=%s effort=%s -> %s", model, effort, mapped)
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
					log.Printf("proxy: stripped field %q for model %s (tiers %v)", field, model, matchedTiers)
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
