package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/convert"
	"github.com/dorokuma/prism/internal/dsml"
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

// upstreamCooldown is the cooldown applied to an account after a temporary
// upstream failure (5xx, connection error, 429 without Retry-After). It is a
// variable (not a const) so tests can shrink it to milliseconds via
// SetUpstreamCooldownForTest; the default 30s is unchanged production
// behavior.
var upstreamCooldown = 30 * time.Second

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
	http.CanonicalHeaderKey("Cookie"):         true,
	http.CanonicalHeaderKey("X-Api-Key"):      true,
	http.CanonicalHeaderKey("X-Auth-Token"):   true,
	http.CanonicalHeaderKey("Api-Key"):        true,
	http.CanonicalHeaderKey("X-Goog-Api-Key"): true,
}

// spoofableClientHeaders are client-supplied forwarding headers that must
// NEVER reach the upstream: a downstream gateway or the upstream itself
// would trust them (or echo them back), letting a client fake its origin IP
// or routing. Normal business headers still pass through.
var spoofableClientHeaders = map[string]bool{
	http.CanonicalHeaderKey("Forwarded"): true,
	http.CanonicalHeaderKey("X-Real-IP"): true,
}

func isHopByHop(key string) bool {
	return hopByHopHeaders[http.CanonicalHeaderKey(key)]
}

// IsPermanentCredentialError checks if the response body indicates a permanent
// credential error (invalid_api_key, revoked, account_deactivated).
func IsPermanentCredentialError(body []byte) bool {
	return pool.IsPermanentCredentialBody(body)
}

// IsQuotaError checks if the response body indicates a permanent quota
// error via the structured error envelope only. Shared with the probe
// loop (pool.IsQuotaErrorBody): error.code "insufficient_quota" /
// "insufficient_user_quota", error.type "gousagelimiterror", or
// error.message containing "pre-consume quota failed" (AgentRouter
// new_api_error; Anthropic /v1/messages often omits error.code).
// Broad substring matching on the raw body is deliberately not used — a
// plain-text "quota exceeded" message on a 429 is a temporary rate limit
// and must go to cooldown, not exhaustion.
func IsQuotaError(body []byte) bool {
	return pool.IsQuotaErrorBody(body)
}

// UpstreamErrorClass classifies an upstream HTTP error for account-state
// decisions. It is the single classification point shared by the runtime
// error handler (handleUpstreamError) and the startup health check in
// cmd/prism, so the two paths cannot diverge again.
type UpstreamErrorClass int

const (
	// UpstreamErrorTemporary: transient failure — cooldown, never exhaustion.
	UpstreamErrorTemporary UpstreamErrorClass = iota
	// UpstreamErrorPermanentCredential: 401/402 by status, or a recognized
	// structured permanent credential error (invalid_api_key / revoked /
	// account_deactivated) — mark the account exhausted.
	UpstreamErrorPermanentCredential
	// UpstreamErrorPermanentQuota: recognized structured quota exhaustion
	// (insufficient_quota / insufficient_user_quota / gousagelimiterror /
	// AgentRouter "pre-consume quota failed") — mark the account exhausted.
	UpstreamErrorPermanentQuota
)

// ClassifyUpstreamError is the single classification point for how an
// upstream HTTP error affects account state: only the statuses 401/402 or a
// recognized structured permanent error body (credential or quota) mark an
// account exhausted; a bare 403 (no recognized envelope) is deliberately NOT
// permanent and classifies as temporary. 401 is a credential (auth) failure
// and 402 is a payment/balance failure — the two are kept distinct so the
// terminal response can tell "upstream auth failed" (502
// upstream_auth_failed) apart from "upstream quota/balance exhausted" (503
// upstream_quota_exhausted). Consumers apply that class differently,
// matching the real code:
//   - Runtime (handleUpstreamResponse 4xx branch): a bare 403 is passed
//     through to the client with its original status and redacted body —
//     no cooldown, no retry. A 403 carrying a recognized structured
//     permanent credential/quota body exhausts the account and is treated
//     like 401: the 403 is not written to the client, and the caller
//     selects another account.
//   - Startup probe (cmd/prism initial health check): a bare 403 is treated
//     as a temporary failure and the account is cooled down (5 minutes, or
//     2 minutes for 429) without being exhausted.
func ClassifyUpstreamError(statusCode int, body []byte) UpstreamErrorClass {
	switch {
	case statusCode == 401:
		return UpstreamErrorPermanentCredential
	case statusCode == 402:
		// Payment Required: the account's balance/payment is the problem,
		// not the credential — classify as quota (money) exhaustion.
		return UpstreamErrorPermanentQuota
	case IsPermanentCredentialError(body):
		return UpstreamErrorPermanentCredential
	case IsQuotaError(body):
		return UpstreamErrorPermanentQuota
	default:
		return UpstreamErrorTemporary
	}
}

// ErrUpstreamResponseTooLarge is returned when the upstream response body
// exceeds the configured cap (max_upstream_response_bytes, default 32 MiB).
// Callers map it to HTTP 502 with code response_too_large. The sentinel and
// the read helper live in internal/util (util.ErrBodyTooLarge /
// util.ReadBodyLimited) so internal/cache can share them without importing
// proxy; this name is kept for proxy callers.
var ErrUpstreamResponseTooLarge = util.ErrBodyTooLarge

// ErrInvalidResponseCap is returned for an invalid maxBytes (<= 0, or
// math.MaxInt64 where the max+1 over-limit probe would overflow int64). The
// read is rejected outright instead of degrading to an unbounded io.ReadAll.
// It is a programmatic-caller guard: production always passes the configured
// cap (LoadConfig bounds max_upstream_response_bytes to the default..256 MiB
// range), and unlike ErrUpstreamResponseTooLarge it is not mapped to HTTP
// 502 response_too_large. Alias of util.ErrInvalidBodyCap.
var ErrInvalidResponseCap = util.ErrInvalidBodyCap

