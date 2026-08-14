package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
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

// gzipBody compresses s with gzip for use as an upstream response body.
func gzipBody(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// proxyChatOneAccount drives one chat request through the real proxy path
// against a single-account pool whose upstream answers with the provided
// handler.
func proxyChatOneAccount(t *testing.T, upstream http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: srv.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{Model: "gpt-4"}, cfg)
	return rec
}

// --- gzip / Content-Encoding: body and headers must stay semantically
// consistent (non-streaming success and 4xx passthrough) ---

// TestUpstreamGzipNonStreamingSuccess: an upstream 200 whose body is
// Content-Encoding: gzip must reach the client DECODED: the raw body is
// uncompressed JSON and no Content-Encoding header is copied, so the body
// the client parses matches the headers it sees. Before the fix the
// compressed bytes were written verbatim with no Content-Encoding header —
// unparseable garbage.
func TestUpstreamGzipNonStreamingSuccess(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	rec := proxyChatOneAccount(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzipBody(t, body))
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("body = %q, want the DECODED upstream body (gzip must be decompressed before the passthrough)", got)
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want empty (decoded body must not be labeled gzip)", ce)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// The body must be parseable as the JSON it claims to be.
	var parsed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Errorf("client body is not valid JSON: %v", err)
	}
}

// TestUpstreamGzipResponsesOutSuccess: the /v1/responses non-streaming
// translation path gets the same treatment — the gzip body is decompressed
// BEFORE the chat-completion→responses conversion, so the translation
// parses real JSON and the client receives decoded content with no
// Content-Encoding header.
func TestUpstreamGzipResponsesOutSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzipBody(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{ResponsesOut: true, Model: "gpt-4"}, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want empty", ce)
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
}

// TestUpstreamGzip4xxPassthrough: a gzip-encoded 4xx body is decompressed
// BEFORE redaction and passthrough, and the Content-Encoding header is
// dropped — the client receives plain-text JSON with matching headers.
// Before the fix the redaction ran on compressed bytes (corrupting the
// stream) and the Content-Encoding header was deleted, leaving the client
// with broken bytes and no way to decode them.
func TestUpstreamGzip4xxPassthrough(t *testing.T) {
	errBody := `{"error":{"message":"forbidden"}}`
	rec := proxyChatOneAccount(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(gzipBody(t, errBody))
	}))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != errBody {
		t.Errorf("body = %q, want the DECODED redacted 4xx body %q (gzip must be decompressed before the passthrough)", got, errBody)
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want empty (decoded body must not be labeled gzip)", ce)
	}
}

// TestUpstreamGzipInvalidBodyFailsClosed: an upstream that CLAIMS
// Content-Encoding: gzip but sends a body that is not valid gzip must not
// push garbage bytes to the client — the decompression error flows through
// the existing body-read error path (structured 502) instead.
func TestUpstreamGzipInvalidBodyFailsClosed(t *testing.T) {
	rec := proxyChatOneAccount(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`this is not gzip`))
	}))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (invalid gzip must fail closed, not pass garbage)", rec.Code)
	}
}

// TestUpstreamGzipResponsesOutStreamSuccess: the /v1/responses STREAMING
// path (ResponsesOut+Stream) receives a Content-Encoding: gzip SSE stream
// and must decompress it BEFORE the SSE translator: the client receives
// translated Responses events parsed from the DECODED stream, a
// text/event-stream content type and NO Content-Encoding header — the
// body the client parses matches the headers it sees. Before the fix the
// translator consumed the compressed bytes as SSE: no event ever matched
// and the stream died with a decompression-style error.
func TestUpstreamGzipResponsesOutStreamSuccess(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: [DONE]\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": {"gzip"}},
		Body:       io.NopCloser(bytes.NewReader(gzipBody(t, sse))),
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	aud := directResponsesStreamResp(t, resp, w)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want empty (decoded stream must not be labeled gzip)", ce)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	// The translated events are the proof that the translator consumed
	// DECODED SSE: compressed bytes would never parse into these frames.
	if !strings.Contains(body, `"type":"response.output_text.delta"`) || !strings.Contains(body, `"delta":"Hello"`) {
		t.Errorf("translated stream missing the output_text.delta event: %q", body)
	}
	if !strings.Contains(body, `"type":"response.completed"`) {
		t.Errorf("translated stream missing response.completed: %q", body)
	}
	if aud.Status != http.StatusOK {
		t.Errorf("audit status = %d, want 200", aud.Status)
	}
}

