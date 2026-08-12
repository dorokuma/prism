package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/mcp"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/sanitize"
	"github.com/dorokuma/prism/internal/util"
)

// ---------------------------------------------------------------------------
// Round-4 audit helpers
// ---------------------------------------------------------------------------

// upstream2xxTruncatedBody answers 200 with a declared Content-Length that
// is never satisfied, so the client-side body read fails with a real read
// error (io.ErrUnexpectedEOF) — the simulation of an upstream that dies
// mid-body after a 2xx status.
func upstream2xxTruncatedBody(t *testing.T, contentType string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":`))
	}))
}

// decodeErrorBody extracts the error envelope from a JSON error response.
func decodeErrorBody(t *testing.T, body string) (code, message string) {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return resp.Error.Code, resp.Error.Message
}

// ---------------------------------------------------------------------------
// Item 1: non-streaming upstream 2xx body-read failure → structured 502
// ---------------------------------------------------------------------------

// TestNonStream2xxBodyReadErrorReturns502Responses pins item 1 for the
// responses translation path: a 2xx whose body cannot be fully read must
// NOT return an empty 200 — the response is still uncommitted, so a
// structured 502 upstream_error is returned and the audit records 502.
func TestNonStream2xxBodyReadErrorReturns502Responses(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	upstream := upstream2xxTruncatedBody(t, "application/json")
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "bodyread-resp-1")
	r = r.WithContext(ctx)

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{ResponsesOut: true}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body read failure must not return an empty 200)", rec.Code)
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_error" {
		t.Errorf("error code = %q, want upstream_error", code)
	}
	out := h.output()
	if !strings.Contains(out, `"status":502`) {
		t.Errorf("audit must record status 502, got: %s", out)
	}
	if !strings.Contains(out, `"error_type":"upstream_refused"`) && !strings.Contains(out, `"error_type":"upstream_error"`) {
		t.Errorf("audit missing a classified error_type: %s", out)
	}
}

// TestNonStream2xxBodyReadErrorReturns502Legacy pins item 1 for the legacy
// chat path: same structured 502 + audit instead of an empty 200.
func TestNonStream2xxBodyReadErrorReturns502Legacy(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	upstream := upstream2xxTruncatedBody(t, "application/json")
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "bodyread-legacy-1")
	r = r.WithContext(ctx)

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body read failure must not return an empty 200)", rec.Code)
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_error" {
		t.Errorf("error code = %q, want upstream_error", code)
	}
	out := h.output()
	if !strings.Contains(out, `"status":502`) {
		t.Errorf("audit must record status 502, got: %s", out)
	}
	if countAuditLines(out) != 1 {
		t.Errorf("expected exactly 1 audit line, got %d: %s", countAuditLines(out), out)
	}
}

// ---------------------------------------------------------------------------
// Item 4: client RawQuery is never forwarded to the upstream
// ---------------------------------------------------------------------------

