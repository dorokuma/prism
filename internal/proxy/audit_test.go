package proxy

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

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

// ---------------------------------------------------------------------------
// Audit log test helpers (duplicated from middleware/logging_test.go to avoid
// circular imports – the middleware package cannot import proxy).
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
func (h *capturingHandler) WithGroup(name string) slog.Handler       { return h }
func (h *capturingHandler) output() string                           { h.mu.Lock(); defer h.mu.Unlock(); return string(h.buf) }

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

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL, Provider: "test"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "test")
	// Inject a request ID to get a meaningful audit.req.
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "audit-test-1")
	r = r.WithContext(ctx)

	ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

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

		cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
		p := pool.NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Provider", "t")

		// Use responsesOut=true so handleUpstreamResponse goes through the
		// responses_json path which calls chatCompletionToResponse and captures usage.
		ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{ResponsesOut: true}, cfg)

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
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":20,"completion_tokens":3,"total_tokens":23}}`))
		}))
		defer upstream.Close()

		cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
		p := pool.NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Provider", "t")

		// Legacy path: responsesOut=false (default), non-streaming.
		ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

		out := h.output()
		if !strings.Contains(out, `"tokens_in":20`) {
			t.Errorf("expected tokens_in=20, got: %s", out)
		}
		if !strings.Contains(out, `"tokens_out":3`) {
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

		cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
		p := pool.NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4","stream":true}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Provider", "t")

		ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4","stream":true}`), time.Now(), ChatForwardOpts{Stream: true}, cfg)

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

		cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
		p := pool.NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4","stream":true}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Provider", "t")

		ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4","stream":true}`), time.Now(), ChatForwardOpts{Stream: true}, cfg)

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

		cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
		p := pool.NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Provider", "t")

		ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

		out := h.output()
		if !strings.Contains(out, `"error_type":"upstream_4xx"`) {
			t.Errorf("expected error_type=upstream_4xx, got: %s", out)
		}
	})

	t.Run("5xx", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		// Both accounts return 500, so each attempt cools the used account
		// down for upstreamCooldown (default 30s). With the default values
		// every retry's SelectByProvider would wait out the 30s cooldown,
		// making this subtest ~30s (and the package >120s overall). Shrink
		// both the cooldown and the select timeout to milliseconds: all four
		// attempts now drain in ~600ms and the error_type stays determinis-
		// tically all_exhausted (select timeout never fires).
		restoreCooldown := SetUpstreamCooldownForTest(10 * time.Millisecond)
		defer restoreCooldown()
		restoreSelect := SetAccountSelectTimeoutForTest(100 * time.Millisecond)
		defer restoreSelect()

		// Two accounts so that when the first cools down, the second
		// is available immediately — test completes in ms not seconds.
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer upstream.Close()

		cfg := &config.Config{Accounts: []config.AccountConfig{
			{Name: "a1", Key: "k1", BaseURL: upstream.URL, Provider: "a"},
			{Name: "a2", Key: "k2", BaseURL: upstream.URL, Provider: "a"},
		}}
		p := pool.NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Provider", "a")

		ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

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

		p := pool.NewPool(nil)
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Provider", "none")

		ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, &config.Config{})

		out := h.output()
		if !strings.Contains(out, `"msg":"request.complete"`) {
			t.Fatalf("expected request.complete for empty pool, got: %s", out)
		}
		if !strings.Contains(out, `"status":503`) {
			t.Errorf("expected status=503 for empty pool, got: %s", out)
		}
	})
}

