package middleware

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"github.com/dorokuma/prism/internal/config"
)

// authPadLen is the fixed comparison length for constant-time token
// comparison. Both the legacy CheckAuth and the key-set Authenticate use the
// same pad so the two implementations cannot diverge. Inputs longer than the
// pad cannot be compared without truncation (a prefix match would pass) and
// are rejected outright; config loading rejects keys longer than this for
// the same reason (config.maxAPIKeyTokenBytes).
const authPadLen = 256

// SplitBearerToken splits an Authorization header value into the auth scheme
// and the credential, applying the shared Bearer semantics used by every
// auth path (business keys via Authenticate, legacy single-token CheckAuth,
// and the usage admin token):
//   - the scheme is matched case-insensitively (Bearer / bearer / BEARER /
//     BeArEr are all accepted); a missing scheme or a scheme other than
//     Bearer is rejected;
//   - an empty credential ("Bearer ") and a whitespace-only credential
//     ("Bearer   ", "Bearer \t") are rejected outright — they are not
//     tokens;
//   - otherwise the token bytes are returned VERBATIM: only the scheme
//     match is case-insensitive, the token itself is never folded or
//     trimmed, and callers compare it with constant-time byte comparison.
func SplitBearerToken(header string) (token string, ok bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	if token == "" || strings.TrimSpace(token) == "" {
		return "", false
	}
	return token, true
}

// CheckAuth returns true if the request carries a valid Authorization header for
// the given token. When token is empty, all requests pass (auth disabled).
// It is kept for legacy single-token callers (e.g. the /metrics endpoint);
// new callers should use Authenticate with the full api_keys set.
func CheckAuth(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	provided := r.Header.Get("Authorization")
	tokenBytes, ok := SplitBearerToken(provided)
	if !ok {
		return false
	}

	// Pad both to a fixed length before constant-time comparison so that
	// unequal lengths do not short-circuit the comparison and leak the
	// length of expected via timing. Either side longer than the pad would
	// be truncated and a prefix could match, so reject those outright.
	if len(tokenBytes) > authPadLen || len(token) > authPadLen {
		return false
	}
	pb := make([]byte, authPadLen)
	eb := make([]byte, authPadLen)
	copy(pb, tokenBytes)
	copy(eb, token)
	return subtle.ConstantTimeCompare(pb, eb) == 1
}

// Authenticate validates the request's Authorization header against a set of
// configured API keys and returns the name of the matched key on success.
//
//   - keys empty → request passes and keyName is "anonymous" (matches the
//     historical auth-disabled behavior).
//   - otherwise the Authorization header MUST carry a Bearer scheme
//     (case-insensitive: "Bearer ", "bearer ", "BEARER " all accepted);
//     a bare token (no scheme) is rejected outright. The scheme is
//     mandatory to keep the auth surface tight (no accidental acceptance
//     of raw tokens). The remainder is compared against every key token
//     with subtle.ConstantTimeCompare (lengths padded to a fixed size so
//     unequal lengths never short-circuit the comparison). Only the scheme
//     match is case-insensitive — the token bytes are compared strictly.
//
// The loop ALWAYS scans the full key set before returning: there is no early
// return on match, so the position of a hit (or whether one exists) cannot
// be observed via timing. Only the key NAME is returned — tokens are never
// exposed to callers or logs.
func Authenticate(r *http.Request, keys []config.APIKey) (keyName string, ok bool) {
	if len(keys) == 0 {
		return "anonymous", true
	}
	provided := r.Header.Get("Authorization")
	token, found := SplitBearerToken(provided)
	if !found {
		return "", false
	}
	// Pad the provided token to a fixed length so that unequal lengths do
	// not short-circuit the comparison and leak the expected length. A
	// provided token longer than the pad cannot be compared without
	// truncation (a prefix match would pass), so it is rejected outright.
	// The length-based reject leaks only the input's own length class (an
	// input property), never anything about the keys; every ≤pad input is
	// still compared against the full key set below.
	if len(token) > authPadLen {
		return "", false
	}
	pb := make([]byte, authPadLen)
	copy(pb, token)
	matched := false
	matchedName := ""
	for _, k := range keys {
		// Keys with an empty or whitespace-only token are skipped
		// defensively (config loading rejects them, but programmatically
		// built key sets must not let an empty token authenticate every
		// "Bearer " request through the all-zero padded comparison).
		// Keys longer than the pad would be truncated by the copy below and
		// could match on a prefix; config loading rejects them, this guard
		// keeps programmatically built key sets safe too. The loop still
		// scans every key (no early return), preserving the timing profile.
		if strings.TrimSpace(k.Token) == "" || len(k.Token) > authPadLen {
			continue
		}
		eb := make([]byte, authPadLen)
		copy(eb, k.Token)
		if subtle.ConstantTimeCompare(pb, eb) == 1 {
			// Deliberately NO early return: keep scanning the full key set
			// so the hit position is not observable via timing.
			matched = true
			matchedName = k.Name
		}
	}
	return matchedName, matched
}

// apiKeyCtxKey is the unexported context key for the authenticated API key
// name. Only the key NAME is ever stored in the context — never the token.
type apiKeyCtxKey struct{}

// WithAPIKey returns a copy of ctx carrying the authenticated API key name.
func WithAPIKey(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, apiKeyCtxKey{}, name)
}

// APIKeyFromContext returns the authenticated API key name from ctx, or ""
// when absent (unauthenticated requests, /health, or requests that did not
// go through the auth middleware).
func APIKeyFromContext(ctx context.Context) string {
	name, _ := ctx.Value(apiKeyCtxKey{}).(string)
	return name
}

// authStatusCtxKey is the unexported context key for the authentication
// STATUS (whether the request was authenticated against a real api_keys
// set). It is distinct from the key NAME: when auth is disabled the
// middleware still installs the configured default key name (for usage
// attribution), and consumers that need to know whether a REAL credential
// was presented must ask IsAuthenticated instead of checking the name.
type authStatusCtxKey struct{}

// WithAuthenticated returns a copy of ctx carrying the authentication
// status (true = the request passed a real api_keys credential check;
// false = auth disabled or the request bypassed the auth middleware).
func WithAuthenticated(ctx context.Context, authed bool) context.Context {
	return context.WithValue(ctx, authStatusCtxKey{}, authed)
}

// IsAuthenticated reports whether the request was authenticated against a
// configured api_keys set. It is false for auth-disabled deployments and
// for requests that never went through the auth middleware.
func IsAuthenticated(ctx context.Context) bool {
	v, _ := ctx.Value(authStatusCtxKey{}).(bool)
	return v
}

// HasForwardedHeaders reports whether the request carries a forwarding
// header (X-Forwarded-For or X-Real-IP). A same-machine reverse proxy
// presents a loopback RemoteAddr AND adds one of these headers, so loopback
// status alone cannot distinguish a direct local client from a proxied one.
// Endpoints that allow loopback without a token (/metrics,
// /admin/usage/summary) must require the token when this reports true.
func HasForwardedHeaders(r *http.Request) bool {
	return r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != ""
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
