package cache

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

func newTestCacheAndHandler(t *testing.T) (*ModelCache, *RefreshHandler) {
	t.Helper()
	cfg := &config.Config{
		Accounts: []config.AccountConfig{
			{Name: "a1", Provider: "p1", BaseURL: "http://127.0.0.1:18790", Key: "k1"},
			{Name: "a2", Provider: "p2", BaseURL: "http://127.0.0.1:18790", Key: "k2"},
		},
	}
	mc := &ModelCache{
		dir: t.TempDir(),
		caches: map[string]*providerCache{
			"p1": {Models: []ModelEntry{{ID: "m1"}}},
		},
		cfg:  cfg,
		pool: pool.NewPool(cfg.Accounts),
		stop: make(chan struct{}),
	}
	holder := config.NewConfigHolder(cfg)
	h := NewRefreshHandler(mc, holder)
	return mc, h
}

func TestRefreshHandler_AuthAndMethod(t *testing.T) {
	os.Setenv("PRISM_ADMIN_TOKEN", "secret-admin-token")
	defer os.Unsetenv("PRISM_ADMIN_TOKEN")

	_, h := newTestCacheAndHandler(t)

	// 1. Unauthorized from non-loopback without token
	req := httptest.NewRequest(http.MethodPost, "/prism/v1/models/refresh", nil)
	req.RemoteAddr = "192.168.1.100:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", rec.Code)
	}

	// 2. Unauthorized from loopback without token when token is set (fail-closed)
	req = httptest.NewRequest(http.MethodPost, "/prism/v1/models/refresh", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for tokenless loopback when token is configured, got %d", rec.Code)
	}

	// 3. Method Not Allowed (DELETE with valid token)
	req = httptest.NewRequest(http.MethodDelete, "/prism/v1/models/refresh", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer secret-admin-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, POST" {
		t.Fatalf("expected Allow: GET, POST header, got %q", allow)
	}

	// 4. Authorized POST -> 202 Accepted
	req = httptest.NewRequest(http.MethodPost, "/prism/v1/models/refresh", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer secret-admin-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp RefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response JSON: %v", err)
	}
	if resp.Status != "accepted" {
		t.Fatalf("expected status 'accepted', got %q", resp.Status)
	}
	if _, ok := resp.Providers["p1"]; !ok {
		t.Fatalf("expected p1 in providers map, got %+v", resp.Providers)
	}

	// 5. Authorized GET -> 200 OK (read-only)
	req = httptest.NewRequest(http.MethodGet, "/prism/v1/models/refresh", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer secret-admin-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	var getResp RefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("failed to decode get response JSON: %v", err)
	}
	if getResp.Status != "ok" {
		t.Fatalf("expected status 'ok', got %q", getResp.Status)
	}
}

func TestRefreshHandler_TokenlessAuth(t *testing.T) {
	os.Unsetenv("PRISM_ADMIN_TOKEN")

	_, h := newTestCacheAndHandler(t)

	// Remote request without token -> 401
	req := httptest.NewRequest(http.MethodGet, "/prism/v1/models/refresh", nil)
	req.RemoteAddr = "192.168.1.100:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for remote request without token, got %d", rec.Code)
	}

	// Loopback request without token -> 200 allowed
	req = httptest.NewRequest(http.MethodGet, "/prism/v1/models/refresh", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for loopback request without token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRefreshHandler_GET_Filtering(t *testing.T) {
	os.Unsetenv("PRISM_ADMIN_TOKEN")
	_, h := newTestCacheAndHandler(t)

	// Filter single valid provider
	req := httptest.NewRequest(http.MethodGet, "/prism/v1/models/refresh?provider=p1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	var resp RefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Providers) != 1 || resp.Providers["p1"].ModelsCount != 1 {
		t.Fatalf("expected 1 provider (p1) with 1 model, got %+v", resp.Providers)
	}

	// Filter unknown provider -> 400 Bad Request
	req = httptest.NewRequest(http.MethodGet, "/prism/v1/models/refresh?provider=unknown", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for unknown provider, got %d", rec.Code)
	}
}

func TestRefreshHandler_UnknownProvider(t *testing.T) {
	os.Unsetenv("PRISM_ADMIN_TOKEN")
	_, h := newTestCacheAndHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/prism/v1/models/refresh?provider=unknown", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for unknown provider, got %d", rec.Code)
	}
}

func TestRefreshHandler_RateLimiting(t *testing.T) {
	os.Unsetenv("PRISM_ADMIN_TOKEN")
	_, h := newTestCacheAndHandler(t)

	// Burst of 2 should succeed
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/prism/v1/models/refresh", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("request %d: expected 202, got %d", i+1, rec.Code)
		}
	}

	// 3rd immediate request should be rate limited (429) and contain Retry-After
	req := httptest.NewRequest(http.MethodPost, "/prism/v1/models/refresh", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request 3: expected 429 Too Many Requests, got %d", rec.Code)
	}
	if retryAfter := rec.Header().Get("Retry-After"); retryAfter != "10" {
		t.Fatalf("expected Retry-After: 10 header, got %q", retryAfter)
	}
}

func TestRefreshHandler_NilCache(t *testing.T) {
	os.Unsetenv("PRISM_ADMIN_TOKEN")
	h := NewRefreshHandler(nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/prism/v1/models/refresh", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable, got %d", rec.Code)
	}
}

func TestRefreshRateLimiter_TokenRefill(t *testing.T) {
	rl := newRefreshRateLimiter()
	if !rl.Allow() {
		t.Fatal("first token should be allowed")
	}
	if !rl.Allow() {
		t.Fatal("second token should be allowed (burst 2)")
	}
	if rl.Allow() {
		t.Fatal("third token should be rejected immediately")
	}

	// Artificially simulate 10s elapsed
	rl.mu.Lock()
	rl.lastCheck = time.Now().Add(-10 * time.Second)
	rl.mu.Unlock()

	if !rl.Allow() {
		t.Fatal("token should be available after 10s elapsed")
	}
}
