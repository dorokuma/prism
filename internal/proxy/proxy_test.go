package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

func TestIsPermanentCredentialError(t *testing.T) {
	makeBody := func(code string) []byte {
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{"code": code},
		})
		return b
	}
	for _, code := range []string{"invalid_api_key", "revoked", "account_deactivated"} {
		if !IsPermanentCredentialError(makeBody(code)) {
			t.Errorf("IsPermanentCredentialError(%q) = false, want true", code)
		}
	}
	if IsPermanentCredentialError(makeBody("insufficient_quota")) {
		t.Errorf("IsPermanentCredentialError(insufficient_quota) = true, want false")
	}
	if IsPermanentCredentialError(nil) {
		t.Errorf("IsPermanentCredentialError(nil) = true, want false")
	}
}

func TestIsQuotaError(t *testing.T) {
	if !IsQuotaError([]byte(`{"error":{"type":"gousagelimiterror"}}`)) {
		t.Error("IsQuotaError(gousagelimiterror) = false, want true")
	}
	if !IsQuotaError([]byte(`{"error":{"code":"insufficient_quota"}}`)) {
		t.Error("IsQuotaError(insufficient_quota) = false, want true")
	}
	if !IsQuotaError([]byte(`quota exceeded`)) {
		t.Error("IsQuotaError('quota exceeded') = false, want true")
	}
	if !IsQuotaError([]byte(`usage limit`)) {
		t.Error("IsQuotaError('usage limit') = false, want true")
	}
	if IsQuotaError([]byte(`{"error":{"code":"invalid_api_key"}}`)) {
		t.Error("IsQuotaError(invalid_api_key) = true, want false")
	}
	if IsQuotaError(nil) {
		t.Error("IsQuotaError(nil) = true, want false")
	}
}

func TestHandleUpstreamErrorNilResp(t *testing.T) {
	// Should not panic
	handleUpstreamError(nil, nil, "test-req", "test-model")
}

func TestHandleUpstreamErrorNoBody(t *testing.T) {
	// Should not panic with empty response
	resp := &http.Response{
		StatusCode: 500,
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}
	p := pool.NewPool([]config.AccountConfig{{Name: "test"}})
	accs := p.AllAccounts()
	handleUpstreamError(accs[0], resp, "test-req", "test-model")
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		header   string
		expected time.Duration
	}{
		{"30", 30 * time.Second},
		{"0", 0},
		{"", 0},
		{"invalid", 0},
	}
	for _, tc := range tests {
		resp := &http.Response{
			Header: http.Header{"Retry-After": {tc.header}},
		}
		got := parseRetryAfter(resp)
		if got != tc.expected {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.header, got, tc.expected)
		}
	}
}

func TestParseRetryAfterNil(t *testing.T) {
	if got := parseRetryAfter(nil); got != 0 {
		t.Errorf("parseRetryAfter(nil) = %v, want 0", got)
	}
}

func TestProxyChatWithBodyEmptyPool(t *testing.T) {
	// Empty pool should return 503 with proper JSON
	p := pool.NewPool(nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(p, w, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, &config.Config{})

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("response missing error object")
	}
	if errObj["message"] != "No accounts configured" {
		t.Errorf("error message = %q, want 'No accounts configured'", errObj["message"])
	}
}

