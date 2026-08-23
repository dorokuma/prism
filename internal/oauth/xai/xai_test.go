package xai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRequestDeviceSuccess(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/device/code" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(body))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev-1",
			"user_code":        "ABCD-EFGH",
			"verification_uri": "https://auth.x.ai/activate",
			"interval":         1,
			"expires_in":       600,
		})
	}))
	defer srv.Close()

	d, err := RequestDevice(context.Background(), Config{
		HTTP:      srv.Client(),
		DeviceURL: srv.URL + "/oauth2/device/code",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.DeviceCode != "dev-1" || d.UserCode != "ABCD-EFGH" {
		t.Fatalf("device = %+v", d)
	}
	if d.VerificationURI != "https://auth.x.ai/activate" {
		t.Errorf("uri = %q", d.VerificationURI)
	}
	if got.Get("client_id") != ClientID {
		t.Errorf("client_id = %q", got.Get("client_id"))
	}
	if got.Get("scope") != Scope {
		t.Errorf("scope = %q", got.Get("scope"))
	}
	if got.Get("referrer") != Referrer {
		t.Errorf("referrer = %q, want prism (must not impersonate pi)", got.Get("referrer"))
	}
}

func TestRequestDeviceRejectsNonHTTPSVerificationURI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev-1",
			"user_code":        "ABCD",
			"verification_uri": "http://evil.example/activate",
			"expires_in":       600,
		})
	}))
	defer srv.Close()
	_, err := RequestDevice(context.Background(), Config{HTTP: srv.Client(), DeviceURL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "untrusted verification URI") {
		t.Fatalf("err = %v", err)
	}
}

func TestPollTokenPendingThenOK(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "acc-1",
			"refresh_token": "ref-1",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	tok, err := PollToken(context.Background(), Config{
		HTTP:     srv.Client(),
		TokenURL: srv.URL,
	}, Device{DeviceCode: "dev-1", Interval: 20 * time.Millisecond, ExpiresIn: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "acc-1" || tok.Refresh != "ref-1" {
		t.Fatalf("tokens = %+v", tok)
	}
	if !tok.ExpiresAt.Before(time.Now().Add(3600 * time.Second)) {
		t.Errorf("expires_at should be skewed before full TTL: %s", tok.ExpiresAt)
	}
	if n < 2 {
		t.Fatalf("polls = %d, want at least 2", n)
	}
}

func TestPollTokenDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
	}))
	defer srv.Close()
	_, err := PollToken(context.Background(), Config{HTTP: srv.Client(), TokenURL: srv.URL},
		Device{DeviceCode: "d", Interval: 10 * time.Millisecond, ExpiresIn: time.Second})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshKeepsPreviousRefreshWhenOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "acc-2",
			"expires_in":   120,
		})
	}))
	defer srv.Close()
	tok, err := Refresh(context.Background(), Config{HTTP: srv.Client(), TokenURL: srv.URL, RefreshSkew: time.Second}, "ref-old")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "acc-2" || tok.Refresh != "ref-old" {
		t.Fatalf("tokens = %+v", tok)
	}
}

func TestRefreshRotatesRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "acc-3",
			"refresh_token": "ref-new",
			"expires_in":    120,
		})
	}))
	defer srv.Close()
	tok, err := Refresh(context.Background(), Config{HTTP: srv.Client(), TokenURL: srv.URL, RefreshSkew: time.Second}, "ref-old")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Refresh != "ref-new" {
		t.Fatalf("refresh = %q, want ref-new", tok.Refresh)
	}
}

func TestRefreshEmptyRejected(t *testing.T) {
	_, err := Refresh(context.Background(), Config{}, "")
	if err == nil {
		t.Fatal("expected error")
	}
}
