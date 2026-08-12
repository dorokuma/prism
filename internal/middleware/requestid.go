package middleware

import (
	"context"
	"net/http"
	"unicode"
	"unicode/utf8"

	"github.com/dorokuma/prism/internal/util"
)

// maxRequestIDLen is the maximum accepted length (in bytes) of a
// client-supplied X-Request-ID. The request ID is echoed into the
// X-Request-ID response header, written to the audit log and persisted into
// the usage database, so an unbounded value would let a client bloat every
// downstream sink. 128 bytes is generous for any real client-generated ID
// while keeping the sinks bounded. Longer values are rejected with HTTP 400
// (explicit error, never silent truncation — truncation could make two
// distinct IDs collide in logs/database).
const maxRequestIDLen = 128

// validRequestID reports whether a client-supplied X-Request-ID is
// acceptable: at most maxRequestIDLen bytes, valid UTF-8, and free of
// control characters. Control characters (C0/C1/DEL — including CR, LF,
// TAB) are rejected because the ID is echoed into the response header, the
// audit log and the usage database: a CR/LF would enable header/log/database
// line injection (e.g. a forged trailing header on the response, or forged
// log lines), and other control characters corrupt the log/database rows.
// Invalid UTF-8 is rejected for the same reason (bare 0x80-0xFF bytes are
// not printable text in any sink). Rejecting the request outright (400
// invalid_request_id) is the explicit, diagnosable choice — silently
// rewriting the ID could hide the attack from operators.
func validRequestID(rid string) bool {
	if len(rid) > maxRequestIDLen {
		return false
	}
	if !utf8.ValidString(rid) {
		return false
	}
	for _, r := range rid {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// RequestIDMiddleware extracts X-Request-ID from the incoming request header or
// generates a new random ID, injects it into the request context, and sets the
// X-Request-ID response header. It must be the outermost middleware so that
// rate-limit, auth, and proxy layers all have access to the request ID.
//
// A client-supplied X-Request-ID is validated before use: over-long values
// and values containing control characters are rejected with HTTP 400
// invalid_request_id (see validRequestID) so the ID can never inject into
// the response header, audit logs or the usage database. Generated IDs are
// always safe and never rejected.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid != "" {
			if !validRequestID(rid) {
				util.WriteJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{
						"message": "invalid X-Request-ID header",
						"code":    "invalid_request_id",
					},
				})
				return
			}
		} else {
			rid = util.RandomID()
		}
		w.Header().Set("X-Request-ID", rid)
		ctx := context.WithValue(r.Context(), util.RequestIDKey{}, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