// readResponseBodyLimited is the proxy-side wrapper of util.ReadBodyLimited:
// reads at most maxBytes+1 bytes so an over-limit body is detected instead
// of being silently truncated (ErrUpstreamResponseTooLarge); invalid caps
// are rejected (ErrInvalidResponseCap) — never an unbounded read.
func readResponseBodyLimited(body io.Reader, maxBytes int64) ([]byte, error) {
	return util.ReadBodyLimited(body, maxBytes)
}

// responseBodyCap returns the non-streaming response body cap for opts: the
// caller-set cap when present, otherwise the configured default (32 MiB).
// proxyChatWithBody fills opts.MaxResponseBytes from cfg, so production
// always uses the configured value; the fallback only guards direct test
// construction of ChatForwardOpts.
func responseBodyCap(opts ChatForwardOpts) int64 {
	if opts.MaxResponseBytes > 0 {
		return opts.MaxResponseBytes
	}
	return config.MaxUpstreamResponseBytesDefault
}

// maxRetryAfterSeconds caps a delta-seconds Retry-After value before it is
// converted to a time.Duration: the caller caps the final cooldown at 5
// minutes anyway, and the cap keeps the duration arithmetic overflow-free
// for arbitrarily large header values.
const maxRetryAfterSeconds = 365 * 24 * 3600 // 1 year

// parseRetryAfter extracts the Retry-After header as a wait duration.
// Both formats defined by RFC 9110 are accepted:
//   - delta-seconds ("120") — an integer number of seconds (capped at
//     maxRetryAfterSeconds so the duration cannot overflow);
//   - HTTP-date ("Sat, 01 Jan 2033 00:00:00 GMT") — the wait is the time
//     until that date; a date in the past (or "now") yields 0, meaning
//     "no wait" (the caller falls back to its default cooldown).
//
// Invalid, empty, negative and zero values yield 0.
func parseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	ra := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if ra == "" {
		return 0
	}
	if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
		if secs > maxRetryAfterSeconds {
			secs = maxRetryAfterSeconds
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(ra); err == nil {
		d := time.Until(t)
		if d <= 0 {
			// The advertised date is already past: the wait has elapsed, so
			// there is nothing to honor — the caller uses its default
			// cooldown instead of an immediate retry storm.
			return 0
		}
		// A far-future HTTP-date is capped like a huge delta-seconds value:
		// the duration stays bounded (no overflow, no multi-century wait)
		// and the caller's own 5-minute cooldown cap decides the final
		// wait.
		if d > maxRetryAfterSeconds*time.Second {
			return maxRetryAfterSeconds * time.Second
		}
		return d
	}
	return 0
}

// handleUpstreamError classifies the upstream error response and updates the
// account state (exhaustion for permanent credential/quota errors, cooldown
// otherwise). It returns the classification so callers can report the real
// cause to the client (item: 401/402 exhaustion must not masquerade as a
// generic 503).
func handleUpstreamError(acc *pool.Account, resp *http.Response, requestID string, model string) UpstreamErrorClass {
	if resp == nil || resp.Body == nil {
		return UpstreamErrorTemporary
	}
	limitReader := io.LimitReader(resp.Body, 4096)
	bodyBytes, err := io.ReadAll(limitReader)
	if err != nil {
		slog.Error("handleUpstreamError read body", "req", requestID, "error", err)
	}

	baseAttrs := []any{"req", requestID, "model", model, "account", acc.Name(), "status", resp.StatusCode}

	class := ClassifyUpstreamError(resp.StatusCode, bodyBytes)
	if acc.PublicService() && class == UpstreamErrorPermanentQuota {
		slog.Info("public_service quota/balance error ignored for pool health", append(baseAttrs, "error_type", "public_service_quota_ignored")...)
		if resp.StatusCode != 429 {
			return UpstreamErrorTemporary
		}
		class = UpstreamErrorTemporary
	}

	switch class {
	case UpstreamErrorPermanentCredential:
		acc.MarkExhaustedWithClass(pool.ExhaustPermanentCredential)
		slog.Error("upstream permanent credential error, marking exhausted", append(baseAttrs, "body", string(util.RedactBodyBytesWithKeys(bodyBytes, []string{acc.Key()})), "error_type", "auth_failed")...)
		return UpstreamErrorPermanentCredential
	case UpstreamErrorPermanentQuota:
		acc.MarkExhaustedWithClass(pool.ExhaustPermanentQuota)
		slog.Error("upstream permanent quota error, marking exhausted", append(baseAttrs, "body", string(util.RedactBodyBytesWithKeys(bodyBytes, []string{acc.Key()})), "error_type", "upstream_ratelimited")...)
		return UpstreamErrorPermanentQuota
	}

	// Temporary: 429 honors Retry-After (capped at 5 minutes), every other
	// status uses the standard upstream cooldown. The account stays healthy.
	cd := upstreamCooldown
	errorType := "upstream_5xx"
	if resp.StatusCode == 429 {
		errorType = "upstream_ratelimited"
		if ra := parseRetryAfter(resp); ra > 0 {
			cd = ra
		}
	}
	if cd > 5*time.Minute {
		cd = 5 * time.Minute
	}
	acc.SetCooldown(cd)
	slog.Warn("upstream temporary error, cooling down", append(baseAttrs, "cooldown", cd.String(), "body", string(util.RedactBodyBytesWithKeys(bodyBytes, []string{acc.Key()})), "error_type", errorType)...)
	return UpstreamErrorTemporary
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
		// Host, Content-Length and Expect are NEVER forwarded: the upstream
		// request is built by prism (Host must be the upstream's own, the
		// body is re-created so the client's Content-Length would be wrong
		// or a lie, and Expect: 100-continue is a client→first-hop
		// negotiation that must not reach the upstream).
		if ck == "Host" || ck == "Content-Length" || ck == "Expect" {
			continue
		}
		if sensitiveClientHeaders[ck] {
			continue
		}
		// Forwarded / X-Forwarded-* / X-Real-IP are client-spoofable
		// forwarding headers: dropping them (item 7) keeps the upstream from
		// trusting a client-supplied origin IP or route.
		if spoofableClientHeaders[ck] || strings.HasPrefix(ck, "X-Forwarded-") {
			continue
		}
		// X-Prism-Provider is prism's OWN routing header (the client uses it
		// to select the provider): it must never reach the upstream — the
		// upstream is not a prism router, and forwarding it would let a
		// client steer a header the upstream might interpret or echo back.
		// Prism's own internally-built upstream requests (model cache
		// fetch) set it explicitly when they need it.
		if ck == "X-Prism-Provider" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// upstreamResponseBody returns a reader over the upstream response body.
// A Content-Encoding: gzip response is transparently decompressed so the
// bytes written to the client match the headers the client sees: the body
// cap, the redaction, the SSE parsing and the JSON parsing all operate on
// the DECODED content, and the gzip header itself is never copied to the
// client (copyUpstreamHeaders' allowlist does not include Content-Encoding).
// Without this, a gzip-encoded upstream response would reach the client as
// compressed bytes labeled application/json with no Content-Encoding
// header — unparseable garbage, and the 4xx redaction would corrupt the
// compressed stream on top of that. The returned reader is valid only for
// the lifetime of resp.Body, which remains owned by the caller (closed by
// the caller's own defer, never by this function). When the returned
// reader is NOT resp.Body itself (a gzip reader), the caller closes it
// once done — gzip.Reader.Close does not close the underlying resp.Body,
// so closing it releases the decompressor without touching the connection
// lifecycle. A decompression failure (e.g. a body that is not valid gzip
// despite the header) is reported either here or on the first read and
// flows through the caller's existing body-read error path (structured
// 502 / warn), never as garbage bytes to the client. Multi-valued or
// non-gzip encodings are left alone.
func upstreamResponseBody(resp *http.Response) (io.ReadCloser, error) {
	if resp == nil || resp.Body == nil {
		return nil, errors.New("nil upstream response body")
	}
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get("Content-Encoding")), "gzip") {
		return gzip.NewReader(resp.Body)
	}
	return resp.Body, nil
}

