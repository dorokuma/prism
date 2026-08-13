package util

import "strings"

// ClassifyConnError maps a transport/URL error to a short safe category for
// structured logging. The RAW error is deliberately never included: a
// *url.Error embeds the full upstream URL — query parameters and any
// credentials in the base URL — which must not reach logs or client-visible
// error text. Every connection-error log site (upstream chat requests,
// model cache fetches, probes, startup checks) classifies through here so a
// ?key=secret style URL can never be written to a log.
func ClassifyConnError(err error) string {
	if err == nil {
		return "upstream_error"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "context deadline exceeded"):
		return "upstream_timeout"
	case strings.Contains(s, "context canceled"):
		return "client_disconnect"
	case strings.Contains(s, "connection refused"):
		return "upstream_refused"
	case strings.Contains(s, "connection reset"):
		return "upstream_refused"
	case strings.Contains(s, "EOF"):
		return "upstream_refused"
	case strings.Contains(s, "no such host"):
		return "upstream_refused"
	}
	return "upstream_error"
}

// ConnErrorRetryable reports whether a transport error is safe to retry on
// another account for a non-idempotent POST. true means the request almost
// certainly never left this process (dial / DNS). false means the request
// may already have reached the upstream (timeout, reset, EOF, broken pipe,
// or anything unrecognized) — retrying would risk a duplicate mutation.
// ClassifyConnError is left unchanged: this function is only the retry gate.
func ConnErrorRetryable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return true
	case strings.Contains(s, "no such host"):
		return true
	case strings.Contains(s, "network is unreachable"):
		return true
	case strings.Contains(s, "no route to host"):
		return true
	case strings.Contains(s, "temporary failure in name resolution"):
		return true
	default:
		return false
	}
}
