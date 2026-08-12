package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dorokuma/prism/internal/util"
)

// okHandler records that it was reached and answers 200.
type okHandler struct {
	called bool
	req    *http.Request
}

func (h *okHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called = true
	h.req = r
	w.WriteHeader(http.StatusOK)
}

func TestRequestID_ValidHeaderPassesThrough(t *testing.T) {
	h := &okHandler{}
	mid := RequestIDMiddleware(h)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "req-123-abc")
	rr := httptest.NewRecorder()
	mid.ServeHTTP(rr, req)

	if !h.called {
		t.Fatal("handler must be reached for a valid X-Request-ID")
	}
	if got := rr.Header().Get("X-Request-ID"); got != "req-123-abc" {
		t.Errorf("response X-Request-ID = %q, want the client value", got)
	}
	if got := util.RequestIDFromCtx(h.req.Context()); got != "req-123-abc" {
		t.Errorf("context request ID = %q, want the client value", got)
	}
}

func TestRequestID_MissingHeaderGeneratesRandomID(t *testing.T) {
	h := &okHandler{}
	mid := RequestIDMiddleware(h)

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	mid.ServeHTTP(rr, req)

	if !h.called {
		t.Fatal("handler must be reached when X-Request-ID is absent")
	}
	got := rr.Header().Get("X-Request-ID")
	if got == "" {
		t.Fatal("a request ID must be generated and set on the response")
	}
}

func TestRequestID_ControlCharactersRejected(t *testing.T) {
	for name, rid := range map[string]string{
		"newline":       "abc\ndef",
		"carriage":      "abc\rdef",
		"tab":           "abc\tdef",
		"esc":           "abc\x1bdef",
		"c1 control":    "abc\x80def",
		"del":           "abc\x7fdef",
		"nul":           "abc\x00def",
		"trailing crlf": "abc\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := &okHandler{}
			mid := RequestIDMiddleware(h)
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("X-Request-ID", rid)
			rr := httptest.NewRecorder()
			mid.ServeHTTP(rr, req)

			if h.called {
				t.Fatal("handler must NOT be reached for a control-character request ID")
			}
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), "invalid_request_id") {
				t.Errorf("body = %q, want the invalid_request_id code", rr.Body.String())
			}
			// The raw ID must never be echoed back in the response header.
			if got := rr.Header().Get("X-Request-ID"); got != "" {
				t.Errorf("response X-Request-ID must be empty for a rejected ID, got %q", got)
			}
		})
	}
}

func TestRequestID_TooLongRejected(t *testing.T) {
	h := &okHandler{}
	mid := RequestIDMiddleware(h)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", strings.Repeat("a", maxRequestIDLen+1))
	rr := httptest.NewRecorder()
	mid.ServeHTTP(rr, req)

	if h.called {
		t.Fatal("handler must NOT be reached for an over-long request ID")
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestRequestID_MaxLengthAccepted(t *testing.T) {
	h := &okHandler{}
	mid := RequestIDMiddleware(h)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", strings.Repeat("a", maxRequestIDLen))
	rr := httptest.NewRecorder()
	mid.ServeHTTP(rr, req)

	if !h.called {
		t.Fatal("handler must be reached for an ID of exactly the max length")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestRequestID_ValidUnicodeAccepted(t *testing.T) {
	// Non-control Unicode (multi-byte) is safe in headers/logs/database and
	// must pass; the byte length cap still applies.
	h := &okHandler{}
	mid := RequestIDMiddleware(h)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "req-\u4e2d\u6587-1")
	rr := httptest.NewRecorder()
	mid.ServeHTTP(rr, req)

	if !h.called {
		t.Fatal("handler must be reached for a non-control Unicode request ID")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}
