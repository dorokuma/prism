package util

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

// TestClassifyConnError_UrlErrorWithSecretQuery pins the core item-10
// guarantee: a *url.Error whose URL carries a ?key=secret query must be
// classified into a short category — the classification output must never
// contain the raw error, the URL, or the secret.
func TestClassifyConnError_UrlErrorWithSecretQuery(t *testing.T) {
	secret := "supersecret-query-value"
	raw := &url.Error{
		Op:  "Get",
		URL: "https://upstream.example/v1/chat/completions?key=" + secret + "&model=gpt-4",
		Err: syscall.ECONNREFUSED,
	}
	class := ClassifyConnError(raw)
	if class == "" {
		t.Fatal("ClassifyConnError returned empty classification")
	}
	for _, leak := range []string{raw.Error(), "upstream.example", "/v1/chat/completions", "key=", secret, "gpt-4"} {
		if strings.Contains(class, leak) {
			t.Errorf("classification %q leaks %q: %v", class, leak, raw)
		}
	}
	if class != "upstream_refused" {
		t.Errorf("classification = %q, want upstream_refused", class)
	}
}

// resetErr builds a realistic connection-reset error chain: a *url.Error
// wrapping a *net.OpError wrapping an *os.SyscallError wrapping the
// ECONNRESET errno — the shape net/http produces when a TCP peer resets the
// connection.
func resetErr() error {
	return &url.Error{
		Op:  "Post",
		URL: "https://x",
		Err: &net.OpError{
			Op:  "read",
			Net: "tcp",
			Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET},
		},
	}
}

