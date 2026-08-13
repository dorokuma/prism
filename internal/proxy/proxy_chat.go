package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/sanitize"
	"github.com/dorokuma/prism/internal/usagemeta"
	"github.com/dorokuma/prism/internal/util"
)

// ChatForwardOpts holds options for forwarding a chat request to the upstream.
type ChatForwardOpts struct {
	ResponsesOut bool
	Stream       bool
	Model        string
	ReqTools     json.RawMessage
	TenantID     string

	// UpstreamPath is the upstream POST path; empty means
	// "/chat/completions" (backward compatible with all existing callers).
	UpstreamPath string

	// MaxResponseBytes caps the non-streaming upstream response body size
	// (both the legacy chat path and the responses translation path). Zero
	// → config default 32 MiB (see config.MaxUpstreamResponseBytes).
	// proxyChatWithBody fills it from cfg.
	MaxResponseBytes int64

	// SkipSanitize skips sanitize.TransformRequestBodyForProvider. It must be
	// true for the anthropic /v1/messages surface whose body is NOT a chat
	// completion body (remap/effort/strip would corrupt it).
	SkipSanitize bool
}

// maxRequestBodyBytes caps the client request body read for the three POST
// surfaces (/v1/chat/completions, /v1/responses, /v1/messages). Bodies over
// the cap are rejected with HTTP 413 request_too_large instead of being
// buffered whole into memory.
const maxRequestBodyBytes = 10 << 20

// ensureStreamOptionsIncludeUsage returns body with
// stream_options.include_usage=true, preserving every other client-supplied
// stream_options field (OpenAI chat-completions semantics). It is a no-op
// when the body is not valid JSON or already sets include_usage=true. A
// client stream_options that is not a JSON object is replaced by the usage
// request (the invalid value could not reach the upstream meaningfully
// anyway). Usage recording is the only reason this runs: without it, an
// OpenAI-compatible upstream stream would never report usage to the audit
// and usage store.
func ensureStreamOptionsIncludeUsage(body []byte) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	var so map[string]any
	if v, ok := raw["stream_options"]; ok && len(v) > 0 && string(v) != "null" {
		if err := json.Unmarshal(v, &so); err != nil || so == nil {
			so = nil // non-object stream_options: replace below
		} else if included, ok := so["include_usage"].(bool); ok && included {
			return body // already ensured — keep the body byte-identical
		}
	}
	if so == nil {
		so = map[string]any{"include_usage": true}
	} else {
		so["include_usage"] = true
	}
	out, err := json.Marshal(so)
	if err != nil {
		return body
	}
	raw["stream_options"] = json.RawMessage(out)
	if re, err := json.Marshal(raw); err == nil {
		return re
	}
	return body
}

// rejectAudit emits exactly ONE request.complete audit line for a request
// rejected BEFORE the forwarding path (body read / conversion / validation
// failures). It is the single early-rejection audit path shared by
// proxyChat / proxyResponses / proxyMessages (via readRequestBody and the
// responses conversion failure), so every surface records the same fields:
// status, error_type, model (empty when the body could not be parsed) and
// request_id. The forwarding path (proxyChatWithBody) emits its own audit in
// a defer, so a request is audited exactly once — never zero, never twice.
func rejectAudit(r *http.Request, start time.Time, status int, errorType, model, errMsg string) {
	middleware.EmitAudit(&middleware.RequestAudit{
		Req:        util.RequestIDFromCtx(r.Context()),
		Method:     r.Method,
		Path:       r.URL.Path,
		Model:      model,
		Status:     status,
		Error:      errMsg,
		ErrorType:  errorType,
		Success:    false,
		DurationMs: float64(time.Since(start).Milliseconds()),
	})
}

// readRequestBody reads and validates the client request body. It is the
// single implementation shared by proxyChat / proxyResponses / proxyMessages:
//   - body over maxRequestBodyBytes → HTTP 413, Content-Type
//     application/json, error.code request_too_large (via
//     http.MaxBytesReader, which surfaces *http.MaxBytesError);
//   - any other read error → HTTP 400, Content-Type application/json,
//     error.code invalid_request.
//
// On error it writes the JSON envelope, emits the request.complete audit for
// the early rejection (exactly one, via rejectAudit) and returns (nil,
// false); on success it returns (body, true) WITHOUT auditing — the
// forwarding path owns the audit then. start is the request's real start
// time (captured by the caller before the read): the rejection audit must
// report the true duration (a slow body read is part of the request), not a
// time.Now() taken at rejection time, which would always be ~0.
func readRequestBody(w http.ResponseWriter, r *http.Request, start time.Time, what string) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			slog.Warn(what+" body too large", "error", err, "max_bytes", maxRequestBodyBytes)
			util.WriteJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error": map[string]any{"message": "request body too large", "code": "request_too_large"},
			})
			rejectAudit(r, start, http.StatusRequestEntityTooLarge, "request_too_large", "", err.Error())
			return nil, false
		}
		slog.Error(what+" body read error", "error", err)
		util.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "failed to read body", "code": "invalid_request"},
		})
		rejectAudit(r, start, http.StatusBadRequest, "invalid_request", "", err.Error())
		return nil, false
	}
	return body, true
}

