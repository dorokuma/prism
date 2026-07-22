package middleware

import (
	"crypto/subtle"
	"net"
	"net/http"
)

// CheckAuth returns true if the request carries a valid Authorization header for
// the given token. When token is empty, all requests pass (auth disabled).
func CheckAuth(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	provided := r.Header.Get("Authorization")
	expected := "Bearer " + token

	// Pad both to a fixed length before constant-time comparison so that
	// unequal lengths do not short-circuit the comparison and leak the
	// length of expected via timing.
	const authPadLen = 128
	pb := make([]byte, authPadLen)
	eb := make([]byte, authPadLen)
	copy(pb, provided)
	copy(eb, expected)
	return subtle.ConstantTimeCompare(pb, eb) == 1
}

// IsLocalhost returns true if the request's RemoteAddr is a loopback address.
func IsLocalhost(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
