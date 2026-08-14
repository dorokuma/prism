package util

import (
	"context"
	"errors"
	"net"
	"syscall"
)

// ClassifyConnError maps a transport/URL error to a short safe category for
// structured logging. The RAW error is deliberately never included: a
// *url.Error embeds the full upstream URL — query parameters and any
// credentials in the base URL — which must not reach logs or client-visible
// error text. Every connection-error log site (upstream chat requests,
// model cache fetches, probes, startup checks) classifies through here so a
// ?key=secret style URL can never be written to a log.
//
// Classification is STRUCTURAL, not textual: an error is matched by
// unwrapping its real chain (context errors, syscall errnos, io sentinels,
// net.DNSError), never by substring matching. A business/parse error whose
// message merely CONTAINS "EOF" or "connection reset" is therefore no
// longer misclassified as an upstream failure. The category strings are
// unchanged (upstream_timeout / client_disconnect / upstream_refused /
// upstream_error), so log consumers and the error_type taxonomy are
// compatible with every existing caller.
//
// A stream that ends prematurely (io.EOF / io.ErrUnexpectedEOF — the shapes
// a truncated gzip body surfaces as) classifies as upstream_error, NOT
// upstream_refused: the request may already have reached the upstream (a
// truncated body means the upstream answered and the connection died
// mid-body), so "refused" would be a lie. The retry gate is unchanged:
// ConnErrorRetryable already treats them as non-retryable, so the
// "may already have been accepted upstream, do not switch accounts"
// semantics are untouched.
func ClassifyConnError(err error) string {
	if err == nil {
		return "upstream_error"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "upstream_timeout"
	case errors.Is(err, context.Canceled):
		return "client_disconnect"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "upstream_refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "upstream_refused"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "upstream_refused"
	}
	return "upstream_error"
}

// ConnErrorRetryable reports whether a transport error is safe to retry on
// another account for a non-idempotent POST. true means the request almost
// certainly never left this process (dial refused / DNS failure / network
// unreachable / no route). false means the request may already have reached
// the upstream (timeout, reset, EOF, broken pipe, or anything unrecognized)
// — retrying would risk a duplicate mutation.
//
// Like ClassifyConnError, matching is STRUCTURAL (syscall errnos and
// *net.DNSError via the error chain): a plain-text error that merely
// contains "connection refused" or "no such host" is not treated as
// retryable anymore. Every real transport failure from net/http unwraps to
// these structured types, so runtime behavior for genuine dial/DNS failures
// is unchanged.
func ConnErrorRetryable(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return true
	case errors.Is(err, syscall.ENETUNREACH):
		return true
	case errors.Is(err, syscall.EHOSTUNREACH):
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}
