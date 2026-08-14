package proxy

import (
	"context"
	"errors"
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

// directLegacyStream drives handleUpstreamResponse directly (bypassing
// proxyChatWithBody and its *middleware.StatusCapture) with a synthetic 200
// upstream response whose body is the given ReadCloser, on the LEGACY chat
// streaming path (ResponsesOut=false, Stream=true). It returns the audit the
// handler saw, so the test can assert the recorded status without depending
// on the audit logger.
func directLegacyStream(t *testing.T, body io.ReadCloser, w responseCommitWriter) *middleware.RequestAudit {
	t.Helper()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://upstream.invalid", Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)
	acc, slot, err := p.SelectByProvider(context.Background(), "gpt-4", 1, "t")
	if err != nil {
		t.Fatalf("select account: %v", err)
	}
	defer p.Release(slot)

	aud := &middleware.RequestAudit{Req: "direct-legacy-1", Method: "POST", Path: "/v1/chat/completions", Model: "gpt-4"}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r = r.WithContext(context.WithValue(r.Context(), middleware.AuditKey{}, aud))

	ctx, cancel := context.WithCancel(r.Context())
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       body,
	}
	done, _, class := handleUpstreamResponse(acc, w, r, resp, nil, time.Now(), ChatForwardOpts{ResponsesOut: false, Stream: true}, "direct-legacy-1", ctx, cancel)
	if !done {
		t.Fatalf("handleUpstreamResponse must report done, class=%d", class)
	}
	return aud
}

// errReadCloser fails on the FIRST Read with the given error — the
// simulation of an upstream stream that dies before delivering any byte.
type errReadCloser struct{ err error }

func (e *errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e *errReadCloser) Close() error             { return nil }

// TestLegacyStream_EmptyStream502 pins an upstream 200 whose body closes
// without a single SSE event: write a terminal 502 upstream_stream_error
// and do not leave the response uncommitted for another account.
func TestLegacyStream_EmptyStream502(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}

	aud := directLegacyStream(t, io.NopCloser(strings.NewReader("")), w) // empty stream: no event was written

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

// TestLegacyStream_PreFirstEventReadFailure502 pins the failure half: an
// upstream stream that errors on the very first read (before any byte
// reached the client) must answer a structured 502 upstream_stream_error
// with a correct audit — never an empty 200.
func TestLegacyStream_PreFirstEventReadFailure502(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}

	aud := directLegacyStream(t, &errReadCloser{err: errors.New("connection reset")}, w)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (pre-first-event read failure must not become an empty 200)", rec.Code)
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_stream_error" {
		t.Errorf("error code = %q, want upstream_stream_error", code)
	}
	if w.headerCalls != 1 {
		t.Errorf("WriteHeader must be called exactly once, got %d calls", w.headerCalls)
	}
	if aud.Status != http.StatusBadGateway {
		t.Errorf("audit status = %d, want 502", aud.Status)
	}
	if aud.ErrorType != "upstream_stream_error" {
		t.Errorf("audit error_type = %q, want upstream_stream_error", aud.ErrorType)
	}
	if !strings.Contains(aud.Error, "connection reset") {
		t.Errorf("audit error = %q, want the underlying read error", aud.Error)
	}
}

// TestLegacyStream_NormalStreamPassthrough pins the healthy-stream half of
// the fix: a normal SSE stream still passes through byte-for-byte with the
// upstream status, Flush keeps working (SSE streaming), and the audit
// records the committed 200.
func TestLegacyStream_NormalStreamPassthrough(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}

	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: [DONE]\n\n"
	aud := directLegacyStream(t, io.NopCloser(strings.NewReader(sse)), w)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (healthy stream keeps the upstream status)", rec.Code)
	}
	if rec.Body.String() != sse {
		t.Errorf("stream body = %q, want byte-for-byte passthrough %q", rec.Body.String(), sse)
	}
	if !rec.Flushed {
		t.Error("Flush must be forwarded to the client writer (SSE streaming must keep working)")
	}
	if w.headerCalls != 1 {
		t.Errorf("WriteHeader must be called exactly once (on the first event), got %d calls", w.headerCalls)
	}
	if aud.Status != http.StatusOK {
		t.Errorf("audit status = %d, want 200", aud.Status)
	}
	if aud.ErrorType != "" {
		t.Errorf("audit error_type = %q, want empty on success", aud.ErrorType)
	}
}