// TestDoUpstreamRequest_DropsClientRawQuery pins item 4: the upstream URL is
// built from the account base URL and the fixed upstream path only — a
// client-supplied query string (?key=secret, routing parameters) must never
// reach the upstream verbatim.
func TestDoUpstreamRequest_DropsClientRawQuery(t *testing.T) {
	var gotQuery string
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)
	acc := p.AllAccounts()[0]

	body := []byte(`{"model":"gpt-4"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions?api_key=secret&foo=bar", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	res := doUpstreamRequest(acc, r, body, ChatForwardOpts{}, "req-query-1")
	if res.resp == nil {
		t.Fatal("expected an upstream response")
	}
	io.Copy(io.Discard, res.resp.Body)
	res.resp.Body.Close()
	res.cancel()

	if gotQuery != "" {
		t.Errorf("upstream received RawQuery %q, want empty (client query must be dropped)", gotQuery)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("upstream path = %q, want /chat/completions", gotPath)
	}
}

// ---------------------------------------------------------------------------
// Item 3: stream_options.include_usage injection when usage is enabled
// ---------------------------------------------------------------------------

// TestEnsureStreamOptionsIncludeUsage is the unit-level pin for item 3:
// include_usage=true is ensured while every other client stream_options
// field is preserved, and an already-ensured body is left byte-identical.
func TestEnsureStreamOptionsIncludeUsage(t *testing.T) {
	// Already true → byte-identical (no re-marshal).
	in := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true,"x":1}}`)
	if out := ensureStreamOptionsIncludeUsage(in); !bytes.Equal(out, in) {
		t.Errorf("already-ensured body changed: %s", out)
	}
	// Absent → added.
	out := ensureStreamOptionsIncludeUsage([]byte(`{"model":"m","stream":true}`))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	so, ok := m["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing: %s", out)
	}
	if so["include_usage"] != true {
		t.Errorf("include_usage = %v, want true", so["include_usage"])
	}
	// Other client fields preserved, include_usage forced true even when the
	// client explicitly set false.
	out = ensureStreamOptionsIncludeUsage([]byte(`{"model":"m","stream":true,"stream_options":{"foo":"bar","include_usage":false,"n":7}}`))
	m = nil
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	so = m["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Errorf("include_usage = %v, want true (client false must be overridden)", so["include_usage"])
	}
	if so["foo"] != "bar" || so["n"] != float64(7) {
		t.Errorf("client stream_options fields lost: %v", so)
	}
	// Non-object stream_options is replaced by the usage request.
	out = ensureStreamOptionsIncludeUsage([]byte(`{"model":"m","stream":true,"stream_options":"bogus"}`))
	m = nil
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	so = m["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Errorf("non-object stream_options not replaced: %v", so)
	}
	// Non-JSON body → unchanged.
	bad := []byte(`{not json`)
	if out := ensureStreamOptionsIncludeUsage(bad); !bytes.Equal(out, bad) {
		t.Errorf("non-JSON body changed")
	}
}

// upstreamBodyCapture records the request body of a single upstream POST.
type upstreamBodyCapture struct {
	body []byte
}

func (c *upstreamBodyCapture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		c.body = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}
}

// TestStreamOptionsInjected_ChatForward pins item 3 end-to-end on the chat
// path: usage enabled + stream:true → the upstream receives
// stream_options.include_usage=true with the client's other fields kept.
func TestStreamOptionsInjected_ChatForward(t *testing.T) {
	capture := &upstreamBodyCapture{}
	upstream := httptest.NewServer(capture.handler(t))
	defer upstream.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}},
		Usage:    config.UsageConfig{Enabled: true},
	}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"foo":"bar"}}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChat(p, rec, r, cfg)

	var sent map[string]any
	if err := json.Unmarshal(capture.body, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, capture.body)
	}
	so, ok := sent["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("upstream body missing stream_options: %s", capture.body)
	}
	if so["include_usage"] != true {
		t.Errorf("include_usage = %v, want true", so["include_usage"])
	}
	if so["foo"] != "bar" {
		t.Errorf("client stream_options field foo lost: %v", so)
	}
}

// TestStreamOptionsInjected_ResponsesForward pins item 3 on the responses
// conversion path: the converted chat body must carry the injected
// stream_options too.
func TestStreamOptionsInjected_ResponsesForward(t *testing.T) {
	capture := &upstreamBodyCapture{}
	upstream := httptest.NewServer(capture.handler(t))
	defer upstream.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}},
		Usage:    config.UsageConfig{Enabled: true},
	}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4","input":"hi","stream":true}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyResponses(p, rec, r, cfg)

	var sent map[string]any
	if err := json.Unmarshal(capture.body, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, capture.body)
	}
	so, ok := sent["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("upstream body missing stream_options: %s", capture.body)
	}
	if so["include_usage"] != true {
		t.Errorf("include_usage = %v, want true", so["include_usage"])
	}
}

// TestStreamOptionsNotInjected_UsageDisabled pins item 3: with usage
// recording disabled the body passes through untouched (no stream_options
// is invented for the upstream).
func TestStreamOptionsNotInjected_UsageDisabled(t *testing.T) {
	capture := &upstreamBodyCapture{}
	upstream := httptest.NewServer(capture.handler(t))
	defer upstream.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}},
		Usage:    config.UsageConfig{Enabled: false},
	}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChat(p, rec, r, cfg)

	var sent map[string]any
	if err := json.Unmarshal(capture.body, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, capture.body)
	}
	if _, ok := sent["stream_options"]; ok {
		t.Errorf("stream_options must NOT be injected when usage is disabled: %s", capture.body)
	}
}

