package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/cache"
)

func TestRunModels_HTTPMode_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prism/v1/models/refresh" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		resp := cache.RefreshResponse{
			Status:   "accepted",
			Provider: r.URL.Query().Get("provider"),
			Providers: map[string]cache.ProviderSnapshot{
				"p1": {ModelsCount: 5},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// 1. Text output
	err := runModels([]string{"refresh", "--url", srv.URL})
	if err != nil {
		t.Fatalf("runModels failed: %v", err)
	}

	// 2. JSON output with provider filter
	err = runModels([]string{"refresh", "--url", srv.URL, "--provider", "p1", "--json"})
	if err != nil {
		t.Fatalf("runModels with json failed: %v", err)
	}
}

func TestRunModels_HTTPMode_StatusReadOnly(t *testing.T) {
	var receivedMethod string
	var receivedProvider string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prism/v1/models/refresh" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		receivedMethod = r.Method
		receivedProvider = r.URL.Query().Get("provider")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		resp := cache.RefreshResponse{
			Status:   "ok",
			Provider: receivedProvider,
			Providers: map[string]cache.ProviderSnapshot{
				"p1": {ModelsCount: 3},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// 1. Text status query
	err := runModels([]string{"status", "--url", srv.URL})
	if err != nil {
		t.Fatalf("runModels status failed: %v", err)
	}
	if receivedMethod != http.MethodGet {
		t.Fatalf("expected GET request for status, got %s", receivedMethod)
	}

	// 2. JSON status query with provider filter
	err = runModels([]string{"status", "--url", srv.URL, "--provider", "p1", "--json"})
	if err != nil {
		t.Fatalf("runModels status json failed: %v", err)
	}
	if receivedProvider != "p1" {
		t.Fatalf("expected provider p1, got %q", receivedProvider)
	}
}

func TestRunModels_CLIValidation(t *testing.T) {
	// Unknown subcommand
	err := runModels([]string{"unknown_subcmd"})
	if err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("expected unknown subcommand error, got %v", err)
	}

	// Rejected --wait flag
	err = runModels([]string{"refresh", "--wait"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -wait") {
		t.Fatalf("expected -wait flag rejected, got %v", err)
	}

	// Extra positional args
	err = runModels([]string{"refresh", "--provider", "p1", "extra"})
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("expected unexpected arguments error, got %v", err)
	}
}

func TestRunModels_HTTPMode_Errors(t *testing.T) {
	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv401.Close()

	err := runModels([]string{"refresh", "--url", srv401.URL})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
	}

	srv429 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv429.Close()

	err = runModels([]string{"refresh", "--url", srv429.URL})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected 429 error, got %v", err)
	}

	srv400 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"unknown provider"}`))
	}))
	defer srv400.Close()

	err = runModels([]string{"refresh", "--url", srv400.URL, "--provider", "bad"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400 error, got %v", err)
	}
}

func TestRunModels_DirectMode(t *testing.T) {
	oldDir := defaultModelCacheDir
	tempCacheDir := filepath.Join(t.TempDir(), "model_cache")
	defaultModelCacheDir = tempCacheDir
	defer func() { defaultModelCacheDir = oldDir }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
	}))
	defer upstream.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgContent := `
