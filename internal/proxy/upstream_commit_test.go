package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/stream"
)

// commitTrackWriter is a deliberately NON-*StatusCapture StatusCommitter
// wrapper: it tracks the first WriteHeader (or the implicit 200 from a
// successful/partial Write) with the same net/http semantics as
// middleware.StatusCapture, but as an independent type. It proves the
// streaming commit-state logic depends on the responseCommitWriter
// interface — never on the concrete middleware.StatusCapture type — and
// counts WriteHeader calls to pin the single-WriteHeader contract.
type commitTrackWriter struct {
	inner       http.ResponseWriter
	code        int
	headerCalls int
}

func (c *commitTrackWriter) Header() http.Header { return c.inner.Header() }

func (c *commitTrackWriter) WriteHeader(code int) {
	if c.code != 0 {
		return // first code wins (net/http semantics), no duplicate WriteHeader
	}
	c.code = code
	c.headerCalls++
	c.inner.WriteHeader(code)
}

func (c *commitTrackWriter) Write(b []byte) (int, error) {
	n, err := c.inner.Write(b)
	if c.code == 0 && n > 0 {
		c.code = http.StatusOK // a (partial) write commits the implicit 200
	}
	return n, err
}

func (c *commitTrackWriter) Committed() bool { return c.code != 0 }

func (c *commitTrackWriter) Flush() {
	if f, ok := c.inner.(http.Flusher); ok {
		f.Flush()
	}
}

// directResponsesStream drives handleUpstreamResponse directly (bypassing
// proxyChatWithBody and its *middleware.StatusCapture) with a synthetic 200
// upstream response whose body is the given SSE text. It returns the audit
// the handler saw, so the test can assert the recorded status without
// depending on the audit logger.
func directResponsesStream(t *testing.T, upstreamBody string, w responseCommitWriter) *middleware.RequestAudit {
	t.Helper()
	return directResponsesStreamResp(t, &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}, w)
}

// directResponsesStreamResp is the header/body-controllable variant of
// directResponsesStream: the caller supplies the full synthetic upstream
// response (e.g. to set Content-Encoding: gzip or a non-string body).
// handleUpstreamResponse owns and closes resp.Body.
func directResponsesStreamResp(t *testing.T, resp *http.Response, w responseCommitWriter) *middleware.RequestAudit {
	t.Helper()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://upstream.invalid", Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)
	acc, slot, err := p.SelectByProvider(context.Background(), "gpt-4", 1, "t")
	if err != nil {
		t.Fatalf("select account: %v", err)
	}
	defer p.Release(slot)

	aud := &middleware.RequestAudit{Req: "direct-commit-1", Method: "POST", Path: "/v1/responses", Model: "gpt-4"}
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r = r.WithContext(context.WithValue(r.Context(), middleware.AuditKey{}, aud))

	ctx, cancel := context.WithCancel(r.Context())
	done, _, class := handleUpstreamResponse(acc, w, r, resp, nil, time.Now(), ChatForwardOpts{ResponsesOut: true, Stream: true}, "direct-commit-1", ctx, cancel)
	if !done {
		t.Fatalf("handleUpstreamResponse must report done for a terminal streaming response, class=%d", class)
	}
	return aud
}

// TestHandleUpstreamResponse_EmptyStream502_NonConcreteWrapper proves an
// uncommitted empty Responses stream writes a terminal 502 and commits it
// so proxyChatWithBody cannot try another account.
func TestHandleUpstreamResponse_EmptyStream502_NonConcreteWrapper(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}

	aud := directResponsesStream(t, "", w) // empty stream: no event was written

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if code, msg := decodeErrorBody(t, rec.Body.String()); code != "upstream_stream_error" {
		t.Errorf("error code = %q, want upstream_stream_error", code)
	} else if msg != "upstream stream closed before any event" {
		t.Errorf("error message = %q, want empty-stream text", msg)
	}
	if w.headerCalls != 1 {
		t.Errorf("WriteHeader must be called exactly once, got %d calls", w.headerCalls)
	}
	if !w.Committed() {
		t.Error("empty stream must commit the 502 so the retry loop cannot switch accounts")
	}
	if aud.Status != http.StatusBadGateway {
		t.Errorf("audit status = %d, want 502", aud.Status)
	}
	if aud.ErrorType != "upstream_stream_error" {
		t.Errorf("audit error_type = %q, want upstream_stream_error", aud.ErrorType)
	}
}

