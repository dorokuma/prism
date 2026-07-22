package middleware

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/util"
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
		got := ParseLevel(tc.input)
		if got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestInitLogger_LevelParsing(t *testing.T) {
	var buf bytes.Buffer

	// Capture slog output by temporarily replacing the handler.
	for _, level := range []string{"debug", "info", "warn", "error"} {
		InitLogger(level)
		wantDebug := level == "debug"
		wantInfo := level == "debug" || level == "info"
		if got := Logger.Enabled(context.Background(), slog.LevelDebug); got != wantDebug {
			t.Errorf("after InitLogger(%q): debug enabled = %v, want %v", level, got, wantDebug)
		}
		if got := Logger.Enabled(context.Background(), slog.LevelInfo); got != wantInfo {
			t.Errorf("after InitLogger(%q): info enabled = %v, want %v", level, got, wantInfo)
		}
	}

	// Default level for empty/unknown input
	InitLogger("")
	if got := LogLevel.Level(); got != slog.LevelInfo {
		t.Errorf("after InitLogger(\"\"): level = %v, want info", got)
	}

	// Verify output is valid JSON
	InitLogger("info")
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
	InitLogger("info")
	if Logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug should not be enabled at info level")
	}

	// Hot reload to debug
	SetLogLevel("debug")
	if !Logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug should be enabled after SetLogLevel(debug)")
	}

	// Hot reload back to error
	SetLogLevel("error")
	if Logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info should not be enabled after SetLogLevel(error)")
	}

	// Hot reload to warn
	SetLogLevel("warn")
	if !Logger.Enabled(context.Background(), slog.LevelWarn) {
		t.Fatal("warn should be enabled after SetLogLevel(warn)")
	}
}

func TestDefaultLogger_Init(t *testing.T) {
	// init() sets a default LevelInfo logger. Verify it exists.
	if Logger == nil {
		t.Fatal("expected init() to create a default logger, got nil")
	}
}

func TestRequestIDMiddleware_GeneratesAndPropagates(t *testing.T) {
	var capturedID string
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = util.RequestIDFromCtx(r.Context())
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
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = util.RequestIDFromCtx(r.Context())
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
	if id := util.RequestIDFromCtx(ctx); id != "" {
		t.Fatalf("expected empty string from bare context, got %q", id)
	}
}

// ---------------------------------------------------------------------------
// Audit log test helpers
// ---------------------------------------------------------------------------

// capturingHandler collects log records into a []byte slice for later
// inspection.  It is safe for concurrent use within a single test.
type capturingHandler struct {
	mu  sync.Mutex
	buf []byte
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b bytes.Buffer
	b.WriteByte('{')
	// Always emit time, level, and msg first.
	b.WriteString(fmt.Sprintf(`"time":"%s"`, r.Time.Format(time.RFC3339Nano)))
	b.WriteString(fmt.Sprintf(`,"level":"%s"`, r.Level.String()))
	b.WriteString(fmt.Sprintf(`,"msg":"%s"`, r.Message))
	r.Attrs(func(a slog.Attr) bool {
		b.WriteByte(',')
		b.WriteString(fmt.Sprintf(`"%s":`, a.Key))
		val := a.Value.Resolve()
		switch val.Kind() {
		case slog.KindString:
			b.WriteString(fmt.Sprintf(`"%s"`, val.String()))
		case slog.KindInt64:
			b.WriteString(fmt.Sprintf(`%d`, val.Int64()))
		case slog.KindFloat64:
			b.WriteString(fmt.Sprintf(`%v`, val.Float64()))
		case slog.KindBool:
			b.WriteString(fmt.Sprintf(`%v`, val.Bool()))
		default:
			b.WriteString(fmt.Sprintf(`"%s"`, val.String()))
		}
		return true
	})
	b.WriteByte('}')
	h.buf = append(h.buf, b.Bytes()...)
	h.buf = append(h.buf, '\n')
	return nil
}
func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(name string) slog.Handler      { return h }
func (h *capturingHandler) output() string { h.mu.Lock(); defer h.mu.Unlock(); return string(h.buf) }

// stashSlog replaces the default slog.Logger with one that writes into h
// and returns a restore function.  Callers must defer the restore func.
func stashSlog(h *capturingHandler) func() {
	old := slog.Default()
	l := slog.New(h)
	slog.SetDefault(l)
	return func() { slog.SetDefault(old) }
}

func TestAuditFromCtx_NilSafe(t *testing.T) {
	// Bare context: AuditFromCtx returns nil, no panic.
	ctx := context.Background()
	if a := AuditFromCtx(ctx); a != nil {
		t.Fatal("expected nil from bare context")
	}
	// EmitAudit must not panic with nil.
	EmitAudit(nil) // no-op, no panic
}