func proxyChat(p *pool.Pool, w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	start := time.Now()
	defer r.Body.Close()
	bodyBytes, ok := readRequestBody(w, r, start, "chat")
	if !ok {
		return
	}
	tenantID := getTenantID(r)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(bodyBytes, &raw)
	stream := util.RawBoolField(raw, "stream")
	model, _ := util.RawStringField(raw, "model")
	proxyChatWithBody(p, w, r, bodyBytes, start, ChatForwardOpts{
		Stream:   stream,
		Model:    model,
		TenantID: tenantID,
	}, cfg)
}

// proxyChatWithBody is the core request forwarding logic shared by both
// /v1/chat/completions and /v1/responses handlers.
func proxyChatWithBody(p *pool.Pool, w http.ResponseWriter, r *http.Request, bodyBytes []byte, start time.Time, opts ChatForwardOpts, cfg *config.Config) {
	requestID := util.RequestIDFromCtx(r.Context())
	aud := &middleware.RequestAudit{
		Req:    requestID,
		Method: r.Method,
		Path:   r.URL.Path,
		Model:  opts.Model,
	}
	r = r.WithContext(context.WithValue(r.Context(), middleware.AuditKey{}, aud))
	sc := &middleware.StatusCapture{ResponseWriter: w}

	defer func() {
		aud.DurationMs = float64(time.Since(start).Milliseconds())
		// The audit status must be a REAL status even when nothing could be
		// written: a client that disconnected before the first response byte
		// leaves sc.Code at 0, which would record a bare 0 in the audit (and
		// a success=false line with no error_type). 499 (nginx's de-facto
		// "client closed request") is recorded instead whenever the client
		// context is gone and no status was committed (audit round 6, item
		// 7). Every other path keeps the committed status.
		if sc.Code == 0 && r.Context().Err() != nil {
			aud.Status = 499
		} else {
			aud.Status = sc.Code
		}
		aud.Success = aud.Status >= 200 && aud.Status < 300 && aud.ErrorType == ""
		middleware.EmitAudit(aud)
	}()

	// Read the upstream provider up front so it can be reused both for the
	// effort-mapping transform and for account selection (SelectByProvider).
	// It selects the effort schema (opencode vs ollama).
	provider := r.Header.Get("X-Prism-Provider")
	if provider == "" {
		if cfg.DefaultProvider != "" {
			// Config-driven fallback: route through the default provider's
			// normal per-provider round-robin.
			provider = cfg.DefaultProvider
		} else {
			// No header and no default → reject. Never fall back to whole-pool
			// selection (that could route an account to the wrong provider).
			aud.Error = "missing X-Prism-Provider header"
			aud.ErrorType = "missing_provider"
			slog.Warn("request rejected: missing X-Prism-Provider header", "request_id", requestID, "path", r.URL.Path)
			util.WriteJSON(sc, 400, map[string]any{
				"error": map[string]any{
					"message": "missing X-Prism-Provider header",
					"type":    "invalid_request_error",
				},
			})
			return
		}
	}
	// Record the effective provider, stream flag and authenticated key name
	// once provider resolution is done. KeyID is the API key NAME from the
	// auth middleware — tokens are never stored or logged.
	aud.Provider = provider
	aud.Stream = opts.Stream
	aud.KeyID = middleware.APIKeyFromContext(r.Context())
	// Traceability for model remap: when remap actually resolves the model to
	// a DIFFERENT upstream name, the audit keeps both the client-requested
	// (virtual) model (Model) and the resolved upstream model
	// (UpstreamModel), so per-model accounting and pricing (which prefers
	// the upstream model — see the wiring-stage Price) never lose either
	// side. When the model passes through unchanged (remap disabled, or the
	// model is not in model_remap) UpstreamModel stays empty — the audit
	// line then carries only the requested model, matching the field
	// comment and the omitempty JSON tag.
	if cfg.ModelRemapEnabled {
		if resolved := cfg.RemapModel(opts.Model); resolved != opts.Model {
			aud.UpstreamModel = resolved
		}
	}
	// Transform normally runs for every request; model remap inside is still
	// gated by cfg.ModelRemapEnabled (real model name passes through when
	// disabled). The /v1/messages surface (anthropic body, not a chat
	// completion) opts out via SkipSanitize: the body must pass through
	// byte-for-byte.
	if !opts.SkipSanitize {
		bodyBytes = sanitize.TransformRequestBodyForProvider(bodyBytes, cfg, provider)
	}
	// Usage-enabled OpenAI-compatible streaming: ensure the upstream reports
	// usage in the stream (stream_options.include_usage=true) so the audit
	// and usage store capture tokens. The client's other stream_options
	// fields are preserved; the Anthropic /v1/messages surface
	// (SkipSanitize) is never touched — stream_options is an OpenAI field
	// and Anthropic must not be modified.
	if opts.Stream && cfg.Usage.Enabled && !opts.SkipSanitize {
		bodyBytes = ensureStreamOptionsIncludeUsage(bodyBytes)
	}
	if p.AccountCount() == 0 {
		aud.Error = "no accounts configured"
		aud.ErrorType = "config_error"
		util.WriteJSON(sc, 503, map[string]any{
			"error": map[string]any{"message": "No accounts configured", "code": "no_accounts"},
		})
		return
	}
	maxAttempts := p.AccountCount() * 2
	// The concurrency key is the client-requested model — the SAME string
	// ResolveMaxConcurrent looks up — so per-model counters and per-model
	// caps stay aligned 1:1 (two models with the same max never share a
	// counter), and the account-wide total cap bounds the sum. The key is
	// stable across reloads: changing a model's max value does not reset or
	// split its in-flight accounting.
	maxConcurrent := config.ResolveMaxConcurrent(opts.Model, cfg)
	if opts.MaxResponseBytes == 0 {
		opts.MaxResponseBytes = cfg.MaxUpstreamResponseBytes
	}
	slog.Debug("proxy request start", "request_id", requestID, "path", r.URL.Path, "stream", opts.Stream, "responses_out", opts.ResponsesOut, "start", start.Format(time.RFC3339Nano), "max_concurrent", maxConcurrent)

	// sawCredential / sawQuota ACCUMULATE the upstream permanent failure
	// classes seen across ALL accounts during the retry loop (item: multi-
	// account permanent errors must not record only the last class). When
	// every account is exhausted, the terminal response must distinguish the
	// real cause from a generic 503 all_exhausted: the gateway's own
	// credentials or balance are broken — the operator, not the client,
	// must fix them.
	var sawCredential, sawQuota, sawEmptyStream bool

	for attempts := 0; attempts < maxAttempts; attempts++ {
		if attempts > 0 {
			time.Sleep(config.UpstreamRetryDelay)
		}

		// The select timeout is bound to r.Context(): a client disconnect
		// cancels the wait immediately (client_canceled) instead of parking
		// the request until the fixed select timeout expires, while the
		// timeout still applies for clients that stay connected.
		selectCtx, cancel := context.WithTimeout(r.Context(), config.AccountSelectTimeout)
		selectStart := time.Now()
		acc, slot, err := p.SelectByProvider(selectCtx, opts.Model, maxConcurrent, provider)
		selectDuration := time.Since(selectStart).Milliseconds()
		cancel()
		accName := "nil"
		if acc != nil {
			accName = acc.Name()
		}
		slog.Debug("pool select done", "request_id", requestID, "attempt", attempts, "select_ms", selectDuration, "account", accName, "error", err)
		if err != nil {
			slog.Error("select account failed", "error", err)
			util.RecordError()
			// When every account was permanently rejected by the upstream
			// (401/402 or recognized credential/quota errors), a generic
			// select failure (no_healthy) would masquerade the real cause:
			// the gateway's own credentials or balance are broken. Report
			// the upstream auth/balance failure instead. Other select
			// failures (timeout, client cancel) are reported as before.
			if errors.Is(err, pool.ErrNoHealthyAccounts) && (sawCredential || sawQuota) {
				writeUpstreamExhausted(sc, aud, sawCredential, sawQuota)
				return
			}
			if errors.Is(err, pool.ErrNoHealthyAccounts) && sawEmptyStream {
				writeEmptyStreamExhausted(sc, aud)
				return
			}
			aud.Error = err.Error()
			// Classify select failures with errors.Is (wrapping-safe) into
			// four distinct response codes — no_healthy / select_timeout /
			// client_canceled / select_failed — while the HTTP status stays
			// 503. This replaces the old single "no_accounts" code for the
			// select path (README records the compat change).
			code := "select_failed"
			aud.ErrorType = "select_failed"
			switch {
			case errors.Is(err, pool.ErrNoHealthyAccounts):
				code = "no_healthy"
				aud.ErrorType = "no_healthy"
			case errors.Is(err, pool.ErrSelectTimeout) || errors.Is(err, context.DeadlineExceeded):
				code = "select_timeout"
				aud.ErrorType = "select_timeout"
			case errors.Is(err, context.Canceled):
				code = "client_canceled"
				aud.ErrorType = "client_canceled"
			}
			util.WriteJSON(sc, 503, map[string]any{
				"error": map[string]any{"message": "No healthy accounts available", "code": code},
			})
			return
		}

		// Record the last attempted account for audit, even if the upstream
		// request later fails.
		aud.Account = acc.Name()
		aud.Concurrency = acc.InFlightCount()

		var terminalDone bool
		var terminalFatalErr error
		var connFatal bool

		func() {
			// Release the acquisition's own lease: slot carries the exact
			// account/key/max, so the release can never mismatch the acquire
			// (the old Release(acc, max) let a wrong max corrupt the
			// accounting). On a retry the slot is freed before the next
			// attempt acquires a new one.
			defer p.Release(slot)
			res := doUpstreamRequest(acc, r, bodyBytes, opts, requestID)
			if res.resp != nil {
				done, fatalErr, upClass := handleUpstreamResponse(acc, sc, r, res.resp, bodyBytes, start, opts, requestID, res.ctx, res.cancel)
				if done {
					terminalDone = true
					terminalFatalErr = fatalErr
					return
				}
				// Retryable upstream failure: accumulate the permanent
				// rejection classes (credential and quota tracked
				// independently — the last account's class must not hide an
				// earlier account's different failure).
				if upClass == UpstreamErrorPermanentCredential {
					sawCredential = true
				} else if upClass == UpstreamErrorPermanentQuota {
					sawQuota = true
				} else if upClass == UpstreamErrorEmptyStream {
					sawEmptyStream = true
				}
				return
			}
			if res.retry {
				return
			}
			terminalDone = true
			terminalFatalErr = res.fatalErr
			connFatal = true
		}()

		if terminalDone {
			if terminalFatalErr != nil {
				// Client gone: no response can be written, but the audit
				// must record a real status instead of a bare 0 (audit
				// round 6, item 7): 499 is the de-facto "client closed
				// request" status (nginx convention).
				if r.Context().Err() != nil {
					aud.Status = 499
					aud.ErrorType = "client_disconnect"
					// Never store terminalFatalErr.Error(): after a
					// non-retryable Do() failure it is the raw *url.Error
					// and embeds the upstream URL (query/userinfo).
					aud.Error = "client disconnected"
					return
				}
				if connFatal {
					// Non-retryable upstream transport error (timeout /
					// reset / EOF / other) before any HTTP response: the
					// request may already have reached the upstream, so
					// we do not switch accounts. Write a structured 502
					// instead of an empty response. Errors returned after
					// handleUpstreamResponse already wrote (or committed)
					// the client body must not write a second envelope.
					errType := util.ClassifyConnError(terminalFatalErr)
					aud.Status = http.StatusBadGateway
					aud.ErrorType = errType
					// The raw *url.Error embeds the upstream URL; never store it.
					aud.Error = "upstream connection failed"
					util.WriteJSON(sc, http.StatusBadGateway, map[string]any{
						"error": map[string]any{"message": "upstream connection failed", "code": errType},
					})
				}
				return
			}
			return
		}
	}
	slog.Error("all accounts exhausted after retries", "request_id", requestID, "attempts", maxAttempts, "elapsed", time.Since(start))
	util.RecordError()
	// Distinguish the real cause when upstreams PERMANENTLY rejected every
	// account: a plain 503 all_exhausted would masquerade the gateway's own
	// broken credential (502 upstream_auth_failed) or exhausted quota/balance
	// (503 upstream_quota_exhausted) as a generic availability problem. The
	// failover across accounts is unchanged; only the terminal response and
	// audit classify the outcome.
	if sawCredential || sawQuota {
		writeUpstreamExhausted(sc, aud, sawCredential, sawQuota)
		return
	}
	if sawEmptyStream {
		writeEmptyStreamExhausted(sc, aud)
		return
	}
	aud.Error = "all accounts exhausted after retries"
	aud.ErrorType = "all_exhausted"
	util.WriteJSON(sc, 503, map[string]any{
		"error": map[string]any{"message": "All accounts exhausted after retries", "code": "all_exhausted"},
	})
}

