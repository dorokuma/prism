package proxy

// xAI OAuth 401 reactive refresh (audit rework round 2):
//   - 401 → refresh (only if the rejected token is still on disk) → retry
//   - refresh failure → original 401 flows through the normal handling
//     chain (body NOT pre-closed, ctx NOT pre-cancelled)
//   - N concurrent 401s → exactly one refresh-token rotation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/oauth"
	"github.com/dorokuma/prism/internal/oauth/xai"
	"github.com/dorokuma/prism/internal/pool"
)

// newOAuthXAIUpstream rejects "Bearer old" with 401 and accepts
// "Bearer new" with a valid non-streaming chat completion.
func newOAuthXAIUpstream(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer new":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","model":"grok","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		case "Bearer old":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"invalid token","code":"invalid_request_error"}}`))
		default:
			t.Errorf("unexpected Authorization header %q", r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"invalid token"}}`))
		}
	}))
}

// newOAIXAIPool builds a single-account pool wired to a REAL oauth.Source
// (file-backed, t.TempDir) so the tests exercise the production token
// path, including the doUpstreamResult.key → RefreshIfStale comparison.
func newOAIXAIPool(t *testing.T, dir string, refresh oauth.RefreshFunc, upstream *httptest.Server) (*pool.Pool, *oauth.Source) {
	t.Helper()
	src := oauth.NewXAISource(dir, "supergrok", refresh)
	p := pool.NewPool([]config.AccountConfig{
		{Name: "supergrok", OAuth: "xai", Provider: "xai", BaseURL: upstream.URL},
	})
	p.AllAccounts()[0].SetTokenSource(src)
	return p, src
}

func newOAuthTestRequest(body []byte) *http.Request {
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	// Provider routing: the account below is registered with Provider "xai".
	r.Header.Set("X-Prism-Provider", "xai")
	return r
}

// Audit item 6c: 401 → rotate once → replay the buffered request → 200.
func TestProxy401_OAuthRefreshThenRetry(t *testing.T) {
	var hits atomic.Int64
	upstream := newOAuthXAIUpstream(t, &hits)
	defer upstream.Close()
	dir := t.TempDir()
	if err := oauth.Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "old", Refresh: "ref-old", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var rotations atomic.Int64
	refresh := func(_ context.Context, rt string) (xai.Tokens, error) {
		if rotations.Add(1) != 1 {
			return xai.Tokens{}, errors.New("unexpected extra rotation")
		}
		if rt != "ref-old" {
			t.Errorf("refresh token = %q, want ref-old", rt)
		}
		return xai.Tokens{Access: "new", Refresh: "ref-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	p, _ := newOAIXAIPool(t, dir, refresh, upstream)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	proxyChatWithBody(p, w, newOAuthTestRequest(body), body, time.Now(), ChatForwardOpts{Model: "gpt-4"}, &config.Config{})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (401 → refresh → retry); body: %s", w.Code, w.Body.String())
	}
	if rotations.Load() != 1 {
		t.Fatalf("rotations = %d, want 1", rotations.Load())
	}
	if hits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (401 + retried 200)", hits.Load())
	}
}

// Audit item 8: when the refresh FAILS, the original 401 response must be
// left untouched (no pre-closed body, no pre-cancelled ctx) and flow
// through the normal 401 handling chain: account exhausted (permanent
// credential) → all accounts failed auth → 502 upstream_auth_failed.
func TestProxy401_OAuthRefreshFailureFallsBack(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid token","code":"invalid_request_error"}}`))
	}))
	defer upstream.Close()
	dir := t.TempDir()
	if err := oauth.Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "old", Refresh: "ref-dead", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var rotations atomic.Int64
	refresh := func(_ context.Context, _ string) (xai.Tokens, error) {
		rotations.Add(1)
		return xai.Tokens{}, errors.New("xAI OAuth token refresh failed: invalid_grant Invalid or unknown refresh token")
	}
	p, src := newOAIXAIPool(t, dir, refresh, upstream)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	proxyChatWithBody(p, w, newOAuthTestRequest(body), body, time.Now(), ChatForwardOpts{Model: "gpt-4"}, &config.Config{})

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj, ok := doc["error"].(map[string]any)
	if !ok {
		t.Fatalf("response missing error object: %s", w.Body.String())
	}
	if errObj["code"] != "upstream_auth_failed" {
		t.Fatalf("error code = %v, want upstream_auth_failed (the original 401 must reach the normal handling chain)", errObj["code"])
	}
	if rotations.Load() != 1 {
		t.Fatalf("rotations = %d, want 1", rotations.Load())
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (no replay after a failed refresh)", hits.Load())
	}
	accs := p.AllAccounts()
	if accs[0].IsHealthy() {
		t.Error("account must be exhausted after the 401 fallback")
	}
	if accs[0].LastExhaustClass() != pool.ExhaustPermanentCredential {
		t.Errorf("exhaust class = %v, want permanent credential", accs[0].LastExhaustClass())
	}
	if !src.OAuthTerminalInvalid() {
		t.Error("terminal latch must be set after invalid_grant")
	}
}

// Audit item 7: N concurrent 401s with the same rejected token must burn
// exactly ONE refresh-token rotation; the losers of the race reuse the
// winner's rotated token from disk.
func TestProxy401_ThunderingHerdBurnsOneRotation(t *testing.T) {
	var hits atomic.Int64
	upstream := newOAuthXAIUpstream(t, &hits)
	defer upstream.Close()
	dir := t.TempDir()
	if err := oauth.Save(dir, "supergrok", "xai", xai.Tokens{
		Access: "old", Refresh: "ref-old", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var rotations atomic.Int64
	refresh := func(_ context.Context, rt string) (xai.Tokens, error) {
		rotations.Add(1)
		if rt != "ref-old" {
			t.Errorf("refresh token = %q, want ref-old", rt)
		}
		return xai.Tokens{Access: "new", Refresh: "ref-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	p, _ := newOAIXAIPool(t, dir, refresh, upstream)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	const n = 5
	cfg := &config.Config{MaxConcurrentPerAccount: map[string]int{"gpt-4": n}}
	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			proxyChatWithBody(p, w, newOAuthTestRequest(body), body, time.Now(), ChatForwardOpts{Model: "gpt-4"}, cfg)
			statuses[i] = w.Code
		}(i)
	}
	wg.Wait()
	for i, code := range statuses {
		if code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, code)
		}
	}
	if got := rotations.Load(); got != 1 {
		t.Fatalf("rotations = %d, want exactly 1 (thundering herd)", got)
	}
}

// The 401 reactive refresh must NOT fire for non-OAuth (static key)
// accounts: a 401 there is the legacy permanent-credential handling.
func TestProxy401_StaticAccountUnaffected(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid token","code":"invalid_request_error"}}`))
	}))
	defer upstream.Close()
	p := pool.NewPool([]config.AccountConfig{
		{Name: "static1", Key: "k1", Provider: "xai", BaseURL: upstream.URL},
	})

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	proxyChatWithBody(p, w, newOAuthTestRequest(body), body, time.Now(), ChatForwardOpts{Model: "gpt-4"}, &config.Config{})

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", w.Code, w.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj, _ := doc["error"].(map[string]any)
	if errObj["code"] != "upstream_auth_failed" {
		t.Fatalf("error code = %v, want upstream_auth_failed", errObj["code"])
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (no retry for static keys)", hits.Load())
	}
}
