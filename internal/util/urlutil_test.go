package util

import "testing"

func TestJoinURLPath(t *testing.T) {
	tests := []struct {
		base     string
		endpoint string
		expected string
	}{
		{"https://opencode.ai/zen/go/v1", "/v1/models", "https://opencode.ai/zen/go/v1/models"},
		{"https://opencode.ai/zen/go/v1/", "/v1/models", "https://opencode.ai/zen/go/v1/models"},
		{"https://opencode.ai/zen/go", "/v1/models", "https://opencode.ai/zen/go/v1/models"},
		{"https://opencode.ai/zen/go/v1", "/chat/completions", "https://opencode.ai/zen/go/v1/chat/completions"},
		{"http://localhost:8001", "/v1/models", "http://localhost:8001/v1/models"},
	}

	for _, tt := range tests {
		got := JoinURLPath(tt.base, tt.endpoint)
		if got != tt.expected {
			t.Errorf("JoinURLPath(%q, %q) = %q, expected %q", tt.base, tt.endpoint, got, tt.expected)
		}
	}
}
