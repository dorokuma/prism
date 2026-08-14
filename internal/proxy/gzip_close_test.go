package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/pool"
)

// ---------------------------------------------------------------------------
// Final review: body-reader close lifecycle on the non-streaming gzip paths
// ---------------------------------------------------------------------------

// closeCountReadCloser counts Close calls on an underlying reader — the
// observable proof that handleUpstreamResponse closes resp.Body exactly
// once (its own top-level defer) and that closeUpstreamBodyReader never
// double-closes it.
type closeCountReadCloser struct {
	io.Reader
	mu     sync.Mutex
	closes int
}

func (c *closeCountReadCloser) Read(p []byte) (int, error) { return c.Reader.Read(p) }
func (c *closeCountReadCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return nil
}
func (c *closeCountReadCloser) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// directNonStreamResp drives handleUpstreamResponse directly (bypassing
// proxyChatWithBody) on a NON-streaming path — the legacy chat passthrough
// (ResponsesOut=false) or the responses translation (ResponsesOut=true) —
// with a synthetic upstream response. handleUpstreamResponse owns and closes
// resp.Body.
func directNonStreamResp(t *testing.T, resp *http.Response, w responseCommitWriter, opts ChatForwardOpts) *middleware.RequestAudit {
	t.Helper()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://upstream.invalid", Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)
	acc, slot, err := p.SelectByProvider(context.Background(), "gpt-4", 1, "t")
	if err != nil {
		t.Fatalf("select account: %v", err)
	}
	defer p.Release(slot)

	aud := &middleware.RequestAudit{Req: "direct-nonstream-1", Method: "POST", Path: "/v1/chat/completions", Model: "gpt-4"}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r = r.WithContext(context.WithValue(r.Context(), middleware.AuditKey{}, aud))

	ctx, cancel := context.WithCancel(r.Context())
	done, _, class := handleUpstreamResponse(acc, w, r, resp, nil, time.Now(), opts, "direct-nonstream-1", ctx, cancel)
	if !done {
		t.Fatalf("handleUpstreamResponse must report done, class=%d", class)
	}
	return aud
}

// TestCloseUpstreamBodyReader pins the close helper's double-close
// semantics: the identity reader (resp.Body itself) is the caller's to close
// (its top-level defer owns it) — the helper must NOT close it; any other
// reader (a gzip reader) must be closed exactly once and must not touch the
// underlying resp.Body; nil inputs are safe.
func TestCloseUpstreamBodyReader(t *testing.T) {
	// Identity: r == resp.Body → no close here.
	body := &closeCountReadCloser{Reader: strings.NewReader("x")}
	closeUpstreamBodyReader(&http.Response{Body: body}, body)
	if body.closeCount() != 0 {
		t.Errorf("identity reader closed %d times, want 0 (the caller's top-level defer owns resp.Body)", body.closeCount())
	}

	// Non-identity (what a gzip reader looks like to the helper): closed
	// exactly once; the underlying resp.Body stays untouched.
	underlying := &closeCountReadCloser{Reader: strings.NewReader("y")}
	other := &closeCountReadCloser{Reader: strings.NewReader("z")}
	closeUpstreamBodyReader(&http.Response{Body: underlying}, other)
	if other.closeCount() != 1 {
		t.Errorf("non-identity reader closed %d times, want 1", other.closeCount())
	}
	if underlying.closeCount() != 0 {
		t.Errorf("closing the gzip reader must not close the underlying resp.Body, got %d closes", underlying.closeCount())
	}

	// nil-safe.
	closeUpstreamBodyReader(nil, nil)
	closeUpstreamBodyReader(&http.Response{Body: body}, nil)
	closeUpstreamBodyReader(nil, other)
}

