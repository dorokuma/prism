package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dorokuma/prism/internal/config"
)

func TestCopyUpstreamHeaders_RateLimitAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	src := http.Header{}
	src.Set("X-RateLimit-Limit-Requests", "1000")
	src.Set("X-RateLimit-Remaining-Requests", "999")
	src.Set("X-RateLimit-Reset-Requests", "2s")
	src.Set("anthropic-ratelimit-requests-limit", "500")

	copyUpstreamHeaders(rec, src, nil, "")

	dst := rec.Header()
	for _, k := range []string{
		"X-RateLimit-Limit-Requests",
		"X-RateLimit-Remaining-Requests",
		"X-RateLimit-Reset-Requests",
		"anthropic-ratelimit-requests-limit",
	} {
		if dst.Get(k) == "" {
			t.Errorf("expected rate limit header %q to be forwarded", k)
		}
	}
}

func TestCopyUpstreamHeaders_QuotaAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	src := http.Header{}
	src.Set("X-Quota-Limit", "5000000")
	src.Set("X-Quota-Remaining", "4950000")

	copyUpstreamHeaders(rec, src, nil, "")

	dst := rec.Header()
	for _, k := range []string{"X-Quota-Limit", "X-Quota-Remaining"} {
		if dst.Get(k) == "" {
			t.Errorf("expected quota header %q to be forwarded", k)
		}
	}
}

func TestCopyUpstreamHeaders_SensitiveDenylist(t *testing.T) {
	rec := httptest.NewRecorder()
	src := http.Header{}
	src.Set("X-RateLimit-User-Id", "user_123")
	src.Set("X-RateLimit-Account-Id", "acc_456")
	src.Set("X-RateLimit-Org-Id", "org_789")
	src.Set("X-Quota-Account-Id", "qa_123")
	src.Set("X-Request-Id", "req_abc")
	src.Set("X-Amzn-Trace-Id", "trace_xyz")
	src.Set("Server", "nginx/1.24")
	src.Set("Via", "1.1 varnish")
	src.Set("X-Powered-By", "Express")
	src.Set("X-Upstream-Server", "up-1")
	src.Set("X-RateLimit-Remaining", "50")

	copyUpstreamHeaders(rec, src, nil, "")

	dst := rec.Header()
	for _, k := range []string{
		"X-RateLimit-User-Id",
		"X-RateLimit-Account-Id",
		"X-RateLimit-Org-Id",
		"X-Quota-Account-Id",
		"X-Request-Id",
		"X-Amzn-Trace-Id",
		"Server",
		"Via",
		"X-Powered-By",
		"X-Upstream-Server",
	} {
		if dst.Get(k) != "" {
			t.Errorf("sensitive/infrastructure header %q must be stripped, got %q", k, dst.Get(k))
		}
	}
	if dst.Get("X-RateLimit-Remaining") != "50" {
		t.Errorf("expected X-RateLimit-Remaining to be forwarded, got %q", dst.Get("X-RateLimit-Remaining"))
	}
}

func TestCopyUpstreamHeaders_ConfigDisabled(t *testing.T) {
	rec := httptest.NewRecorder()
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("Content-Disposition", "attachment; filename=test.json")
	src.Set("Content-Language", "en")
	src.Set("Retry-After", "30")
	src.Set("X-RateLimit-Remaining", "99")
	src.Set("X-Quota-Remaining", "1000")

	disabled := false
	hp := &config.HeaderPassthroughConfig{Enabled: &disabled}
	copyUpstreamHeaders(rec, src, hp, "")

	dst := rec.Header()
	for _, allowed := range []string{"Content-Type", "Content-Disposition", "Content-Language", "Retry-After"} {
		if dst.Get(allowed) == "" {
			t.Errorf("allowed header %q missing from response when disabled", allowed)
		}
	}
	for _, disallowed := range []string{"X-RateLimit-Remaining", "X-Quota-Remaining"} {
		if dst.Get(disallowed) != "" {
			t.Errorf("header %q must be stripped when header_passthrough is disabled", disallowed)
		}
	}
}