// TestStreamOptionsNotInjected_Anthropic pins item 3: the /v1/messages
// (Anthropic) surface is never touched — even with usage enabled and
// stream:true the body reaches the upstream byte-identical (stream_options
// is an OpenAI field and Anthropic must not be modified).
func TestStreamOptionsNotInjected_Anthropic(t *testing.T) {
	capture := &upstreamBodyCapture{}
	upstream := httptest.NewServer(capture.handler(t))
	defer upstream.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}},
		Usage:    config.UsageConfig{Enabled: true},
	}
	p := pool.NewPool(cfg.Accounts)

	body := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyMessages(p, rec, r, cfg)

	if !bytes.Equal(capture.body, body) {
		t.Errorf("anthropic body modified: got %s, want byte-identical %s", capture.body, body)
	}
}

// ---------------------------------------------------------------------------
// Item 6: 401/402 exhaustion must not masquerade as generic 503
// ---------------------------------------------------------------------------

// TestAllAccounts401_UpstreamAuthFailed pins item 6 for a single account:
// 401 exhausts the account; the terminal response distinguishes the real
// cause — 502 upstream_auth_failed, not 503 all_exhausted/no_healthy.
func TestAllAccounts401_UpstreamAuthFailed(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"invalid_api_key"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "exhaust-401-1")
	r = r.WithContext(ctx)

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 upstream_auth_failed", rec.Code)
	}
	if code, msg := decodeErrorBody(t, rec.Body.String()); code != "upstream_auth_failed" || !strings.Contains(msg, "authentication") {
		t.Errorf("error = (%q, %q), want upstream_auth_failed", code, msg)
	}
	out := h.output()
	if !strings.Contains(out, `"error_type":"upstream_auth_failed"`) {
		t.Errorf("audit error_type must be upstream_auth_failed: %s", out)
	}
	if p.AllAccounts()[0].Status() != pool.StatusExhausted {
		t.Error("account must be exhausted after the 401")
	}
}

// TestAllAccounts402_UpstreamQuotaExhausted pins item 6 for the balance
// signal: 402 (Payment Required) is a money failure, not a credential one —
// the terminal response is 503 upstream_quota_exhausted.
func TestAllAccounts402_UpstreamQuotaExhausted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"error":{"message":"balance exhausted"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 upstream_quota_exhausted", rec.Code)
	}
	if code, msg := decodeErrorBody(t, rec.Body.String()); code != "upstream_quota_exhausted" || !strings.Contains(msg, "quota or balance") {
		t.Errorf("error = (%q, %q), want upstream_quota_exhausted", code, msg)
	}
}

// TestAllAccounts401_MultiAccount pins item 6 across several accounts: every
// account is tried (failover preserved) and the terminal response still
// reports the upstream auth failure.
func TestAllAccounts401_MultiAccount(t *testing.T) {
	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"invalid_api_key"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "a", Key: "k1", BaseURL: upstream.URL, Provider: "t"},
		{Name: "b", Key: "k2", BaseURL: upstream.URL, Provider: "t"},
	}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 upstream_auth_failed", rec.Code)
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_auth_failed" {
		t.Errorf("error code = %q, want upstream_auth_failed", code)
	}
	if got := atomic.LoadInt32(&upstreamCalls); got < 2 {
		t.Errorf("upstream calls = %d, want >= 2 (failover across accounts must be preserved)", got)
	}
	for _, acc := range p.AllAccounts() {
		if acc.Status() != pool.StatusExhausted {
			t.Errorf("account %s must be exhausted", acc.Name())
		}
	}
}

// TestUpstream429QuotaExhaustion_Classified pins item 6 for the quota
// signal via 429 + structured insufficient_quota: the account is exhausted
// and the terminal response is 503 upstream_quota_exhausted (not a generic
// all_exhausted).
func TestUpstream429QuotaExhaustion_Classified(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":"insufficient_quota"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 upstream_quota_exhausted", rec.Code)
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_quota_exhausted" {
		t.Errorf("error code = %q, want upstream_quota_exhausted", code)
	}
}

// ---------------------------------------------------------------------------
// Item 8: streaming responses — delayed header, 502 pre-first-event,
// response.failed mid-stream
// ---------------------------------------------------------------------------

