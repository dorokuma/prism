package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRedactBody(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    `{"error":"invalid api key FAKE_KEY_FOR_TESTING_1234567890","message":"bad key"}`,
			expected: `{"error":"invalid api key sk-***","message":"bad key"}`,
		},
		{
			input:    `{"error":"Bearer abc123def456ghi789jkl012 token invalid"}`,
			expected: `{"error":"Bearer *** token invalid"}`,
		},
		{
			input:    `{"error":"api key FAKE_KEY_WITH_DASHES_FOR_TESTING with dashes"}`,
			expected: `{"error":"api key sk-*** with dashes"}`,
		},
		{
			input:    `{"error":"api key FAKE_KEY_WITH_UNDERSCORES_FOR_TESTING"}`,
			expected: `{"error":"api key sk-***"}`,
		},
		{
			input:    `{"message":"no sensitive data here"}`,
			expected: `{"message":"no sensitive data here"}`,
		},
	}
	for _, tc := range tests {
		got := redactBody([]byte(tc.input))
		if got != tc.expected {
			t.Errorf("redactBody(%q) = %q, want %q", tc.input, got, tc.expected)
		}
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
	handleUpstreamError(nil, nil)
}

func TestHandleUpstreamErrorNoBody(t *testing.T) {
	// Should not panic with empty response
	resp := &http.Response{
		StatusCode: 500,
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}
	handleUpstreamError(&Account{cfg: AccountConfig{Name: "test"}}, resp)
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
	handler := NewProxyHandler(pool, WireAPIBoth, cfg)

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
	metricsRequestsTotal.Set(0)
	metricsErrorsTotal.Set(0)
	metricsRateLimitedTotal.Set(0)
	metricsUpstreamRetries.Set(0)

	recordRequest(100 * time.Millisecond)
	recordError()
	recordRateLimited()
	recordUpstreamRetry()

	if metricsRequestsTotal.Value() != 1 {
		t.Errorf("requests_total = %d, want 1", metricsRequestsTotal.Value())
	}
	if metricsErrorsTotal.Value() != 1 {
		t.Errorf("errors_total = %d, want 1", metricsErrorsTotal.Value())
	}
	if metricsRateLimitedTotal.Value() != 1 {
		t.Errorf("rate_limited_total = %d, want 1", metricsRateLimitedTotal.Value())
	}
	if metricsUpstreamRetries.Value() != 1 {
		t.Errorf("upstream_retries = %d, want 1", metricsUpstreamRetries.Value())
	}
}

func TestGetClientIP(t *testing.T) {
	// Test X-Forwarded-For
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.1, 10.0.0.1, 192.168.1.1")
	if ip := getClientIP(r); ip != "203.0.113.1" {
		t.Errorf("X-Forwarded-For: got %q, want 203.0.113.1", ip)
	}

	// Test X-Forwarded-For with only private IPs — should skip to X-Real-IP
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("X-Forwarded-For", "10.0.0.1, 192.168.1.1")
	r2.Header.Set("X-Real-IP", "203.0.113.2")
	if ip := getClientIP(r2); ip != "203.0.113.2" {
		t.Errorf("X-Real-IP: got %q, want 203.0.113.2", ip)
	}

	// Test RemoteAddr fallback
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.RemoteAddr = "198.51.100.1:34567"
	if ip := getClientIP(r3); ip != "198.51.100.1" {
		t.Errorf("RemoteAddr: got %q, want 198.51.100.1", ip)
	}
}
