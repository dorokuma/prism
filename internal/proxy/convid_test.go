package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

func newTestAccount(name, provider, baseURL string, headers map[string]string) *pool.Account {
	p := pool.NewPool([]config.AccountConfig{
		{
			Name:     name,
			Provider: provider,
			BaseURL:  baseURL,
			Key:      "test-key",
			Headers:  headers,
		},
	})
	acc, _, err := p.Select(nil, "test-model", 10)
	if err != nil {
		panic(err)
	}
	return acc
}

func TestStripLaneSuffix(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "uuid:lane stripped to uuid",
			input: "019565df-038b-7000-8000-000000000001:lane-1",
			want:  "019565df-038b-7000-8000-000000000001",
		},
		{
			name:  "pure uuid unchanged",
			input: "019565df-038b-7000-8000-000000000001",
			want:  "019565df-038b-7000-8000-000000000001",
		},
		{
			name:  "non-uuid with colon not stripped (custom namespace)",
			input: "custom-namespace:session-123",
			want:  "custom-namespace:session-123",
		},
		{
			name:  "non-uuid short with colon not stripped",
			input: "short:lane",
			want:  "short:lane",
		},
		{
			name:  "oversized lane suffix stripped preserving uuid",
			input: "019565df-038b-7000-8000-000000000001:" + strings.Repeat("x", 200),
			want:  "019565df-038b-7000-8000-000000000001",
		},
		{
			name:  "without colon not stripped",
			input: "simple-conv-session-id",
			want:  "simple-conv-session-id",
		},
		{
			name:  "uuid:multiple:colons stripped to uuid",
			input: "019565df-038b-7000-8000-000000000001:lane1:sub2",
			want:  "019565df-038b-7000-8000-000000000001",
		},
		{
			name:  "uuid with empty suffix not stripped",
			input: "019565df-038b-7000-8000-000000000001:",
			want:  "019565df-038b-7000-8000-000000000001:",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripLaneSuffix(tc.input)
			if got != tc.want {
				t.Errorf("stripLaneSuffix(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestApplyXaiConvID_Success(t *testing.T) {
	acc := newTestAccount("xai-acc", "xai", "https://api.x.ai", nil)
	src := http.Header{"Session_id": []string{"conv_abc123"}}
	dst := make(http.Header)

	applyXaiConvID(dst, src, acc)

	if got := dst.Get("X-Grok-Conv-Id"); got != "conv_abc123" {
		t.Fatalf("expected X-Grok-Conv-Id = %q, got %q", "conv_abc123", got)
	}
}

func TestApplyXaiConvID_LaneStripping(t *testing.T) {
	acc := newTestAccount("xai-acc", "xai", "https://api.x.ai", nil)
	src := http.Header{"Session_id": []string{"019565df-038b-7000-8000-000000000001:lane-worker-2"}}
	dst := make(http.Header)

	applyXaiConvID(dst, src, acc)

	if got := dst.Get("X-Grok-Conv-Id"); got != "019565df-038b-7000-8000-000000000001" {
		t.Fatalf("expected X-Grok-Conv-Id = %q, got %q", "019565df-038b-7000-8000-000000000001", got)
	}
}

func TestApplyXaiConvID_OutboundHeaderCleansing(t *testing.T) {
	acc := newTestAccount("xai-acc", "xai", "https://api.x.ai", nil)

	casings := []struct {
		name      string
		headerKey string
		headerVal string
		otherKey  string
		otherVal  string
	}{
		{
			name:      "Session_id canonical net/http",
			headerKey: "Session_id",
			headerVal: "conv-1",
			otherKey:  "X-Other-Header",
			otherVal:  "val-1",
		},
		{
			name:      "session_id lowercase",
			headerKey: "session_id",
			headerVal: "conv-2",
			otherKey:  "X-Other-Header",
			otherVal:  "val-2",
		},
		{
			name:      "Session-Id canonical dash",
			headerKey: "Session-Id",
			headerVal: "conv-3",
			otherKey:  "X-Other-Header",
			otherVal:  "val-3",
		},
		{
			name:      "session-id lowercase dash",
			headerKey: "session-id",
			headerVal: "conv-4",
			otherKey:  "X-Other-Header",
			otherVal:  "val-4",
		},
		{
			name:      "SESSION_ID uppercase",
			headerKey: "SESSION_ID",
			headerVal: "conv-5",
			otherKey:  "X-Other-Header",
			otherVal:  "val-5",
		},
		{
			name:      "SESSION-ID uppercase dash",
			headerKey: "SESSION-ID",
			headerVal: "conv-6",
			otherKey:  "X-Other-Header",
			otherVal:  "val-6",
		},
	}

	for _, tc := range casings {
		t.Run(tc.name, func(t *testing.T) {
			src := http.Header{tc.headerKey: []string{tc.headerVal}}
			dst := make(http.Header)
			// Simulate copyClientHeaders having already copied the header to dst
			dst.Set(tc.headerKey, tc.headerVal)
			dst.Set(tc.otherKey, tc.otherVal)

			applyXaiConvID(dst, src, acc)

			if got := dst.Get("X-Grok-Conv-Id"); got != tc.headerVal {
				t.Fatalf("expected X-Grok-Conv-Id = %q, got %q", tc.headerVal, got)
			}
			if got := dst.Get("Session_id"); got != "" {
				t.Errorf("expected Session_id to be deleted from dst, got %q", got)
			}
			if got := dst.Get("session_id"); got != "" {
				t.Errorf("expected session_id to be deleted from dst, got %q", got)
			}
			if got := dst.Get("Session-Id"); got != "" {
				t.Errorf("expected Session-Id to be deleted from dst, got %q", got)
			}
			if got := dst.Get("session-id"); got != "" {
				t.Errorf("expected session-id to be deleted from dst, got %q", got)
			}
			if got := dst.Get(tc.otherKey); got != tc.otherVal {
				t.Errorf("expected %s to be preserved, got %q", tc.otherKey, got)
			}
		})
	}
}

func TestApplyXaiConvID_CasingVariants(t *testing.T) {
	acc := newTestAccount("xai-acc", "xai", "https://api.x.ai", nil)

	variants := []struct {
		name string
		src  http.Header
		want string
	}{
		{
			name: "Session-Id canonical",
			src:  http.Header{"Session-Id": []string{"id-canonical"}},
			want: "id-canonical",
		},
		{
			name: "session-id lowercase",
			src:  http.Header{"session-id": []string{"id-lower-dash"}},
			want: "id-lower-dash",
		},
		{
			name: "Session_id canonical net/http",
			src:  http.Header{"Session_id": []string{"id-canonical-under"}},
			want: "id-canonical-under",
		},
		{
			name: "session_id lowercase",
			src:  http.Header{"session_id": []string{"id-lower-under"}},
			want: "id-lower-under",
		},
		{
			name: "SESSION_ID uppercase",
			src:  http.Header{"SESSION_ID": []string{"id-upper-under"}},
			want: "id-upper-under",
		},
		{
			name: "SESSION-ID uppercase",
			src:  http.Header{"SESSION-ID": []string{"id-upper-dash"}},
			want: "id-upper-dash",
		},
	}

	for _, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			dst := make(http.Header)
			applyXaiConvID(dst, tc.src, acc)
			if got := dst.Get("X-Grok-Conv-Id"); got != tc.want {
				t.Errorf("for %s: got %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestApplyXaiConvID_Priority(t *testing.T) {
	acc := newTestAccount("xai-acc", "xai", "https://api.x.ai", nil)

	// 1. Explicit x-grok-conv-id overrides session_id, x-session-affinity
	{
		src := http.Header{
			"X-Grok-Conv-Id":     []string{"explicit-grok-id"},
			"Session_id":         []string{"session-id-val"},
			"X-Session-Affinity": []string{"affinity-val"},
		}
		dst := make(http.Header)
		// Simulating copyClientHeaders copying X-Grok-Conv-Id first
		dst.Set("X-Grok-Conv-Id", "explicit-grok-id")
		applyXaiConvID(dst, src, acc)
		if got := dst.Get("X-Grok-Conv-Id"); got != "explicit-grok-id" {
			t.Errorf("priority 1 failed: want explicit-grok-id, got %q", got)
		}
	}

	// 2. session_id overrides x-session-affinity
	{
		src := http.Header{
			"session_id":         []string{"session-id-val"},
			"x-session-affinity": []string{"affinity-val"},
		}
		dst := make(http.Header)
		applyXaiConvID(dst, src, acc)
		if got := dst.Get("X-Grok-Conv-Id"); got != "session-id-val" {
			t.Errorf("priority 2 failed: want session-id-val, got %q", got)
		}
	}

	// 3. x-session-affinity is used when session_id is absent
	{
		src := http.Header{
			"x-session-affinity": []string{"affinity-val"},
		}
		dst := make(http.Header)
		applyXaiConvID(dst, src, acc)
		if got := dst.Get("X-Grok-Conv-Id"); got != "affinity-val" {
			t.Errorf("priority 3 failed: want affinity-val, got %q", got)
		}
	}

	// 4. x-client-request-id is NOT consumed / fallback removed
	{
		src := http.Header{
			"x-client-request-id": []string{"client-req-val"},
		}
		dst := make(http.Header)
		applyXaiConvID(dst, src, acc)
		if got := dst.Get("X-Grok-Conv-Id"); got != "" {
			t.Errorf("x-client-request-id should not be consumed as conversation ID, got %q", got)
		}
	}
}

func TestApplyXaiConvID_NonXaiAccount(t *testing.T) {
	tests := []struct {
		provider string
	}{
		{"openai"},
		{"anthropic"},
		{"deepseek"},
		{""},
	}

	for _, tc := range tests {
		t.Run("provider_"+tc.provider, func(t *testing.T) {
			acc := newTestAccount("test-acc", tc.provider, "https://api.example.com", nil)
			src := http.Header{
				"Session_id": []string{"my-session-123"},
			}
			dst := make(http.Header)
			dst.Set("Session_id", "my-session-123")
			applyXaiConvID(dst, src, acc)
			if got := dst.Get("X-Grok-Conv-Id"); got != "" {
				t.Errorf("non-xai provider %q should not set X-Grok-Conv-Id, got %q", tc.provider, got)
			}
			// Non-xai path should not touch session_id in dst
			if got := dst.Get("Session_id"); got != "my-session-123" {
				t.Errorf("non-xai provider %q should preserve Session_id, got %q", tc.provider, got)
			}
		})
	}

	// Nil account
	dst := make(http.Header)
	src := http.Header{"Session_id": []string{"my-session-123"}}
	applyXaiConvID(dst, src, nil)
	if got := dst.Get("X-Grok-Conv-Id"); got != "" {
		t.Errorf("nil account should not set X-Grok-Conv-Id, got %q", got)
	}
}

func TestApplyXaiConvID_Sanitize(t *testing.T) {
	acc := newTestAccount("xai-acc", "xai", "https://api.x.ai", nil)

	// 1. Truncate to 128 characters
	{
		longStr := strings.Repeat("a", 200)
		src := http.Header{"session_id": []string{longStr}}
		dst := make(http.Header)
		applyXaiConvID(dst, src, acc)
		got := dst.Get("X-Grok-Conv-Id")
		if len(got) != 128 {
			t.Errorf("expected 128 chars, got %d", len(got))
		}
		if got != strings.Repeat("a", 128) {
			t.Errorf("mismatched truncated content")
		}
	}

	// 2. Remove invalid characters and keep [A-Za-z0-9._:-]
	{
		raw := "  hello@world #123 (test); key=value:sub.item_1-2  "
		src := http.Header{"session_id": []string{raw}}
		dst := make(http.Header)
		applyXaiConvID(dst, src, acc)
		want := "helloworld123testkeyvalue:sub.item_1-2"
		if got := dst.Get("X-Grok-Conv-Id"); got != want {
			t.Errorf("sanitize mismatch: got %q, want %q", got, want)
		}
	}

	// 3. Empty or only invalid characters should result in empty / no header set
	{
		for _, invalid := range []string{"", "   ", "@#$%^&*() \t\n"} {
			src := http.Header{"session_id": []string{invalid}}
			dst := make(http.Header)
			applyXaiConvID(dst, src, acc)
			if got := dst.Get("X-Grok-Conv-Id"); got != "" {
				t.Errorf("expected no X-Grok-Conv-Id for %q, got %q", invalid, got)
			}
		}
	}
}

func TestApplyXaiConvID_ExistingDstHeaderRetained(t *testing.T) {
	acc := newTestAccount("xai-acc", "xai", "https://api.x.ai", nil)
	src := http.Header{
		"session_id": []string{"new-session-id"},
	}
	dst := http.Header{
		"X-Grok-Conv-Id": []string{"original-conv-id"},
	}

	applyXaiConvID(dst, src, acc)

	if got := dst.Get("X-Grok-Conv-Id"); got != "original-conv-id" {
		t.Errorf("expected existing X-Grok-Conv-Id to be preserved, got %q", got)
	}
}

func TestApplyXaiConvID_PreserveOtherHeaders(t *testing.T) {
	acc := newTestAccount("xai-acc", "xai", "https://api.x.ai", nil)
	src := http.Header{
		"Session_id":   []string{"conv_123"},
		"X-Custom-Req": []string{"req_value"},
	}
	dst := http.Header{
		"X-Pre-Existing": []string{"pre_value"},
	}

	applyXaiConvID(dst, src, acc)

	if dst.Get("X-Pre-Existing") != "pre_value" {
		t.Errorf("existing header was modified")
	}
	if dst.Get("X-Custom-Req") != "" {
		t.Errorf("applyXaiConvID should not copy unrelated headers from src to dst")
	}
	if dst.Get("X-Grok-Conv-Id") != "conv_123" {
		t.Errorf("expected X-Grok-Conv-Id = conv_123")
	}
}

func TestDoUpstreamRequest_XaiConvID_Integration(t *testing.T) {
	var capturedHeader http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-123","object":"chat.completion"}`))
	}))
	defer ts.Close()

	// Case A: xai provider with session_id header -> mapped to X-Grok-Conv-Id, session_id removed
	{
		acc := newTestAccount("xai-live", "xai", ts.URL, nil)
		clientReq, err := http.NewRequest("POST", "http://prism.test/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		clientReq.Header.Set("Session_id", "sess_integration_999")

		res := doUpstreamRequest(acc, clientReq, []byte(`{}`), ChatForwardOpts{Model: "grok-beta"}, "req-1")
		if res.resp != nil {
			_ = res.resp.Body.Close()
		}
		if res.fatalErr != nil {
			t.Fatalf("unexpected fatalErr: %v", res.fatalErr)
		}
		if got := capturedHeader.Get("X-Grok-Conv-Id"); got != "sess_integration_999" {
			t.Errorf("xai upstream did not receive X-Grok-Conv-Id: got %q, want sess_integration_999", got)
		}
		if got := capturedHeader.Get("Session_id"); got != "" {
			t.Errorf("xai upstream received unexpected Session_id header: got %q", got)
		}
	}

	// Case B: non-xai provider (openai) with session_id header -> untouched
	{
		acc := newTestAccount("openai-live", "openai", ts.URL, nil)
		clientReq, err := http.NewRequest("POST", "http://prism.test/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		clientReq.Header.Set("Session_id", "sess_integration_999")

		res := doUpstreamRequest(acc, clientReq, []byte(`{}`), ChatForwardOpts{Model: "gpt-4o"}, "req-2")
		if res.resp != nil {
			_ = res.resp.Body.Close()
		}
		if res.fatalErr != nil {
			t.Fatalf("unexpected fatalErr: %v", res.fatalErr)
		}
		if got := capturedHeader.Get("X-Grok-Conv-Id"); got != "" {
			t.Errorf("non-xai upstream received unexpected X-Grok-Conv-Id: got %q", got)
		}
	}

	// Case C: xai provider with explicit client X-Grok-Conv-Id
	{
		acc := newTestAccount("xai-live", "xai", ts.URL, nil)
		clientReq, err := http.NewRequest("POST", "http://prism.test/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		clientReq.Header.Set("X-Grok-Conv-Id", "client-explicit-conv-id")
		clientReq.Header.Set("Session_id", "sess_should_not_override")

		res := doUpstreamRequest(acc, clientReq, []byte(`{}`), ChatForwardOpts{Model: "grok-beta"}, "req-3")
		if res.resp != nil {
			_ = res.resp.Body.Close()
		}
		if res.fatalErr != nil {
			t.Fatalf("unexpected fatalErr: %v", res.fatalErr)
		}
		if got := capturedHeader.Get("X-Grok-Conv-Id"); got != "client-explicit-conv-id" {
			t.Errorf("explicit X-Grok-Conv-Id was overridden: got %q, want client-explicit-conv-id", got)
		}
	}

	// Case D: xai provider with lane suffix in session_id -> stripped to uuid
	{
		acc := newTestAccount("xai-live", "xai", ts.URL, nil)
		clientReq, err := http.NewRequest("POST", "http://prism.test/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		clientReq.Header.Set("Session_id", "019565df-038b-7000-8000-000000000001:lane-subagent-worker")

		res := doUpstreamRequest(acc, clientReq, []byte(`{}`), ChatForwardOpts{Model: "grok-beta"}, "req-4")
		if res.resp != nil {
			_ = res.resp.Body.Close()
		}
		if res.fatalErr != nil {
			t.Fatalf("unexpected fatalErr: %v", res.fatalErr)
		}
		if got := capturedHeader.Get("X-Grok-Conv-Id"); got != "019565df-038b-7000-8000-000000000001" {
			t.Errorf("xai upstream did not strip lane suffix: got %q, want 019565df-038b-7000-8000-000000000001", got)
		}
		if got := capturedHeader.Get("Session_id"); got != "" {
			t.Errorf("xai upstream received unexpected Session_id header: got %q", got)
		}
	}

	// Case E: xai provider with x-client-request-id only -> not consumed
	{
		acc := newTestAccount("xai-live", "xai", ts.URL, nil)
		clientReq, err := http.NewRequest("POST", "http://prism.test/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		clientReq.Header.Set("X-Client-Request-Id", "trace-id-12345")

		res := doUpstreamRequest(acc, clientReq, []byte(`{}`), ChatForwardOpts{Model: "grok-beta"}, "req-5")
		if res.resp != nil {
			_ = res.resp.Body.Close()
		}
		if res.fatalErr != nil {
			t.Fatalf("unexpected fatalErr: %v", res.fatalErr)
		}
		if got := capturedHeader.Get("X-Grok-Conv-Id"); got != "" {
			t.Errorf("xai upstream should not consume x-client-request-id as conv-id: got %q", got)
		}
	}
}