// TestClassifyConnError_Categories covers the classification branches with
// STRUCTURED errors (real errno chains, context errors, io sentinels, DNS
// errors) — the shapes net/http actually produces. Classification must
// unwrap the error chain, not match substrings.
func TestClassifyConnError_Categories(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "upstream_error"},
		{&url.Error{Op: "Get", URL: "https://x", Err: context.DeadlineExceeded}, "upstream_timeout"},
		{context.DeadlineExceeded, "upstream_timeout"},
		{&url.Error{Op: "Get", URL: "https://x", Err: context.Canceled}, "client_disconnect"},
		{&url.Error{Op: "Post", URL: "https://x", Err: syscall.ECONNREFUSED}, "upstream_refused"},
		{&url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}}, "upstream_refused"},
		{resetErr(), "upstream_refused"},
		{&url.Error{Op: "Get", URL: "https://x", Err: io.ErrUnexpectedEOF}, "upstream_error"},
		{io.EOF, "upstream_error"},
		{&url.Error{Op: "Get", URL: "https://x", Err: &net.DNSError{Err: "no such host", Name: "nope", IsNotFound: true}}, "upstream_refused"},
		{&url.Error{Op: "Get", URL: "https://x", Err: errors.New("tls: handshake failure")}, "upstream_error"},
	}
	for _, c := range cases {
		if got := ClassifyConnError(c.err); got != c.want {
			t.Errorf("ClassifyConnError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestClassifyConnError_TruncatedStreamNotRefused pins the final-review
// fix: a stream that ends prematurely (io.EOF / io.ErrUnexpectedEOF — the
// shapes a truncated gzip body surfaces as) is NOT a connection refusal:
// the upstream already answered and the connection died mid-body, so the
// classification is the generic upstream_error. The retry gate is
// unchanged: ConnErrorRetryable still treats both as non-retryable (the
// request may already have been accepted upstream), so the "do not switch
// accounts" semantics of a truncated stream stay intact.
func TestClassifyConnError_TruncatedStreamNotRefused(t *testing.T) {
	truncated := &url.Error{Op: "Post", URL: "https://x", Err: io.ErrUnexpectedEOF}
	if got := ClassifyConnError(truncated); got != "upstream_error" {
		t.Errorf("ClassifyConnError(truncated gzip) = %q, want upstream_error (not upstream_refused)", got)
	}
	if got := ClassifyConnError(io.EOF); got != "upstream_error" {
		t.Errorf("ClassifyConnError(io.EOF) = %q, want upstream_error (not upstream_refused)", got)
	}
	// Retry semantics unchanged: a truncated stream may already have been
	// accepted upstream, so it must stay non-retryable (no account switch
	// that could double-submit the POST).
	if ConnErrorRetryable(truncated) {
		t.Error("ConnErrorRetryable(truncated gzip) = true, want false (unchanged)")
	}
	if ConnErrorRetryable(io.EOF) {
		t.Error("ConnErrorRetryable(io.EOF) = true, want false (unchanged)")
	}
}

// TestClassifyConnError_SubstringNoLongerMisclassified pins the audit fix:
// classification is structural, so a PLAIN-TEXT error whose message merely
// CONTAINS "EOF", "connection reset" or similar must NOT be classified as
// an upstream failure anymore — only the real structured errors are. These
// exact strings used to be misclassified by substring matching.
func TestClassifyConnError_SubstringNoLongerMisclassified(t *testing.T) {
	messages := []string{
		"EOF",
		"unexpected EOF",
		"connection reset by peer",
		"connection refused",
		"no such host",
		"config parse error at line 3: unexpected EOF in value",
		"quota check failed: connection reset by peer counter",
	}
	for _, msg := range messages {
		if got := ClassifyConnError(errors.New(msg)); got != "upstream_error" {
			t.Errorf("ClassifyConnError(%q) = %q, want upstream_error (plain text must not be classified)", msg, got)
		}
	}
}

// TestClassifyConnError_UnknownErrorNeverLeaksRawText: an UNKNOWN error
// (not a *url.Error, no recognized keyword) must fall back to the safe
// upstream_error category and the classification must never embed the raw
// error text (which could carry a URL, a query or a secret in its message).
// This pins the legacy upstreamErrorType default ("upstream_refused" for
// everything unknown) being replaced by the shared classifier's
// "upstream_error" default — the compat mapping keeps the three recognized
// strings (upstream_timeout / upstream_refused / client_disconnect)
// identical, only the unknown-error default is now accurate.
func TestClassifyConnError_UnknownErrorNeverLeaksRawText(t *testing.T) {
	raw := errors.New("weird transport hiccup: https://user:sekrit@upstream.example/v1?key=topsecret")
	class := ClassifyConnError(raw)
	if class != "upstream_error" {
		t.Errorf("ClassifyConnError(unknown) = %q, want upstream_error", class)
	}
	for _, leak := range []string{"upstream.example", "sekrit", "topsecret", "key=", "/v1", raw.Error()} {
		if strings.Contains(class, leak) {
			t.Errorf("classification %q leaks %q from the raw error", class, leak)
		}
	}
}

func TestConnErrorRetryable(t *testing.T) {
	dialErr := func(errno syscall.Errno) error {
		return &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: &os.SyscallError{Syscall: "connect", Err: errno},
		}
	}
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{dialErr(syscall.ECONNREFUSED), true},
		{&net.DNSError{Err: "no such host", Name: "nope", IsNotFound: true}, true},
		{&net.DNSError{Err: "temporary failure in name resolution", Name: "api", IsTemporary: true}, true},
		{dialErr(syscall.ENETUNREACH), true},
		{dialErr(syscall.EHOSTUNREACH), true},
		{context.DeadlineExceeded, false},
		{resetErr(), false},
		{io.ErrUnexpectedEOF, false},
		{&os.SyscallError{Syscall: "write", Err: syscall.EPIPE}, false},
		{errors.New("tls: handshake failure"), false},
	}
	for _, c := range cases {
		if got := ConnErrorRetryable(c.err); got != c.want {
			t.Errorf("ConnErrorRetryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestConnErrorRetryable_SubstringNoLongerRetried pins the same structural
// rule for the retry gate: a plain-text error containing "connection
// refused" / "no such host" / "network is unreachable" is NOT retryable —
// only the real structured errors are. Retrying on a text match could
// double-submit a POST that actually reached the upstream.
func TestConnErrorRetryable_SubstringNoLongerRetried(t *testing.T) {
	messages := []string{
		"connection refused",
		"dial tcp 127.0.0.1:1: connect: connection refused",
		"no such host",
		"network is unreachable",
		"no route to host",
		"temporary failure in name resolution",
	}
	for _, msg := range messages {
		if ConnErrorRetryable(errors.New(msg)) {
			t.Errorf("ConnErrorRetryable(%q) = true, want false (plain text must not be retried)", msg)
		}
	}
}
