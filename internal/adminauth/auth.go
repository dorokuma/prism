// Package adminauth is the shared fail-closed check for admin HTTP endpoints
// (/admin/usage/summary, /admin/quota). It matches the previous
// usage.SummaryHandler.authorized behaviour byte-for-byte.
package adminauth

import (
	"crypto/subtle"
	"net/http"
	"os"

	"github.com/dorokuma/prism/internal/middleware"
)

// TokenPadLen is the fixed pad for constant-time admin-token comparison.
// A token longer than the pad cannot be compared without truncation and is
// rejected outright.
const TokenPadLen = 256

// Authorized decides admin access. Fail-closed: a configured
// PRISM_ADMIN_TOKEN means loopback is NOT trusted and every request must
// present the correct Bearer token. The token is re-read from the
// environment on every call, so a rotation takes effect without a restart.
// Only when the token is unset does the loopback shortcut apply, and even
// then only for DIRECT local requests (no forwarding headers).
func Authorized(r *http.Request) bool {
	token := os.Getenv("PRISM_ADMIN_TOKEN")
	if token != "" {
		got, ok := middleware.SplitBearerToken(r.Header.Get("Authorization"))
		if !ok {
			return false
		}
		if len(got) > TokenPadLen || len(token) > TokenPadLen {
			return false
		}
		pb := make([]byte, TokenPadLen)
		eb := make([]byte, TokenPadLen)
		copy(pb, got)
		copy(eb, token)
		return subtle.ConstantTimeCompare(pb, eb) == 1
	}
	return middleware.IsLocalhost(r) && !middleware.HasForwardedHeaders(r)
}
