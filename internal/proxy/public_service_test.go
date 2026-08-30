package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

func TestHandleUpstreamError_PublicService402(t *testing.T) {
	p := pool.NewPool([]config.AccountConfig{{
		Name:          "ps",
		Key:           "k",
		BaseURL:       "http://localhost:1",
		Provider:      "ps",
		PublicService: true,
	}})
	acc := p.AllAccounts()[0]

	resp := &http.Response{
		StatusCode: http.StatusPaymentRequired,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"code":"insufficient_quota","message":"quota"}}`))),
	}
	class := handleUpstreamError(acc, resp, "req-ps-402", "m")

	if class != UpstreamErrorTemporary {
		t.Fatalf("class = %v, want UpstreamErrorTemporary", class)
	}
	if acc.Status() != pool.StatusHealthy {
		t.Fatalf("account status = %v, want healthy", acc.Status())
	}
	if acc.IsInCooldown() {
		t.Fatal("account must NOT be in cooldown after 402 on public_service")
	}
}

func TestHandleUpstreamError_PublicService401(t *testing.T) {
	p := pool.NewPool([]config.AccountConfig{{
		Name:          "ps",
		Key:           "k",
		BaseURL:       "http://localhost:1",
		Provider:      "ps",
		PublicService: true,
	}})
	acc := p.AllAccounts()[0]

	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"message":"invalid key"}}`))),
	}
	class := handleUpstreamError(acc, resp, "req-ps-401", "m")

	if class != UpstreamErrorPermanentCredential {
		t.Fatalf("class = %v, want UpstreamErrorPermanentCredential", class)
	}
	if acc.Status() != pool.StatusExhausted {
		t.Fatalf("account status = %v, want exhausted (401 is always permanent)", acc.Status())
	}
	if got := acc.LastExhaustClass(); got != pool.ExhaustPermanentCredential {
		t.Fatalf("LastExhaustClass() = %v, want ExhaustPermanentCredential", got)
	}
}

func TestHandleUpstreamError_PublicService429PlainText(t *testing.T) {
	p := pool.NewPool([]config.AccountConfig{{
		Name:          "ps",
		Key:           "k",
		BaseURL:       "http://localhost:1",
		Provider:      "ps",
		PublicService: true,
	}})
	acc := p.AllAccounts()[0]

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(bytes.NewReader([]byte(`rate limit exceeded, retry later`))),
	}
	class := handleUpstreamError(acc, resp, "req-ps-429-plain", "m")

	if class != UpstreamErrorTemporary {
		t.Fatalf("class = %v, want UpstreamErrorTemporary", class)
	}
	if acc.Status() != pool.StatusHealthy {
		t.Fatalf("account status = %v, want healthy", acc.Status())
	}
	if !acc.IsInCooldown() {
		t.Fatal("account must be in cooldown after 429")
	}
}

func TestHandleUpstreamError_PublicService429StructuredQuota(t *testing.T) {
	p := pool.NewPool([]config.AccountConfig{{
		Name:          "ps",
		Key:           "k",
		BaseURL:       "http://localhost:1",
		Provider:      "ps",
		PublicService: true,
	}})
	acc := p.AllAccounts()[0]

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"code":"insufficient_quota","message":"quota"}}`))),
	}
	class := handleUpstreamError(acc, resp, "req-ps-429-struct", "m")

	if class != UpstreamErrorTemporary {
		t.Fatalf("class = %v, want UpstreamErrorTemporary for public_service", class)
	}
	if acc.Status() != pool.StatusHealthy {
		t.Fatalf("account status = %v, want healthy (public_service does not exhaust on quota)", acc.Status())
	}
	if !acc.IsInCooldown() {
		t.Fatal("account must be in cooldown after 429 on public_service")
	}
}

func TestChat_PublicServiceSingleAccount402(t *testing.T) {
	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "ps-single", Key: "k1", BaseURL: upstream.URL, Provider: "test-ps", PublicService: true},
	}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "test-ps")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("client status = %d, want 503", rec.Code)
	}
	code := decodeErrorCode(t, rec.Body.String())
	if code == "upstream_quota_exhausted" {
		t.Errorf("error code must NOT be upstream_quota_exhausted, got %q", code)
	}
	if code != "all_exhausted" {
		t.Errorf("error code = %q, want all_exhausted", code)
	}
	acc := p.AllAccounts()[0]
	if acc.Status() != pool.StatusHealthy {
		t.Errorf("account status = %v, want healthy after public_service 402", acc.Status())
	}
}

func TestChat_PublicServiceDualAccount402Failover(t *testing.T) {
	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusPaymentRequired)
			w.Write([]byte(`{"error":{"code":"insufficient_quota","message":"quota"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "ps-a", Key: "k1", BaseURL: upstream.URL, Provider: "test-failover", PublicService: true},
		{Name: "norm-b", Key: "k2", BaseURL: upstream.URL, Provider: "test-failover"},
	}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "test-failover")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("client status = %d, want 200 OK after failover, body: %s", rec.Code, rec.Body.String())
	}
	accA := p.AllAccounts()[0]
	if accA.Status() != pool.StatusHealthy {
		t.Errorf("account A status = %v, want healthy", accA.Status())
	}
}

func TestChat_PublicService403StructuredQuota(t *testing.T) {
	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":"insufficient_quota","message":"quota forbidden"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "ps-403", Key: "k1", BaseURL: upstream.URL, Provider: "test-ps-403", PublicService: true},
	}}
	p := pool.NewPool(cfg.Accounts)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "test-ps-403")

	proxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("client status = %d, want 403 (bare passthrough for public service)", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); !strings.Contains(got, "quota forbidden") {
		t.Errorf("client body = %q, want passed through quota forbidden body", got)
	}
	if n := atomic.LoadInt32(&upstreamCalls); n != 1 {
		t.Errorf("upstream calls = %d, want 1 (must not retry 403 passthrough)", n)
	}
	acc := p.AllAccounts()[0]
	if acc.Status() != pool.StatusHealthy {
		t.Errorf("account status = %v, want healthy", acc.Status())
	}
}
