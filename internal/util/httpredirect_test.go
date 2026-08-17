package util

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestCheckSameHostRedirect_SameHostAllowed(t *testing.T) {
	orig := &http.Request{URL: mustURL(t, "https://api.example.com/v1/usage")}
	next := &http.Request{URL: mustURL(t, "https://API.example.com/v1/usage/current")}
	if err := CheckSameHostRedirect(next, []*http.Request{orig}); err != nil {
		t.Fatalf("same host must be allowed: %v", err)
	}
}

func TestCheckSameHostRedirect_CrossHostRefused(t *testing.T) {
	orig := &http.Request{URL: mustURL(t, "https://api.example.com/v1/usage")}
	next := &http.Request{URL: mustURL(t, "https://evil.example/steal")}
	err := CheckSameHostRedirect(next, []*http.Request{orig})
	if err == nil {
		t.Fatal("cross-host redirect must be refused")
	}
	if !strings.Contains(err.Error(), "cross-host redirect") {
		t.Errorf("error = %q, want cross-host redirect", err)
	}
}

func TestCheckSameHostRedirect_LoopCapped(t *testing.T) {
	orig := &http.Request{URL: mustURL(t, "https://api.example.com/a")}
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = orig
	}
	next := &http.Request{URL: mustURL(t, "https://api.example.com/b")}
	err := CheckSameHostRedirect(next, via)
	if err == nil || !strings.Contains(err.Error(), "10 redirects") {
		t.Fatalf("error = %v, want stopped after 10 redirects", err)
	}
}