func TestNewProxyHandlerModelsEndpoint(t *testing.T) {
	cfg := &config.Config{
		ModelRemap: map[string]string{"gpt-4": "premium"},
		ModelTiers: map[string]string{"premium": "gpt-4-turbo"},
	}
	p := pool.NewPool(nil)
	holder := config.NewConfigHolder(cfg)
	handler := NewProxyHandler(p, config.WireAPIBoth, holder, nil)

	// Test GET /v1/models
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	handler.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestIsHopByHop(t *testing.T) {
	if !isHopByHop("Connection") {
		t.Error("isHopByHop(Connection) = false, want true")
	}
	if !isHopByHop("Transfer-Encoding") {
		t.Error("isHopByHop(Transfer-Encoding) = false, want true")
	}
	if isHopByHop("Content-Type") {
		t.Error("isHopByHop(Content-Type) = true, want false")
	}
	if isHopByHop("Authorization") {
		t.Error("isHopByHop(Authorization) = true, want false")
	}
}

func TestCopyClientHeaders(t *testing.T) {
	src := httptest.NewRequest("GET", "/", nil)
	src.Header.Set("Content-Type", "application/json")
	src.Header.Set("Accept", "text/plain")
	src.Header.Set("Cookie", "session=abc123")
	src.Header.Set("X-Api-Key", "sk-test123")
	src.Header.Set("X-Auth-Token", "token123")
	src.Header.Set("Authorization", "Bearer sk-test456")
	src.Header.Set("Connection", "keep-alive")

	dst := make(http.Header)
	copyClientHeaders(dst, src.Header)

	// Sensitive headers should NOT be copied
	if dst.Get("Cookie") != "" {
		t.Error("Cookie header was copied, should have been filtered")
	}
	if dst.Get("X-Api-Key") != "" {
		t.Error("X-Api-Key header was copied, should have been filtered")
	}
	if dst.Get("X-Auth-Token") != "" {
		t.Error("X-Auth-Token header was copied, should have been filtered")
	}
	if dst.Get("Authorization") != "" {
		t.Error("Authorization header was copied, should have been filtered")
	}
	if dst.Get("Connection") != "" {
		t.Error("Connection header was copied, should have been filtered (hop-by-hop)")
	}

	// Safe headers should be copied
	if dst.Get("Content-Type") != "application/json" {
		t.Error("Content-Type header was not copied")
	}
	if dst.Get("Accept") != "text/plain" {
		t.Error("Accept header was not copied")
	}
}

func TestUpstreamError4xxPassthrough(t *testing.T) {
	// Case 1: upstream returns application/json → client gets application/json
	upstreamJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"bad request","code":"invalid_request","api_key":"sk-secret"}}`))
	}))
	defer upstreamJSON.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstreamJSON.URL}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != 400 {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// Body should be redacted (api_key → ***)
	if !strings.Contains(rec.Body.String(), `"***"`) {
		t.Errorf("expected redacted api_key in body, got %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-secret") {
		t.Errorf("body should not contain sk-secret, got %s", rec.Body.String())
	}
	// Content-Length must not be present (body was redacted, length changed)
	if rec.Header().Get("Content-Length") != "" {
		t.Error("Content-Length should not be set after body redaction")
	}

	// Account should NOT be in cooldown (4xx is client error)
	accs := p.AllAccounts()
	if accs[0].IsInCooldown() {
		t.Error("account should NOT be in cooldown after 4xx")
	}

	// Case 2: upstream returns text/plain → client gets text/plain
	upstreamPlain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(404)
		w.Write([]byte("not found"))
	}))
	defer upstreamPlain.Close()

	cfg2 := &config.Config{Accounts: []config.AccountConfig{{Name: "test2", Key: "k", BaseURL: upstreamPlain.URL}}}
	p2 := pool.NewPool(cfg2.Accounts)

	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r2.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(p2, rec2, r2, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg2)

	if rec2.Code != 404 {
		t.Errorf("case text/plain: expected status 404, got %d", rec2.Code)
	}
	if ct := rec2.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("case text/plain: Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
}

func TestUpstream5xxCooldown(t *testing.T) {
	// Mock upstream returning 503
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"message":"service unavailable"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	// After retries, should get 503 (all accounts exhausted)
	if rec.Code != 503 {
		t.Errorf("expected status 503, got %d", rec.Code)
	}

	// Account should be in cooldown
	accs := p.AllAccounts()
	if !accs[0].IsInCooldown() {
		t.Error("account should be in cooldown after 5xx")
	}
}

func TestClientDisconnectedNoRetry(t *testing.T) {
	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.WriteHeader(200)
		w.Write([]byte(`{"choices":[{"delta":{"content":"hello"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r = r.WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	cancel()

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if strings.Contains(rec.Body.String(), "all_exhausted") {
		t.Error("should not have reached 'all accounts exhausted' for disconnected client")
	}
	accs := p.AllAccounts()
	if accs[0].IsInCooldown() {
		t.Error("account should NOT be in cooldown after client disconnect")
	}
	if atomic.LoadInt32(&upstreamCalls) > 1 {
		t.Errorf("upstream calls = %d, want <= 1", upstreamCalls)
	}
}

func TestUpstreamConnectionErrorRetry(t *testing.T) {
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: "http://127.0.0.1:1"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != 503 {
		t.Errorf("expected status 503, got %d", rec.Code)
	}
	accs := p.AllAccounts()
	if !accs[0].IsInCooldown() {
		t.Error("account should be in cooldown after connection error")
	}
}

func TestUpstream401Retry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != 503 {
		t.Errorf("expected status 503, got %d", rec.Code)
	}
	accs := p.AllAccounts()
	if accs[0].Status() != pool.StatusExhausted {
		t.Errorf("account status = %d, want StatusExhausted", accs[0].Status())
	}
}

func TestUpstream429CooldownRetry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != 503 {
		t.Errorf("expected status 503, got %d", rec.Code)
	}
	accs := p.AllAccounts()
	if !accs[0].IsInCooldown() {
		t.Error("account should be in cooldown after 429")
	}
}

func TestChatPassthrough2xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"hello"}}]}

