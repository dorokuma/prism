package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

const chatSSEHello = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: [DONE]\n\n"

func TestProxyChat_EmptyStreamDoesNotFailover(t *testing.T) {
	restoreCooldown := SetUpstreamCooldownForTest(5 * time.Second)
	defer restoreCooldown()

	var firstHits, secondHits int32
	emptyUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&firstHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer emptyUp.Close()
	okUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, chatSSEHello)
	}))
	defer okUp.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "empty", Key: "k1", BaseURL: emptyUp.URL, Provider: "p"},
		{Name: "ok", Key: "k2", BaseURL: okUp.URL, Provider: "p"},
	}}
	p := pool.NewPool(cfg.Accounts)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"gpt-4","stream":true}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "p")

	proxyChatWithBody(p, rec, r, body, time.Now(), ChatForwardOpts{Stream: true, Model: "gpt-4"}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "upstream_stream_error" {
		t.Errorf("error code = %q, want upstream_stream_error", code)
	}
	if atomic.LoadInt32(&firstHits) != 1 {
		t.Errorf("first account hits = %d, want 1", firstHits)
	}
	if atomic.LoadInt32(&secondHits) != 0 {
		t.Errorf("second account hits = %d, want 0", secondHits)
	}
}

func TestProxyChat_EmptyStreamSingleAccount502(t *testing.T) {
	restoreCooldown := SetUpstreamCooldownForTest(5 * time.Second)
	defer restoreCooldown()

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "empty", Key: "k1", BaseURL: upstream.URL, Provider: "p"},
	}}
	p := pool.NewPool(cfg.Accounts)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"gpt-4","stream":true}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "p")

	proxyChatWithBody(p, rec, r, body, time.Now(), ChatForwardOpts{Stream: true, Model: "gpt-4"}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "upstream_stream_error" {
		t.Errorf("error code = %q, want upstream_stream_error (not all_exhausted)", code)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("upstream hits = %d, want 1", hits)
	}
}

func TestProxyResponses_EmptyStreamDoesNotFailover(t *testing.T) {
	restoreCooldown := SetUpstreamCooldownForTest(5 * time.Second)
	defer restoreCooldown()

	var firstHits, secondHits int32
	emptyUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&firstHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer emptyUp.Close()
	okUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, chatSSEHello)
	}))
	defer okUp.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "empty", Key: "k1", BaseURL: emptyUp.URL, Provider: "p"},
		{Name: "ok", Key: "k2", BaseURL: okUp.URL, Provider: "p"},
	}}
	p := pool.NewPool(cfg.Accounts)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"gpt-4","stream":true}`)
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "p")

	proxyChatWithBody(p, rec, r, body, time.Now(), ChatForwardOpts{
		ResponsesOut: true, Stream: true, Model: "gpt-4",
	}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec.Body.String()); code != "upstream_stream_error" {
		t.Errorf("error code = %q, want upstream_stream_error", code)
	}
	if atomic.LoadInt32(&firstHits) != 1 {
		t.Errorf("first account hits = %d, want 1", firstHits)
	}
	if atomic.LoadInt32(&secondHits) != 0 {
		t.Errorf("second account hits = %d, want 0", secondHits)
	}
}
