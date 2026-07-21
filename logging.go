package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

var (
	logger   *slog.Logger
	logLevel slog.LevelVar
)

func init() {
	// Default logger: LevelInfo JSON to stderr.
	// main() will call initLogger() early to override with user config.
	logLevel.Set(slog.LevelInfo)
	logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: &logLevel,
	}))
}

// initLogger initialises the global slog logger with the given level string.
// "debug" / "info" / "warn" / "error" (case-insensitive). Defaults to "info".
func initLogger(level string) {
	l := parseLevel(level)
	logLevel.Set(l)
	logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: &logLevel,
	}))
	slog.SetDefault(logger)
}

// setLogLevel changes the log level at runtime (for SIGHUP hot-reload).
func setLogLevel(level string) {
	l := parseLevel(level)
	logLevel.Set(l)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// requestIDKey is the context key type for request IDs injected by requestIDMiddleware.
type requestIDKey struct{}

// requestIDMiddleware extracts X-Request-ID from the incoming request header or
// generates a new random ID, injects it into the request context, and sets the
// X-Request-ID response header. It must be the outermost middleware so that
// rate-limit, auth, and proxy layers all have access to the request ID.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = randomID()
		}
		w.Header().Set("X-Request-ID", rid)
		ctx := context.WithValue(r.Context(), requestIDKey{}, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestIDFromCtx retrieves the request ID from the context, or returns "" if
// none is present.
func requestIDFromCtx(ctx context.Context) string {
	if v := ctx.Value(requestIDKey{}); v != nil {
		return v.(string)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Request audit log — structured single-line per-request summary emitted at
// request completion.  Fields are populated across the call chain and flushed
// once in a deferred emitAudit at the top-level handler.
// ---------------------------------------------------------------------------

// requestAudit carries per-request fields for the final request.complete log line.
type requestAudit struct {
	req        string  // X-Request-ID
	method     string  // HTTP method
	path       string  // URL path
	model      string  // requested model name
	account    string  // upstream account used (last successful or attempted)
	error      string  // error message (empty on success)
	errorType  string  // short category for the error (empty on success)
	status     int     // HTTP status written to client
	durationMs float64 // total wall-clock duration in milliseconds
	tokensIn   int     // prompt/input tokens consumed
	tokensOut  int     // completion/output tokens produced
}

// auditKey is the context key for *requestAudit.
type auditKey struct{}

// auditFromCtx retrieves *requestAudit from ctx, or nil when absent (nil-safe).
func auditFromCtx(ctx context.Context) *requestAudit {
	a, _ := ctx.Value(auditKey{}).(*requestAudit)
	return a
}

// emitAudit writes a single-line structured log at INFO level for the given
// audit record.  It is a no-op when a is nil.
func emitAudit(a *requestAudit) {
	if a == nil {
		return
	}
	slog.Info("request.complete",
		"req", a.req,
		"method", a.method,
		"path", a.path,
		"model", a.model,
		"account", a.account,
		"status", a.status,
		"duration_ms", a.durationMs,
		"tokens_in", a.tokensIn,
		"tokens_out", a.tokensOut,
		"error", a.error,
		"error_type", a.errorType,
	)
}

// ---------------------------------------------------------------------------
// statusCapture — ResponseWriter wrapper that records the HTTP status code
// written via WriteHeader without interfering with SSE/streaming Flush.
// ---------------------------------------------------------------------------

// statusCapture wraps an http.ResponseWriter and captures the first status
// code written via WriteHeader.  Flush() is transparently forwarded to the
// inner writer when it implements http.Flusher, so SSE streaming is unaffected.
type statusCapture struct {
	http.ResponseWriter
	code int
}

func (s *statusCapture) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher by forwarding to the inner ResponseWriter
// when it also implements http.Flusher.  Without this explicit method the
// embedded interface promotion does not expose Flush to type-assertion
// callers like streamResponseBody / translateChatStreamToResponses, which
// would break SSE streaming.
func (s *statusCapture) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the inner ResponseWriter (for use with io.Writer assertions).
func (s *statusCapture) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