// writeEmptyStreamExhausted writes the terminal 502 when every retry saw
// an uncommitted empty upstream SSE and nothing else produced a response.
func writeEmptyStreamExhausted(w http.ResponseWriter, aud *middleware.RequestAudit) {
	aud.Error = "upstream stream closed before any event"
	aud.ErrorType = "upstream_stream_error"
	util.WriteJSON(w, http.StatusBadGateway, map[string]any{
		"error": map[string]any{"message": "upstream stream closed before any event", "code": "upstream_stream_error"},
	})
}

// writeUpstreamExhausted writes the terminal response and audit fields for
// the case where every upstream account was permanently rejected. The
// accumulated classes decide the outcome with a FIXED priority (independent
// of account order):
//   - credential failures win over quota exhaustion → 502 upstream_auth_failed.
//     Auth is the most diagnostic signal: broken keys make every other fix
//     (topping up quota, client retries) useless until the keys are replaced,
//     and 502 keeps a gateway-side credential problem distinct from both
//     client errors and availability issues.
//   - otherwise quota/balance exhaustion (any 402 or recognized structured
//     quota error) → 503 upstream_quota_exhausted.
//
// It is the single write point used by the select-failure path (all accounts
// exhausted → no healthy account) and the retry-loop exhaustion path, and it
// reuses the existing dedicated error codes — no new public error code is
// introduced.
func writeUpstreamExhausted(w http.ResponseWriter, aud *middleware.RequestAudit, sawCredential, sawQuota bool) {
	switch {
	case sawCredential:
		aud.Error = "all upstream accounts failed authentication"
		aud.ErrorType = "upstream_auth_failed"
		util.WriteJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "All upstream accounts failed authentication", "code": "upstream_auth_failed"},
		})
	case sawQuota:
		aud.Error = "all upstream accounts exhausted quota or balance"
		aud.ErrorType = "upstream_quota_exhausted"
		util.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "All upstream accounts exhausted quota or balance", "code": "upstream_quota_exhausted"},
		})
	}
}

