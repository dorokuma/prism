package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

// ---------------------------------------------------------------------------
// Round-3 audit: copyClientHeaders forwarding-header strip (item 7)
// ---------------------------------------------------------------------------

// TestCopyClientHeaders_StripsForwardingHeaders pins item 7: Forwarded,
// every X-Forwarded-* header and X-Real-IP must never reach the upstream
// (a client could fake its origin IP or route), while normal business
// headers still pass through unchanged.
func TestCopyClientHeaders_StripsForwardingHeaders(t *testing.T) {
	src := http.Header{
		"Content-Type":      {"application/json"},
		"X-Custom-Biz":      {"keep-me"},
		"Forwarded":         {"for=192.0.2.60;proto=http"},
		"X-Forwarded-For":   {"203.0.113.9"},
		"X-Forwarded-Proto": {"https"},
		"X-Forwarded-Host":  {"evil.example"},
		"X-Real-IP":         {"198.51.100.7"},
	}
	dst := make(http.Header)
	copyClientHeaders(dst, src)

	// Business headers pass through.
	if dst.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type should pass through, got %q", dst.Get("Content-Type"))
	}
	if dst.Get("X-Custom-Biz") != "keep-me" {
		t.Errorf("X-Custom-Biz should pass through, got %q", dst.Get("X-Custom-Biz"))
	}
	// Forwarding headers are stripped (canonical-name and prefix checks).
	for k := range dst {
		ck := http.CanonicalHeaderKey(k)
		if ck == "Forwarded" || ck == "X-Real-IP" || strings.HasPrefix(ck, "X-Forwarded-") {
			t.Errorf("forwarding header %q was copied to the upstream", k)
		}
	}
	if len(dst) != 2 {
		t.Errorf("dst has %d headers, want exactly 2 (Content-Type, X-Custom-Biz): %v", len(dst), dst)
	}
}

// ---------------------------------------------------------------------------
// Round-3 audit: connection-error logs must not leak URL/query (item 10)
// ---------------------------------------------------------------------------

// TestDoUpstreamRequest_ConnErrorLogNoURLLeak pins item 10 for the runtime
// path: a connection failure produces a *url.Error whose text embeds the
// full upstream URL INCLUDING the query string. The log line must carry only
// error_type and safe fields — never the raw error, the URL, or the secret.
func TestDoUpstreamRequest_ConnErrorLogNoURLLeak(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	// A closed listener so the client gets a real connection-refused
	// *url.Error with the full URL (base + path + ?key=secret).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	ln.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "test", Key: "k", BaseURL: "http://" + deadAddr, Provider: "test"},
	}}
	p := pool.NewPool(cfg.Accounts)
	acc := p.AllAccounts()[0]

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions?key=secret-query-val", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "test")

	res := doUpstreamRequest(acc, r, body, ChatForwardOpts{Model: "gpt-4"}, "req-conn-1")
	if !res.retry {
		t.Fatal("connection failure must be retryable")
	}

	out := h.output()
	if !strings.Contains(out, `"msg":"chat retry, upstream connection error"`) {
		t.Fatalf("expected connection-error log line, got: %s", out)
	}
	if !strings.Contains(out, `"error_type":"upstream_refused"`) {
		t.Errorf("expected error_type upstream_refused, got: %s", out)
	}
	for _, leak := range []string{deadAddr, "secret-query-val", "key=secret", "/chat/completions", "connection refused"} {
		if strings.Contains(out, leak) {
			t.Errorf("connection-error log leaks %q: %s", leak, out)
		}
	}
}

// ---------------------------------------------------------------------------
// Final-review audit: rejection audits carry the REAL request duration
// ---------------------------------------------------------------------------

// auditDurationMS extracts the duration_ms value of the first
// request.complete line from a captured log.
func auditDurationMS(t *testing.T, out string) float64 {
	t.Helper()
	const key = `"duration_ms":`
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, `"msg":"request.complete"`) {
			continue
		}
		idx := strings.Index(line, key)
		if idx < 0 {
			continue
		}
		rest := line[idx+len(key):]
		end := strings.IndexByte(rest, ',')
		if end < 0 {
			end = len(rest)
		}
		if v, err := strconv.ParseFloat(rest[:end], 64); err == nil {
			return v
		}
	}
	t.Fatalf("no request.complete duration_ms found in: %s", out)
	return 0
}

// slowBodyReader wraps a reader and sleeps per Read call, making a body
// read measurably slow so the rejection audit's duration is meaningful.
type slowBodyReader struct {
	r   io.Reader
	per time.Duration
}

func (s *slowBodyReader) Read(p []byte) (int, error) {
	time.Sleep(s.per)
	return s.r.Read(p)
}

