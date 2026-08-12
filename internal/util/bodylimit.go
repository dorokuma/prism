package util

import (
	"errors"
	"io"
	"math"
)

// ErrBodyTooLarge is returned by ReadBodyLimited when the body exceeds the
// given cap. It is the shared sentinel behind the proxy-level
// ErrUpstreamResponseTooLarge (mapped to HTTP 502 response_too_large) and
// the model-cache fetch failure path (mapped to HTTP 502
// model_fetch_failed).
var ErrBodyTooLarge = errors.New("body exceeds the configured size cap")

// ErrInvalidBodyCap is returned by ReadBodyLimited for an invalid maxBytes
// (<= 0, or math.MaxInt64 where the max+1 over-limit probe would overflow
// int64). The read is rejected outright instead of degrading to an unbounded
// io.ReadAll.
var ErrInvalidBodyCap = errors.New("invalid body cap: maxBytes must be > 0 and < math.MaxInt64")

// ReadBodyLimited reads from body with a hard cap of maxBytes: it reads
// maxBytes+1 bytes so an over-limit body is detected instead of being
// silently truncated, and returns ErrBodyTooLarge in that case. Invalid
// caps are rejected with ErrInvalidBodyCap — never an unbounded read:
//   - maxBytes <= 0 would otherwise mean "no cap"; the unbounded io.ReadAll
//     fallback is deliberately gone, so misuse cannot silently bypass the
//     memory bound;
//   - maxBytes == math.MaxInt64 is the overflow boundary — maxBytes+1 would
//     wrap int64 and io.LimitReader's limit would go negative, reading
//     NOTHING and silently reporting an empty body instead of the real
//     content.
//
// The helper lives in internal/util so both internal/proxy (upstream
// response bodies) and internal/cache (model cache success bodies) can share
// it without a cache↔proxy import cycle.
func ReadBodyLimited(body io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return nil, ErrInvalidBodyCap
	}
	b, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, ErrBodyTooLarge
	}
	return b, nil
}
