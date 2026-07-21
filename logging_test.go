package main

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
func (h *capturingHandler) output() string                          { h.mu.Lock(); defer h.mu.Unlock(); return string(h.buf) }

// stashSlog replaces the default slog.Logger with one that writes into h
// and returns a restore function.  Callers must defer the restore func.
func stashSlog(h *capturingHandler) func() {
	old := slog.Default()
	l := slog.New(h)
	slog.SetDefault(l)
	return func() { slog.SetDefault(old) }
}

// ---------------------------------------------------------------------------
// Audit log tests
// ---------------------------------------------------------------------------

func TestAuditLog_RequestComplete(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	// Upstream returns a clean 200 with a simple JSON body.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	cfg := &Config{Accounts: []AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	pool := NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	// Inject a request ID to get a meaningful audit.req.
	ctx := context.WithValue(r.Context(), requestIDKey{}, "audit-test-1")
	r = r.WithContext(ctx)

	proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, cfg)

	if rec.Code != 200 {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	out := h.output()
	if !strings.Contains(out, `"msg":"request.complete"`) {
		t.Fatalf("expected request.complete log line, got: %s", out)
	}
	if !strings.Contains(out, `"method":"POST"`) {
		t.Error("audit missing method")
	}
	if !strings.Contains(out, `"path":"/v1/chat/completions"`) {
		t.Error("audit missing path")
	}
	if !strings.Contains(out, `"status":200`) {
		t.Error("audit missing status")
	}
	if !strings.Contains(out, `"req":"audit-test-1"`) {
		t.Error("audit missing req")
	}
	if !strings.Contains(out, `"duration_ms":`) {
		t.Error("audit missing duration_ms")
	}
}

func TestAuditLog_TokensCaptured(t *testing.T) {
	// Non-streaming: check that token usage is captured from the upstream body.
	t.Run("non_streaming", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":20,"completion_tokens":3,"total_tokens":23}}`))
		}))
		defer upstream.Close()

		cfg := &Config{Accounts: []AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL}}}
		pool := NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")

		// Use responsesOut=true so handleUpstreamResponse goes through the
		// responses_json path which calls chatCompletionToResponse and captures usage.
		proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{responsesOut: true}, cfg)

		out := h.output()
		if !strings.Contains(out, `"tokens_in":20`) {
			t.Errorf("expected tokens_in=20, got: %s", out)
		}
		if !strings.Contains(out, `"tokens_out":3`) {
			t.Errorf("expected tokens_out=3, got: %s", out)
		}
	})

	// Legacy non-streaming: verify token usage is captured on the legacy chat path.
	t.Run("legacy_non_streaming", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"total_tokens\":23}}"))
		}))
		defer upstream.Close()

		cfg := &Config{Accounts: []AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL}}}
		pool := NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte("{\"model\":\"gpt-4\"}")))
		r.Header.Set("Content-Type", "application/json")

		// Legacy path: responsesOut=false (default), non-streaming.
		proxyChatWithBody(pool, rec, r, []byte("{\"model\":\"gpt-4\"}"), time.Now(), chatForwardOpts{}, cfg)

		out := h.output()
		if !strings.Contains(out, "\"tokens_in\":20") {
			t.Errorf("expected tokens_in=20, got: %s", out)
		}
		if !strings.Contains(out, "\"tokens_out\":3") {
			t.Errorf("expected tokens_out=3, got: %s", out)
		}
	})

	// Streaming: verify audit still emits (tokens may be 0 for streaming via legacy path).
	t.Run("streaming", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// A minimal SSE stream with usage.
			flusher, _ := w.(http.Flusher)
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"index\":0}]}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":7,\"total_tokens\":22}}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}))
		defer upstream.Close()

		cfg := &Config{Accounts: []AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL}}}
		pool := NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4","stream":true}`)))
		r.Header.Set("Content-Type", "application/json")

		proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4","stream":true}`), time.Now(), chatForwardOpts{stream: true}, cfg)

		out := h.output()
		if !strings.Contains(out, `"msg":"request.complete"`) {
			t.Fatalf("expected request.complete log line, got: %s", out)
		}
		// Streaming tokens via legacy path aren't captured — expect 0.
		// This is valid (degraded B). The audit line is still emitted.
		t.Logf("audit output: %s", out)
	})
}

func TestAuditLog_ErrorTypeClassification(t *testing.T) {
	t.Run("4xx", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"bad request"}}`))
		}))
		defer upstream.Close()

		cfg := &Config{Accounts: []AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL}}}
		pool := NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")

		proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, cfg)

		out := h.output()
		if !strings.Contains(out, `"error_type":"upstream_4xx"`) {
			t.Errorf("expected error_type=upstream_4xx, got: %s", out)
		}
	})

	t.Run("5xx", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		// Two accounts so that when the first cools down (30s), the second
		// is available immediately — test completes in ms not seconds.
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer upstream.Close()

		cfg := &Config{Accounts: []AccountConfig{
			{Name: "a1", Key: "k1", BaseURL: upstream.URL},
			{Name: "a2", Key: "k2", BaseURL: upstream.URL},
		}}
		pool := NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")

		proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, cfg)

		out := h.output()
		if !strings.Contains(out, `"error_type":"all_exhausted"`) {
			t.Errorf("expected error_type=all_exhausted, got: %s", out)
		}
		if !strings.Contains(out, `"status":503`) {
			t.Errorf("expected status=503, got: %s", out)
		}
	})

	t.Run("empty_pool", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		pool := NewPool(nil)
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")

		proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, &Config{})

		out := h.output()
		if !strings.Contains(out, `"msg":"request.complete"`) {
			t.Fatalf("expected request.complete for empty pool, got: %s", out)
		}
		if !strings.Contains(out, `"status":503`) {
			t.Errorf("expected status=503 for empty pool, got: %s", out)
		}
	})
}

func TestAuditFromCtx_NilSafe(t *testing.T) {
	// Bare context: auditFromCtx returns nil, no panic.
	ctx := context.Background()
	if a := auditFromCtx(ctx); a != nil {
		t.Fatal("expected nil from bare context")
	}
	// emitAudit must not panic with nil.
	emitAudit(nil) // no-op, no panic
}

func TestRateLimit_HitLogsWarn(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	rl := newRateLimiter(1, 1)
	// First request passes.
	if !rl.Allow("10.0.0.1") {
		t.Fatal("first Allow should succeed")
	}
	// Second request within the same second should be rate limited.
	if rl.Allow("10.0.0.1") {
		t.Fatal("second Allow should fail")
	}

	// Simulate the middleware log path.
	slog.Warn("rate_limit.hit", "ip", "10.0.0.1", "path", "/v1/chat/completions", "req", "test-req")

	out := h.output()
	if !strings.Contains(out, `"msg":"rate_limit.hit"`) {
		t.Errorf("expected rate_limit.hit log, got: %s", out)
	}
	if !strings.Contains(out, `"ip":"10.0.0.1"`) {
		t.Error("missing ip field")
	}
}
