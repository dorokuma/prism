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

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://upstream.invalid", Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)
	acc, err := p.SelectByProvider(context.Background(), 1, "t")
	if err != nil {
		t.Fatalf("select account: %v", err)
	}
	defer p.Release(acc)

	aud := &middleware.RequestAudit{Req: "direct-commit-1", Method: "POST", Path: "/v1/responses", Model: "gpt-4"}
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r = r.WithContext(context.WithValue(r.Context(), middleware.AuditKey{}, aud))

	ctx, cancel := context.WithCancel(r.Context())
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}
	done, _, _ := handleUpstreamResponse(acc, w, r, resp, nil, time.Now(), ChatForwardOpts{ResponsesOut: true, Stream: true}, "direct-commit-1", ctx, cancel)
	if !done {
		t.Fatal("handleUpstreamResponse must report done for a terminal streaming response")
	}
	return aud
}

// TestHandleUpstreamResponse_StreamPreFirstEventFailure502_NonConcreteWrapper
// proves the explicit commit-state source at the bypass level: with a
// NON-*StatusCapture writer implementing responseCommitWriter, a failure
// before the first event (empty upstream stream) returns a structured 502 —
// never an empty 200 — the status is written exactly once, and the audit
// records 502.
func TestHandleUpstreamResponse_StreamPreFirstEventFailure502_NonConcreteWrapper(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}

	aud := directResponsesStream(t, "", w) // empty stream: no event was written

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (pre-first-event failure must not become an empty 200)", rec.Code)
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_stream_error" {
		t.Errorf("error code = %q, want upstream_stream_error", code)
	}
	if w.headerCalls != 1 {
		t.Errorf("WriteHeader must be called exactly once, got %d calls", w.headerCalls)
	}
	if w.code != http.StatusBadGateway {
		t.Errorf("commit wrapper must record 502, got %d", w.code)
	}
	if !w.Committed() {
		t.Error("commit wrapper must report committed after the 502")
	}
	if aud.Status != http.StatusBadGateway {
		t.Errorf("audit status = %d, want 502", aud.Status)
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