// closeUpstreamBodyReader releases a body reader returned by
// upstreamResponseBody once reading is complete. The identity case — the
// reader IS resp.Body — is a deliberate no-op: resp.Body is owned by the
// caller and closed by the caller's own top-level defer, so closing it here
// would double-close the same object (a NopCloser-style body tolerates it
// silently at best, a tracked body double-frees at worst). A gzip reader is
// a separate object: closing it releases the decompressor, and
// gzip.Reader.Close does NOT close the underlying resp.Body, so the
// connection lifecycle stays with the caller. Callers must invoke it after
// the read returns — on success AND on read error, so a truncated gzip
// stream cannot leak its decompressor.
func closeUpstreamBodyReader(resp *http.Response, bodyReader io.ReadCloser) {
	if resp == nil || bodyReader == nil || bodyReader == resp.Body {
		return
	}
	_ = bodyReader.Close()
}

// readUpstreamBodyLimited reads at most maxBytes+1 bytes from the upstream
// response body (transparently decompressing a Content-Encoding: gzip body
// via upstreamResponseBody) and closes the body reader once the read
// completes — on success AND on read error, so a truncated gzip stream
// cannot leak its decompressor. The identity reader (resp.Body itself) is
// NOT closed here: resp.Body stays owned by the caller and is closed by the
// caller's own defer (see closeUpstreamBodyReader). Returns
// ErrUpstreamResponseTooLarge when the body exceeds maxBytes.
func readUpstreamBodyLimited(resp *http.Response, maxBytes int64) ([]byte, error) {
	bodyReader, err := upstreamResponseBody(resp)
	if err != nil {
		return nil, err
	}
	defer closeUpstreamBodyReader(resp, bodyReader)
	return readResponseBodyLimited(bodyReader, maxBytes)
}

// clearResponsesStreamHeaders removes the SSE-only headers the Responses
// streaming branch set for a stream that never started: a pre-first-event
// failure answers a JSON 502, and that error response must not carry
// Cache-Control: no-cache or Connection: keep-alive (stream semantics) —
// its headers must describe the error, not a stream. Content-Type is
// overwritten by util.WriteJSON, so it needs no clearing here.
func clearResponsesStreamHeaders(w http.ResponseWriter) {
	w.Header().Del("Cache-Control")
	w.Header().Del("Connection")
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
	// key is the exact credential sent upstream with this request ("" on
	// the retry/fatal paths). The 401 reactive refresh compares it against
	// the on-disk token to detect whether a concurrent caller already
	// rotated the token (thundering-herd protection: N concurrent 401s
	// must burn exactly one refresh-token rotation).
	key string
}