listen: "127.0.0.1:18790"
accounts:
  - name: a1
    provider: test_prov
    base_url: ` + upstream.URL + `
    key: k1
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Direct mode all providers
	err := runModels([]string{"--direct", "--config", cfgPath})
	if err != nil {
		t.Fatalf("direct refresh failed: %v", err)
	}

	// 2. Direct mode single provider JSON
	err = runModels([]string{"refresh", "--direct", "--config", cfgPath, "--provider", "test_prov", "--json"})
	if err != nil {
		t.Fatalf("direct refresh single provider failed: %v", err)
	}

	// 3. Direct mode status (read-only)
	err = runModels([]string{"status", "--direct", "--config", cfgPath, "--provider", "test_prov", "--json"})
	if err != nil {
		t.Fatalf("direct status failed: %v", err)
	}

	// 4. Direct mode unknown provider
	err = runModels([]string{"--direct", "--config", cfgPath, "--provider", "nonexistent"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestRunModels_DirectMode_FailFast(t *testing.T) {
	oldDir := defaultModelCacheDir
	// Point to a file so MkdirAll fails
	filePath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	invalidDir := filepath.Join(filePath, "child_cache")
	defaultModelCacheDir = invalidDir
	defer func() { defaultModelCacheDir = oldDir }()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgContent := `
listen: "127.0.0.1:18790"
accounts:
  - name: a1
    provider: test_prov
    base_url: http://127.0.0.1:18790
    key: k1
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}

	err := runModels([]string{"--direct", "--config", cfgPath})
	if err == nil || !strings.Contains(err.Error(), "create cache dir") {
		t.Fatalf("expected fail-fast error creating cache dir, got %v", err)
	}
}

func TestResolveBaseURLFromConfig(t *testing.T) {
	cases := []struct {
		listen   string
		tls      bool
		expected string
	}{
		{"0.0.0.0:8080", false, "http://127.0.0.1:8080"},
		{":8080", false, "http://127.0.0.1:8080"},
		{"192.168.1.5:9000", true, "https://192.168.1.5:9000"},
		{"[::1]:18790", false, "http://[::1]:18790"},
		{"::1:18790", false, "http://[::1]:18790"},
		{"[2001:db8::1]:8080", false, "http://[2001:db8::1]:8080"},
		{"::18790", false, "http://127.0.0.1:18790"},
	}

	for _, tc := range cases {
		cfgPath := filepath.Join(t.TempDir(), "config.yaml")
		content := "listen: '" + tc.listen + "'\nauth_token: test-token\nallow_insecure_http: true\naccounts:\n  - name: a\n    provider: p\n    key: k\n    base_url: http://example.com\n"
		if tc.tls {
			content = "listen: '" + tc.listen + "'\nauth_token: test-token\ntls_cert_file: 'a'\ntls_key_file: 'b'\naccounts:\n  - name: a\n    provider: p\n    key: k\n    base_url: http://example.com\n"
		}
		_ = os.WriteFile(cfgPath, []byte(content), 0600)
		got := resolveBaseURLFromConfig(cfgPath)
		if got != tc.expected {
			t.Errorf("resolveBaseURLFromConfig(%q, tls=%v) = %q, want %q", tc.listen, tc.tls, got, tc.expected)
		}
	}
}

func TestRunModels_QueryEscaping(t *testing.T) {
	var receivedRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedRawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","providers":{}}`))
	}))
	defer srv.Close()

	err := runModels([]string{"status", "--url", srv.URL, "--provider", "p1&special=true"})
	if err != nil {
		t.Fatalf("runModels status failed: %v", err)
	}
	if receivedRawQuery != "provider=p1%26special%3Dtrue" {
		t.Fatalf("expected raw query 'provider=p1%%26special%%3Dtrue', got %q", receivedRawQuery)
	}
}

func TestRunModels_DirectMode_RefreshAll_FailureReturnsError(t *testing.T) {
	oldDir := defaultModelCacheDir
	tempCacheDir := filepath.Join(t.TempDir(), "model_cache")
	defaultModelCacheDir = tempCacheDir
	defer func() { defaultModelCacheDir = oldDir }()

	// Server that fails with 500
	upstreamFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal upstream error"}`))
	}))
	defer upstreamFail.Close()

	// Server that succeeds
	upstreamOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
	}))
	defer upstreamOK.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgContent := `
listen: "127.0.0.1:18790"
accounts:
  - name: a1
    provider: prov_ok
    base_url: ` + upstreamOK.URL + `
    key: k1
  - name: a2
    provider: prov_fail
    base_url: ` + upstreamFail.URL + `
    key: k2
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Full refresh with failing provider must return non-nil error
	err := runModels([]string{"--direct", "--config", cfgPath})
	if err == nil {
		t.Fatal("expected direct full refresh with failing provider to return error, got nil")
	}
	if !strings.Contains(err.Error(), "prov_fail") {
		t.Fatalf("expected error to mention failed provider prov_fail, got: %v", err)
	}

	// 2. Full refresh in JSON mode with failing provider must return non-nil error
	err = runModels([]string{"refresh", "--direct", "--config", cfgPath, "--json"})
	if err == nil {
		t.Fatal("expected direct full refresh in JSON mode to return error, got nil")
	}
	if !strings.Contains(err.Error(), "prov_fail") {
		t.Fatalf("expected JSON error to mention failed provider prov_fail, got: %v", err)
	}
}

func TestPrintSnapshotTable(t *testing.T) {
	tm := time.Date(2026, 9, 1, 8, 51, 23, 0, time.Local)
	snaps := map[string]cache.ProviderSnapshot{
		"agentrouter": {ModelsCount: 15, UpdatedAt: &tm},
		"x-api":       {ModelsCount: 3, UpdatedAt: nil},
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	printSnapshotTable(snaps)
	w.Close()
	os.Stdout = oldStdout

	var buf strings.Builder
	_, _ = io.Copy(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "供应商") || !strings.Contains(out, "模型") || !strings.Contains(out, "更新时间") {
		t.Errorf("missing expected Chinese headers, got:\n%s", out)
	}
	if strings.Contains(out, "BACKOFF") || strings.Contains(out, "PROVIDER") {
		t.Errorf("old headers should not appear, got:\n%s", out)
	}
	if !strings.Contains(out, "09-01 08:51") {
		t.Errorf("expected short time format '09-01 08:51', got:\n%s", out)
	}
	if strings.Contains(out, "2026-09-01") {
		t.Errorf("year should not appear in short time format, got:\n%s", out)
	}
}

func TestPrintSnapshotTable_Empty(t *testing.T) {
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	printSnapshotTable(nil)
	w.Close()
	os.Stdout = oldStdout

	var buf strings.Builder
	_, _ = io.Copy(&buf, r)
	out := buf.String()

	if strings.TrimSpace(out) != "无供应商" {
		t.Errorf("expected '无供应商', got %q", out)
	}
}