// TestUpstreamGzipLegacyStreamSuccess: the legacy chat STREAMING path
// (ResponsesOut=false, Stream=true) receives a Content-Encoding: gzip SSE
// stream and must decompress it BEFORE the stream copier: the client
// receives the SSE bytes byte-for-byte (decoded), the upstream's
// Content-Type, and NO Content-Encoding header. Before the fix the
// compressed bytes were copied to the client verbatim.
func TestUpstreamGzipLegacyStreamSuccess(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: [DONE]\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "Content-Encoding": {"gzip"}},
		Body:       io.NopCloser(bytes.NewReader(gzipBody(t, sse))),
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	aud := directLegacyStreamResp(t, resp, w)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != sse {
		t.Errorf("stream body = %q, want the DECODED SSE %q (gzip must be decompressed before the copier)", got, sse)
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want empty (decoded stream must not be labeled gzip)", ce)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream (copied from the upstream)", ct)
	}
	if aud.Status != http.StatusOK {
		t.Errorf("audit status = %d, want 200", aud.Status)
	}
}

// TestUpstreamGzipResponsesOutStreamInvalidFailsClosed: an upstream that
// CLAIMS Content-Encoding: gzip on the Responses streaming path but sends
// a body that is not valid gzip must fail closed with a structured 502
// while the response is still uncommitted (the status is delayed until the
// first event) — no garbage byte reaches the client, no empty 200 is
// committed, and the retry loop cannot switch accounts.
func TestUpstreamGzipResponsesOutStreamInvalidFailsClosed(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": {"gzip"}},
		// The raw body LOOKS like valid SSE: if the fail-closed check were
		// removed, the translator would parse this garbage into events and
		// commit a 200 — the test would fail on the 502 assertion instead
		// of coincidentally passing via the empty-stream path.
		Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"GARBAGE\"}}]}\n\n")),
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	aud := directResponsesStreamResp(t, resp, w)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (invalid gzip must fail closed, not stream garbage); body=%s", rec.Code, rec.Body.String())
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_stream_error" {
		t.Errorf("error code = %q, want upstream_stream_error", code)
	}
	if w.headerCalls != 1 {
		t.Errorf("WriteHeader must be called exactly once (the 502), got %d calls", w.headerCalls)
	}
	if !w.Committed() {
		t.Error("the 502 must be committed so the retry loop cannot switch accounts")
	}
	if aud.Status != http.StatusBadGateway {
		t.Errorf("audit status = %d, want 502", aud.Status)
	}
}

// TestUpstreamGzipLegacyStreamInvalidFailsClosed: the legacy streaming
// path gets the same fail-closed treatment — an upstream that CLAIMS
// Content-Encoding: gzip but sends a non-gzip body must answer a
// structured 502 before any byte reached the client, never the compressed
// garbage verbatim.
func TestUpstreamGzipLegacyStreamInvalidFailsClosed(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Encoding": {"gzip"}},
		// The raw body LOOKS like valid SSE: if the fail-closed check were
		// removed, the stream copier would copy this garbage verbatim and
		// commit a 200 — the test would fail on the 502 assertion instead
		// of coincidentally passing via the empty-stream path.
		Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"GARBAGE\"}}]}\n\n")),
	}
	rec := httptest.NewRecorder()
	w := &commitTrackWriter{inner: rec}
	aud := directLegacyStreamResp(t, resp, w)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (invalid gzip must fail closed, not stream garbage); body=%s", rec.Code, rec.Body.String())
	}
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_stream_error" {
		t.Errorf("error code = %q, want upstream_stream_error", code)
	}
	if w.headerCalls != 1 {
		t.Errorf("WriteHeader must be called exactly once (the 502), got %d calls", w.headerCalls)
	}
	if !w.Committed() {
		t.Error("the 502 must be committed so the retry loop cannot switch accounts")
	}
	if aud.Status != http.StatusBadGateway {
		t.Errorf("audit status = %d, want 502", aud.Status)
	}
	if aud.ErrorType != "upstream_stream_error" {
		t.Errorf("audit error_type = %q, want upstream_stream_error", aud.ErrorType)
	}
}

