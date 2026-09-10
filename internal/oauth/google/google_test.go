package google

import (
	"context"
	"encoding/json"
	"errors"
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
	// Real expiry is expires_in+now (3600s), not reduced by RefreshSkew.
	if tok.ExpiresAt.Before(time.Now().Add(55 * time.Minute)) {
		t.Fatalf("expiry too soon (skewed?): %s", tok.ExpiresAt)
	}
	if tok.ExpiresAt.After(time.Now().Add(65 * time.Minute)) {
		t.Fatalf("expiry too late: %s", tok.ExpiresAt)
	}
}

func TestRefreshSkewIs5Minutes(t *testing.T) {
	if RefreshSkew != 5*time.Minute {
		t.Fatalf("RefreshSkew = %s, want 5m", RefreshSkew)
	}
}

func TestSkewedExpiry(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	real := now.Add(time.Hour)
	got := SkewedExpiry(real)
	want := now.Add(55 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("SkewedExpiry = %s, want %s", got, want)
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

func TestStoreAgyTokenRoundTripPreservesEnvelope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "antigravity-oauth-token")
	exp := time.Date(2026, 9, 2, 0, 44, 27, 0, time.UTC)
	body, err := json.Marshal(map[string]any{
		"auth_method": "consumer",
		"id_token":    "keep-this-id-token",
		"token": map[string]any{
			"access_token":  "old-acc",
			"refresh_token": "old-ref",
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
	nextExp := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	if err := StoreAgyToken(path, Tokens{
		Access:    "new-acc",
		Refresh:   "new-ref",
		ExpiresAt: nextExp,
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["auth_method"] != "consumer" {
		t.Fatalf("auth_method = %v", envelope["auth_method"])
	}
	if envelope["id_token"] != "keep-this-id-token" {
		t.Fatalf("id_token = %v", envelope["id_token"])
	}
	nested, ok := envelope["token"].(map[string]any)
	if !ok {
		t.Fatalf("token nested missing: %s", raw)
	}
	if nested["access_token"] != "new-acc" || nested["refresh_token"] != "new-ref" {
		t.Fatalf("token = %+v", nested)
	}
	if nested["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v", nested["token_type"])
	}
	loaded, err := LoadAgyToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Access != "new-acc" || loaded.Refresh != "new-ref" {
		t.Fatalf("loaded=%+v", loaded)
	}
	if loaded.ExpiresAt.IsZero() {
		t.Fatal("expiry missing after store")
	}
}

func TestStoreAgyTokenKeepsOldRefreshWhenOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "antigravity-oauth-token")
	body, err := json.Marshal(map[string]any{
		"auth_method": "consumer",
		"id_token":    "id",
		"token": map[string]any{
			"access_token":  "acc",
			"refresh_token": "keep-ref",
			"token_type":    "Bearer",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := StoreAgyToken(path, Tokens{Access: "acc2"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAgyToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Refresh != "keep-ref" || loaded.Access != "acc2" {
		t.Fatalf("loaded=%+v", loaded)
	}
}

func TestStoreAgyTokenUnwritable(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "notdir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := StoreAgyToken(filepath.Join(parent, "antigravity-oauth-token"), Tokens{
		Access:  "acc",
		Refresh: "ref",
	})
	if err == nil {
		t.Fatal("expected error on non-directory parent")
	}
}

func TestStoreAgyTokenRefusesInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "antigravity-oauth-token")
	orig := []byte("not-json{")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	err := StoreAgyToken(path, Tokens{Access: "acc", Refresh: "ref", ExpiresAt: time.Now().Add(time.Hour)})
	if !errors.Is(err, ErrAgyInvalidJSON) {
		t.Fatalf("err = %v, want ErrAgyInvalidJSON", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(orig) {
		t.Fatalf("invalid JSON file was overwritten: %q", after)
	}
}

func TestStoreAgyTokenWritesRealExpiry(t *testing.T) {
	t.Setenv(ClientSecretEnv, "test-client-secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"new-acc","expires_in":3600}`)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "antigravity-oauth-token")
	body, err := json.Marshal(map[string]any{
		"auth_method": "consumer",
		"token": map[string]any{
			"access_token":  "old-acc",
			"refresh_token": "old-ref",
			"token_type":    "Bearer",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	tok, err := Refresh(context.Background(), Config{HTTP: srv.Client(), TokenURL: srv.URL}, "old-ref")
	if err != nil {
		t.Fatal(err)
	}
	if err := StoreAgyToken(path, tok); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAgyToken(path)
	if err != nil {
		t.Fatal(err)
	}
	// Real expiry is ~3600s from before; RefreshSkew (5m) must not be subtracted.
	minExp := before.Add(55 * time.Minute)
	maxExp := before.Add(65 * time.Minute)
	if loaded.ExpiresAt.Before(minExp) {
		t.Fatalf("agy expiry %s is skewed (before %s)", loaded.ExpiresAt, minExp)
	}
	if loaded.ExpiresAt.After(maxExp) {
		t.Fatalf("agy expiry %s too late (after %s)", loaded.ExpiresAt, maxExp)
	}
}