// doUpstreamRequest builds and sends the upstream HTTP request.
// On success it returns the response plus ctx/cancel for the caller (segment 3)
// to manage. On any error it explicitly cancels the upstream context and returns
// a result describing whether the caller should retry.
func doUpstreamRequest(acc *pool.Account, r *http.Request, bodyBytes []byte, opts ChatForwardOpts, requestID string) doUpstreamResult {
	ctx, cancel := upstreamContext(r, opts.Stream)

	upPath := opts.UpstreamPath
	if upPath == "" {
		upPath = "/chat/completions"
	}
	targetURL := util.JoinURLPath(acc.BaseURL(), upPath)
	// The client's RawQuery is deliberately NOT forwarded: a client-supplied
	// query string would reach the upstream verbatim (letting a client
	// append ?key=secret or routing parameters to the upstream URL) and no
	// prism feature depends on it. The upstream URL is built solely from the
	// account base URL and the fixed upstream path.
	_ = r.URL.RawQuery

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		cancel()
		// Safe fields only: the raw error may embed the target URL (parse
		// errors) and must never reach logs.
		slog.Error("failed to create upstream request", "req", requestID, "model", opts.Model, "account", acc.Name(), "error_type", "invalid_upstream_url")
		util.RecordUpstreamRetry()
		return doUpstreamResult{retry: true}
	}
	// Header order (account headers can never override the credential):
	//   1. copy safe client headers (Authorization/hop-by-hop/sensitive dropped)
	//   2. apply account-level headers (override same-named client headers)
	//   3. apply the account credential header (Authorization: Bearer <key>,
	//      or the account's custom auth_header when configured)
	//   4. default Content-Type to application/json when unset (accounts may
	//      explicitly set their own Content-Type)
	copyClientHeaders(req.Header, r.Header)
	pool.ApplyAccountHeaders(req.Header, acc)
	// Resolve the credential exactly once and send the SAME value that is
	// recorded on the result (doUpstreamResult.key) for the 401 reactive
	// refresh's token comparison — a second Key() call could straddle a
	// refresh and record a token that was never sent.
	key := acc.Key()
	pool.ApplyAuthHeaderWithValue(req.Header, acc, key)
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := acc.Client().Do(req)
	if err != nil {
		cancel()
		// If client disconnected, don't retry
		if r.Context().Err() != nil {
			slog.Warn("client disconnected, aborting retry", "req", requestID, "model", opts.Model, "error_type", "client_disconnect")
			util.RecordError()
			return doUpstreamResult{retry: false, fatalErr: fmt.Errorf("client disconnected: %w", r.Context().Err())}
		}
		acc.SetCooldown(upstreamCooldown)
		// Safe fields only (item 10): the raw *url.Error embeds the full
		// upstream URL — including the query string (?key=secret) and any
		// credentials in the base URL — and must never reach logs. The
		// classification is the single shared one (util.ClassifyConnError):
		// the same strings (upstream_timeout / upstream_refused /
		// client_disconnect / upstream_error) are used by the model cache
		// fetch, the probe and the startup check, so the log taxonomy cannot
		// diverge between surfaces.
		// Retry is a separate gate (ConnErrorRetryable): a refused/DNS
		// failure almost certainly never left this process; timeout /
		// reset / EOF / broken pipe may already have reached the
		// upstream, so a non-idempotent POST must not switch accounts.
		errType := util.ClassifyConnError(err)
		if util.ConnErrorRetryable(err) {
			slog.Warn("chat retry, upstream connection error", "req", requestID, "model", opts.Model, "account", acc.Name(), "error_type", errType)
			util.RecordUpstreamRetry()
			return doUpstreamResult{retry: true}
		}
		slog.Warn("upstream connection error, not retrying", "req", requestID, "model", opts.Model, "account", acc.Name(), "error_type", errType)
		util.RecordError()
		return doUpstreamResult{retry: false, fatalErr: err}
	}

	return doUpstreamResult{resp: resp, ctx: ctx, cancel: cancel, key: key}
}

// responseCommitWriter is the writer contract for handleUpstreamResponse's
// streaming branch: an http.ResponseWriter that ALSO reports whether the
// response status has been committed (see middleware.StatusCommitter). The
// commit source is EXPLICIT and stable — the caller always hands over a
// writer with commit state (production passes the *middleware.StatusCapture
// it owns in proxyChatWithBody), so the pre-first-event failure path never
// has to guess "committed" for an unknown writer and can never turn a
// failure into an empty 200. Direct test callers must provide a writer with
// the same commit semantics (e.g. a StatusCapture-like wrapper).
type responseCommitWriter interface {
	http.ResponseWriter
	middleware.StatusCommitter
}

// legacyStreamWriter delays the status commit of the legacy chat SSE
// passthrough until the first event write (or Flush, which net/http would
// commit as an implicit 200 anyway): the upstream status is preserved for a
// healthy stream, but a failure or an empty upstream stream before the first
// event leaves the response UNCOMMITTED, so the caller can still answer a
// real HTTP 502 instead of committing an empty 200. The first WriteHeader
// wins (net/http semantics); Flush is forwarded to the inner writer so SSE
// streaming keeps working. Committed() delegates to the inner writer, which
// is the single source of commit truth for the caller's pre-first-event
// check.
type legacyStreamWriter struct {
	responseCommitWriter
	status    int
	committed bool
}

func (l *legacyStreamWriter) WriteHeader(code int) {
	if l.committed {
		return
	}
	l.committed = true
	l.status = code
}

func (l *legacyStreamWriter) Write(p []byte) (int, error) {
	l.commit()
	return l.responseCommitWriter.Write(p)
}