// TestUpstreamGzipResponsesOutStreamCorruptFailsClosed: a gzip stream
// whose HEADER is valid but whose payload is truncated must still fail
// closed before the first event: the decompression error surfaces on the
// very first read, the SSE translator has written nothing yet, and the
// proxy answers a structured 502 instead of an empty 200 or garbage.
func TestUpstreamGzipResponsesOutStreamCorruptFailsClosed(t *testing.T) {
	h := &capturingHandler{}
	restore := stashSlog(h)
	defer restore()

	full := gzipBody(t, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
	if len(full) < 14 {
		t.Fatalf("gzip fixture too small (%d bytes) to truncate after the header", len(full))
	}
	// Keep only the 10-byte gzip header plus a few payload bytes: the
	// header check passes at construction, the first Read fails with a
	// decompression error — the deterministic "corrupts mid-stream before
	// any event" case.
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
	if code, _ := decodeErrorBody(t, rec.Body.String()); code != "upstream_stream_error" {
		t.Errorf("error code = %q, want upstream_stream_error", code)
	}
	if !w.Committed() {
		t.Error("the 502 must be committed so the retry loop cannot switch accounts")
	}
	if aud.Status != http.StatusBadGateway {
		t.Errorf("audit status = %d, want 502", aud.Status)
	}
}

// --- X-Prism-Provider must never be forwarded to the upstream ---

// TestXPrismProviderNotForwardedUpstream: the client-supplied
// X-Prism-Provider header selects the provider inside prism but must not
// reach the upstream (the upstream is not a prism router; a client could
// otherwise steer a header the upstream might interpret or echo).
func TestXPrismProviderNotForwardedUpstream(t *testing.T) {
	var gotHeader string
	rec := proxyChatOneAccount(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Prism-Provider")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotHeader != "" {
		t.Errorf("upstream received X-Prism-Provider = %q, want empty (routing header must not be forwarded)", gotHeader)
	}
}

// TestCopyClientHeaders_DropsPrismProvider is the unit-level guard: the
// header is dropped while normal business headers still pass through.
func TestCopyClientHeaders_DropsPrismProvider(t *testing.T) {
	src := http.Header{
		"X-Prism-Provider": {"agentrouter-anthropic"},
		"X-Custom-Biz":     {"keep-me"},
	}
	dst := make(http.Header)
	copyClientHeaders(dst, src)
	if dst.Get("X-Prism-Provider") != "" {
		t.Errorf("copyClientHeaders forwarded X-Prism-Provider = %q, want dropped", dst.Get("X-Prism-Provider"))
	}
	if dst.Get("X-Custom-Biz") != "keep-me" {
		t.Errorf("copyClientHeaders dropped a normal business header: %q", dst.Get("X-Custom-Biz"))
	}
}

// --- ensureStreamOptionsIncludeUsage must not scramble top-level key order ---

// topLevelKeys extracts the top-level object keys of a JSON body in
// document order.
func topLevelKeys(t *testing.T, body []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if tok != json.Delim('{') {
		t.Fatalf("top level of %q is not an object", body)
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			t.Fatalf("decode key: %v", err)
		}
		keys = append(keys, kt.(string))
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			t.Fatalf("decode value: %v", err)
		}
	}
	return keys
}

// TestEnsureStreamOptionsIncludeUsage_KeyOrderPreserved pins the audit fix:
// when the body must be rewritten, the top-level key ORDER and every other
// value are preserved — the old whole-body map re-marshal scrambled the key
// order randomly on every call.
func TestEnsureStreamOptionsIncludeUsage_KeyOrderPreserved(t *testing.T) {
	// Existing stream_options: every other key keeps its position and value.
	in := []byte(`{"model":"m","stream":true,"temperature":1,"stream_options":{"foo":"bar"}}`)
	out := ensureStreamOptionsIncludeUsage(in)
	got := topLevelKeys(t, out)
	want := []string{"model", "stream", "temperature", "stream_options"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("top-level key order = %v, want %v (body %s)", got, want, out)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	so := m["stream_options"].(map[string]any)
	if so["include_usage"] != true || so["foo"] != "bar" {
		t.Errorf("stream_options semantics lost: %v", so)
	}
	if m["temperature"] != float64(1) || m["model"] != "m" {
		t.Errorf("other fields lost: %v", m)
	}

	// Absent stream_options: appended at the end, existing keys untouched.
	in2 := []byte(`{"model":"m","stream":true}`)
	out2 := ensureStreamOptionsIncludeUsage(in2)
	got2 := topLevelKeys(t, out2)
	want2 := []string{"model", "stream", "stream_options"}
	if strings.Join(got2, ",") != strings.Join(want2, ",") {
		t.Errorf("top-level key order = %v, want %v (body %s)", got2, want2, out2)
	}
	var m2 map[string]any
	if err := json.Unmarshal(out2, &m2); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if so2, ok := m2["stream_options"].(map[string]any); !ok || so2["include_usage"] != true {
		t.Errorf("stream_options missing or wrong: %v", m2["stream_options"])
	}

	// Non-object stream_options: replaced in place, key order still kept.
	in3 := []byte(`{"model":"m","stream":true,"stream_options":"bogus"}`)
	out3 := ensureStreamOptionsIncludeUsage(in3)
	got3 := topLevelKeys(t, out3)
	want3 := []string{"model", "stream", "stream_options"}
	if strings.Join(got3, ",") != strings.Join(want3, ",") {
		t.Errorf("top-level key order = %v, want %v (body %s)", got3, want3, out3)
	}
}

// --- writeUpstreamExhausted default fallback ---

// TestWriteUpstreamExhaustedDefaultFallback pins the defensive fallback: a
// call with no recorded permanent class (sawCredential=false, sawQuota=false)
// must still produce a terminal 503 all_exhausted response and audit fields
// — never a silent empty response.
func TestWriteUpstreamExhaustedDefaultFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	aud := &middleware.RequestAudit{}
	writeUpstreamExhausted(rec, aud, false, false)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "all_exhausted" {
		t.Errorf("error code = %q, want all_exhausted", code)
	}
	if aud.ErrorType != "all_exhausted" {
		t.Errorf("audit error_type = %q, want all_exhausted", aud.ErrorType)
	}
	if aud.Error == "" {
		t.Error("audit error must be set by the fallback")
	}
}
