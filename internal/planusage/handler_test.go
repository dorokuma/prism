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
