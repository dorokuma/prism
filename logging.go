package main

import (
	"log/slog"
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
