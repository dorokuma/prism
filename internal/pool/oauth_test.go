package pool

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/dorokuma/prism/internal/config"
)

type stubTokens struct {
	tok string
	err error
}

func (s stubTokens) Token(context.Context) (string, error) { return s.tok, s.err }

func TestKeyUsesTokenSource(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "s", Key: "static"}}
	acc.SetTokenSource(stubTokens{tok: "live"})
	if got := acc.Key(); got != "live" {
		t.Fatalf("Key = %q, want live", got)
	}
}

func TestApplyAuthHeaderOAuthBearer(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "s"}}
	acc.SetTokenSource(stubTokens{tok: "live-token"})
	h := make(http.Header)
	ApplyAuthHeader(h, acc)
	if got := h.Get("Authorization"); got != "Bearer live-token" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestApplyAuthHeaderSkipsEmptyKey(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "s"}}
	acc.SetTokenSource(stubTokens{err: context.Canceled})
	h := make(http.Header)
	ApplyAuthHeader(h, acc)
	if h.Get("Authorization") != "" {
		t.Fatalf("empty key must not write Authorization, got %q", h.Get("Authorization"))
	}
}

// stubReactiveTokens mimics the reactive-refresh surface of oauth.Source
// (structural assertion in Account.OAuthRefreshIfStale).
type stubReactiveTokens struct {
	tok   string
	stale string
}

func (s stubReactiveTokens) Token(context.Context) (string, error) { return s.tok, nil }
func (s stubReactiveTokens) RefreshIfStale(_ context.Context, stale string) (string, error) {
	if stale != s.stale {
		return "", fmt.Errorf("unexpected stale token %q", stale)
	}
	return "rotated", nil
}

// Audit item 7: the 401 path must forward the REJECTED token to the
// source (not re-resolve it), so the source can detect a concurrent
// rotation on disk.
func TestOAuthRefreshIfStaleForwardsStaleToken(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "s"}}
	acc.SetTokenSource(stubReactiveTokens{tok: "live", stale: "live"})
	got, err := acc.OAuthRefreshIfStale(context.Background(), "live")
	if err != nil {
		t.Fatal(err)
	}
	if got != "rotated" {
		t.Fatalf("token = %q, want rotated", got)
	}
}

// Static-key accounts (no token source) must report reactive refresh as
// unsupported instead of panicking on the assertion.
func TestOAuthRefreshIfStaleUnsupported(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "static", Key: "k"}}
	if _, err := acc.OAuthRefreshIfStale(context.Background(), "x"); err == nil {
		t.Fatal("static-key account must report reactive refresh unsupported")
	}
}

// Audit item 7 plumbing: ApplyAuthHeaderWithValue must write exactly the
// caller-provided credential (the same value doUpstreamRequest records on
// the result), and ApplyAuthHeader keeps its legacy resolution via
// acc.Key().
func TestApplyAuthHeaderWithValueSendsGivenKey(t *testing.T) {
	acc := &Account{cfg: config.AccountConfig{Name: "s"}}
	acc.SetTokenSource(stubTokens{tok: "from-key"})
	h := make(http.Header)
	ApplyAuthHeaderWithValue(h, acc, "recorded")
	if got := h.Get("Authorization"); got != "Bearer recorded" {
		t.Fatalf("Authorization = %q, want Bearer recorded", got)
	}
	// Legacy entry point still resolves via acc.Key().
	h2 := make(http.Header)
	ApplyAuthHeader(h2, acc)
	if got := h2.Get("Authorization"); got != "Bearer from-key" {
		t.Fatalf("Authorization = %q, want Bearer from-key", got)
	}
	// Empty key writes nothing (both entries).
	h3 := make(http.Header)
	ApplyAuthHeaderWithValue(h3, acc, "")
	if h3.Get("Authorization") != "" {
		t.Fatalf("empty key must not write Authorization, got %q", h3.Get("Authorization"))
	}
	// Custom auth_header still gets the raw key without Bearer prefix.
	acc2 := &Account{cfg: config.AccountConfig{Name: "s2", AuthHeader: "x-api-key"}}
	h4 := make(http.Header)
	ApplyAuthHeaderWithValue(h4, acc2, "rawkey")
	if h4.Get("x-api-key") != "rawkey" || h4.Get("Authorization") != "" {
		t.Fatalf("custom header = %q, Authorization = %q", h4.Get("x-api-key"), h4.Get("Authorization"))
	}
}
