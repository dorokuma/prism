package middleware_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/middleware"
)

// TestAuthenticate_EmptyKeysAnonymous: with no keys configured, every request
// passes and the key name is "anonymous" (matches the historical
// auth-disabled behavior).
func TestAuthenticate_EmptyKeysAnonymous(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer whatever")
	name, ok := middleware.Authenticate(r, nil)
	if !ok {
		t.Fatal("empty keys must allow all requests")
	}
	if name != "anonymous" {
		t.Errorf("keyName = %q, want anonymous", name)
	}
	// Also with an empty (non-nil) slice.
	name, ok = middleware.Authenticate(r, []config.APIKey{})
	if !ok || name != "anonymous" {
		t.Errorf("empty slice: (%q, %v), want (anonymous, true)", name, ok)
	}
}

// TestAuthenticate_MultiKeyHitsDifferentNames: each key in the set
// authenticates and resolves to its own name (first / middle / last
// positions all covered).
func TestAuthenticate_MultiKeyHitsDifferentNames(t *testing.T) {
	keys := []config.APIKey{
		{Name: "ci-bot", Token: "sk-ci-111"},
		{Name: "human", Token: "sk-human-222"},
		{Name: "ops", Token: "sk-ops-333"},
	}

	// First key
	r := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-ci-111")
	name, ok := middleware.Authenticate(r, keys)
	if !ok || name != "ci-bot" {
		t.Errorf("hit first key: (%q, %v), want (ci-bot, true)", name, ok)
	}

	// Middle key
	r.Header.Set("Authorization", "Bearer sk-human-222")
	name, ok = middleware.Authenticate(r, keys)
	if !ok || name != "human" {
		t.Errorf("hit middle key: (%q, %v), want (human, true)", name, ok)
	}

	// Last key
	r.Header.Set("Authorization", "Bearer sk-ops-333")
	name, ok = middleware.Authenticate(r, keys)
	if !ok || name != "ops" {
		t.Errorf("hit last key: (%q, %v), want (ops, true)", name, ok)
	}
}

// TestAuthenticate_WrongTokenRejected: a token that matches no key is
// rejected with an empty key name.
func TestAuthenticate_WrongTokenRejected(t *testing.T) {
	keys := []config.APIKey{
		{Name: "ci-bot", Token: "sk-ci-111"},
		{Name: "human", Token: "sk-human-222"},
	}
	r := httptest.NewRequest("GET", "/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer sk-wrong-999")
	name, ok := middleware.Authenticate(r, keys)
	if ok {
		t.Fatal("wrong token must be rejected")
	}
	if name != "" {
		t.Errorf("keyName on failure = %q, want empty", name)
	}
}

// TestAuthenticate_NoHeaderRejected: a request without an Authorization
// header is rejected when keys are configured.
func TestAuthenticate_NoHeaderRejected(t *testing.T) {
	keys := []config.APIKey{{Name: "ci-bot", Token: "sk-ci-111"}}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	if _, ok := middleware.Authenticate(r, keys); ok {
		t.Fatal("missing Authorization header must be rejected")
	}
}

// TestAuthenticate_LongWrongTokenRejected: a token longer than the internal
// pad length must still be rejected (padding must not create a match).
func TestAuthenticate_LongWrongTokenRejected(t *testing.T) {
	keys := []config.APIKey{{Name: "ci-bot", Token: "sk-ci-111"}}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+string(make([]byte, 400)))
	if _, ok := middleware.Authenticate(r, keys); ok {
		t.Fatal("long wrong token must be rejected")
	}
}

// TestAuthenticate_LongKeyPrefixMatchRejected is the regression test for the
// pad-truncation bug: a configured key longer than the fixed pad must never
// match on a prefix. The old padded comparison truncated both sides to 256
// bytes, so a token sharing the first 256 bytes of a longer key would pass
// even with a different suffix.
func TestAuthenticate_LongKeyPrefixMatchRejected(t *testing.T) {
	longKey := strings.Repeat("k", 300)
	keys := []config.APIKey{{Name: "long", Token: longKey}}

	// Provided token = first 256 bytes of the key + a different suffix:
	// the old truncated comparison would accept this.
	prefix := longKey[:256]
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+prefix+"DIFFERENT-SUFFIX")
	if name, ok := middleware.Authenticate(r, keys); ok {
		t.Fatalf("prefix match must be rejected, got key %q (truncation bug)", name)
	}

	// Even the exact full key cannot authenticate: a key longer than the
	// comparison pad is unsupported and must never be silently truncated
	// into a weaker comparison.
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("Authorization", "Bearer "+longKey)
	if _, ok := middleware.Authenticate(r2, keys); ok {
		t.Fatal("a key longer than the pad must not authenticate through truncation")
	}
}