// TestResponsesStream_PreFirstEventFailure502 pins item 8: the upstream
// answers 200 text/event-stream but dies before delivering ANY event; the
// HTTP status is still uncommitted, so the proxy returns a structured 502
// (never an empty 200).
func TestResponsesStream_PreFirstEventFailure502(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	upstream := upstream2xxTruncatedBody(t, "text/event-stream")
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4","input":"hi","stream":true}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "stream-prefirst-1")
	r = r.WithContext(ctx)

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4","stream":true}`), time.Now(), ChatForwardOpts{ResponsesOut: true, Stream: true}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (failure before the first event must not commit 200)", rec.Code)
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_stream_error" {
		t.Errorf("error code = %q, want upstream_stream_error", code)
	}
	out := h.output()
	if !strings.Contains(out, `"status":502`) {
		t.Errorf("audit must record 502 for a pre-first-event failure: %s", out)
	}
}

// TestResponsesStream_MidStreamFailureSendsFailedEvent pins item 8 for the
// mid-stream case: after the first event committed the 200, a stream
// failure must deliver the protocol failure terminal event
// (response.failed) and the audit keeps the committed 200 — the HTTP status
// cannot be changed after commit.
func TestResponsesStream_MidStreamFailureSendsFailedEvent(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	// One valid SSE event, then the upstream dies (declared length never
	// satisfied) → scanner error AFTER an event was delivered.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4","input":"hi","stream":true}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "stream-mid-1")
	r = r.WithContext(ctx)

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4","stream":true}`), time.Now(), ChatForwardOpts{ResponsesOut: true, Stream: true}, cfg)

	// The first event committed the 200; it cannot be changed afterwards.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (committed by the first event; HTTP status cannot change after commit)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.output_text.delta") {
		t.Errorf("expected the delivered content event in the body: %s", body)
	}
	if !strings.Contains(body, "response.failed") || !strings.Contains(body, "upstream_stream_error") {
		t.Errorf("mid-stream failure must deliver the response.failed terminal event: %s", body)
	}
	out := h.output()
	if !strings.Contains(out, `"status":200`) {
		t.Errorf("audit must record the committed 200: %s", out)
	}
	if !strings.Contains(out, `"error_type":"upstream_refused"`) && !strings.Contains(out, `"error_type":"upstream_error"`) {
		t.Errorf("audit missing the classified stream error: %s", out)
	}
}

// ---------------------------------------------------------------------------
// Item 12: Retry-After delta-seconds and HTTP-date
// ---------------------------------------------------------------------------

func retryAfterHeader(v string) *http.Response {
	return &http.Response{Header: http.Header{"Retry-After": {v}}}
}