data: {"choices":[{"delta":{"content":" world"}}]}

data: [DONE]

`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{ResponsesOut: false, Stream: false}, cfg)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Error("body should contain SSE content 'hello'")
	}
}

func TestHandleUpstreamResponse_NoDoubleCount(t *testing.T) {
	// Reset metrics
	util.MetricsRequestsTotal.Set(0)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if util.MetricsRequestsTotal.Value() != 1 {
		t.Errorf("requests_total = %d, want 1 (double counting detected)", util.MetricsRequestsTotal.Value())
	}
}

func TestCopyClientHeaders_StripsAcceptEncoding(t *testing.T) {
	src := http.Header{
		"Content-Type":    {"application/json"},
		"Accept-Encoding": {"gzip"},
		"Authorization":   {"Bearer sk-test"},
	}
	dst := http.Header{}
	copyClientHeaders(dst, src)

	// Authorization and Accept-Encoding must be stripped.
	if dst.Get("Authorization") != "" {
		t.Errorf("Authorization should be stripped, got %q", dst.Get("Authorization"))
	}
	for k := range dst {
		if http.CanonicalHeaderKey(k) == "Accept-Encoding" {
			t.Errorf("Accept-Encoding should be stripped, found %q", k)
		}
	}
	// Content-Type must pass through.
	if dst.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type should pass through, got %q", dst.Get("Content-Type"))
	}
}

func TestUpstream4xx_NoGzipRedactOK(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assert upstream received no Accept-Encoding.
		if r.Header.Get("Accept-Encoding") != "" {
			t.Errorf("upstream received Accept-Encoding: %s", r.Header.Get("Accept-Encoding"))
		}
		body := `{"error":{"message":"bad request, api_key=sk-exposed123","code":"invalid_request"}}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(body))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "sk-test-key", BaseURL: upstream.URL}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept-Encoding", "gzip")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != 400 {
		t.Fatalf("expected status 400, got %d", rec.Code)
	}
	// Body must be valid JSON after redaction.
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	// api_key value should be redacted to ***.
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatal("response missing error object")
	}
	msg, _ := errObj["message"].(string)
	if strings.Contains(msg, "sk-exposed123") {
		t.Error("sensitive value not redacted in 4xx body")
	}
}

func TestCopyUpstreamHeaders_Allowlist(t *testing.T) {
	rec := httptest.NewRecorder()

	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("Content-Disposition", "attachment; filename=out.json")
	src.Set("Content-Language", "en")
	src.Set("Retry-After", "120")
	src.Set("Server", "nginx/1.21")
	src.Set("Via", "1.1 proxy")
	src.Set("X-RateLimit-Remaining", "99")
	src.Set("X-RateLimit-Limit", "100")
	src.Set("X-Request-ID", "abc123")

	copyUpstreamHeaders(rec, src)

	dst := rec.Header()

	// Allowed headers must be present.
	for _, allowed := range []string{"Content-Type", "Content-Disposition", "Content-Language", "Retry-After"} {
		if dst.Get(allowed) == "" {
			t.Errorf("allowed header %q missing from response", allowed)
		}
	}

	// Disallowed headers must be stripped.
	for _, disallowed := range []string{"Server", "Via", "X-RateLimit-Remaining", "X-RateLimit-Limit", "X-Request-ID"} {
		if dst.Get(disallowed) != "" {
			t.Errorf("disallowed header %q leaked through", disallowed)
		}
	}
}
