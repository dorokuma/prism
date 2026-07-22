package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// Logger is the package-level logger used by InitLogger, EmitAudit, etc.
var Logger *slog.Logger

// LogLevel is the package-level log level variable.
var LogLevel slog.LevelVar

func init() {
	// Default logger: LevelInfo JSON to stderr.
	LogLevel.Set(slog.LevelInfo)
	Logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: &LogLevel,
	}))
}

// InitLogger initialises the global slog logger with the given level string.
// "debug" / "info" / "warn" / "error" (case-insensitive). Defaults to "info".
func InitLogger(level string) {
	l := ParseLevel(level)
	LogLevel.Set(l)
	Logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: &LogLevel,
	}))
	slog.SetDefault(Logger)
}

// SetLogLevel changes the log level at runtime (for SIGHUP hot-reload).
func SetLogLevel(level string) {
	l := ParseLevel(level)
	LogLevel.Set(l)
}

// ParseLevel converts a log level string to slog.Level.
func ParseLevel(s string) slog.Level {
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

// ---------------------------------------------------------------------------
// Request audit log — structured single-line per-request summary emitted at
// request completion.  Fields are populated across the call chain and flushed
// once in a deferred EmitAudit at the top-level handler.
// ---------------------------------------------------------------------------

// RequestAudit carries per-request fields for the final request.complete log line.
type RequestAudit struct {
	Req         string  // X-Request-ID
	Method      string  // HTTP method
	Path        string  // URL path
	Model       string  // requested model name
	Account     string  // upstream account used (last successful or attempted)
	Error       string  // error message (empty on success)
	ErrorType   string  // short category for the error (empty on success)
	Status      int     // HTTP status written to client
	DurationMs  float64 // total wall-clock duration in milliseconds
	TokensIn    int     // prompt/input tokens consumed
	TokensOut   int     // completion/output tokens produced
	Concurrency int     // in-flight count on the selected account at select time
}

// AuditKey is the context key for *RequestAudit.
type AuditKey struct{}

// AuditFromCtx retrieves *RequestAudit from ctx, or nil when absent (nil-safe).
func AuditFromCtx(ctx context.Context) *RequestAudit {
	a, _ := ctx.Value(AuditKey{}).(*RequestAudit)
	return a
}

// EmitAudit writes a single-line structured log at INFO level for the given
// audit record.  It is a no-op when a is nil.
func EmitAudit(a *RequestAudit) {
	if a == nil {
		return
	}
	slog.Info("request.complete",
		"req", a.Req,
		"method", a.Method,
		"path", a.Path,
		"model", a.Model,
		"account", a.Account,
		"status", a.Status,
		"duration_ms", a.DurationMs,
		"tokens_in", a.TokensIn,
		"tokens_out", a.TokensOut,
		"concurrency", a.Concurrency,
		"error", a.Error,
		"error_type", a.ErrorType,
	)
}

// ---------------------------------------------------------------------------
// StatusCapture — ResponseWriter wrapper that records the HTTP status code
// written via WriteHeader without interfering with SSE/streaming Flush.
// ---------------------------------------------------------------------------

// StatusCapture wraps an http.ResponseWriter and captures the first status
// code written via WriteHeader.  Flush() is transparently forwarded to the
// inner writer when it implements http.Flusher, so SSE streaming is unaffected.
type StatusCapture struct {
	http.ResponseWriter
	Code int
}

func (s *StatusCapture) WriteHeader(code int) {
	if s.Code == 0 {
		s.Code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher by forwarding to the inner ResponseWriter
// when it also implements http.Flusher.  Without this explicit method the
// embedded interface promotion does not expose Flush to type-assertion
// callers like streamResponseBody / translateChatStreamToResponses, which
// would break SSE streaming.
func (s *StatusCapture) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the inner ResponseWriter (for use with io.Writer assertions).
func (s *StatusCapture) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
