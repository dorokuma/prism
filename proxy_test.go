package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/util"
)

func TestRedactBody(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    `{"error":"invalid api key sk-FAKE_KEY_FOR_TESTING_1234567890","message":"bad key"}`,
			expected: `{"error":"invalid api key sk-***","message":"bad key"}`,
		},
		{
			input:    `{"error":"Bearer abc123def456ghi789jkl012 token invalid"}`,
			expected: `{"error":"Bearer *** token invalid"}`,
		},
		{
			input:    `{"error":"api key sk-FAKE-KEY-WITH-DASHES-FOR-TESTING with dashes"}`,
			expected: `{"error":"api key sk-*** with dashes"}`,
		},
		{
			input:    `{"error":"api key sk-FAKE_KEY_WITH_UNDERSCORES_FOR_TESTING"}`,
			expected: `{"error":"api key sk-***"}`,
		},
		{
			input:    `{"message":"no sensitive data here"}`,
			expected: `{"message":"no sensitive data here"}`,
		},
		{
			input:    `{"api_key":"sk-xxx","data":{"token":"t1","name":"ok"}}`,
			expected: `{"api_key":"***","data":{"name":"ok","token":"***"}}`,
		},
		{
			input:    `{"password":"secret123","nested":{"ACCESS_TOKEN":"tok-abc","info":"keep"}}`,
			expected: `{"nested":{"ACCESS_TOKEN":"***","info":"keep"},"password":"***"}`,
		},
		{
			input:    `{"authorization":"Bearer abc","secret":"s3cr3t"}`,
			expected: `{"authorization":"***","secret":"***"}`,
		},
	}
	for _, tc := range tests {
		got := redactBody([]byte(tc.input))
		// Normalize via json.Unmarshal + json.Marshal to handle key ordering differences
		var gotMap, wantMap map[string]any
		json.Unmarshal([]byte(got), &gotMap)
		json.Unmarshal([]byte(tc.expected), &wantMap)
		gotNorm, _ := json.Marshal(gotMap)
		wantNorm, _ := json.Marshal(wantMap)
		if string(gotNorm) != string(wantNorm) {
			t.Errorf("redactBody(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestRedactJSONTooDeep(t *testing.T) {
	// Build a JSON object with 21+ levels of nesting.
	// The innermost level should become "<redacted:too deep>".
	var build func(depth int) any
	build = func(depth int) any {
		if depth >= 22 {
			return map[string]any{"secret": "sk-leak"}
		}
		return map[string]any{"a": build(depth + 1)}
	}
	deep := build(1)
	raw, err := json.Marshal(deep)
	if err != nil {
		t.Fatal(err)
	}

	result := redactBody(raw)

	// The innermost object at depth > 20 should be replaced.
	// json.Marshal escapes < and >, so look for the escaped form.
	if !strings.Contains(result, `\u003credacted:too deep\u003e`) {
		t.Fatalf("expected escaped '<redacted:too deep>' in output, got: %s", result)
	}
	// The secret key should NOT appear.
	if strings.Contains(result, "sk-leak") {
		t.Fatal("sk-leak should not appear in the redacted output")
	}

	// Verify the outer structure is still JSON.
	var parsed any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("redacted output is not valid JSON: %v", err)
	}

	// Check that redactJSON returns the marker for depth > 20.
	tooDeep := map[string]any{
		"l1": map[string]any{
			"l2": map[string]any{
				"l3": map[string]any{
					"l4": map[string]any{
						"l5": map[string]any{
							"l6": map[string]any{
								"l7": map[string]any{
									"l8": map[string]any{
										"l9": map[string]any{
											"l10": map[string]any{
												"l11": map[string]any{
													"l12": map[string]any{
														"l13": map[string]any{
															"l14": map[string]any{
																"l15": map[string]any{
																	"l16": map[string]any{
																		"l17": map[string]any{
																			"l18": map[string]any{
																				"l19": map[string]any{
																					"l20": map[string]any{
																						"l21": map[string]any{
																							"token": "abc",
																						},
																					},
																				},
																			},
																		},
																	},
																},
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	raw2, _ := json.Marshal(tooDeep)
	result2 := redactBody(raw2)
	// The inner 21st level depth reaches > 20, so it should be redacted:too deep.
	if !strings.Contains(result2, `\u003credacted:too deep\u003e`) {
		t.Fatalf("nested literal: expected escaped '<redacted:too deep>' in output, got: %s", result2)
	}
}

func TestIsPermanentCredentialError(t *testing.T) {
	makeBody := func(code string) []byte {
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{"code": code},
		})
		return b
	}
	for _, code := range []string{"invalid_api_key", "revoked", "account_deactivated"} {
		if !isPermanentCredentialError(makeBody(code)) {
			t.Errorf("isPermanentCredentialError(%q) = false, want true", code)
		}
	}
	if isPermanentCredentialError(makeBody("insufficient_quota")) {
		t.Errorf("isPermanentCredentialError(insufficient_quota) = true, want false")
	}
	if isPermanentCredentialError(nil) {
		t.Errorf("isPermanentCredentialError(nil) = true, want false")
	}
}

func TestIsQuotaError(t *testing.T) {
	if !isQuotaError([]byte(`{"error":{"type":"gousagelimiterror"}}`)) {
		t.Error("isQuotaError(gousagelimiterror) = false, want true")
	}
	if !isQuotaError([]byte(`{"error":{"code":"insufficient_quota"}}`)) {
		t.Error("isQuotaError(insufficient_quota) = false, want true")
	}
	if !isQuotaError([]byte(`quota exceeded`)) {
		t.Error("isQuotaError('quota exceeded') = false, want true")
	}
	if !isQuotaError([]byte(`usage limit`)) {
		t.Error("isQuotaError('usage limit') = false, want true")
	}
	if isQuotaError([]byte(`{"error":{"code":"invalid_api_key"}}`)) {
		t.Error("isQuotaError(invalid_api_key) = true, want false")
	}
	if isQuotaError(nil) {
		t.Error("isQuotaError(nil) = true, want false")
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
	handleUpstreamError(&Account{cfg: AccountConfig{Name: "test"}}, resp, "test-req", "test-model")
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
	pool := NewPool(nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(pool, w, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, &Config{})

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
	cfg := &Config{
		ModelRemap: map[string]string{"gpt-4": "premium"},
		ModelTiers: map[string]string{"premium": "gpt-4-turbo"},
	}
	pool := NewPool(nil)
	holder := NewConfigHolder(cfg)
	handler := NewProxyHandler(pool, WireAPIBoth, holder)

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

func TestRateLimiterAllow(t *testing.T) {
	rl := newRateLimiter(10, 5) // 10 tokens/sec, burst of 5
	// Burst should allow 5 immediate requests
	for i := 0; i < 5; i++ {
		if !rl.Allow("192.168.1.1") {
			t.Errorf("burst allow %d: expected true, got false", i)
		}
	}
	// 6th request within burst window should be denied
	if rl.Allow("192.168.1.1") {
		t.Error("expected rate limited after burst consumed")
	}
	// Different IP should be allowed (separate bucket)
	if !rl.Allow("192.168.1.2") {
		t.Error("different IP should be allowed")
	}
}

func TestRateLimiterCleanup(t *testing.T) {
	rl := newRateLimiter(10, 5)
	// Create some buckets
	rl.Allow("10.0.0.1")
	rl.Allow("10.0.0.2")
	if len(rl.buckets) != 2 {
		t.Errorf("expected 2 buckets, got %d", len(rl.buckets))
	}
}

func TestRecordMetrics(t *testing.T) {
	// Reset metrics
	util.MetricsRequestsTotal.Set(0)
	util.MetricsErrorsTotal.Set(0)
	util.MetricsRateLimitedTotal.Set(0)
	util.MetricsUpstreamRetries.Set(0)

	recordRequest(100 * time.Millisecond)
	recordError()
	recordRateLimited()
	recordUpstreamRetry()

	if util.MetricsRequestsTotal.Value() != 1 {
		t.Errorf("requests_total = %d, want 1", util.MetricsRequestsTotal.Value())
	}
	if util.MetricsErrorsTotal.Value() != 1 {
		t.Errorf("errors_total = %d, want 1", util.MetricsErrorsTotal.Value())
	}
	if util.MetricsRateLimitedTotal.Value() != 1 {
		t.Errorf("rate_limited_total = %d, want 1", util.MetricsRateLimitedTotal.Value())
	}
	if util.MetricsUpstreamRetries.Value() != 1 {
		t.Errorf("upstream_retries = %d, want 1", util.MetricsUpstreamRetries.Value())
	}
}

func TestGetClientIP(t *testing.T) {
	// (a) No trustedProxies: XFF is ignored entirely, use RemoteAddr
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.1, 10.0.0.1")
	r.RemoteAddr = "198.51.100.1:34567"
	if ip := getClientIP(r, nil); ip != "198.51.100.1" {
		t.Errorf("no trusted proxies: got %q, want 198.51.100.1", ip)
	}

	// (b) Trusted proxies + RemoteAddr in CIDR + XFF → rightmost XFF
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	trusted := []*net.IPNet{cidr}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("X-Forwarded-For", "10.0.0.1, 203.0.113.5")
	r2.RemoteAddr = "198.51.100.2:34567"
	if ip := getClientIP(r2, trusted); ip != "203.0.113.5" {
		t.Errorf("trusted proxy + XFF: got %q, want 203.0.113.5", ip)
	}

	// (c) Trusted proxies + RemoteAddr NOT in CIDR + XFF → use RemoteAddr
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.Header.Set("X-Forwarded-For", "10.0.0.1, 203.0.113.5")
	r3.RemoteAddr = "100.64.0.1:34567"
	if ip := getClientIP(r3, trusted); ip != "100.64.0.1" {
		t.Errorf("untrusted remote + XFF: got %q, want 100.64.0.1", ip)
	}

	// (d) Trusted proxies + RemoteAddr trusted + X-Real-IP (no XFF)
	r4 := httptest.NewRequest("GET", "/", nil)
	r4.Header.Set("X-Real-IP", "203.0.113.6")
	r4.RemoteAddr = "198.51.100.3:34567"
	if ip := getClientIP(r4, trusted); ip != "203.0.113.6" {
		t.Errorf("trusted proxy + X-Real-IP: got %q, want 203.0.113.6", ip)
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

func TestIsLocalhost(t *testing.T) {
	tests := []struct {
		remote  string
		want    bool
	}{
		{"127.0.0.1:12345", true},
		{"127.0.0.1:0", true},
		{"[::1]:12345", true},
		{"::1", true},
		{"[::ffff:127.0.0.1]:12345", true},   // IPv4-mapped IPv6 loopback
		{"localhost:1234", false},           // hostname, not IP – rejected
		{"192.168.1.1:12345", false},
		{"10.0.0.1:8080", false},
		{"", false},
	}
	for _, tc := range tests {
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.RemoteAddr = tc.remote
		if got := IsLocalhost(r); got != tc.want {
			t.Errorf("IsLocalhost(%q) = %v, want %v", tc.remote, got, tc.want)
		}
	}
}

func TestCheckAuth(t *testing.T) {
	// Auth disabled (empty token) → always pass
	if !CheckAuth(httptest.NewRequest("GET", "/", nil), "") {
		t.Error("CheckAuth with empty token should return true")
	}

	// No header → fail
	r := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	if CheckAuth(r, "secret") {
		t.Error("CheckAuth with no header should return false")
	}

	// Wrong header → fail
	r.Header.Set("Authorization", "Bearer wrong")
	if CheckAuth(r, "secret") {
		t.Error("CheckAuth with wrong token should return false")
	}

	// Correct header → pass
	r.Header.Set("Authorization", "Bearer secret")
	if !CheckAuth(r, "secret") {
		t.Error("CheckAuth with correct token should return true")
	}

	// Long wrong token must not pass (length difference must not leak expected length).
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("Authorization", "Bearer "+string(make([]byte, 200)))
	if CheckAuth(r2, "secret") {
		t.Error("CheckAuth with long wrong token should return false")
	}
}

func TestUpstreamError4xxPassthrough(t *testing.T) {
	// Case 1: upstream returns application/json → client gets application/json
	upstreamJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "req-123")
		w.Header().Set("Date", "Thu, 01 Jan 2025 00:00:00 GMT")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"bad request","code":"invalid_request","api_key":"sk-secret"}}`))
	}))
	defer upstreamJSON.Close()

	cfg := &Config{Accounts: []AccountConfig{{Name: "test", Key: "k", BaseURL: upstreamJSON.URL}}}
	pool := NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, cfg)

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
	// X-Request-ID must NOT be forwarded (excluded by allowlist).
	if rid := rec.Header().Get("X-Request-ID"); rid != "" {
		t.Errorf("X-Request-ID should not be forwarded, got %q", rid)
	}
	// Content-Length must not be present (body was redacted, length changed)
	if rec.Header().Get("Content-Length") != "" {
		t.Error("Content-Length should not be set after body redaction")
	}

	// Account should NOT be in cooldown (4xx is client error)
	accs := pool.AllAccounts()
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

	cfg2 := &Config{Accounts: []AccountConfig{{Name: "test2", Key: "k", BaseURL: upstreamPlain.URL}}}
	pool2 := NewPool(cfg2.Accounts)

	rec2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r2.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(pool2, rec2, r2, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, cfg2)

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

	cfg := &Config{Accounts: []AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	pool := NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, cfg)

	// After retries, should get 503 (all accounts exhausted)
	if rec.Code != 503 {
		t.Errorf("expected status 503, got %d", rec.Code)
	}

	// Account should be in cooldown
	accs := pool.AllAccounts()
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

	cfg := &Config{Accounts: []AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	pool := NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r = r.WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	cancel()

	proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, cfg)

	if strings.Contains(rec.Body.String(), "all_exhausted") {
		t.Error("should not have reached 'all accounts exhausted' for disconnected client")
	}
	accs := pool.AllAccounts()
	if accs[0].IsInCooldown() {
		t.Error("account should NOT be in cooldown after client disconnect")
	}
	if atomic.LoadInt32(&upstreamCalls) > 1 {
		t.Errorf("upstream calls = %d, want <= 1", upstreamCalls)
	}
}

func TestUpstreamConnectionErrorRetry(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{Name: "test", Key: "k", BaseURL: "http://127.0.0.1:1"}}}
	pool := NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, cfg)

	if rec.Code != 503 {
		t.Errorf("expected status 503, got %d", rec.Code)
	}
	accs := pool.AllAccounts()
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

	cfg := &Config{Accounts: []AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	pool := NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, cfg)

	if rec.Code != 503 {
		t.Errorf("expected status 503, got %d", rec.Code)
	}
	accs := pool.AllAccounts()
	if accs[0].Status() != StatusExhausted {
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

	cfg := &Config{Accounts: []AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	pool := NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, cfg)

	if rec.Code != 503 {
		t.Errorf("expected status 503, got %d", rec.Code)
	}
	accs := pool.AllAccounts()
	if !accs[0].IsInCooldown() {
		t.Error("account should be in cooldown after 429")
	}
}

func TestChatPassthrough2xx(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", "req-12345")
		w.WriteHeader(200)
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"hello"}}]}

data: {"choices":[{"delta":{"content":" world"}}]}

data: [DONE]

`))
	}))
	defer upstream.Close()

	cfg := &Config{Accounts: []AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	pool := NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{responsesOut: false, stream: false}, cfg)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	// X-Request-ID must NOT be forwarded (excluded by allowlist).
	if rec.Header().Get("X-Request-ID") != "" {
		t.Errorf("X-Request-ID should not be forwarded, got %q", rec.Header().Get("X-Request-ID"))
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

	cfg := &Config{Accounts: []AccountConfig{{Name: "test", Key: "k", BaseURL: upstream.URL}}}
	pool := NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")

	proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), chatForwardOpts{}, cfg)

	if util.MetricsRequestsTotal.Value() != 1 {
		t.Errorf("requests_total = %d, want 1 (double counting detected)", util.MetricsRequestsTotal.Value())
	}
}

func TestCopyClientHeaders_StripsAcceptEncoding(t *testing.T) {
	src := http.Header{
		"Content-Type":    {"application/json"},
		"Accept-Encoding": {"gzip"},
		"Accept-encoding": {"br"},
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

	cfg := &Config{Accounts: []AccountConfig{{Name: "test", Key: "sk-test-key", BaseURL: upstream.URL}}}
	pool := NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept-Encoding", "gzip")

	proxyChatWithBody(pool, rec, r, []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`), time.Now(), chatForwardOpts{}, cfg)

	if rec.Code != 400 {
		t.Fatalf("expected status 400, got %d", rec.Code)
	}
	// Body must be valid JSON after redaction.
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	// api_key value should be redacted to **.
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatal("response missing error object")
	}
	msg, _ := errObj["message"].(string)
	if strings.Contains(msg, "sk-exposed123") {
		t.Error("sensitive value not redacted in 4xx body")
	}
}

func TestRedact_AccountKey(t *testing.T) {
	// redactBodyBytesWithKeys replaces the account key as a literal substring.
	body := []byte(`{"error":{"message":"auth failed for key abc123sekret","code":"unauthorized"}}`)
	got := redactBodyBytesWithKeys(body, []string{"abc123sekret"})
	if bytes.Contains(got, []byte("abc123sekret")) {
		t.Errorf("account key not redacted: %s", got)
	}
	if !bytes.Contains(got, []byte("***")) {
		t.Error("expected *** redaction marker not found")
	}

	// sensitiveJSONKeys with key/client_key/session_key → values replaced with ***.
	body2 := []byte(`{"key":"my-secret-key","client_key":"ck-secret","session_key":"sk-secret","name":"ok"}`)
	got2 := redactBodyBytes(body2)
	var m map[string]any
	if err := json.Unmarshal(got2, &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, k := range []string{"key", "client_key", "session_key"} {
		if v, ok := m[k]; !ok || v != "***" {
			t.Errorf("sensitive key %q not redacted: %v", k, v)
		}
	}
	if m["name"] != "ok" {
		t.Errorf("non-sensitive key 'name' was modified: %v", m["name"])
	}

	// sk- prefixed tokens still covered by original regex.
	body3 := []byte(`{"error":"invalid key sk-FAKE1234567890"}`)
	got3 := redactBodyBytes(body3)
	if bytes.Contains(got3, []byte("sk-FAKE1234567890")) {
		t.Errorf("sk- key not redacted by regex: %s", got3)
	}
	if !bytes.Contains(got3, []byte("sk-***")) {
		t.Errorf("expected 'sk-***' redaction marker, got: %s", got3)
	}
}

func TestRedact_ExistingBehaviorUnchanged(t *testing.T) {
	// Ensure redactBodyBytes without extraKeys behaves identically to before.
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    `{"error":"api key sk-FAKE_KEY_FOR_TESTING_1234567890","message":"bad"}`,
			expected: `{"error":"api key sk-***","message":"bad"}`,
		},
		{
			input:    `{"api_key":"sk-xxx","data":{"token":"t1","name":"ok"}}`,
			expected: `{"api_key":"***","data":{"name":"ok","token":"***"}}`,
		},
	}
	for _, tc := range tests {
		got := redactBodyBytes([]byte(tc.input))
		var gotMap, wantMap map[string]any
		json.Unmarshal(got, &gotMap)
		json.Unmarshal([]byte(tc.expected), &wantMap)
		gotNorm, _ := json.Marshal(gotMap)
		wantNorm, _ := json.Marshal(wantMap)
		if string(gotNorm) != string(wantNorm) {
			t.Errorf("redactBodyBytes(%q) = %s, want %s", tc.input, gotNorm, wantNorm)
		}
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
