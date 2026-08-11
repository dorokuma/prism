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

func proxyChat(p *pool.Pool, w http.ResponseWriter, r *http.Request, cfg *config.Config) {
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
		aud.Status = sc.Code
		aud.Success = sc.Code >= 200 && sc.Code < 300 && aud.ErrorType == ""
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
	// Transform normally runs for every request; model remap inside is still
	// gated by cfg.ModelRemapEnabled (real model name passes through when
	// disabled). The /v1/messages surface (anthropic body, not a chat
	// completion) opts out via SkipSanitize: the body must pass through
	// byte-for-byte.
	if !opts.SkipSanitize {
		bodyBytes = sanitize.TransformRequestBodyForProvider(bodyBytes, cfg, provider)
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
	maxConcurrent := config.ResolveMaxConcurrent(opts.Model, cfg)
	if opts.MaxResponseBytes == 0 {
		opts.MaxResponseBytes = cfg.MaxUpstreamResponseBytes
	}
	slog.Debug("proxy request start", "request_id", requestID, "path", r.URL.Path, "stream", opts.Stream, "responses_out", opts.ResponsesOut, "start", start.Format(time.RFC3339Nano), "max_concurrent", maxConcurrent)

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
		acc, err := p.SelectByProvider(selectCtx, maxConcurrent, provider)
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

		func() {
			defer p.Release(acc)
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
	util.RecordError()
	aud.Error = "all accounts exhausted after retries"
	aud.ErrorType = "all_exhausted"
	util.WriteJSON(sc, 503, map[string]any{
		"error": map[string]any{"message": "All accounts exhausted after retries", "code": "all_exhausted"},
	})
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
