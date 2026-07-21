package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"Debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"Info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"Warn", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"error", slog.LevelError},
		{"Error", slog.LevelError},
		{"ERROR", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
		{"garbage", slog.LevelInfo},
	}
	for _, tc := range tests {
		got := parseLevel(tc.input)
		if got != tc.want {
			t.Errorf("parseLevel(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestInitLogger_LevelParsing(t *testing.T) {
	var buf bytes.Buffer

	// Capture slog output by temporarily replacing the handler.
	for _, level := range []string{"debug", "info", "warn", "error"} {
		initLogger(level)
		wantDebug := level == "debug"
		wantInfo := level == "debug" || level == "info"
		if got := logger.Enabled(context.Background(), slog.LevelDebug); got != wantDebug {
			t.Errorf("after initLogger(%q): debug enabled = %v, want %v", level, got, wantDebug)
		}
		if got := logger.Enabled(context.Background(), slog.LevelInfo); got != wantInfo {
			t.Errorf("after initLogger(%q): info enabled = %v, want %v", level, got, wantInfo)
		}
	}

	// Default level for empty/unknown input
	initLogger("")
	if got := logLevel.Level(); got != slog.LevelInfo {
		t.Errorf("after initLogger(\"\"): level = %v, want info", got)
	}

	// Verify output is valid JSON
	initLogger("info")
	buf.Reset()
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	l := slog.New(h)
	l.Info("test message", "key", "value")
	out := buf.String()
	if !strings.Contains(out, `"level":"INFO"`) {
		t.Errorf("expected INFO level in JSON output, got: %s", out)
	}
	if !strings.Contains(out, `"msg":"test message"`) {
		t.Errorf("expected message in JSON output, got: %s", out)
	}
}

func TestSetLogLevel_HotReload(t *testing.T) {
	// Start at info
	initLogger("info")
	if logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug should not be enabled at info level")
	}

	// Hot reload to debug
	setLogLevel("debug")
	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug should be enabled after setLogLevel(debug)")
	}

	// Hot reload back to error
	setLogLevel("error")
	if logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info should not be enabled after setLogLevel(error)")
	}

	// Hot reload to warn
	setLogLevel("warn")
	if !logger.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("warn should be enabled after setLogLevel(warn)")
	}
}

func TestDefaultLogger_Init(t *testing.T) {
	// init() sets a default LevelInfo logger. Verify it exists.
	if logger == nil {
		t.Fatal("expected init() to create a default logger, got nil")
	}
}

func TestRequestIDMiddleware_GeneratesAndPropagates(t *testing.T) {
	var capturedID string
	handler := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = requestIDFromCtx(r.Context())
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/health", nil)
	// No X-Request-ID header set — middleware should generate one.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedID == "" {
		t.Fatal("expected non-empty request ID from context after middleware")
	}
	respID := rec.Header().Get("X-Request-ID")
	if respID == "" {
		t.Fatal("expected X-Request-ID response header to be set")
	}
	if capturedID != respID {
		t.Fatalf("context request ID %q != response header %q", capturedID, respID)
	}
}

func TestRequestIDMiddleware_PreservesClientHeader(t *testing.T) {
	var capturedID string
	handler := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = requestIDFromCtx(r.Context())
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/v1/models", nil)
	clientID := "my-client-id-123"
	req.Header.Set("X-Request-ID", clientID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedID != clientID {
		t.Fatalf("context request ID %q != client ID %q", capturedID, clientID)
	}
	respID := rec.Header().Get("X-Request-ID")
	if respID != clientID {
		t.Fatalf("response header X-Request-ID %q != client ID %q", respID, clientID)
	}
}

func TestRequestIDFromCtx_EmptyWithoutMiddleware(t *testing.T) {
	ctx := context.Background()
	if id := requestIDFromCtx(ctx); id != "" {
		t.Fatalf("expected empty string from bare context, got %q", id)
	}
}