// TestUpstreamGzipNonStreamClosesBodyOnce: the legacy non-streaming gzip
// path closes the decompressed body reader after the read — resp.Body is
// closed exactly once (the handler's own defer), never twice, and the
// client still receives the DECODED body.
func TestUpstreamGzipNonStreamClosesBodyOnce(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`
	tracked := &closeCountReadCloser{Reader: bytes.NewReader(gzipBody(t, body))}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": {"gzip"}},
		Body:       tracked,
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	aud := directNonStreamResp(t, resp, w, ChatForwardOpts{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("body = %q, want the DECODED upstream body", got)
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want empty (decoded body must not be labeled gzip)", ce)
	}
	if tracked.closeCount() != 1 {
		t.Errorf("resp.Body closed %d times, want exactly 1 (top-level defer; no double close)", tracked.closeCount())
	}
	if aud.Status != http.StatusOK {
		t.Errorf("audit status = %d, want 200", aud.Status)
	}
}

// TestUpstreamGzipResponsesOutNonStreamClosesBodyOnce: the /v1/responses
// non-streaming gzip path gets the same lifecycle — decoded body reaches the
// client, resp.Body is closed exactly once.
func TestUpstreamGzipResponsesOutNonStreamClosesBodyOnce(t *testing.T) {
	tracked := &closeCountReadCloser{Reader: bytes.NewReader(gzipBody(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": {"gzip"}},
		Body:       tracked,
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	aud := directNonStreamResp(t, resp, w, ChatForwardOpts{ResponsesOut: true})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var parsed struct {
		Output []any `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("client body is not valid responses JSON: %v", err)
	}
	if len(parsed.Output) == 0 {
		t.Errorf("responses output missing: %s", rec.Body.String())
	}
	if tracked.closeCount() != 1 {
		t.Errorf("resp.Body closed %d times, want exactly 1 (top-level defer; no double close)", tracked.closeCount())
	}
	if aud.Status != http.StatusOK {
		t.Errorf("audit status = %d, want 200", aud.Status)
	}
}

// TestUpstreamGzipNonStreamTruncated502ClosesBody: a truncated gzip body
// fails the read; the handler answers a structured 502, classifies the
// truncation as upstream_error (NOT upstream_refused — the upstream already
// answered, a refusal would be a lie) and still closes the body reader
// exactly once: the read-error path must not leak the decompressor.
func TestUpstreamGzipNonStreamTruncated502ClosesBody(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	full := gzipBody(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	truncated := full[:len(full)-4] // 4 of the 8 trailer bytes missing → read ends with io.ErrUnexpectedEOF
	tracked := &closeCountReadCloser{Reader: bytes.NewReader(truncated)}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": {"gzip"}},
		Body:       tracked,
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	aud := directNonStreamResp(t, resp, w, ChatForwardOpts{})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (truncated gzip must fail closed, not pass partial bytes); body=%s", rec.Code, rec.Body.String())
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_error" {
		t.Errorf("error code = %q, want upstream_error", code)
	}
	if aud.ErrorType != "upstream_error" {
		t.Errorf("audit error_type = %q, want upstream_error (truncation is not upstream_refused)", aud.ErrorType)
	}
	if tracked.closeCount() != 1 {
		t.Errorf("resp.Body closed %d times, want exactly 1 (close must happen on the read-error path too)", tracked.closeCount())
	}
}

// TestUpstreamGzipResponsesOutNonStreamTruncated502ClosesBody: the
// /v1/responses non-streaming path gets the same fail-closed + close-on-error
// treatment.
func TestUpstreamGzipResponsesOutNonStreamTruncated502ClosesBody(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	full := gzipBody(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	truncated := full[:len(full)-4]
	tracked := &closeCountReadCloser{Reader: bytes.NewReader(truncated)}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": {"gzip"}},
		Body:       tracked,
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	aud := directNonStreamResp(t, resp, w, ChatForwardOpts{ResponsesOut: true})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (truncated gzip must fail closed); body=%s", rec.Code, rec.Body.String())
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_error" {
		t.Errorf("error code = %q, want upstream_error", code)
	}
	if aud.ErrorType != "upstream_error" {
		t.Errorf("audit error_type = %q, want upstream_error (truncation is not upstream_refused)", aud.ErrorType)
	}
	if tracked.closeCount() != 1 {
		t.Errorf("resp.Body closed %d times, want exactly 1 (close must happen on the read-error path too)", tracked.closeCount())
	}
}

// TestUpstreamGzip4xxClosesBodyOnce: the gzip-encoded 4xx passthrough path
// reads the decompressed error body and closes the body reader after the
// read — resp.Body is closed exactly once and the redacted decoded body
// still reaches the client.
func TestUpstreamGzip4xxClosesBodyOnce(t *testing.T) {
	errBody := `{"error":{"message":"forbidden"}}`
	tracked := &closeCountReadCloser{Reader: bytes.NewReader(gzipBody(t, errBody))}
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Encoding": {"gzip"}},
		Body:       tracked,
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	directNonStreamResp(t, resp, w, ChatForwardOpts{})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != errBody {
		t.Errorf("body = %q, want the DECODED redacted 4xx body %q", got, errBody)
	}
	if tracked.closeCount() != 1 {
		t.Errorf("resp.Body closed %d times, want exactly 1 (top-level defer; no double close)", tracked.closeCount())
	}
}

