package util

import (
	"errors"
	"net/url"
	"strings"
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
		Err: errors.New("connection refused"),
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

// TestClassifyConnError_Categories covers the classification branches.
func TestClassifyConnError_Categories(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "upstream_error"},
		{&url.Error{Op: "Get", URL: "https://x", Err: errors.New("context deadline exceeded")}, "upstream_timeout"},
		{&url.Error{Op: "Get", URL: "https://x", Err: errors.New("context canceled")}, "client_disconnect"},
		{&url.Error{Op: "Post", URL: "https://x", Err: errors.New("connection refused")}, "upstream_refused"},
		{&url.Error{Op: "Post", URL: "https://x", Err: errors.New("connection reset by peer")}, "upstream_refused"},
		{&url.Error{Op: "Get", URL: "https://x", Err: errors.New("unexpected EOF")}, "upstream_refused"},
		{&url.Error{Op: "Get", URL: "https://x", Err: errors.New("dial tcp: lookup nope: no such host")}, "upstream_refused"},
		{&url.Error{Op: "Get", URL: "https://x", Err: errors.New("tls: handshake failure")}, "upstream_error"},
	}
	for _, c := range cases {
		if got := ClassifyConnError(c.err); got != c.want {
			t.Errorf("ClassifyConnError(%v) = %q, want %q", c.err, got, c.want)
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
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), true},
		{errors.New("dial tcp: lookup nope: no such host"), true},
		{errors.New("dial tcp: network is unreachable"), true},
		{errors.New("dial tcp: no route to host"), true},
		{errors.New("dial tcp: lookup api: temporary failure in name resolution"), true},
		{errors.New("context deadline exceeded"), false},
		{errors.New("read tcp: connection reset by peer"), false},
		{errors.New("unexpected EOF"), false},
		{errors.New("write: broken pipe"), false},
		{errors.New("tls: handshake failure"), false},
	}
	for _, c := range cases {
		if got := ConnErrorRetryable(c.err); got != c.want {
			t.Errorf("ConnErrorRetryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
