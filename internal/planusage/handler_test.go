package planusage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandlerAuth(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "sekret")
	c := NewCache()
	c.Store("k", Snapshot{Provider: "opencode-go", FetchedAt: time.Now()})
	h := NewHandler(c, func() bool { return true })

	for _, addr := range []string{"127.0.0.1:1", "10.1.2.3:9"} {
		req := httptest.NewRequest(http.MethodGet, "/admin/quota", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s no token: %d", addr, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/quota", nil)
	req.RemoteAddr = "10.1.2.3:9"
	req.Header.Set("Authorization", "Bearer sekret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with token: %d %s", rec.Code, rec.Body.String())
	}
	var body Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Providers) != 1 {
		t.Fatalf("providers = %d", len(body.Providers))
	}
}

func TestHandlerUnavailable(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "")
	h := NewHandler(nil, func() bool { return false })
	req := httptest.NewRequest(http.MethodGet, "/admin/quota", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "quota_unavailable") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestCacheStoreFailedKeepsWindows(t *testing.T) {
	c := NewCache()
	c.Store("fp", Snapshot{
		Provider: "opencode-go",
		Windows:  []Window{{Name: "rolling", Percent: 10, Status: "ok"}},
	})
	c.StoreFailed("fp", "opencode-go", []string{"a"}, "fetch_failed")
	got := c.List()
	if len(got) != 1 || !got[0].Stale || got[0].Windows[0].Percent != 10 {
		t.Fatalf("got %+v", got)
	}
}

func TestCacheStoreFailedAuthClearsWindows(t *testing.T) {
	for _, code := range []string{"unauthorized", "no_subscription"} {
		c := NewCache()
		c.Store("fp", Snapshot{
			Provider: "opencode-go",
			Windows:  []Window{{Name: "rolling", Percent: 10, Status: "ok"}},
		})
		c.StoreFailed("fp", "opencode-go", []string{"a"}, code)
		got := c.List()
		if len(got) != 1 {
			t.Fatalf("%s: list = %d", code, len(got))
		}
		if got[0].Stale || len(got[0].Windows) != 0 || got[0].Err != code {
			t.Fatalf("%s: got %+v", code, got[0])
		}
	}
}

func TestHandlerFetchedAtUsesOldestSnapshot(t *testing.T) {
	t.Setenv("PRISM_ADMIN_TOKEN", "")
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	newer := old.Add(time.Hour)
	c := NewCache()
	c.Store("a", Snapshot{Provider: "opencode-go", FetchedAt: newer})
	c.Store("b", Snapshot{Provider: "opencode-go", FetchedAt: old})
	h := NewHandler(c, func() bool { return true })
	req := httptest.NewRequest(http.MethodGet, "/admin/quota", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	before := time.Now().UTC()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d %s", rec.Code, rec.Body.String())
	}
	var body Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.FetchedAt.Equal(old) {
		t.Fatalf("fetched_at = %v, want oldest %v (not request time %v)", body.FetchedAt, old, before)
	}
}

func TestWriteJSONUsesOldestFetchedAt(t *testing.T) {
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var buf strings.Builder
	if err := WriteJSON(&buf, []Snapshot{
		{Provider: "p", FetchedAt: old.Add(time.Hour)},
		{Provider: "q", FetchedAt: old},
	}); err != nil {
		t.Fatal(err)
	}
	var body Response
	if err := json.Unmarshal([]byte(buf.String()), &body); err != nil {
		t.Fatal(err)
	}
	if !body.FetchedAt.Equal(old) {
		t.Fatalf("fetched_at = %v, want %v", body.FetchedAt, old)
	}
}
