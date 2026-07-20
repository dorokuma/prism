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

var (
	reAPIKey = regexp.MustCompile(`sk-[A-Za-z0-9_-]{10,}`)
	reBearer = regexp.MustCompile(`Bearer [A-Za-z0-9_-]{10,}`)
)

func isHopByHop(key string) bool {
	return hopByHopHeaders[http.CanonicalHeaderKey(key)]
}

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
	bodyBytes, _ := io.ReadAll(limitReader)
	defer func() {
		resp.Body.Close()
	}()

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

func upstreamContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), upstreamTimeout)
}

func copyClientHeaders(dst http.Header, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		if http.CanonicalHeaderKey(k) == "Authorization" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

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
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusMethodNotAllowed)
				json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"message": "method not allowed", "code": "method_not_allowed"},
				})
				return
			}
			proxyModels(pool, w, r, cfg)
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			if !wire.allowsLegacy() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"message": "wire_api=responses: /v1/chat/completions disabled", "code": "disabled"},
				})
				return
			}
			proxyChat(pool, w, r, cfg)
			return
		}
		if r.URL.Path == "/v1/responses" {
			if !wire.allowsResponses() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"message": "wire_api=legacy: /v1/responses disabled", "code": "disabled"},
				})
				return
			}
			proxyResponses(pool, w, r, cfg)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{
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
	proxyChatWithBody(pool, w, r, bodyBytes, start, chatForwardOpts{
		tenantID: tenantID,
	}, cfg)
}

func proxyChatWithBody(pool *Pool, w http.ResponseWriter, r *http.Request, bodyBytes []byte, start time.Time, opts chatForwardOpts, cfg *Config) {
	// Remap model name if configured
	bodyBytes = remapModelInBody(bodyBytes, cfg)
	bodyBytes = remapThinkingForDeepSeek(bodyBytes)
	bodyBytes = stripUnsupportedFields(bodyBytes)
	if len(pool.accounts) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "No accounts configured", "code": "no_accounts"},
		})
		return
	}
	maxAttempts := len(pool.accounts) * 2
	requestID := randomID()
	log.Printf("proxy: req=%s path=%s stream=%v responsesOut=%v totalStart=%s", requestID, r.URL.Path, opts.stream, opts.responsesOut, start.Format(time.RFC3339Nano))

	for attempts := 0; attempts < maxAttempts; attempts++ {
		if attempts > 0 {
			time.Sleep(200 * time.Millisecond)
		}

		selectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": "No healthy accounts available", "code": "no_accounts"},
			})
			return
		}

		done, streamErr := func() (bool, error) {
			defer pool.Release(acc)

			ctx, cancel := upstreamContext(r)
			defer cancel()

			targetURL := acc.BaseURL() + "/chat/completions"
			if r.URL.RawQuery != "" {
				targetURL += "?" + r.URL.RawQuery
			}

			req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(bodyBytes))
			if err != nil {
				log.Printf("proxy: failed to create request for %s: %v", acc.Name(), err)
				recordUpstreamRetry()
				return false, nil
			}
			copyClientHeaders(req.Header, r.Header)
			req.Header.Set("Authorization", "Bearer "+acc.Key())
			req.Header.Set("Content-Type", "application/json")

			resp, err := acc.Client().Do(req)
			if err != nil {
				// If client disconnected, don't retry
				if r.Context().Err() != nil {
					log.Printf("proxy: req=%s client disconnected, aborting retry", requestID)
					recordError()
					return true, fmt.Errorf("client disconnected: %w", r.Context().Err())
				}
				acc.SetCooldown(30 * time.Second)
				log.Printf("proxy: chat retry via %s (upstream connection error), cooling down 30s: %v", acc.Name(), err)
				recordUpstreamRetry()
				return false, nil
			}
			defer resp.Body.Close()

			if resp.StatusCode == 429 || resp.StatusCode == 402 || resp.StatusCode == 401 || resp.StatusCode == 403 {
				handleUpstreamError(acc, resp)
				recordUpstreamRetry()
				return false, nil
			}

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				recordRequest(time.Since(start))
			} else {
				recordError()
				acc.SetCooldown(30 * time.Second)
				log.Printf("proxy: %s non-2xx error (status=%d), cooling down 30s", acc.Name(), resp.StatusCode)
				// Still forward the error to client this time
				errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				log.Printf("proxy: chat upstream error via %s, status=%d, body=%s", acc.Name(), resp.StatusCode, redactBody(errBody))
				copyUpstreamHeaders(w, resp.Header)
				w.WriteHeader(resp.StatusCode)
				n, _ := w.Write(errBody)
				log.Printf("proxy: chat upstream error via %s, written=%d", acc.Name(), n)
				return true, nil
			}

			if opts.responsesOut && opts.stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.WriteHeader(http.StatusOK)
				translateStart := time.Now()
				log.Printf("proxy: req=%s account=%s mode=responses_stream translate.start=%s", requestID, acc.Name(), translateStart.Format(time.RFC3339Nano))
				err = translateChatStreamToResponses(w, resp.Body, opts.model, opts.reqTools, getSearchToolCache(opts.tenantID))
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
					log.Printf("proxy: req=%s account=%s mode=responses_json translate.error translate_ms=%d body_ms=%d elapsed=%v", requestID, acc.Name(), translateElapsed, bodyReadElapsed, time.Since(start))
					recordError()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(rawBody)
					recordRequest(time.Since(start))
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
		}()
		if done {
			if streamErr != nil {
				return
			}
			return
		}
	}
	log.Printf("proxy: req=%s channel=all_exhausted attempts=%d elapsed=%v", requestID, maxAttempts, time.Since(start))
	recordError()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(503)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": "All accounts exhausted after retries", "code": "all_exhausted"},
	})
}

