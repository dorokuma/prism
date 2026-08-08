package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/convert"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/sanitize"
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
	maxConcurrent := resolveMaxConcurrent(opts.Model, cfg)
	slog.Debug("proxy request start", "request_id", requestID, "path", r.URL.Path, "stream", opts.Stream, "responses_out", opts.ResponsesOut, "start", start.Format(time.RFC3339Nano), "max_concurrent", maxConcurrent)

	for attempts := 0; attempts < maxAttempts; attempts++ {
		if attempts > 0 {
			time.Sleep(config.UpstreamRetryDelay)
		}

		selectCtx, cancel := context.WithTimeout(context.Background(), config.AccountSelectTimeout)
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
			if err == pool.ErrNoHealthyAccounts {
				aud.ErrorType = "no_healthy"
			} else if err == pool.ErrSelectTimeout {
				aud.ErrorType = "select_timeout"
			} else {
				aud.ErrorType = "select_failed"
			}
			util.WriteJSON(sc, 503, map[string]any{
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

// resolveMaxConcurrent returns the maximum concurrent requests per account
// for the given model. Resolution order:
//  1. cfg.MaxConcurrentPerAccount[model]  (exact match)
//  2. cfg.MaxConcurrentPerAccount["*"]   (wildcard default)
//  3. model name contains "flash" → deepseekV4FlashConcurrency * defaultConcurrencyRatio / 100
//  4. model name contains "pro"   → deepseekV4ProConcurrency * defaultConcurrencyRatio / 100
//  5. fallback: deepseekV4ProConcurrency * defaultConcurrencyRatio / 100 + warn
func resolveMaxConcurrent(model string, cfg *config.Config) int {
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
		return config.DeepseekV4FlashConcurrency * config.DefaultConcurrencyRatio / 100
	}
	if strings.Contains(modelLower, "pro") {
		return config.DeepseekV4ProConcurrency * config.DefaultConcurrencyRatio / 100
	}
	// Default for unknown models
	slog.Warn("unknown model, using default concurrency", "model", model)
	return config.DeepseekV4ProConcurrency * config.DefaultConcurrencyRatio / 100
}

// parseUsageFromChatCompletion extracts input/output token counts from a raw
// chat completion response body (non-streaming).  Returns 0, 0 when the body
// cannot be parsed or the usage field is absent.
func parseUsageFromChatCompletion(body []byte) (tokensIn, tokensOut int) {
	var comp convert.ChatCompletionResponse
	if err := json.Unmarshal(body, &comp); err != nil || comp.Usage == nil {
		return 0, 0
	}
	return comp.Usage.PromptTokens, comp.Usage.CompletionTokens
}

// ProxyChatWithBody is an exported wrapper around proxyChatWithBody for use by
// the root package's test shims. Root tests pass through this entry point.
func ProxyChatWithBody(p *pool.Pool, w http.ResponseWriter, r *http.Request, bodyBytes []byte, start time.Time, opts ChatForwardOpts, cfg *config.Config) {
	proxyChatWithBody(p, w, r, bodyBytes, start, opts, cfg)
}
