package pool

import (
	"context"
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