func proxyModels(pool *Pool, w http.ResponseWriter, r *http.Request, cfg *Config) {
	log.Printf("proxy: models request from %s", r.RemoteAddr)

	modelIDs := cfg.AllModels()
	if len(modelIDs) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		json.NewEncoder(w).Encode(map[string]any{
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
	log.Printf("proxy: models returning %d models", len(modelIDs))
}

// redactBody masks common sensitive patterns in error response bodies for safe logging.
func redactBody(body []byte) string {
	s := string(body)
	result := reAPIKey.ReplaceAllString(s, "sk-***")
	result = reBearer.ReplaceAllString(result, "Bearer ***")
	return result
}

// remapModelInBody replaces the model field in a JSON chat completions body.
func remapModelInBody(body []byte, cfg *Config) []byte {
	if cfg == nil {
		return body
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	model, ok := rawStringField(raw, "model")
	if !ok || model == "" {
		return body
	}
	remapped := cfg.RemapModel(model)
	if remapped == model {
		return body
	}
	rawBytes, marshalErr := json.Marshal(remapped)
	if marshalErr != nil {
		return body
	}
	raw["model"] = json.RawMessage(rawBytes)
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	log.Printf("proxy: model remap %s -> %s", model, remapped)
	return out
}

// stripUnsupportedFields removes fields from the request body that are not
// supported by certain upstream providers. Currently handles:
//   - prompt_cache_retention: not supported by GLM/z-ai upstream ("Error from
//     provider: Extra inputs are not permitted, field: 'prompt_cache_retention'")
func stripUnsupportedFields(body []byte) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}

	model, _ := rawStringField(raw, "model")
	modelLower := strings.ToLower(model)

	// Only GLM/z-ai upstream is known to reject prompt_cache_retention;
	// other models pass through unchanged.
	if !strings.Contains(modelLower, "glm") && !strings.Contains(modelLower, "z-ai") {
		return body
	}
	if _, ok := raw["prompt_cache_retention"]; !ok {
		return body
	}

	delete(raw, "prompt_cache_retention")
	log.Printf("proxy: stripped prompt_cache_retention for model %s", model)
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
