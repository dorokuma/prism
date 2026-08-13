package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

// decodeErrorCode extracts the error.code field from a JSON response body.
func decodeErrorCode(t *testing.T, body string) string {
	t.Helper()
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return resp.Error.Code
}

// --- Item 4 + 9: select timeout bound to r.Context(); errors.Is classification ---

// TestSelectClientCanceled verifies the select context is bound to
// r.Context(): with every account occupied and the request context already
// canceled, the select fails immediately with context.Canceled and the
// response is 503 with code client_canceled. Before the fix the select used
// context.Background() and parked the request for the full select timeout.
func TestSelectClientCanceled(t *testing.T) {
	restore := SetAccountSelectTimeoutForTest(50 * time.Millisecond) // fast-fail regression
	defer restore()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}},
		// gpt-4 would otherwise resolve to the 450 default and fit in the
		// occupied slot; pin it to 1 so the test request must wait.
		MaxConcurrentPerAccount: map[string]int{"gpt-4": 1},
	}
	p := pool.NewPool(cfg.Accounts)

	// Occupy the only slot.
	_, slot, err := p.Select(context.Background(), "gpt-4", 1)
	if err != nil {
		t.Fatalf("occupy slot: %v", err)
	}
	defer p.Release(slot)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // request already canceled

	rec := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(ctx, "POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	start := time.Now()
	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{Model: "gpt-4"}, cfg)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "client_canceled" {
		t.Errorf("error code = %q, want client_canceled", code)
	}
	// Must fail fast (well under the 50ms select timeout — actually
	// immediately, since the context is pre-canceled).
	if elapsed > 2*time.Second {
		t.Errorf("client cancel took %v, want immediate fail (select was not bound to r.Context())", elapsed)
	}
}

// TestSelectNoHealthyCode verifies an all-exhausted pool maps to 503 with
// code no_healthy (previously the generic "no_accounts").
func TestSelectNoHealthyCode(t *testing.T) {
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://localhost:1", Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)
	p.AllAccounts()[0].MarkExhausted()

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "no_healthy" {
		t.Errorf("error code = %q, want no_healthy", code)
	}
}

// TestSelectTimeoutCode verifies a saturated pool with a short select
// timeout maps to 503 with code select_timeout.
func TestSelectTimeoutCode(t *testing.T) {
	restore := SetAccountSelectTimeoutForTest(80 * time.Millisecond)
	defer restore()

	cfg := &config.Config{
		Accounts:                []config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://localhost:1", Provider: "t"}},
		MaxConcurrentPerAccount: map[string]int{"gpt-4": 1},
	}
	p := pool.NewPool(cfg.Accounts)

	_, slot, err := p.Select(context.Background(), "gpt-4", 1)
	if err != nil {
		t.Fatalf("occupy slot: %v", err)
	}
	defer p.Release(slot)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{Model: "gpt-4"}, cfg)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "select_timeout" {
		t.Errorf("error code = %q, want select_timeout", code)
	}
}

// --- Item 5: 32 MiB default cap on non-streaming success reads, 502 response_too_large ---

func TestUpstreamResponseTooLargeLegacy(t *testing.T) {
	// 32 bytes of body with a 16-byte cap: read max+1 = 17 bytes, detect the
	// overflow, and answer 502 response_too_large instead of buffering the
	// whole body.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"0123456789abcdef0123456789abcdef"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{MaxResponseBytes: 16}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "response_too_large" {
		t.Errorf("error code = %q, want response_too_large", code)
	}
}

func TestUpstreamResponseTooLargeResponsesOut(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"0123456789abcdef0123456789abcdef"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{
		ResponsesOut:     true,
		MaxResponseBytes: 16,
	}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "response_too_large" {
		t.Errorf("error code = %q, want response_too_large", code)
	}
}

// TestUpstreamResponseAtCapPasses verifies the boundary: a body exactly at
// the cap (16 bytes) passes through untouched.
func TestUpstreamResponseAtCapPasses(t *testing.T) {
	body := `0123456789abcdef` // exactly 16 bytes
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{MaxResponseBytes: 16}, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body at cap must pass)", rec.Code)
	}
	if rec.Body.String() != body {
		t.Errorf("body = %q, want %q (pass-through must be byte-exact)", rec.Body.String(), body)
	}
}

