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
