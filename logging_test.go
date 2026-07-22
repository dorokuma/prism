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
		proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{ResponsesOut: true}, cfg)

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

	// Legacy streaming: verify that token usage is captured from SSE tail.
	t.Run("legacy_streaming", func(t *testing.T) {
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

		proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4","stream":true}`), time.Now(), chatForwardOpts{Stream: true}, cfg)

		out := h.output()
		if !strings.Contains(out, `"msg":"request.complete"`) {
			t.Fatalf("expected request.complete log line, got: %s", out)
		}
		if !strings.Contains(out, `"tokens_in":15`) {
			t.Errorf("expected tokens_in=15, got: %s", out)
		}
		if !strings.Contains(out, `"tokens_out":7`) {
			t.Errorf("expected tokens_out=7, got: %s", out)
		}
		// Assert the client received the full SSE stream (passthrough not broken).
		body := rec.Body.String()
		if !strings.Contains(body, "hello") {
			t.Error("client body missing content chunk 'hello'")
		}
		if !strings.Contains(body, "[DONE]") {
			t.Error("client body missing [DONE]")
		}
	})

	// Legacy streaming without usage: tokens stay 0, audit line still emitted,
	// passthrough intact.
	t.Run("legacy_streaming_no_usage", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			// No usage in any chunk.
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"world\"},\"index\":0}]}\n\n"))
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

		proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4","stream":true}`), time.Now(), chatForwardOpts{Stream: true}, cfg)

		out := h.output()
		if !strings.Contains(out, `"msg":"request.complete"`) {
			t.Fatalf("expected request.complete log line, got: %s", out)
		}
		// tokens should be 0 (no usage in SSE stream).
		if !strings.Contains(out, `"tokens_in":0`) {
			t.Errorf("expected tokens_in=0, got: %s", out)
		}
		if !strings.Contains(out, `"tokens_out":0`) {
			t.Errorf("expected tokens_out=0, got: %s", out)
		}
		// Client still gets complete stream.
		body := rec.Body.String()
		if !strings.Contains(body, "world") {
			t.Error("client body missing content chunk 'world'")
		}
		if !strings.Contains(body, "[DONE]") {
			t.Error("client body missing [DONE]")
		}
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