// TestAuthenticate_BareTokenRejected: an Authorization header without a
// "Bearer " prefix is rejected outright — the prefix is mandatory, matching
// the legacy CheckAuth behavior. A bare token must never authenticate.
func TestAuthenticate_BareTokenRejected(t *testing.T) {
	keys := []config.APIKey{{Name: "ci-bot", Token: "sk-ci-111"}}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "sk-ci-111")
	name, ok := middleware.Authenticate(r, keys)
	if ok {
		t.Errorf("bare token: (%q, %v), want rejected", name, ok)
	}
	if name != "" {
		t.Errorf("keyName on rejection = %q, want empty", name)
	}
}

// TestAuthenticate_BearerPrefixOnlyRejected: "Bearer " with nothing after it
// is not a valid credential.
func TestAuthenticate_BearerPrefixOnlyRejected(t *testing.T) {
	keys := []config.APIKey{{Name: "ci-bot", Token: "sk-ci-111"}}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer ")
	if _, ok := middleware.Authenticate(r, keys); ok {
		t.Fatal("prefix-only Authorization header must be rejected")
	}
}

// TestAuthenticate_LegacyAuthTokenExpansion: the exact key set LoadConfig
// produces from a lone legacy auth_token ({name: "default", token: t}) must
// authenticate a "Bearer <t>" request — i.e. the old single-token setup
// keeps working end-to-end.
func TestAuthenticate_LegacyAuthTokenExpansion(t *testing.T) {
	// Mirrors LoadConfig's backward-compat expansion.
	keys := []config.APIKey{{Name: "default", Token: "legacy-token-123"}}
	r := httptest.NewRequest("GET", "/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer legacy-token-123")
	name, ok := middleware.Authenticate(r, keys)
	if !ok || name != "default" {
		t.Errorf("legacy auth_token key: (%q, %v), want (default, true)", name, ok)
	}
}

// TestAPIKeyContextRoundtrip: WithAPIKey stores the key name and
// APIKeyFromContext retrieves it; a bare context yields "".
func TestAPIKeyContextRoundtrip(t *testing.T) {
	ctx := middleware.WithAPIKey(httptest.NewRequest("GET", "/", nil).Context(), "ci-bot")
	if got := middleware.APIKeyFromContext(ctx); got != "ci-bot" {
		t.Errorf("APIKeyFromContext = %q, want ci-bot", got)
	}
	if got := middleware.APIKeyFromContext(httptest.NewRequest("GET", "/", nil).Context()); got != "" {
		t.Errorf("APIKeyFromContext on bare ctx = %q, want empty", got)
	}
}

// TestAuthenticate_EmptyTokenKeySkipped guards the defensive empty-token
// skip: a key whose token is empty or whitespace-only (programmatically
// built key sets; config loading rejects them) must never authenticate a
// "Bearer " request through the all-zero padded comparison, and must not
// break matching of the other keys in the set.
func TestAuthenticate_EmptyTokenKeySkipped(t *testing.T) {
	keys := []config.APIKey{
		{Name: "bad-empty", Token: ""},
		{Name: "bad-space", Token: "   "},
		{Name: "ci-bot", Token: "sk-ci-111"},
	}

	// "Bearer " with nothing after it must NOT match the empty-token key.
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer ")
	if name, ok := middleware.Authenticate(r, keys); ok {
		t.Fatalf("empty token key authenticated a prefix-only request as %q", name)
	}

	// "Bearer  " (single space) must NOT match the whitespace-only key.
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("Authorization", "Bearer  ")
	if name, ok := middleware.Authenticate(r2, keys); ok {
		t.Fatalf("whitespace token key authenticated a whitespace request as %q", name)
	}

	// A real key in the same set still authenticates.
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.Header.Set("Authorization", "Bearer sk-ci-111")
	name, ok := middleware.Authenticate(r3, keys)
	if !ok || name != "ci-bot" {
		t.Errorf("real key next to empty-token keys: (%q, %v), want (ci-bot, true)", name, ok)
	}
}
