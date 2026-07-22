package middleware

import (
	"context"
	"net/http"

	"github.com/dorokuma/prism/internal/util"
)

// RequestIDMiddleware extracts X-Request-ID from the incoming request header or
// generates a new random ID, injects it into the request context, and sets the
// X-Request-ID response header. It must be the outermost middleware so that
// rate-limit, auth, and proxy layers all have access to the request ID.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = util.RandomID()
		}
		w.Header().Set("X-Request-ID", rid)
		ctx := context.WithValue(r.Context(), util.RequestIDKey{}, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
