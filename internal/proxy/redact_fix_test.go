package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

// ---------------------------------------------------------------------------
// Batch-2 audit item: key-aware redaction of upstream error bodies
// ---------------------------------------------------------------------------

// TestUpstreamErrorBodiesRedactNonBearerKey guards key-aware redaction on
// every upstream error path (401/402/429, bare 4xx, 5xx): the account key
// "raw-key-98765" has no sk- or Bearer prefix, so the regex-only redaction
// would leave it untouched. The body (which echoes the key) must be scrubbed
// in the client response (4xx passthrough), in the audit error, and in every
// slog line — via RedactBodyBytesWithKeys(..., acc.Key()).
func TestUpstreamErrorBodiesRedactNonBearerKey(t *testing.T) {
	const key = "raw-key-98765"
	const echoedBody = `{"error":{"message":"bad key raw-key-98765"}}`

	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"401", http.StatusUnauthorized, echoedBody},
		{"402", http.StatusPaymentRequired, echoedBody},
		{"429", http.StatusTooManyRequests, echoedBody},
		{"403", http.StatusForbidden, echoedBody},
		{"500", http.StatusInternalServerError, echoedBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 429/500 cool the account down before the retry; shrink the
			// cooldown so the retry's pool select does not wait 30s.
			cooldownRestore := SetUpstreamCooldownForTest(time.Millisecond)
			defer cooldownRestore()

			h := &capturingHandler{}
			restore := stashSlog(h)
			defer restore()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()

			cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "test", Key: key, BaseURL: upstream.URL, Provider: "test"}}}
			p := pool.NewPool(cfg.Accounts)

			rec := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"gpt-4"}`)))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Prism-Provider", "test")

			ProxyChatWithBody(p, rec, r, []byte(`{"model":"gpt-4"}`), time.Now(), ChatForwardOpts{}, cfg)

			out := h.output()
			if strings.Contains(out, key) {
				t.Errorf("log output leaks the non-sk account key:\n%s", out)
			}
			if !strings.Contains(out, "***") {
				t.Errorf("log output should carry the redaction marker:\n%s", out)
			}
			if strings.Contains(rec.Body.String(), key) {
				t.Errorf("client body leaks the non-sk account key: %s", rec.Body.String())
			}
			// 4xx is passed through to the client: the redacted marker must
			// be visible in the client response (audit + log covered above).
			if tc.status == http.StatusForbidden {
				if !strings.Contains(rec.Body.String(), "***") {
					t.Errorf("4xx client body should carry the redaction marker, got %q", rec.Body.String())
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Batch-2 audit item: bounded request bodies (413 / 400)
// ---------------------------------------------------------------------------

// errReader fails on the first Read with a generic error (not a size
// overflow), so it exercises the non-MaxBytesError read-error branch.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// TestRequestBodyTooLarge413 guards the 10 MiB request-body cap on all three
// POST surfaces: an over-cap body gets HTTP 413 with Content-Type
// application/json and error.code request_too_large — not a 500 with a plain
// text body.
func TestRequestBodyTooLarge413(t *testing.T) {
	big := strings.Repeat("a", (10<<20)+1)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://localhost:1", Provider: "t"}}}
			p := pool.NewPool(cfg.Accounts)
			h := NewProxyHandler(p, config.WireAPIBoth, config.NewConfigHolder(cfg), nil)

			rec := httptest.NewRecorder()
			r := httptest.NewRequest("POST", path, strings.NewReader(big))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Prism-Provider", "t")
			h.ServeHTTP(rec, r)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if code := decodeErrorCode(t, rec.Body.String()); code != "request_too_large" {
				t.Errorf("error code = %q, want request_too_large", code)
			}
		})
	}
}

// TestRequestBodyReadError400 guards the non-size read error path: a body
// that fails mid-read gets HTTP 400 with the unified JSON envelope
// (error.code invalid_request) on all three surfaces — not a 500 with a
// plain text body.
func TestRequestBodyReadError400(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://localhost:1", Provider: "t"}}}
			p := pool.NewPool(cfg.Accounts)
			h := NewProxyHandler(p, config.WireAPIBoth, config.NewConfigHolder(cfg), nil)

			rec := httptest.NewRecorder()
			r := httptest.NewRequest("POST", path, errReader{})
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Prism-Provider", "t")
			h.ServeHTTP(rec, r)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if code := decodeErrorCode(t, rec.Body.String()); code != "invalid_request" {
				t.Errorf("error code = %q, want invalid_request", code)
			}
		})
	}
}

// TestRequestBodyAtCapPasses guards the boundary: a body of exactly 10 MiB
// is accepted (readable), only beyond the cap is rejected. The upstream
// answers 200 so a successful read must reach the proxy path.
func TestRequestBodyAtCapPasses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: upstream.URL, Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)
	h := NewProxyHandler(p, config.WireAPIBoth, config.NewConfigHolder(cfg), nil)

	// Pad so the FULL body (JSON envelope included) is exactly 10 MiB.
	const prefix = `{"model":"gpt-4","prompt":"`
	const suffix = `"}`
	body := prefix + strings.Repeat("a", (10<<20)-len(prefix)-len(suffix)) + suffix
	if len(body) != 10<<20 {
		t.Fatalf("test body size = %d, want %d", len(body), 10<<20)
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "t")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body at the cap must pass)", rec.Code)
	}
}
