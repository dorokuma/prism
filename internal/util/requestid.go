package util

import "context"

// RequestIDKey is the context key type for request IDs injected by
// RequestIDMiddleware.
type RequestIDKey struct{}

// RequestIDFromCtx retrieves the request ID from the context, or returns "" if
// none is present.
func RequestIDFromCtx(ctx context.Context) string {
	if v := ctx.Value(RequestIDKey{}); v != nil {
		return v.(string)
	}
	return ""
}
