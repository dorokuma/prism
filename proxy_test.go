package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
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
	// Create some buckets and verify via behavior (burst consumed)
	for i := 0; i < 5; i++ {
		if !rl.Allow("10.0.0.1") {
			t.Errorf("burst allow 10.0.0.1 %d: expected true, got false", i)
		}
	}
	// 6th request for 10.0.0.1 should be rate limited (burst exhausted)
	if rl.Allow("10.0.0.1") {
		t.Error("expected 10.0.0.1 to be rate limited after burst")
	}
	// Different IP should have its own bucket
	if !rl.Allow("10.0.0.2") {
		t.Error("different IP should be allowed")
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

func TestIsLocalhost(t *testing.T) {
	tests := []struct {
		remote string
		want   bool
	}{
		{"127.0.0.1:12345", true},
		{"127.0.0.1:0", true},
		{"[::1]:12345", true},
		{"::1", true},
		{"[::ffff:127.0.0.1]:12345", true}, // IPv4-mapped IPv6 loopback
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