// TestRejectAudit_UsesRealStart pins the final-review fix at the unit
// level: rejectAudit must measure duration from the request's REAL start
// time, not from a time.Now() taken at rejection time (which would always
// be ~0 even after a slow body read).
func TestRejectAudit_UsesRealStart(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	start := time.Now()
	time.Sleep(30 * time.Millisecond) // simulated request processing before the rejection
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "audit-dur-1")
	r = r.WithContext(ctx)

	rejectAudit(r, start, http.StatusBadRequest, "invalid_request", "m", "boom")

	if got := auditDurationMS(t, h.output()); got < 20 {
		t.Errorf("rejection duration_ms = %v, want >= 20 (must be measured from the real request start)", got)
	}
}

// TestChat_ReadRejectionAuditRealDuration is the end-to-end pin: a 413
// rejection AFTER a slow body read must report the full duration (the read
// is part of the request), not a near-zero value. Before the fix
// readRequestBody passed time.Now() into rejectAudit, so the duration was
// always ~0 regardless of how long the body read took.
func TestChat_ReadRejectionAuditRealDuration(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	cfg := &config.Config{}
	p := pool.NewPool(nil)

	big := bytes.Repeat([]byte("a"), maxRequestBodyBytes+1)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", &slowBodyReader{r: bytes.NewReader(big), per: 2 * time.Millisecond})
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "audit-dur-2")
	r = r.WithContext(ctx)

	proxyChat(p, rec, r, cfg)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if got := auditDurationMS(t, h.output()); got < 15 {
		t.Errorf("rejection duration_ms = %v, want >= 15 (the slow body read must be counted)", got)
	}
}

// ---------------------------------------------------------------------------
// Round-3 audit: /v1/responses early-rejection audits (item 6)
// ---------------------------------------------------------------------------

// countAuditLines returns how many request.complete lines the capture holds.
func countAuditLines(out string) int {
	return strings.Count(out, `"msg":"request.complete"`)
}

// TestResponses_ConvertRejectionSingleAudit: a /v1/responses request that
// fails conversion (previous_response_id) must produce EXACTLY ONE
// request.complete audit with the correct status, error_type, model and
// request id — the forwarding path is never reached, so no second audit can
// fire.
func TestResponses_ConvertRejectionSingleAudit(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	cfg := &config.Config{}
	p := pool.NewPool(nil)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4","input":"hi","previous_response_id":"abc"}`)))
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "audit-responses-1")
	r = r.WithContext(ctx)

	proxyResponses(p, rec, r, cfg)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	out := h.output()
	if n := countAuditLines(out); n != 1 {
		t.Fatalf("request.complete lines = %d, want exactly 1: %s", n, out)
	}
	for _, want := range []string{
		`"status":400`,
		`"error_type":"previous_response_id"`,
		`"model":"gpt-4"`,
		`"req":"audit-responses-1"`,
		`"success":false`,
		`"path":"/v1/responses"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit missing %s: %s", want, out)
		}
	}
}

func TestResponses_ItemReferenceRejectionClassified(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	cfg := &config.Config{}
	p := pool.NewPool(nil)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4","input":[{"type":"item_reference","id":"msg_1"}]}`)))
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "audit-item-ref-1")
	r = r.WithContext(ctx)

	proxyResponses(p, rec, r, cfg)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	out := h.output()
	if !strings.Contains(out, `"error_type":"item_reference"`) {
		t.Errorf("audit missing item_reference: %s", out)
	}
}

// TestResponses_ReadRejectionSingleAudit: a /v1/responses body over the cap
// (readRequestBody failure) must also produce exactly one audit — status 413
// with error_type request_too_large.
func TestResponses_ReadRejectionSingleAudit(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	cfg := &config.Config{}
	p := pool.NewPool(nil)

	big := bytes.Repeat([]byte("a"), maxRequestBodyBytes+1)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(big))
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "audit-responses-2")
	r = r.WithContext(ctx)

	proxyResponses(p, rec, r, cfg)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	out := h.output()
	if n := countAuditLines(out); n != 1 {
		t.Fatalf("request.complete lines = %d, want exactly 1: %s", n, out)
	}
	for _, want := range []string{
		`"status":413`,
		`"error_type":"request_too_large"`,
		`"req":"audit-responses-2"`,
		`"success":false`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit missing %s: %s", want, out)
		}
	}
}

// TestResponses_SuccessSingleAudit: the success path must NOT double-audit —
// readRequestBody and the conversion emit nothing on success, and the
// forwarding path emits exactly one request.complete line.
func TestResponses_SuccessSingleAudit(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL, Provider: "test"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4","input":"hi"}`)))
	r.Header.Set("X-Prism-Provider", "test")
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "audit-responses-3")
	r = r.WithContext(ctx)

	proxyResponses(p, rec, r, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	out := h.output()
	if n := countAuditLines(out); n != 1 {
		t.Fatalf("request.complete lines = %d, want exactly 1 (no double audit): %s", n, out)
	}
	if !strings.Contains(out, `"req":"audit-responses-3"`) {
		t.Errorf("audit missing request id: %s", out)
	}
}