func TestCopyUpstreamHeaders_NormalizeOpenAIPath(t *testing.T) {
	rec := httptest.NewRecorder()
	src := http.Header{}
	src.Set("anthropic-ratelimit-requests-limit", "500")
	src.Set("anthropic-ratelimit-requests-remaining", "490")
	src.Set("anthropic-ratelimit-requests-reset", "2024-02-15T00:00:00Z")
	src.Set("anthropic-ratelimit-tokens-limit", "100000")
	src.Set("anthropic-ratelimit-tokens-remaining", "95000")
	src.Set("anthropic-ratelimit-tokens-reset", "2024-02-15T00:00:00.000Z")

	hp := &config.HeaderPassthroughConfig{Mode: "normalize"}
	copyUpstreamHeaders(rec, src, hp, "/chat/completions")

	dst := rec.Header()
	if dst.Get("X-RateLimit-Limit-Requests") != "500" {
		t.Errorf("X-RateLimit-Limit-Requests = %q, want 500", dst.Get("X-RateLimit-Limit-Requests"))
	}
	if dst.Get("X-RateLimit-Remaining-Requests") != "490" {
		t.Errorf("X-RateLimit-Remaining-Requests = %q, want 490", dst.Get("X-RateLimit-Remaining-Requests"))
	}
	// 2024-02-15T00:00:00Z -> 1707955200 (seconds)
	if dst.Get("X-RateLimit-Reset-Requests") != "1707955200" {
		t.Errorf("X-RateLimit-Reset-Requests = %q, want 1707955200", dst.Get("X-RateLimit-Reset-Requests"))
	}
	if dst.Get("X-RateLimit-Limit-Tokens") != "100000" {
		t.Errorf("X-RateLimit-Limit-Tokens = %q, want 100000", dst.Get("X-RateLimit-Limit-Tokens"))
	}
	if dst.Get("X-RateLimit-Remaining-Tokens") != "95000" {
		t.Errorf("X-RateLimit-Remaining-Tokens = %q, want 95000", dst.Get("X-RateLimit-Remaining-Tokens"))
	}
	if dst.Get("X-RateLimit-Reset-Tokens") != "1707955200" {
		t.Errorf("X-RateLimit-Reset-Tokens = %q, want 1707955200", dst.Get("X-RateLimit-Reset-Tokens"))
	}
}

func TestCopyUpstreamHeaders_NormalizeMessagesPathBypassed(t *testing.T) {
	rec := httptest.NewRecorder()
	src := http.Header{}
	src.Set("anthropic-ratelimit-requests-limit", "500")
	src.Set("anthropic-ratelimit-requests-reset", "2024-02-15T00:00:00Z")

	hp := &config.HeaderPassthroughConfig{Mode: "normalize"}
	// /v1/messages should NOT be rewritten to OpenAI headers or have timestamp altered
	copyUpstreamHeaders(rec, src, hp, "/v1/messages")

	dst := rec.Header()
	if dst.Get("anthropic-ratelimit-requests-limit") != "500" {
		t.Errorf("anthropic-ratelimit-requests-limit = %q, want 500", dst.Get("anthropic-ratelimit-requests-limit"))
	}
	if dst.Get("anthropic-ratelimit-requests-reset") != "2024-02-15T00:00:00Z" {
		t.Errorf("anthropic-ratelimit-requests-reset = %q, want 2024-02-15T00:00:00Z", dst.Get("anthropic-ratelimit-requests-reset"))
	}
	if dst.Get("X-RateLimit-Limit-Requests") != "" {
		t.Errorf("X-RateLimit-Limit-Requests should be empty on /v1/messages, got %q", dst.Get("X-RateLimit-Limit-Requests"))
	}
}