// TestReadResponseBodyLimitedUnit is the unit-level guard for the max+1
// read: an over-limit body is detected (ErrUpstreamResponseTooLarge), an
// at-limit body passes, the over-limit bytes are never returned, and an
// invalid cap (0) is rejected with ErrInvalidResponseCap instead of
// falling back to an unbounded io.ReadAll.
func TestReadResponseBodyLimitedUnit(t *testing.T) {
	if _, err := readResponseBodyLimited(strings.NewReader("0123456789abcdef0"), 16); err != ErrUpstreamResponseTooLarge {
		t.Errorf("17 bytes with cap 16: err = %v, want ErrUpstreamResponseTooLarge", err)
	}
	b, err := readResponseBodyLimited(strings.NewReader("0123456789abcdef"), 16)
	if err != nil || string(b) != "0123456789abcdef" {
		t.Errorf("16 bytes with cap 16: (b=%q, err=%v), want exact pass-through", b, err)
	}
	b, err = readResponseBodyLimited(strings.NewReader("0123456789abcdef"), 0)
	if !errors.Is(err, ErrInvalidResponseCap) {
		t.Errorf("cap 0: err = %v, want ErrInvalidResponseCap (no unbounded read fallback)", err)
	}
	if b != nil {
		t.Errorf("cap 0: got body %q, want nil (invalid cap must not look like a successful read)", b)
	}
}

// TestReadResponseBodyLimitedEdgeCaps guards the invalid-cap boundaries: a
// negative cap and math.MaxInt64 are rejected with ErrInvalidResponseCap
// instead of the old unbounded read. math.MaxInt64 is the overflow
// boundary — maxBytes+1 would wrap negative and io.LimitReader would read
// NOTHING, silently returning an empty body instead of the real content;
// rejecting the cap outright beats both the empty-body trap and the
// unbounded fallback. The error must never be mistaken for a successful
// body (nil body on error).
func TestReadResponseBodyLimitedEdgeCaps(t *testing.T) {
	body := "0123456789abcdef"
	b, err := readResponseBodyLimited(strings.NewReader(body), -1)
	if !errors.Is(err, ErrInvalidResponseCap) {
		t.Errorf("negative cap: err = %v, want ErrInvalidResponseCap", err)
	}
	if b != nil {
		t.Errorf("negative cap: got body %q, want nil (invalid cap must not look like a successful read)", b)
	}
	b, err = readResponseBodyLimited(strings.NewReader(body), math.MaxInt64)
	if !errors.Is(err, ErrInvalidResponseCap) {
		t.Errorf("math.MaxInt64 cap (overflow boundary): err = %v, want ErrInvalidResponseCap", err)
	}
	if b != nil {
		t.Errorf("math.MaxInt64 cap: got body %q, want nil (invalid cap must not look like a successful read)", b)
	}
}

// --- Item 6: IsQuotaError strictness — 429 classification ---

// TestHandleUpstreamError429PlainTextCooldown verifies a 429 whose body is
// plain text (no structured quota envelope) is a TEMPORARY error: the
// account goes to cooldown, not exhaustion.
func TestHandleUpstreamError429PlainTextCooldown(t *testing.T) {
	p := pool.NewPool([]config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://localhost:1", Provider: "t"}})
	acc := p.AllAccounts()[0]

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(bytes.NewReader([]byte(`quota exceeded, retry later`))),
	}
	handleUpstreamError(acc, resp, "req-1", "m")

	if acc.Status() != pool.StatusHealthy {
		t.Fatalf("account status = %v, want healthy (429 with plain-text quota must NOT exhaust)", acc.Status())
	}
	if !acc.IsInCooldown() {
		t.Error("account must be in cooldown after temporary 429")
	}
}

// TestHandleUpstreamError429StructuredQuotaExhausts verifies a 429 carrying
// the structured insufficient_quota envelope is a PERMANENT quota error:
// the account is exhausted.
func TestHandleUpstreamError429StructuredQuotaExhausts(t *testing.T) {
	p := pool.NewPool([]config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://localhost:1", Provider: "t"}})
	acc := p.AllAccounts()[0]

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"code":"insufficient_quota","message":"quota"}}`))),
	}
	handleUpstreamError(acc, resp, "req-1", "m")

	if acc.Status() != pool.StatusExhausted {
		t.Fatalf("account status = %v, want exhausted (structured insufficient_quota is permanent)", acc.Status())
	}
	if got := acc.LastExhaustClass(); got != pool.ExhaustPermanentQuota {
		t.Fatalf("LastExhaustClass() = %v, want ExhaustPermanentQuota", got)
	}
}

// TestHandleUpstreamError402SetsPermanentQuotaClass pins the 402 path:
// Payment Required is permanent quota exhaustion and must record
// ExhaustPermanentQuota so a later HTTP 200 probe cannot revive it.
func TestHandleUpstreamError402SetsPermanentQuotaClass(t *testing.T) {
	p := pool.NewPool([]config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://localhost:1", Provider: "t"}})
	acc := p.AllAccounts()[0]

	resp := &http.Response{
		StatusCode: http.StatusPaymentRequired,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"code":"insufficient_quota","message":"quota"}}`))),
	}
	handleUpstreamError(acc, resp, "req-1", "m")

	if acc.Status() != pool.StatusExhausted {
		t.Fatalf("account status = %v, want exhausted (402 is permanent quota)", acc.Status())
	}
	if got := acc.LastExhaustClass(); got != pool.ExhaustPermanentQuota {
		t.Fatalf("LastExhaustClass() = %v, want ExhaustPermanentQuota (bare MarkExhausted leaves class 0 and a 200 probe would revive)", got)
	}
}