// TestAuditLog_AnthropicNonStreamingUsage verifies that a non-streaming
// /v1/messages (Anthropic Messages API) response has its input/output/cache
// token usage captured. Regression for the bug where the OpenAI field names
// (prompt_tokens/completion_tokens) were looked up in an Anthropic body and
// everything came out as zero.
func TestAuditLog_AnthropicNonStreamingUsage(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_01XFDUDYJgAACzvnptvVoYEL","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":250,"output_tokens":40,"cache_creation_input_tokens":50,"cache_read_input_tokens":200}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL, Provider: "test"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "test")

	ProxyChatWithBody(p, rec, r, []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`), time.Now(), ChatForwardOpts{
		UpstreamPath: "/v1/messages",
		SkipSanitize: true,
	}, cfg)

	if rec.Code != 200 {
		t.Fatalf("expected status 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	out := h.output()
	// Legacy aliases stay populated.
	if !strings.Contains(out, `"tokens_in":250`) {
		t.Errorf("expected tokens_in=250 (input_tokens), got: %s", out)
	}
	if !strings.Contains(out, `"tokens_out":40`) {
		t.Errorf("expected tokens_out=40 (output_tokens), got: %s", out)
	}
	// v2 fields.
	if !strings.Contains(out, `"prompt_tokens":250`) {
		t.Errorf("expected prompt_tokens=250, got: %s", out)
	}
	if !strings.Contains(out, `"completion_tokens":40`) {
		t.Errorf("expected completion_tokens=40, got: %s", out)
	}
	if !strings.Contains(out, `"total_tokens":290`) {
		t.Errorf("expected total_tokens=290 (250+40), got: %s", out)
	}
	if !strings.Contains(out, `"cached_tokens":200`) {
		t.Errorf("expected cached_tokens=200 (cache_read_input_tokens), got: %s", out)
	}
	if !strings.Contains(out, `"cache_write_tokens":50`) {
		t.Errorf("expected cache_write_tokens=50 (cache_creation_input_tokens), got: %s", out)
	}
}

// TestAuditLog_AnthropicStreamingUsage verifies that a streaming /v1/messages
// response has input tokens captured from the stream-head message_start event
// and output tokens from the stream-tail message_delta event, with the SSE
// bytes passed through unchanged.
func TestAuditLog_AnthropicStreamingUsage(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	sse := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[],\"usage\":{\"input_tokens\":250,\"cache_creation_input_tokens\":50,\"cache_read_input_tokens\":200}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":40}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sse))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL, Provider: "test"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "test")

	ProxyChatWithBody(p, rec, r, []byte(`{"model":"claude-opus-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`), time.Now(), ChatForwardOpts{
		Stream:       true,
		UpstreamPath: "/v1/messages",
		SkipSanitize: true,
	}, cfg)

	if rec.Code != 200 {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	// SSE passed through byte-for-byte.
	if rec.Body.String() != sse {
		t.Errorf("SSE passthrough broken:\n got %q\nwant %q", rec.Body.String(), sse)
	}

	out := h.output()
	if !strings.Contains(out, `"tokens_in":250`) {
		t.Errorf("expected tokens_in=250 (message_start input_tokens), got: %s", out)
	}
	if !strings.Contains(out, `"tokens_out":40`) {
		t.Errorf("expected tokens_out=40 (message_delta output_tokens), got: %s", out)
	}
	if !strings.Contains(out, `"total_tokens":290`) {
		t.Errorf("expected total_tokens=290, got: %s", out)
	}
	if !strings.Contains(out, `"cached_tokens":200`) {
		t.Errorf("expected cached_tokens=200, got: %s", out)
	}
	if !strings.Contains(out, `"cache_write_tokens":50`) {
		t.Errorf("expected cache_write_tokens=50, got: %s", out)
	}
}

// TestAuditLog_Metadata verifies the proxy chat concurrency point fills
// Provider (resolved provider), Stream (request stream flag), KeyID (API key
// name from the auth middleware context) and Success (2xx && no error type).
func TestAuditLog_Metadata(t *testing.T) {
	t.Run("success_non_streaming", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}))
		defer upstream.Close()

		cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL, Provider: "test"}}}
		p := pool.NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Provider", "test")
		// Simulate the auth middleware having recorded the key NAME.
		r = r.WithContext(middleware.WithAPIKey(r.Context(), "key-1"))

		ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

		out := h.output()
		for _, want := range []string{`"provider":"test"`, `"stream":false`, `"key_id":"key-1"`, `"success":true`, `"status":200`} {
			if !strings.Contains(out, want) {
				t.Errorf("audit missing %s, got: %s", want, out)
			}
		}
	})

	t.Run("success_streaming", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
		}))
		defer upstream.Close()

		cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL, Provider: "test"}}}
		p := pool.NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4","stream":true}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Provider", "test")
		r = r.WithContext(middleware.WithAPIKey(r.Context(), "key-2"))

		ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4","stream":true}`), time.Now(), ChatForwardOpts{Stream: true}, cfg)

		out := h.output()
		for _, want := range []string{`"provider":"test"`, `"stream":true`, `"key_id":"key-2"`, `"success":true`} {
			if !strings.Contains(out, want) {
				t.Errorf("audit missing %s, got: %s", want, out)
			}
		}
	})

	t.Run("failure_4xx", func(t *testing.T) {
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"bad request"}}`))
		}))
		defer upstream.Close()

		cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL, Provider: "test"}}}
		p := pool.NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Provider", "test")
		r = r.WithContext(middleware.WithAPIKey(r.Context(), "key-3"))

		ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

		out := h.output()
		for _, want := range []string{`"provider":"test"`, `"stream":false`, `"key_id":"key-3"`, `"success":false`, `"error_type":"upstream_4xx"`, `"status":400`} {
			if !strings.Contains(out, want) {
				t.Errorf("audit missing %s, got: %s", want, out)
			}
		}
	})

	t.Run("provider_default_fallback", func(t *testing.T) {
		// No X-Prism-Provider header: cfg.DefaultProvider resolves the
		// provider and the audit must record the effective one.
		h := &capturingHandler{}
		restore := stashSlog(h)
		defer restore()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}))
		defer upstream.Close()

		cfg := &config.Config{
			Accounts:        []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL, Provider: "defprov"}},
			DefaultProvider: "defprov",
		}
		p := pool.NewPool(cfg.Accounts)

		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")

		ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

		out := h.output()
		if !strings.Contains(out, `"provider":"defprov"`) {
			t.Errorf("expected provider=defprov (default fallback), got: %s", out)
		}
	})
}