// TestUpstreamGzip4xxTruncatedClosesBody: a truncated gzip 4xx body fails
// the read; the 4xx passthrough keeps its status, and the body reader is
// still closed exactly once (close on the read-error path).
func TestUpstreamGzip4xxTruncatedClosesBody(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	full := gzipBody(t, `{"error":{"message":"forbidden"}}`)
	truncated := full[:len(full)-4]
	tracked := &closeCountReadCloser{Reader: bytes.NewReader(truncated)}
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Encoding": {"gzip"}},
		Body:       tracked,
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	directNonStreamResp(t, resp, w, ChatForwardOpts{})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (4xx passthrough keeps its status despite the read error)", rec.Code)
	}
	if tracked.closeCount() != 1 {
		t.Errorf("resp.Body closed %d times, want exactly 1 (close must happen on the read-error path too)", tracked.closeCount())
	}
}

// TestUpstreamNonStreamIdentityBodyClosedOnce: with NO Content-Encoding the
// body reader IS resp.Body (identity) — closeUpstreamBodyReader must skip
// it, and the handler's top-level defer closes resp.Body exactly once. A
// double close would surface here as closeCount()==2.
func TestUpstreamNonStreamIdentityBodyClosedOnce(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`
	tracked := &closeCountReadCloser{Reader: strings.NewReader(body)}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       tracked,
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	directNonStreamResp(t, resp, w, ChatForwardOpts{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if tracked.closeCount() != 1 {
		t.Errorf("resp.Body closed %d times, want exactly 1 (identity reader must not be double-closed)", tracked.closeCount())
	}
}

// ---------------------------------------------------------------------------
// Final review: pre-first-event 502 must not carry SSE-only headers
// ---------------------------------------------------------------------------

// TestResponsesStream_PreFirstEventEmpty502ClearsSSEHeaders: a pre-first-
// event 502 (empty upstream stream) must not carry the SSE-only headers
// (Cache-Control: no-cache, Connection: keep-alive) that the streaming
// branch set for the stream that never started — the JSON error response's
// headers must describe the error, not a stream.
func TestResponsesStream_PreFirstEventEmpty502ClearsSSEHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	directResponsesStream(t, "", w)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control = %q, want cleared on the 502 (SSE-only header)", got)
	}
	if got := rec.Header().Get("Connection"); got != "" {
		t.Errorf("Connection = %q, want cleared on the 502 (SSE-only header)", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (must not stay text/event-stream)", got)
	}
}

// TestResponsesStream_PreFirstEventTruncatedGzip502ClearsSSEHeaders pins the
// generic pre-first-event 502 branch: a gzip stream that corrupts before any
// event (header valid, payload truncated) fails the very first read; the
// JSON 502 must carry neither the SSE headers nor the stream content type,
// and the audit classifies the truncation as upstream_error — not
// upstream_refused.
func TestResponsesStream_PreFirstEventTruncatedGzip502ClearsSSEHeaders(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	full := gzipBody(t, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
	// Keep only the 10-byte gzip header plus a few payload bytes: the
	// header check passes at construction, the very first read fails with a
	// decompression error — the deterministic "corrupts before any event"
	// case (a trailer-only cut would surface AFTER the first event, i.e.
	// mid-stream with a committed 200).
	truncated := full[:14]
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": {"gzip"}},
		Body:       io.NopCloser(bytes.NewReader(truncated)),
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	aud := directResponsesStreamResp(t, resp, w)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (corrupt gzip payload must fail closed); body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control = %q, want cleared on the 502 (SSE-only header)", got)
	}
	if got := rec.Header().Get("Connection"); got != "" {
		t.Errorf("Connection = %q, want cleared on the 502 (SSE-only header)", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (must not stay text/event-stream)", got)
	}
	if aud.Status != http.StatusBadGateway {
		t.Errorf("audit status = %d, want 502", aud.Status)
	}
	if aud.ErrorType != "upstream_error" {
		t.Errorf("audit error_type = %q, want upstream_error (gzip truncation is not upstream_refused)", aud.ErrorType)
	}
}