// TestChat_ReadRejectionAudited: the shared readRequestBody now audits for
// every surface — the chat path's 413 must carry exactly one audit too, so
// the three POST surfaces stay consistent.
func TestChat_ReadRejectionAudited(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	cfg := &config.Config{}
	p := pool.NewPool(nil)

	big := bytes.Repeat([]byte("a"), maxRequestBodyBytes+1)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(big))
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "audit-chat-1")
	r = r.WithContext(ctx)

	proxyChat(p, rec, r, cfg)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	out := h.output()
	if n := countAuditLines(out); n != 1 {
		t.Fatalf("request.complete lines = %d, want exactly 1: %s", n, out)
	}
	if !strings.Contains(out, `"error_type":"request_too_large"`) {
		t.Errorf("audit missing error_type: %s", out)
	}
}

// errBodyReader fails the body read with a non-MaxBytes error, forcing the
// readRequestBody 400 invalid_request branch (a size violation would hit
// the 413 branch instead).
type errBodyReader struct{}

func (errBodyReader) Read([]byte) (int, error) { return 0, errors.New("simulated body read failure") }

// TestMessages_ReadRejectionSingleAudit completes the chat/responses/
// messages trio for the shared readRequestBody early-rejection audit: a
// /v1/messages body over the cap must produce EXACTLY ONE request.complete
// audit — status 413, error_type request_too_large, empty model (the body
// could not be parsed), the request's id and path, success false — and the
// response envelope must carry error.code request_too_large. Because
// proxyMessages returns before the forwarding path, the single audit proves
// the messages surface cannot double-audit with chat/responses: the same
// shared rejection path is audited exactly once per surface.
func TestMessages_ReadRejectionSingleAudit(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	cfg := &config.Config{}
	p := pool.NewPool(nil)

	big := bytes.Repeat([]byte("a"), maxRequestBodyBytes+1)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(big))
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "audit-messages-1")
	r = r.WithContext(ctx)

	proxyMessages(p, rec, r, cfg)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"request_too_large"`) {
		t.Errorf("response missing error.code request_too_large: %s", rec.Body.String())
	}
	out := h.output()
	if n := countAuditLines(out); n != 1 {
		t.Fatalf("request.complete lines = %d, want exactly 1 (no double audit): %s", n, out)
	}
	for _, want := range []string{
		`"status":413`,
		`"error_type":"request_too_large"`,
		`"model":""`,
		`"req":"audit-messages-1"`,
		`"path":"/v1/messages"`,
		`"success":false`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit missing %s: %s", want, out)
		}
	}
}

// TestMessages_ReadErrorRejectionSingleAudit pins the 400 flavor of the
// /v1/messages early-body rejection: a body read that fails with a
// non-size error must produce exactly one request.complete audit — status
// 400, error_type invalid_request, error.code invalid_request in the
// response — and never a second audit from the forwarding path.
func TestMessages_ReadErrorRejectionSingleAudit(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	cfg := &config.Config{}
	p := pool.NewPool(nil)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages", errBodyReader{})
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "audit-messages-2")
	r = r.WithContext(ctx)

	proxyMessages(p, rec, r, cfg)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"invalid_request"`) {
		t.Errorf("response missing error.code invalid_request: %s", rec.Body.String())
	}
	out := h.output()
	if n := countAuditLines(out); n != 1 {
		t.Fatalf("request.complete lines = %d, want exactly 1 (no double audit): %s", n, out)
	}
	for _, want := range []string{
		`"status":400`,
		`"error_type":"invalid_request"`,
		`"model":""`,
		`"req":"audit-messages-2"`,
		`"path":"/v1/messages"`,
		`"success":false`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit missing %s: %s", want, out)
		}
	}
}

// TestResponses_UnknownModelConversionErrorAudited: an empty-input
// conversion rejection (a plain invalid_request) must also be audited with
// the model field filled.
func TestResponses_UnknownModelConversionErrorAudited(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	cfg := &config.Config{}
	p := pool.NewPool(nil)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "audit-responses-4")
	r = r.WithContext(ctx)

	proxyResponses(p, rec, r, cfg)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	out := h.output()
	if n := countAuditLines(out); n != 1 {
		t.Fatalf("request.complete lines = %d, want exactly 1: %s", n, out)
	}
	if !strings.Contains(out, `"error_type":"invalid_request"`) || !strings.Contains(out, `"model":"gpt-4"`) {
		t.Errorf("audit missing invalid_request classification or model: %s", out)
	}
}