// --- Item 7: shared classification — bare 403 is temporary ---

func TestClassifyUpstreamError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   []byte
		want   UpstreamErrorClass
	}{
		{"401 bare", 401, nil, UpstreamErrorPermanentCredential},
		{"402 bare (balance)", 402, nil, UpstreamErrorPermanentQuota},
		{"403 bare", 403, nil, UpstreamErrorTemporary},
		{"403 with structured invalid_api_key", 403, []byte(`{"error":{"code":"invalid_api_key"}}`), UpstreamErrorPermanentCredential},
		{"403 with structured insufficient_quota", 403, []byte(`{"error":{"code":"insufficient_quota"}}`), UpstreamErrorPermanentQuota},
		{"429 plain text quota", 429, []byte(`quota exceeded`), UpstreamErrorTemporary},
		{"429 structured quota", 429, []byte(`{"error":{"code":"insufficient_quota"}}`), UpstreamErrorPermanentQuota},
		{"500", 500, nil, UpstreamErrorTemporary},
		{"503", 503, []byte(`{"error":{"code":"invalid_api_key"}}`), UpstreamErrorPermanentCredential},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyUpstreamError(tc.status, tc.body); got != tc.want {
				t.Errorf("ClassifyUpstreamError(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// --- Item 3: runtime 403 — structured permanent body exhausts and
// retries another account; bare 403 still passes through ---

// proxyChatForStatus drives one chat request through the real proxy path
// against an upstream that answers with status+body, and returns the
// account state plus the client-visible response. upstreamCalls counts how
// many times the upstream was hit (a terminal 4xx must not retry).
func proxyChatForStatus(t *testing.T, status int, body string) (*pool.Account, *httptest.ResponseRecorder, *int32) {
	t.Helper()
	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: "xyz", BaseURL: upstream.URL, Provider: "test"}}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "test")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)
	return p.AllAccounts()[0], rec, &upstreamCalls
}

// TestUpstream403BarePassThrough: a bare 403 (no recognized structured
// envelope) is passed through to the client with its original status and
// body, is NOT retried, and does NOT exhaust the account — it is a
// temporary error, not a permanent one.
func TestUpstream403BarePassThrough(t *testing.T) {
	acc, rec, upstreamCalls := proxyChatForStatus(t, http.StatusForbidden, `{"error":{"message":"forbidden"}}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("client status = %d, want 403 (original status passed through)", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":{"message":"forbidden"}}` {
		t.Errorf("client body = %q, want the original (redacted) 403 body passed through", got)
	}
	if acc.Status() != pool.StatusHealthy {
		t.Errorf("account status = %v, want healthy (bare 403 must NOT exhaust)", acc.Status())
	}
	if acc.IsInCooldown() {
		t.Error("account must not be in cooldown after a bare 403 (terminal 4xx, no retry)")
	}
	if n := atomic.LoadInt32(upstreamCalls); n != 1 {
		t.Errorf("upstream calls = %d, want 1 (terminal 403 must not retry)", n)
	}
}

// TestUpstream403StructuredCredentialRetries: a 403 whose body carries the
// structured invalid_api_key envelope is permanent: the account is
// exhausted and the 403 is NOT written to the client (same as 401). With
// one account the terminal response is 502 upstream_auth_failed.
func TestUpstream403StructuredCredentialRetries(t *testing.T) {
	body := `{"error":{"code":"invalid_api_key","message":"forbidden"}}`
	acc, rec, _ := proxyChatForStatus(t, http.StatusForbidden, body)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("client status = %d, want 502 (403 permanent credential must not pass through)", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "upstream_auth_failed" {
		t.Errorf("error code = %q, want upstream_auth_failed", code)
	}
	if acc.Status() != pool.StatusExhausted {
		t.Errorf("account status = %v, want exhausted (structured credential 403 is permanent)", acc.Status())
	}
}

// TestUpstream403StructuredQuotaRetries: same rule for the structured
// quota envelope — exhausted, 403 not written, terminal 503
// upstream_quota_exhausted when no other account remains.
func TestUpstream403StructuredQuotaRetries(t *testing.T) {
	body := `{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`
	acc, rec, _ := proxyChatForStatus(t, http.StatusForbidden, body)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("client status = %d, want 503 (403 permanent quota must not pass through)", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "upstream_quota_exhausted" {
		t.Errorf("error code = %q, want upstream_quota_exhausted", code)
	}
	if acc.Status() != pool.StatusExhausted {
		t.Errorf("account status = %v, want exhausted (structured quota 403 is permanent)", acc.Status())
	}
	if got := acc.LastExhaustClass(); got != pool.ExhaustPermanentQuota {
		t.Errorf("LastExhaustClass() = %v, want ExhaustPermanentQuota", got)
	}
}

// proxyChatTwoAccountsFirstThenOK drives a chat request against two
// accounts sharing one upstream: the first call returns status+body, later
// calls return 200. Used to pin that a permanent 403 switches accounts.
func proxyChatTwoAccountsFirstThenOK(t *testing.T, status int, errBody string) (*pool.Pool, *httptest.ResponseRecorder, *int32) {
	t.Helper()
	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(status)
			w.Write([]byte(errBody))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "a", Key: "k1", BaseURL: upstream.URL, Provider: "test"},
		{Name: "b", Key: "k2", BaseURL: upstream.URL, Provider: "test"},
	}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "test")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)
	return p, rec, &upstreamCalls
}