// TestParseRetryAfter_RFC9110Formats pins item 12: both RFC 9110 formats
// are honored (delta-seconds and HTTP-date), past dates / invalid values /
// zero / negative yield 0, and huge delta-seconds cannot overflow the
// duration. (The pre-existing TestParseRetryAfter covers the basic
// delta-seconds cases.)
func TestParseRetryAfter_RFC9110Formats(t *testing.T) {
	if got := parseRetryAfter(nil); got != 0 {
		t.Errorf("nil response: got %v, want 0", got)
	}
	if got := parseRetryAfter(retryAfterHeader("")); got != 0 {
		t.Errorf("empty header: got %v, want 0", got)
	}
	if got := parseRetryAfter(retryAfterHeader("120")); got != 120*time.Second {
		t.Errorf("delta 120: got %v, want 120s", got)
	}
	if got := parseRetryAfter(retryAfterHeader("  30  ")); got != 30*time.Second {
		t.Errorf("padded delta 30: got %v, want 30s", got)
	}
	if got := parseRetryAfter(retryAfterHeader("0")); got != 0 {
		t.Errorf("delta 0: got %v, want 0", got)
	}
	if got := parseRetryAfter(retryAfterHeader("-5")); got != 0 {
		t.Errorf("delta -5: got %v, want 0", got)
	}
	if got := parseRetryAfter(retryAfterHeader("abc")); got != 0 {
		t.Errorf("invalid: got %v, want 0", got)
	}
	// HTTP-date in the future: the wait is the time until the date.
	future := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(retryAfterHeader(future)); got < 90*time.Second || got > 130*time.Second {
		t.Errorf("future HTTP-date: got %v, want ~120s", got)
	}
	// HTTP-date in the past: the wait has elapsed → 0 (caller falls back to
	// its default cooldown).
	past := time.Now().Add(-1 * time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(retryAfterHeader(past)); got != 0 {
		t.Errorf("past HTTP-date: got %v, want 0", got)
	}
	// A huge delta-seconds value (parsable as int64 but far beyond any sane
	// wait) must not overflow the duration math — it is capped.
	if got := parseRetryAfter(retryAfterHeader("999999999999999999")); got <= 0 || got > maxRetryAfterSeconds*time.Second {
		t.Errorf("huge delta: got %v, want capped > 0", got)
	}
}

// TestHandleUpstreamError_RetryAfterHonoredAndCapped pins item 12 at the
// cooldown level: a 429 Retry-After is honored (cooldown = the advertised
// wait) and capped at 5 minutes even for an absurdly large value.
func TestHandleUpstreamError_RetryAfterHonoredAndCapped(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	p := pool.NewPool([]config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://localhost:1", Provider: "t"}})
	acc := p.AllAccounts()[0]

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": {"90"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}
	handleUpstreamError(acc, resp, "req-ra-1", "m")
	if out := h.output(); !strings.Contains(out, `"cooldown":"1m30s"`) {
		t.Errorf("429 with Retry-After 90 must cool down for 1m30s, got: %s", out)
	}

	h2 := &capturingHandler{}
	restore2 := stashSlog(h2)
	defer restore2()
	resp2 := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": {"999999999"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}
	handleUpstreamError(acc, resp2, "req-ra-2", "m")
	if out := h2.output(); !strings.Contains(out, `"cooldown":"5m0s"`) {
		t.Errorf("a huge Retry-After must be capped at 5m, got: %s", out)
	}
}

// ---------------------------------------------------------------------------
// Item 7: /health routing
// ---------------------------------------------------------------------------

// TestProxyHandler_HealthServesOK pins the routing half of item 7:
// NewProxyHandler answers /health with 200 "ok" without auth or provider
// requirements, and the exemption never spills onto other paths.
func TestProxyHandler_HealthServesOK(t *testing.T) {
	h := newHandlerForMethodCheck(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("/health body = %q, want ok", rec.Body.String())
	}
	// Other paths are unaffected (a business path without a provider header
	// is still a normal 400, not an accidental health-style bypass).
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`)))
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("/v1/chat/completions without provider: status = %d, want 400 (not exempted)", rec2.Code)
	}
}

// ---------------------------------------------------------------------------
// Item 2: MCP cache identity = authenticated API key name
// ---------------------------------------------------------------------------

// TestGetTenantID_IdentityByAuthStatus pins the MCP-cache identity rule at
// the proxy level: an authenticated request (real api_keys credential) gets
// the API key NAME (never the token); a request that did NOT go through a
// real credential check gets the fixed read-only
// mcp.UnauthenticatedIdentity — never a shared writable bucket that would
// let different local clients pollute each other when auth is disabled.
func TestGetTenantID_IdentityByAuthStatus(t *testing.T) {
	// No auth context at all (direct handler test, auth disabled): the
	// unauthenticated identity — NOT "default" (a writable shared bucket).
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	if got := getTenantID(r); got != mcp.UnauthenticatedIdentity {
		t.Errorf("no auth context: identity = %q, want %q", got, mcp.UnauthenticatedIdentity)
	}

	// A key name alone (WithAPIKey without the auth-status flag) is not
	// enough: the request must also be marked authenticated.
	r = r.WithContext(middleware.WithAPIKey(r.Context(), "ci-bot"))
	if got := getTenantID(r); got != mcp.UnauthenticatedIdentity {
		t.Errorf("key name without auth status: identity = %q, want %q (unauthenticated until marked)", got, mcp.UnauthenticatedIdentity)
	}

	// Authenticated request: the key NAME is the identity.
	r = r.WithContext(middleware.WithAuthenticated(r.Context(), true))
	if got := getTenantID(r); got != "ci-bot" {
		t.Errorf("with auth context: identity = %q, want ci-bot", got)
	}

	// Authenticated but no key name installed: plain "default" bucket.
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	r2 = r2.WithContext(middleware.WithAuthenticated(r2.Context(), true))
	if got := getTenantID(r2); got != "default" {
		t.Errorf("authenticated without key name: identity = %q, want default", got)
	}
}

// ---------------------------------------------------------------------------
// Item 9: model remap — audit keeps virtual + upstream model traceability
// ---------------------------------------------------------------------------

// TestAuditLog_ModelRemapTraceability pins item 9: with model remap enabled
// the audit records BOTH the client-requested virtual model (model) and the
// resolved upstream model (upstream_model), so accounting and pricing never
// lose either side.
func TestAuditLog_ModelRemapTraceability(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Accounts:          []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}},
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"virtual-model": "tier1"},
		ModelTiers:        map[string]string{"tier1": "real-upstream-model"},
	}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"virtual-model","messages":[{"role":"user","content":"hi"}]}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "remap-audit-1")
	r = r.WithContext(ctx)

	proxyChat(p, rec, r, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	out := h.output()
	if !strings.Contains(out, `"model":"virtual-model"`) {
		t.Errorf("audit must keep the requested virtual model: %s", out)
	}
	if !strings.Contains(out, `"upstream_model":"real-upstream-model"`) {
		t.Errorf("audit must record the resolved upstream model: %s", out)
	}
}

// TestAuditLog_NoUpstreamModelWithoutRemap pins the omitempty behavior: a
// deployment without model remap records no upstream_model field (zero
// audit-log churn for the common case).
func TestAuditLog_NoUpstreamModelWithoutRemap(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChat(p, rec, r, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The audit line follows the fixed-field convention (empty values are
	// emitted): without remap upstream_model is empty, never a resolved name.
	if out := h.output(); !strings.Contains(out, `"upstream_model":""`) {
		t.Errorf("without remap upstream_model must be empty: %s", out)
	}
}

// ---------------------------------------------------------------------------
// Item 8 rework: StatusCapture commit semantics — first-event write failure
// ---------------------------------------------------------------------------

// failFirstWriteResponseWriter fails the FIRST underlying Write (0 bytes,
// or `partial` bytes before the error) and forwards every later call — the
// simulation of a client connection that dies on the first event write.
type failFirstWriteResponseWriter struct {
	inner   http.ResponseWriter
	failed  bool
	partial int
}

func (f *failFirstWriteResponseWriter) Header() http.Header  { return f.inner.Header() }
func (f *failFirstWriteResponseWriter) WriteHeader(code int) { f.inner.WriteHeader(code) }
func (f *failFirstWriteResponseWriter) Write(p []byte) (int, error) {
	if !f.failed {
		f.failed = true
		if f.partial > 0 {
			return f.partial, errors.New("simulated partial write failure")
		}
		return 0, errors.New("simulated first-write failure")
	}
	return f.inner.Write(p)
}
func (f *failFirstWriteResponseWriter) Flush() {
	if fl, ok := f.inner.(http.Flusher); ok {
		fl.Flush()
	}
}

// TestResponsesStream_FirstEventWriteFailure502 pins the item-8 rework at
// the proxy level: when the FIRST event write fails with 0 bytes, the
// response is still UNCOMMITTED (StatusCapture must not have recorded the
// implicit 200), so the proxy returns a structured 502 upstream_stream_error
// and the audit records 502 — never an empty 200.
func TestResponsesStream_FirstEventWriteFailure502(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	// One valid SSE event, then a clean EOF: the translator's first emit
	// hits the failing client writer.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	w := &failFirstWriteResponseWriter{inner: rec}
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4","input":"hi","stream":true}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "stream-firstwrite-1")
	r = r.WithContext(ctx)

	proxyChatWithBody(p, w, r, []byte(`{"model":"gpt-4","stream":true}`), time.Now(), ChatForwardOpts{ResponsesOut: true, Stream: true}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (a 0-byte first-event write failure must keep the response uncommitted)", rec.Code)
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_stream_error" {
		t.Errorf("error code = %q, want upstream_stream_error", code)
	}
	out := h.output()
	if !strings.Contains(out, `"status":502`) {
		t.Errorf("audit must record 502 for a 0-byte first-event write failure: %s", out)
	}
}

// TestResponsesStream_PartialWriteFailureKeepsCommitted200 pins the other
// half of the item-8 rework: a first event write that delivers SOME bytes
// and then fails IS committed (net/http cannot change the status after the
// first byte reached the wire), so the audit keeps the committed 200 with a
// classified error — the proxy must not attempt a 502 over a committed
// response.
func TestResponsesStream_PartialWriteFailureKeepsCommitted200(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	w := &failFirstWriteResponseWriter{inner: rec, partial: 2}
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4","input":"hi","stream":true}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "stream-partialwrite-1")
	r = r.WithContext(ctx)

	proxyChatWithBody(p, w, r, []byte(`{"model":"gpt-4","stream":true}`), time.Now(), ChatForwardOpts{ResponsesOut: true, Stream: true}, cfg)

	out := h.output()
	if !strings.Contains(out, `"status":200`) {
		t.Errorf("audit must keep the committed 200 after a partial write failure: %s", out)
	}
	if !strings.Contains(out, `"error_type":"upstream_error"`) {
		t.Errorf("audit must carry the classified error: %s", out)
	}
	// Nothing may be written after the commit (no 502 attempt over a
	// committed response).
	if rec.Body.Len() != 0 {
		t.Errorf("client must not receive a 502 body after the partial commit, got: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Item 6 rework: mixed 401+402 terminal state is deterministic
// ---------------------------------------------------------------------------

// TestMixed401And402_TerminalStateDeterministic pins the item-6 rework:
// permanent failure classes ACCUMULATE across accounts (the last account's
// class must not win) and the terminal state is deterministic regardless of
// account order. Priority: credential (401 / structured credential error)
// over quota (402 / structured quota error) — broken credentials are the
// most diagnostic signal (every other fix is useless until the keys are
// replaced) and map to the existing 502 upstream_auth_failed code.
func TestMixed401And402_TerminalStateDeterministic(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"invalid_api_key"}}`))
	}))
	defer auth.Close()
	quota := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"error":{"message":"balance exhausted"}}`))
	}))
	defer quota.Close()

	run := func(t *testing.T, accs []config.AccountConfig) (int, string) {
		t.Helper()
		p := pool.NewPool(accs)
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Prism-Provider", "t")
		proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, &config.Config{})
		code, _ := decodeErrorBody(t, rec.Body.String())
		return rec.Code, code
	}

	// Order 1: auth account first, quota second.
	status1, code1 := run(t, []config.AccountConfig{
		{Name: "a", Key: "k1", BaseURL: auth.URL, Provider: "t"},
		{Name: "b", Key: "k2", BaseURL: quota.URL, Provider: "t"},
	})
	// Order 2: quota account first, auth second.
	status2, code2 := run(t, []config.AccountConfig{
		{Name: "a", Key: "k1", BaseURL: quota.URL, Provider: "t"},
		{Name: "b", Key: "k2", BaseURL: auth.URL, Provider: "t"},
	})

	if status1 != http.StatusBadGateway || code1 != "upstream_auth_failed" {
		t.Errorf("order 1 = (%d, %q), want (502, upstream_auth_failed): credential wins over quota", status1, code1)
	}
	if status2 != http.StatusBadGateway || code2 != "upstream_auth_failed" {
		t.Errorf("order 2 = (%d, %q), want (502, upstream_auth_failed): the terminal state must not depend on account order", status2, code2)
	}
	if status1 != status2 || code1 != code2 {
		t.Errorf("mixed 401+402 must be deterministic across account orders, got (%d,%q) vs (%d,%q)", status1, code1, status2, code2)
	}
}

// ---------------------------------------------------------------------------
// Item 5 rework: too-deep tools schema → 400 invalid_request end-to-end
// ---------------------------------------------------------------------------

// deepToolSchemaJSON builds a tool parameter schema nested far beyond
// sanitize.MaxJSONSchemaDepth and embeds it in a /v1/responses body.
func deepToolSchemaJSON(t *testing.T) string {
	t.Helper()
	deep := map[string]any{"type": "object"}
	cur := deep
	for i := 0; i < sanitize.MaxJSONSchemaDepth+10; i++ {
		nested := map[string]any{"type": "object"}
		cur["properties"] = map[string]any{"nested": nested}
		cur = nested
	}
	paramsJSON, err := json.Marshal(deep)
	if err != nil {
		t.Fatal(err)
	}
	return `{"model":"gpt-4","input":"hi","tools":[{"type":"function","name":"deep_tool","parameters":` + string(paramsJSON) + `}]}`
}

// TestResponsesDeepSchemaReturns400InvalidRequest is the end-to-end pin for
// the depth-bounded schema simplification: a too-deep tool schema in a
// /v1/responses request travels the full HTTP handler chain (handler →
// proxyResponses → convert → mcp sanitize → ErrSchemaTooDeep) and answers
// 400 invalid_request — never a 200 with an unsafe schema, never a 500.
func TestResponsesDeepSchemaReturns400InvalidRequest(t *testing.T) {
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://localhost:1", Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)
	h := NewProxyHandler(p, config.WireAPIBoth, config.NewConfigHolder(cfg), nil)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(deepToolSchemaJSON(t)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 invalid_request (a too-deep schema must fail the request), body: %s", rec.Code, rec.Body.String())
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "invalid_request" {
		t.Errorf("error code = %q, want invalid_request", code)
	}
}

// ---------------------------------------------------------------------------
// Item 12 rework: parseRetryAfter caps a far-future HTTP-date internally
// ---------------------------------------------------------------------------

// TestParseRetryAfter_FarFutureHTTPDateCapped pins the item-12 rework: the
// HTTP-date branch applies the same internal cap as the delta-seconds branch
// (a far-future date must not yield a multi-century duration — the caller's
// 5-minute cooldown cap then decides the actual wait).
func TestParseRetryAfter_FarFutureHTTPDateCapped(t *testing.T) {
	far := time.Now().Add(100 * 365 * 24 * time.Hour).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(retryAfterHeader(far))
	if got <= 0 || got > maxRetryAfterSeconds*time.Second {
		t.Errorf("far-future HTTP-date: got %v, want capped at maxRetryAfterSeconds (%v)", got, maxRetryAfterSeconds*time.Second)
	}
	if got != maxRetryAfterSeconds*time.Second {
		t.Errorf("far-future HTTP-date: got %v, want exactly the cap %v", got, maxRetryAfterSeconds*time.Second)
	}
}

// ---------------------------------------------------------------------------
// Item 9 rework: upstream_model is set only when remap changed the model
// ---------------------------------------------------------------------------

// TestAuditLog_NoUpstreamModelForUnmappedModel pins the item-9 rework: with
// remap ENABLED but the requested model NOT in model_remap, the model passes
// through unchanged and upstream_model stays empty — the audit field is
// reserved for actually-remapped models, so the log semantics match the
// field comment and the omitempty JSON tag.
func TestAuditLog_NoUpstreamModelForUnmappedModel(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Accounts:          []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}},
		ModelRemapEnabled: true,
		ModelRemap:        map[string]string{"virtual-model": "tier1"},
		ModelTiers:        map[string]string{"tier1": "real-upstream-model"},
	}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChat(p, rec, r, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if out := h.output(); !strings.Contains(out, `"upstream_model":""`) {
		t.Errorf("an unremapped model must leave upstream_model empty: %s", out)
	}
}

// TestAuditLog_CacheOnlyUsageAccepted pins item 13 end-to-end: an Anthropic
// /v1/messages response whose usage carries ONLY cache tokens
// (cache_read_input_tokens, zero input/output) must still be applied to the
// audit — a cache-only hit consumes upstream quota and costs money.
func TestAuditLog_CacheOnlyUsageAccepted(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":50,"cache_creation_input_tokens":0}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages",
		bytes.NewReader([]byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	ctx := context.WithValue(r.Context(), util.RequestIDKey{}, "cacheonly-1")
	r = r.WithContext(ctx)

	proxyMessages(p, rec, r, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	out := h.output()
	if !strings.Contains(out, `"cached_tokens":50`) {
		t.Errorf("cache-only usage must reach the audit (cached_tokens=50): %s", out)
	}
	if !strings.Contains(out, `"total_tokens":0`) {
		t.Errorf("cache-only usage must not fabricate prompt/completion tokens: %s", out)
	}
}