// TestHandleUpstreamResponse_StreamPartialWriteKeepsCommittedStatus_NonConcreteWrapper
// proves the other half at the bypass level: a first event write that
// delivers SOME bytes and then fails IS committed (net/http cannot change
// the status after the first byte reached the wire), so the handler must
// NOT attempt a 502 over the committed response — no second WriteHeader, no
// error body — and the audit keeps the committed 200.
func TestHandleUpstreamResponse_StreamPartialWriteKeepsCommittedStatus_NonConcreteWrapper(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	rec := httptest.NewRecorder()
	failing := &failFirstWriteResponseWriter{inner: rec, partial: 2}
	w := &commitTrackWriter{inner: failing}

	aud := directResponsesStream(t, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n", w)

	if w.code != http.StatusOK {
		t.Fatalf("commit state must be the implicit 200 after a partial write, got %d", w.code)
	}
	if !w.Committed() {
		t.Fatal("a partial write must be committed (the status can never change after the first byte)")
	}
	if w.headerCalls != 0 {
		t.Errorf("no WriteHeader may reach the wire in the partial-write case, got %d calls", w.headerCalls)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("no error body may be written over a committed response, got %q", rec.Body.String())
	}
	if aud.Status != http.StatusOK {
		t.Errorf("audit must keep the committed 200 after a partial write, got %d", aud.Status)
	}
}

// TestHandleUpstreamResponse_StreamPreFirstEventTooLarge502 pins the
// pre-first-event mapping of ErrResponsesStreamTooLarge through the
// production handler path: when the accumulation cap is exceeded before any
// event was delivered, the proxy answers a structured 502 with the SAME
// diagnosable code the translator uses mid-stream (response_too_large), not
// the generic upstream_stream_error, and the audit records the code as the
// error_type.
func TestHandleUpstreamResponse_StreamPreFirstEventTooLarge502(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	// Production limits are 16 MiB per buffer / 32 MiB total — a pre-first
	// event overrun cannot be triggered with them (a single SSE line is
	// capped at 4 MiB), so the test shrinks the caps via the stream test
	// hook. The first (and only) delta already exceeds the TOTAL cap while
	// staying under the per-buffer cap.
	t.Cleanup(stream.SetResponsesStreamLimitsForTest(1<<20, 1000))

	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}

	body := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 2000) + "\"}}]}\n\n"
	aud := directResponsesStream(t, body, w)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (pre-first-event too-large must not become an empty 200)", rec.Code)
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "response_too_large" {
		t.Errorf("error code = %q, want response_too_large (same code as the mid-stream frame)", code)
	}
	if aud.Status != http.StatusBadGateway {
		t.Errorf("audit status = %d, want 502", aud.Status)
	}
	if aud.ErrorType != "response_too_large" {
		t.Errorf("audit error_type = %q, want response_too_large", aud.ErrorType)
	}
}

// TestHandleUpstreamResponse_StreamPreFirstEventLineTooLong502 pins the
// pre-first-event mapping of a single SSE line over the scanner cap through
// the production handler path: the proxy answers a structured 502 with the
// same diagnosable code the translator uses mid-stream
// (stream_line_too_long), not the generic upstream_stream_error.
func TestHandleUpstreamResponse_StreamPreFirstEventLineTooLong502(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	// One single line strictly over the production 4 MiB scanner cap; the
	// scanner fails on the very first line, so no event was delivered.
	body := "data: " + strings.Repeat("x", config.StreamScannerMaxBuf+1) + "\n\n"
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}

	aud := directResponsesStream(t, body, w)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (pre-first-event line-too-long must not become an empty 200)", rec.Code)
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "stream_line_too_long" {
		t.Errorf("error code = %q, want stream_line_too_long (same code as the mid-stream frame)", code)
	}
	if aud.Status != http.StatusBadGateway {
		t.Errorf("audit status = %d, want 502", aud.Status)
	}
	if aud.ErrorType != "stream_line_too_long" {
		t.Errorf("audit error_type = %q, want stream_line_too_long", aud.ErrorType)
	}
}
