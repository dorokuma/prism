package google

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefreshOK(t *testing.T) {
	t.Setenv(ClientSecretEnv, "test-client-secret")
	var sawGrant, sawRefresh bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form := string(body)
		sawGrant = strings.Contains(form, "grant_type=refresh_token")
		sawRefresh = strings.Contains(form, "refresh_token=old-refresh")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"new-access","expires_in":3600}`)
	}))
	defer srv.Close()

	tok, err := Refresh(context.Background(), Config{HTTP: srv.Client(), TokenURL: srv.URL}, "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if !sawGrant || !sawRefresh {
		t.Fatalf("form grant=%v refresh=%v", sawGrant, sawRefresh)
	}
	if tok.Access != "new-access" || tok.Refresh != "old-refresh" {
		t.Fatalf("tok=%+v", tok)
	}
	if tok.ExpiresAt.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("expiry too soon: %s", tok.ExpiresAt)
	}
}

func TestRefreshKeepsRotatedRefresh(t *testing.T) {
	t.Setenv(ClientSecretEnv, "test-client-secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"a","refresh_token":"rotated","expires_in":3600}`)
	}))
	defer srv.Close()
	tok, err := Refresh(context.Background(), Config{HTTP: srv.Client(), TokenURL: srv.URL}, "old")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Refresh != "rotated" {
		t.Fatalf("refresh = %q", tok.Refresh)
	}
}

func TestRefreshInvalidGrant(t *testing.T) {
	t.Setenv(ClientSecretEnv, "test-client-secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
	}))
	defer srv.Close()
	_, err := Refresh(context.Background(), Config{HTTP: srv.Client(), TokenURL: srv.URL}, "dead")
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshEmpty(t *testing.T) {
	_, err := Refresh(context.Background(), Config{}, "")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadAgyTokenNested(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "antigravity-oauth-token")
	exp := time.Date(2026, 9, 2, 0, 44, 27, 0, time.FixedZone("CST", 8*3600))
	body, err := json.Marshal(map[string]any{
		"auth_method": "consumer",
		"token": map[string]any{
			"access_token":  "acc",
			"refresh_token": "ref",
			"token_type":    "Bearer",
			"expiry":        exp.Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadAgyToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "acc" || tok.Refresh != "ref" {
		t.Fatalf("tok=%+v", tok)
	}
	if tok.ExpiresAt.IsZero() {
		t.Fatal("expiry missing")
	}
}

func TestLoadAgyTokenMissing(t *testing.T) {
	_, err := LoadAgyToken(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected error")
	}
}