// TestUpstream403InvalidAPIKeySwitchesAccount: 403 + invalid_api_key
// exhausts the first account and the second account serves the request.
func TestUpstream403InvalidAPIKeySwitchesAccount(t *testing.T) {
	p, rec, upstreamCalls := proxyChatTwoAccountsFirstThenOK(t, http.StatusForbidden, `{"error":{"code":"invalid_api_key","message":"forbidden"}}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("client status = %d, want 200 after switching account (body %q)", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(upstreamCalls); n < 2 {
		t.Errorf("upstream calls = %d, want >= 2 (403+invalid_api_key must switch account)", n)
	}
	var exhausted, healthy int
	for _, acc := range p.AllAccounts() {
		switch acc.Status() {
		case pool.StatusExhausted:
			exhausted++
		case pool.StatusHealthy:
			healthy++
		}
	}
	if exhausted != 1 || healthy != 1 {
		t.Errorf("account states: exhausted=%d healthy=%d, want 1 exhausted and 1 healthy", exhausted, healthy)
	}
}

// TestUpstream403InsufficientQuotaSwitchesAccount: 403 + insufficient_quota
// exhausts the first account and the second account serves the request.
func TestUpstream403InsufficientQuotaSwitchesAccount(t *testing.T) {
	p, rec, upstreamCalls := proxyChatTwoAccountsFirstThenOK(t, http.StatusForbidden, `{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("client status = %d, want 200 after switching account (body %q)", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(upstreamCalls); n < 2 {
		t.Errorf("upstream calls = %d, want >= 2 (403+insufficient_quota must switch account)", n)
	}
	var exhausted, healthy int
	for _, acc := range p.AllAccounts() {
		switch acc.Status() {
		case pool.StatusExhausted:
			exhausted++
		case pool.StatusHealthy:
			healthy++
		}
	}
	if exhausted != 1 || healthy != 1 {
		t.Errorf("account states: exhausted=%d healthy=%d, want 1 exhausted and 1 healthy", exhausted, healthy)
	}
}

// --- Item 13: non-POST to chat/completions and responses → 405 ---

func newHandlerForMethodCheck(t *testing.T) http.Handler {
	t.Helper()
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://localhost:1", Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)
	return NewProxyHandler(p, config.WireAPIBoth, config.NewConfigHolder(cfg), nil)
}

func TestChatCompletionsMethodNotAllowed(t *testing.T) {
	h := newHandlerForMethodCheck(t)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/chat/completions", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/chat/completions: status = %d, want 405", method, rec.Code)
		}
		if code := decodeErrorCode(t, rec.Body.String()); code != "method_not_allowed" {
			t.Errorf("%s /v1/chat/completions: code = %q, want method_not_allowed", method, code)
		}
	}
}

func TestResponsesMethodNotAllowed(t *testing.T) {
	h := newHandlerForMethodCheck(t)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/responses", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/responses: status = %d, want 405", method, rec.Code)
		}
		if code := decodeErrorCode(t, rec.Body.String()); code != "method_not_allowed" {
			t.Errorf("%s /v1/responses: code = %q, want method_not_allowed", method, code)
		}
	}
}

// TestChatCompletionsMethodAllowed guards the happy path: POST still routes
// to the proxy and succeeds (not a 405/404).
func TestChatCompletionsMethodAllowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)
	h := NewProxyHandler(p, config.WireAPIBoth, config.NewConfigHolder(cfg), nil)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/chat/completions: status = %d, want 200 (routed to proxy)", rec.Code)
	}
}
