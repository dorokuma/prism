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
