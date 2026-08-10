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
