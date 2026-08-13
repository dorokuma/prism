package middleware_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dorokuma/prism/internal/middleware"
)

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
		{"localhost:1234", false},          // hostname, not IP – rejected
		{"192.168.1.1:12345", false},
		{"10.0.0.1:8080", false},
		{"", false},
	}
	for _, tc := range tests {
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.RemoteAddr = tc.remote
		if got := middleware.IsLocalhost(r); got != tc.want {
			t.Errorf("IsLocalhost(%q) = %v, want %v", tc.remote, got, tc.want)
		}
	}
}

// TestHasForwardedHeaders pins the forwarding-header detection: any of
// X-Forwarded-For, X-Real-IP or the RFC 7239 Forwarded header marks a
// request as proxied (a same-machine reverse proxy presents a loopback
// RemoteAddr AND adds one of these), and absence means a direct client.
func TestHasForwardedHeaders(t *testing.T) {
	for name, hdr := range map[string]string{
		"X-Forwarded-For": "10.0.0.9",
		"X-Real-IP":       "10.0.0.9",
		"Forwarded":       "for=10.0.0.9;proto=https",
		"forwarded":       "for=10.0.0.9", // case-insensitive header lookup
	} {
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set(name, hdr)
		if !middleware.HasForwardedHeaders(r) {
			t.Errorf("HasForwardedHeaders(%s=%q) = false, want true (proxied request)", name, hdr)
		}
	}

	// A direct local request carries none of them.
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	if middleware.HasForwardedHeaders(r) {
		t.Error("HasForwardedHeaders(direct loopback) = true, want false")
	}
	// An empty header value does not count.
	r2 := httptest.NewRequest("GET", "/metrics", nil)
	r2.RemoteAddr = "127.0.0.1:12345"
	r2.Header.Set("Forwarded", "")
	if middleware.HasForwardedHeaders(r2) {
		t.Error("HasForwardedHeaders with an empty Forwarded value = true, want false")
	}
}

func TestCheckAuth(t *testing.T) {
	// Auth disabled (empty token) → always pass
	if !middleware.CheckAuth(httptest.NewRequest("GET", "/", nil), "") {
		t.Error("CheckAuth with empty token should return true")
	}

	// No header → fail
	r := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	if middleware.CheckAuth(r, "secret") {
		t.Error("CheckAuth with no header should return false")
	}

	// Wrong header → fail
	r.Header.Set("Authorization", "Bearer wrong")
	if middleware.CheckAuth(r, "secret") {
		t.Error("CheckAuth with wrong token should return false")
	}

	// Correct header → pass
	r.Header.Set("Authorization", "Bearer secret")
	if !middleware.CheckAuth(r, "secret") {
		t.Error("CheckAuth with correct token should return true")
	}

	// Case-insensitive scheme: bearer/BEARER/BeArEr all pass with the same
	// token bytes; only the scheme is folded, the token stays byte-strict.
	for _, scheme := range []string{"bearer", "BEARER", "BeArEr"} {
		rScheme := httptest.NewRequest("GET", "/", nil)
		rScheme.Header.Set("Authorization", scheme+" secret")
		if !middleware.CheckAuth(rScheme, "secret") {
			t.Errorf("CheckAuth with scheme %q should pass", scheme)
		}
	}
	// Case-folded token must NOT match (token comparison is byte-strict).
	rFold := httptest.NewRequest("GET", "/", nil)
	rFold.Header.Set("Authorization", "Bearer SECRET")
	if middleware.CheckAuth(rFold, "secret") {
		t.Error("CheckAuth with a case-folded token should fail (byte-strict comparison)")
	}
	// Bare token (no scheme) still fails.
	rBare := httptest.NewRequest("GET", "/", nil)
	rBare.Header.Set("Authorization", "secret")
	if middleware.CheckAuth(rBare, "secret") {
		t.Error("CheckAuth with a bare token should fail")
	}

	// An empty or whitespace-only credential after the Bearer scheme is
	// rejected outright (byte-strict token semantics, matching Authenticate):
	// "Bearer ", "Bearer   " and "Bearer \t" are not tokens.
	for _, auth := range []string{"Bearer ", "Bearer   ", "Bearer \t"} {
		rEmpty := httptest.NewRequest("GET", "/", nil)
		rEmpty.Header.Set("Authorization", auth)
		if middleware.CheckAuth(rEmpty, "secret") {
			t.Errorf("CheckAuth with Authorization %q should fail (empty/whitespace token)", auth)
		}
	}
	// A double-space "Bearer  secret" must NOT be trimmed into "secret":
	// the token bytes are compared verbatim.
	rDouble := httptest.NewRequest("GET", "/", nil)
	rDouble.Header.Set("Authorization", "Bearer  secret")
	if middleware.CheckAuth(rDouble, "secret") {
		t.Error("CheckAuth with a double-space Bearer token should fail (token bytes are strict)")
	}

	// Long wrong token must not pass (length difference must not leak expected length).
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("Authorization", "Bearer "+string(make([]byte, 200)))
	if middleware.CheckAuth(r2, "secret") {
		t.Error("CheckAuth with long wrong token should return false")
	}

	// Regression: an EXPECTED token longer than the pad must not be
	// truncated into a prefix comparison — a provided value that shares the
	// first bytes but differs at the end must be rejected.
	longToken := strings.Repeat("s", 300)
	prefix := longToken[:250]
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.Header.Set("Authorization", "Bearer "+prefix+"DIFFERENT")
	if middleware.CheckAuth(r3, longToken) {
		t.Error("CheckAuth with a prefix of a too-long token should return false (pad truncation)")
	}
	r4 := httptest.NewRequest("GET", "/", nil)
	r4.Header.Set("Authorization", "Bearer "+longToken)
	if middleware.CheckAuth(r4, longToken) {
		t.Error("CheckAuth with a token longer than the pad must be rejected, not truncated")
	}
}