func (l *legacyStreamWriter) Flush() {
	l.commit()
	if f, ok := l.responseCommitWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// commit writes the delayed status to the inner writer exactly once (on the
// first write/flush). The inner writer then records the committed state that
// Committed() reports.
func (l *legacyStreamWriter) commit() {
	if l.committed {
		return
	}
	l.committed = true
	l.responseCommitWriter.WriteHeader(l.status)
}

// writeEmptyUpstreamStream writes the terminal 502 for an uncommitted empty
// upstream SSE. The account is cooled down so the next request does not
// immediately reselect it. The client and audit get a fixed safe message
// (no upstream body or URL).
func writeEmptyUpstreamStream(w http.ResponseWriter, r *http.Request, acc *pool.Account) {
	acc.SetCooldown(upstreamCooldown)
	const msg = "upstream stream closed before any event"
	util.WriteJSON(w, http.StatusBadGateway, map[string]any{
		"error": map[string]any{"message": msg, "code": "upstream_stream_error"},
	})
	if a := middleware.AuditFromCtx(r.Context()); a != nil {
		a.Status = http.StatusBadGateway
		a.Account = acc.Name()
		a.ErrorType = "upstream_stream_error"
		a.Error = msg
	}
}

// handleUpstreamResponse processes the upstream HTTP response and writes the
// result to the client. It owns the lifecycle of ctx (via the provided cancel)
// and resp.Body. The third return value reports how the failure affected the
// upstream account (UpstreamErrorClass): proxyChatWithBody uses it to answer
// with a status that distinguishes upstream auth/balance exhaustion from a
// generic all-exhausted 503 (item: 401/402 must not masquerade as 503).
func handleUpstreamResponse(acc *pool.Account, w responseCommitWriter, r *http.Request, resp *http.Response, bodyBytes []byte, start time.Time, opts ChatForwardOpts, requestID string, ctx context.Context, cancel context.CancelFunc) (done bool, fatalErr error, upstreamClass UpstreamErrorClass) {
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode == 429 || resp.StatusCode == 402 || resp.StatusCode == 401 {
		class := handleUpstreamError(acc, resp, requestID, opts.Model)
		util.RecordUpstreamRetry()
		return false, nil, class
	}

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// 4xx client error (other than 401/402/429 handled above).
		// The body is read exactly ONCE and reused for classification and
		// any passthrough below. A gzip-encoded upstream body is
		// decompressed first: the redacted passthrough body is then plain
		// text and the Content-Encoding header is dropped below, keeping
		// body and headers semantically consistent.
		bodyReader, bodyErr := upstreamResponseBody(resp)
		var errBody []byte
		var readErr error
		if bodyErr != nil {
			readErr = bodyErr
		} else {
			errBody, readErr = io.ReadAll(io.LimitReader(bodyReader, config.MaxErrorBodyBytes))
			// The read is done (success or error): release the decompressor
			// now. closeUpstreamBodyReader skips the identity reader (resp.Body
			// stays owned by the top-level defer) and closes a gzip reader
			// without touching resp.Body — a truncated gzip 4xx body cannot
			// leak its decompressor.
			closeUpstreamBodyReader(resp, bodyReader)
		}
		if readErr != nil {
			slog.Warn("failed to read upstream 4xx body", "req", requestID, "error", readErr)
		}
		// Shared classification (ClassifyUpstreamError, the same point
		// handleUpstreamError and the startup probe use): a 403 whose body
		// is a recognized structured permanent credential/quota error
		// exhausts the account and is retried on another account — the
		// 403 is not written to the client (same as 401). A bare 403 (no
		// recognized envelope) stays temporary, is passed through
		// unchanged, and does NOT exhaust.
		var class UpstreamErrorClass
		switch ClassifyUpstreamError(resp.StatusCode, errBody) {
		case UpstreamErrorPermanentCredential:
			class = UpstreamErrorPermanentCredential
			acc.MarkExhaustedWithClass(pool.ExhaustPermanentCredential)
			slog.Error("upstream 4xx permanent credential error, marking exhausted", "req", requestID, "model", opts.Model, "account", acc.Name(), "status", resp.StatusCode, "body", string(util.RedactBodyBytesWithKeys(errBody, []string{acc.Key()})), "error_type", "auth_failed")
		case UpstreamErrorPermanentQuota:
			if acc.PublicService() {
				slog.Info("public_service quota/balance error ignored for pool health", "req", requestID, "model", opts.Model, "account", acc.Name(), "status", resp.StatusCode, "error_type", "public_service_quota_ignored")
				class = UpstreamErrorTemporary
			} else {
				class = UpstreamErrorPermanentQuota
				acc.MarkExhaustedWithClass(pool.ExhaustPermanentQuota)
				slog.Error("upstream 4xx permanent quota error, marking exhausted", "req", requestID, "model", opts.Model, "account", acc.Name(), "status", resp.StatusCode, "body", string(util.RedactBodyBytesWithKeys(errBody, []string{acc.Key()})), "error_type", "upstream_ratelimited")
			}
		}
		if resp.StatusCode == http.StatusForbidden &&
			(class == UpstreamErrorPermanentCredential || class == UpstreamErrorPermanentQuota) {
			util.RecordUpstreamRetry()
			return false, nil, class
		}
		errStr := string(util.RedactBodyBytesWithKeys(errBody, []string{acc.Key()}))
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
		return true, nil, class
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 5xx / 1xx / 3xx: cool the account down but do not switch accounts.
		// The request may already have been accepted upstream; retrying the
		// same POST on another account would double-submit. Same rule as
		// util.ConnErrorRetryable for timeout/reset/EOF.
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			slog.Warn("failed to read upstream 5xx body", "req", requestID, "error", readErr)
		}
		acc.SetCooldown(upstreamCooldown)
		slog.Warn("upstream 5xx error, cooling down", "req", requestID, "model", opts.Model, "account", acc.Name(), "status", resp.StatusCode, "body", string(util.RedactBodyBytesWithKeys(errBody, []string{acc.Key()})), "error_type", "upstream_5xx")
		util.RecordError()
		const msg = "upstream returned a server error"
		util.WriteJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": msg, "code": "upstream_5xx"},
		})
		if a := middleware.AuditFromCtx(r.Context()); a != nil {
			a.Status = http.StatusBadGateway
			a.Account = acc.Name()
			a.ErrorType = "upstream_5xx"
			a.Error = msg
		}
		return true, nil, UpstreamErrorTemporary
	}

	if opts.ResponsesOut && opts.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// The HTTP status is deliberately NOT committed here: it is delayed
		// until the first event is written. A failure before the first event
		// (empty upstream stream, upstream connection dying before any data)
		// can therefore still answer a real HTTP 502 instead of committing
		// an empty 200. Once the first event is written the 200 is committed
		// and can never be changed (net/http semantics); mid-stream failures
		// then use the protocol's own failure event (response.failed), which
		// the translator emits when it can still write.
		translateStart := time.Now()
		slog.Debug("responses_stream translate start", "request_id", requestID, "account", acc.Name(), "translate_start", translateStart.Format(time.RFC3339Nano))
		// A Content-Encoding: gzip upstream body is decompressed BEFORE the
		// SSE translator: the client never receives a Content-Encoding header
		// (copyUpstreamHeaders' allowlist does not include it), so the bytes
		// the translator parses must already be decoded — the compressed
		// stream would otherwise be consumed as SSE garbage. The gzip header
		// is validated HERE while the status is still uncommitted (it is
		// delayed until the first event): an upstream that claims gzip but
		// sends a non-gzip body fails closed through the same pre-first-event
		// 502 path below, never as garbage bytes to the client. A stream that
		// corrupts AFTER the header surfaces on the first read and flows
		// through the translator's scanner error path (502 before the first
		// event, response.failed after it). The gzip reader is closed when
		// the translation returns; resp.Body stays owned by this function
		// (deferred close at the top) — gzip.Reader.Close does not close it.
		bodyReader, bodyErr := upstreamResponseBody(resp)
		var err error
		if bodyErr != nil {
			err = bodyErr
		} else {
			if gz, ok := bodyReader.(*gzip.Reader); ok {
				defer gz.Close()
			}
			var closeDump func()
			bodyReader, closeDump = teeUpstreamRawDump(resp, requestID, bodyReader)
			defer closeDump()
			if opts.DSMLGuard {
				bodyReader = dsml.NewGuardReader(bodyReader)
			}
			err = stream.TranslateChatStreamToResponses(w, bodyReader, opts.Model, opts.ReqTools, mcp.GetSearchToolCache(opts.TenantID), ctx)
		}
		translateElapsed := time.Since(translateStart).Milliseconds()
		if err != nil {
			slog.Error("responses_stream translate error", "req", requestID, "model", opts.Model, "account", acc.Name(), "error", err, "translate_ms", translateElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
			util.RecordError()
			// Commit-state check via the explicit responseCommitWriter
			// contract (the caller always passes a writer that reports
			// commit state — production owns a *middleware.StatusCapture):
			// the first successful/partial event write commits the 200 and
			// the status can then never be changed (net/http semantics).
			// Before the first event nothing reached the client and a real
			// 502 is still ours to choose. There is no unknown-writer
			// fallback: guessing "committed" for a writer without commit
			// state could turn a pre-first-event failure into an empty 200.
			// The state is captured ONCE, before any write below — after
			// the 502 is written the same writer would report committed and
			// the audit would misrecord the delivered status.
			committed := w.Committed()
			// Pre-first-event failures map to the SAME diagnosable error
			// codes the translator uses mid-stream, so operators see one
			// consistent taxonomy regardless of commit state:
			//   - ErrResponsesStreamTooLarge (per-buffer or total
			//     accumulation cap) → response_too_large (mid-stream: the
			//     response.failed frame carries the same code);
			//   - bufio.ErrTooLong (single SSE line over the scanner cap) →
			//     stream_line_too_long;
			//   - everything else → the generic upstream_stream_error.
			errCode := "upstream_stream_error"
			errMsg := "upstream stream failed before any event"
			switch {
			case errors.Is(err, stream.ErrResponsesStreamTooLarge):
				errCode = "response_too_large"
				errMsg = "upstream stream output exceeded the accumulated-size limit"
			case errors.Is(err, bufio.ErrTooLong):
				errCode = "stream_line_too_long"
				errMsg = err.Error()
			}
			if !committed && errors.Is(err, stream.ErrEmptyUpstreamStream) {
				// Empty upstream SSE before any event: the request may
				// already have been billed. Do not switch accounts.
				// RecordError ran above; do not count this as a retry.
				slog.Warn("responses_stream empty stream", "req", requestID, "model", opts.Model, "account", acc.Name(), "error_type", "upstream_stream_error")
				// The JSON 502 must not carry the SSE-only headers
				// (Cache-Control: no-cache, Connection: keep-alive) that were
				// set for the stream that never started.
				clearResponsesStreamHeaders(w)
				writeEmptyUpstreamStream(w, r, acc)
				return true, err, UpstreamErrorTemporary
			}
			if !committed {
				// Nothing was written to the client yet (pre-first-event
				// failure): return a structured 502 instead of an empty 200.
				// util.WriteJSON writes the status exactly once — the response
				// is still uncommitted, so this is the first and only
				// WriteHeader on the wire. The SSE-only headers set for the
				// stream that never started are cleared first so the error
				// response headers are consistent (no Cache-Control: no-cache
				// / Connection: keep-alive on a JSON error).
				clearResponsesStreamHeaders(w)
				util.WriteJSON(w, http.StatusBadGateway, map[string]any{
					"error": map[string]any{"message": errMsg, "code": errCode},
				})
			}
			if a := middleware.AuditFromCtx(r.Context()); a != nil {
				// The audit status must match what was actually delivered:
				// 502 when nothing was written, the committed 200 when the
				// response was already under way (the translator emits the
				// protocol failure event response.failed when it can still
				// write).
				if !committed {
					a.Status = http.StatusBadGateway
				} else {
					a.Status = http.StatusOK
				}
				a.Account = acc.Name()
				// The two diagnosable failures keep their code as the audit
				// error_type (like the non-streaming response_too_large
				// path); everything else stays on the generic connection
				// classification.
				if errCode != "upstream_stream_error" {
					a.ErrorType = errCode
				} else {
					a.ErrorType = util.ClassifyConnError(err)
				}
				a.Error = err.Error()
			}
			return true, err, UpstreamErrorTemporary
		}
		slog.Debug("responses_stream translate done", "request_id", requestID, "account", acc.Name(), "translate_ms", translateElapsed, "elapsed", time.Since(start))
		util.RecordRequest(time.Since(start))
		if a := middleware.AuditFromCtx(r.Context()); a != nil {
			a.Status = http.StatusOK
			a.Account = acc.Name()
		}
		return true, nil, UpstreamErrorTemporary
	}

	if opts.ResponsesOut && !opts.Stream {
		bodyReadStart := time.Now()
		// Decompress a gzip-encoded upstream body before the cap check and
		// the conversion: the translation parses real JSON, and the client
		// must receive decoded content with no Content-Encoding header. The
		// unified helper also closes the body reader when the read completes
		// (success or error), so a truncated gzip body cannot leak its
		// decompressor.
		rawBody, err := readUpstreamBodyLimited(resp, responseBodyCap(opts))
		bodyReadElapsed := time.Since(bodyReadStart).Milliseconds()
		if err == nil {
			util.DumpUpstreamRawBytes(upstreamRawDumpMeta(resp, requestID), rawBody)
		}
		if err != nil {
			if errors.Is(err, ErrUpstreamResponseTooLarge) {
				slog.Error("responses_json body too large", "request_id", requestID, "model", opts.Model, "account", acc.Name(), "max_bytes", responseBodyCap(opts), "elapsed", time.Since(start), "error_type", "response_too_large")
				util.RecordError()
				util.WriteJSON(w, http.StatusBadGateway, map[string]any{
					"error": map[string]any{"message": "upstream response too large", "code": "response_too_large"},
				})
				if a := middleware.AuditFromCtx(r.Context()); a != nil {
					a.Status = http.StatusBadGateway
					a.Account = acc.Name()
					a.ErrorType = "response_too_large"
					a.Error = err.Error()
				}
				return true, nil, UpstreamErrorTemporary
			}
			// Any other read failure while the response is still uncommitted:
			// return a structured 502 instead of an empty 200, and audit the
			// real status.
			slog.Warn("responses_json body read error", "request_id", requestID, "model", opts.Model, "account", acc.Name(), "body_ms", bodyReadElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
			util.RecordError()
			util.WriteJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]any{"message": "failed to read upstream response", "code": "upstream_error"},
			})
			if a := middleware.AuditFromCtx(r.Context()); a != nil {
				a.Status = http.StatusBadGateway
				a.Account = acc.Name()
				a.ErrorType = util.ClassifyConnError(err)
				a.Error = err.Error()
			}
			return true, nil, UpstreamErrorTemporary
		}
		util.DumpDebugUpstreamResponse(rawBody, []string{acc.Key()})
		if opts.DSMLGuard {
			rawBody = dsml.RewriteCompletion(rawBody)
		}
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
			return true, nil, UpstreamErrorTemporary
		}
		// Capture token usage from the response body for non-streaming audit.
		// The parser is selected by the upstream path (see
		// parseUsageForResponseBody): /v1/messages responses are Anthropic
		// form (input_tokens/...), everything else is OpenAI form. The gate
		// accepts any non-empty usage, including cache-only usage (shared
		// usagemeta.HasTokens).
		if a := middleware.AuditFromCtx(r.Context()); a != nil {
			if u := parseUsageForResponseBody(rawBody, opts); u.HasTokens() {
				a.ApplyUsage(u)
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
		return true, nil, UpstreamErrorTemporary
	}

	// Legacy chat path (no responses translation).
	if opts.Stream {
		// Streaming: pass through SSE chunks without token capture.
		// Streaming token interception is complex and risks breaking
		// the SSE stream; tokens_in/tokens_out remain 0 (acceptable).
		copyUpstreamHeaders(w, resp.Header)
		// The HTTP status is deliberately NOT committed here: it is delayed
		// until the first event write (legacyStreamWriter holds the upstream
		// status and commits it on the first write/flush). A failure or an
		// empty upstream stream before the first event therefore still
		// leaves the response UNCOMMITTED, and this branch answers a real
		// HTTP 502 instead of committing an empty 200 (net/http semantics:
		// once the first byte reached the wire the status can never change).
		lw := &legacyStreamWriter{responseCommitWriter: w, status: resp.StatusCode}
		bodyReadStart := time.Now()
		// A Content-Encoding: gzip upstream body is decompressed BEFORE the
		// stream copier: the client never receives a Content-Encoding header
		// (copyUpstreamHeaders' allowlist does not include it), so the bytes
		// copied to the client must already be decoded — the compressed
		// stream would otherwise reach the client as garbage. The gzip header
		// is validated HERE while the response is still uncommitted (the
		// legacy writer delays the status until the first event): an upstream
		// that claims gzip but sends a non-gzip body fails closed through the
		// same pre-first-event 502 path below, never as garbage bytes to the
		// client. A stream that corrupts AFTER the header surfaces on the
		// first read and flows through the io.Copy error path (502 before the
		// first byte, the broken stream simply cut after it). The gzip reader
		// is closed when the copy returns; resp.Body stays owned by this
		// function (deferred close at the top) — gzip.Reader.Close does not
		// close it.
		bodyReader, bodyErr := upstreamResponseBody(resp)
		var n int64
		var err error
		if bodyErr != nil {
			err = bodyErr
		} else {
			if gz, ok := bodyReader.(*gzip.Reader); ok {
				defer gz.Close()
			}
			var closeDump func()
			bodyReader, closeDump = teeUpstreamRawDump(resp, requestID, bodyReader)
			defer closeDump()
			if opts.DSMLGuard {
				bodyReader = dsml.NewGuardReader(bodyReader)
			}
			n, err = stream.StreamResponseBody(lw, bodyReader, r, acc.Name())
		}
		bodyReadElapsed := time.Since(bodyReadStart).Milliseconds()
		// Commit state is captured ONCE, before any write below (after the
		// 502 is written the same writer would report committed and the
		// audit would misrecord the delivered status).
		committed := lw.Committed()
		if err != nil {
			slog.Warn("legacy_stream body read error", "request_id", requestID, "model", opts.Model, "account", acc.Name(), "body_ms", bodyReadElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
			util.RecordError()
			if !committed {
				// Nothing reached the client (the upstream died before the
				// first event): answer a structured 502 instead of leaving
				// the connection without a real status.
				util.WriteJSON(w, http.StatusBadGateway, map[string]any{
					"error": map[string]any{"message": "upstream stream failed before any event", "code": "upstream_stream_error"},
				})
			}
			if a := middleware.AuditFromCtx(r.Context()); a != nil {
				// The audit status must match what was actually delivered:
				// 502 when nothing was written, the committed upstream status
				// when the stream was already under way.
				if !committed {
					a.Status = http.StatusBadGateway
					a.ErrorType = "upstream_stream_error"
				} else {
					a.Status = resp.StatusCode
					a.ErrorType = util.ClassifyConnError(err)
				}
				a.Account = acc.Name()
				a.Error = err.Error()
			}
			return true, err, UpstreamErrorTemporary
		}
		if !committed {
			// The upstream answered 2xx but closed the stream without a
			// single event. The request may already have been billed:
			// write a terminal 502 and do not switch accounts.
			slog.Warn("legacy_stream empty stream", "request_id", requestID, "model", opts.Model, "account", acc.Name(), "elapsed", time.Since(start), "error_type", "upstream_stream_error")
			util.RecordError()
			writeEmptyUpstreamStream(w, r, acc)
			return true, nil, UpstreamErrorTemporary
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
		return true, nil, UpstreamErrorTemporary
	}

	// Non-streaming legacy: read full body, capture token usage, write to client.
	bodyReadStart := time.Now()
	// Decompress a gzip-encoded upstream body before the cap check, usage
	// parsing and the passthrough write: the client must receive decoded
	// content (no Content-Encoding header is copied), so the body it gets
	// matches the headers it sees. The unified helper also closes the body
	// reader when the read completes (success or error), so a truncated
	// gzip body cannot leak its decompressor.
	rawBody, err := readUpstreamBodyLimited(resp, responseBodyCap(opts))
	bodyReadElapsed := time.Since(bodyReadStart).Milliseconds()
	if err == nil {
		util.DumpUpstreamRawBytes(upstreamRawDumpMeta(resp, requestID), rawBody)
	}
	if err != nil {
		if errors.Is(err, ErrUpstreamResponseTooLarge) {
			slog.Error("legacy_nonstream body too large", "request_id", requestID, "model", opts.Model, "account", acc.Name(), "max_bytes", responseBodyCap(opts), "elapsed", time.Since(start), "error_type", "response_too_large")
			util.RecordError()
			util.WriteJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]any{"message": "upstream response too large", "code": "response_too_large"},
			})
			if a := middleware.AuditFromCtx(r.Context()); a != nil {
				a.Status = http.StatusBadGateway
				a.Account = acc.Name()
				a.ErrorType = "response_too_large"
				a.Error = err.Error()
			}
			return true, nil, UpstreamErrorTemporary
		}
		// Any other read failure while the response is still uncommitted:
		// return a structured 502 instead of an empty 200, and audit the
		// real status.
		slog.Warn("legacy_nonstream body read error", "request_id", requestID, "model", opts.Model, "account", acc.Name(), "body_ms", bodyReadElapsed, "elapsed", time.Since(start), "error_type", "upstream_5xx")
		util.RecordError()
		util.WriteJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "failed to read upstream response", "code": "upstream_error"},
		})
		if a := middleware.AuditFromCtx(r.Context()); a != nil {
			a.Status = http.StatusBadGateway
			a.Account = acc.Name()
			a.ErrorType = util.ClassifyConnError(err)
			a.Error = err.Error()
		}
		return true, nil, UpstreamErrorTemporary
	}
	util.DumpDebugUpstreamResponse(rawBody, []string{acc.Key()})
	// Capture token usage from the response body for non-streaming audit.
	// The parser is selected by the upstream path (see
	// parseUsageForResponseBody): /v1/messages responses are Anthropic form
	// (input_tokens/...), everything else is OpenAI form. This is the path
	// that fixes Anthropic usage being counted as zero: previously the
	// OpenAI prompt_tokens/completion_tokens field names were looked up in
	// an Anthropic body and always resolved to 0. The gate accepts any
	// non-empty usage, including cache-only usage (shared usagemeta.HasTokens).
	if a := middleware.AuditFromCtx(r.Context()); a != nil {
		if u := parseUsageForResponseBody(rawBody, opts); u.HasTokens() {
			a.ApplyUsage(u)
		}
	}
	if opts.DSMLGuard {
		rawBody = dsml.RewriteCompletion(rawBody)
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
	return true, nil, UpstreamErrorTemporary
}

func upstreamRawDumpMeta(resp *http.Response, requestID string) util.UpstreamRawDumpMeta {
	meta := util.UpstreamRawDumpMeta{RequestID: requestID}
	if resp == nil {
		return meta
	}
	meta.Status = resp.StatusCode
	if resp.Request != nil {
		meta.Header = resp.Request.Header
		if resp.Request.URL != nil {
			meta.URL = resp.Request.URL.Redacted()
		}
	}
	return meta
}

// teeUpstreamRawDump copies upstream body bytes to a dump file when the
// PRISM_DUMP_UPSTREAM_SSE env switch is on. When the switch is off it
// returns body unchanged and a no-op closer, so the copy path matches
// the pre-dump code.
func teeUpstreamRawDump(resp *http.Response, requestID string, body io.ReadCloser) (io.ReadCloser, func()) {
	dump := util.StartUpstreamRawDump(upstreamRawDumpMeta(resp, requestID))
	if dump == nil {
		return body, func() {}
	}
	return io.NopCloser(io.TeeReader(body, dump)), func() {
		if err := dump.Close(); err != nil {
			slog.Warn("upstream raw dump close failed", "error", err)
		}
	}
}