// resolveMaxConcurrent moved to config.ResolveMaxConcurrent — the single
// concurrency-resolution implementation shared with the model cache fetch
// path (cache cannot import proxy). See internal/config/config.go.

// parseUsageFromChatCompletion extracts input/output token counts from a raw
// chat completion response body (non-streaming). Returns 0, 0 when the body
// cannot be parsed or the usage field is absent. It is a thin wrapper over
// the shared OpenAI-form parser (usagemeta.ParseOpenAI) — there is no second
// parallel parsing implementation. Callers that need the full field set use
// parseUsageForResponseBody + middleware.RequestAudit.ApplyUsage instead.
func parseUsageFromChatCompletion(body []byte) (tokensIn, tokensOut int) {
	u := usagemeta.ParseOpenAI(body)
	return u.Prompt, u.Completion
}

// parseUsageForResponseBody selects the usage parser for a non-streaming
// upstream response body by the upstream path the request was sent to:
// /v1/messages (Anthropic Messages API) returns input_tokens/output_tokens/
// cache_read_input_tokens/cache_creation_input_tokens, every other upstream
// returns the OpenAI prompt_tokens/completion_tokens form. Path-based
// selection is correct because the upstream path determines the wire
// format: /v1/messages always speaks Anthropic, /chat/completions and
// /v1/responses always speak OpenAI.
func parseUsageForResponseBody(body []byte, opts ChatForwardOpts) usagemeta.Usage {
	if opts.UpstreamPath == "/v1/messages" {
		return usagemeta.ParseAnthropic(body)
	}
	return usagemeta.ParseOpenAI(body)
}

// ProxyChatWithBody is an exported wrapper around proxyChatWithBody for use by
// the root package's test shims. Root tests pass through this entry point.
func ProxyChatWithBody(p *pool.Pool, w http.ResponseWriter, r *http.Request, bodyBytes []byte, start time.Time, opts ChatForwardOpts, cfg *config.Config) {
	proxyChatWithBody(p, w, r, bodyBytes, start, opts, cfg)
}
