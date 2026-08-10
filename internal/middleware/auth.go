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

// CheckAuth returns true if the request carries a valid Authorization header for
// the given token. When token is empty, all requests pass (auth disabled).
// It is kept for legacy single-token callers (e.g. the /metrics endpoint);
// new callers should use Authenticate with the full api_keys set.
func CheckAuth(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	provided := r.Header.Get("Authorization")
	expected := "Bearer " + token

	// Pad both to a fixed length before constant-time comparison so that
	// unequal lengths do not short-circuit the comparison and leak the
	// length of expected via timing. Either side longer than the pad would
	// be truncated and a prefix could match, so reject those outright.
	if len(provided) > authPadLen || len(expected) > authPadLen {
		return false
	}
	pb := make([]byte, authPadLen)
	eb := make([]byte, authPadLen)
	copy(pb, provided)
	copy(eb, expected)
	return subtle.ConstantTimeCompare(pb, eb) == 1
}

// Authenticate validates the request's Authorization header against a set of
// configured API keys and returns the name of the matched key on success.
//
//   - keys empty → request passes and keyName is "anonymous" (matches the
//     historical auth-disabled behavior).
//   - otherwise the Authorization header MUST start with "Bearer "; a bare
//     token (no prefix) is rejected outright. The prefix is mandatory to
//     match the legacy CheckAuth behavior and keep the auth surface tight
//     (no accidental acceptance of raw tokens). The remainder is compared
//     against every key token with subtle.ConstantTimeCompare (lengths
//     padded to a fixed size so unequal lengths never short-circuit the
//     comparison).
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
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(provided, bearerPrefix) {
		return "", false
	}
	provided = provided[len(bearerPrefix):]
	// Pad the provided token to a fixed length so that unequal lengths do
	// not short-circuit the comparison and leak the expected length. A
	// provided token longer than the pad cannot be compared without
	// truncation (a prefix match would pass), so it is rejected outright.
	// The length-based reject leaks only the input's own length class (an
	// input property), never anything about the keys; every ≤pad input is
	// still compared against the full key set below.
	if len(provided) > authPadLen {
		return "", false
	}
	pb := make([]byte, authPadLen)
	copy(pb, provided)
	matched := false
	matchedName := ""
	for _, k := range keys {
		// Keys longer than the pad would be truncated by the copy below and
		// could match on a prefix; config loading rejects them, this guard
		// keeps programmatically built key sets safe too. The loop still
		// scans every key (no early return), preserving the timing profile.
		if len(k.Token) > authPadLen {
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

// IsLocalhost returns true if the request's RemoteAddr is a loopback address.
func IsLocalhost(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
