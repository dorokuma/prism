package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

const chatSSEHello = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: [DONE]\n\n"

func TestProxyChat_EmptyStreamFailsOverToSecondAccount(t *testing.T) {
	// Keep the empty account in cooldown across UpstreamRetryDelay (200ms)
	// so the second select must pick the healthy sibling.
	restoreCooldown := SetUpstreamCooldownForTest(5 * time.Second)
	defer restoreCooldown()
	restoreSelect := SetAccountSelectTimeoutForTest(500 * time.Millisecond)
	defer restoreSelect()

	var secondHits int32
	emptyUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover; body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&secondHits) < 1 {
		t.Fatal("second account must serve the request after the first empty stream")
	}
	if !strings.Contains(rec.Body.String(), "Hello") {
		t.Errorf("body = %q, want Hello from the second account", rec.Body.String())
	}
}

func TestProxyChat_EmptyStreamSingleAccount502(t *testing.T) {
	restoreCooldown := SetUpstreamCooldownForTest(10 * time.Millisecond)
	defer restoreCooldown()
	restoreSelect := SetAccountSelectTimeoutForTest(200 * time.Millisecond)
	defer restoreSelect()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
}

func TestProxyResponses_EmptyStreamFailsOverToSecondAccount(t *testing.T) {
	restoreCooldown := SetUpstreamCooldownForTest(5 * time.Second)
	defer restoreCooldown()
	restoreSelect := SetAccountSelectTimeoutForTest(500 * time.Millisecond)
	defer restoreSelect()

	var secondHits int32
	emptyUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover; body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&secondHits) < 1 {
		t.Fatal("second account must serve the Responses stream after the first empty stream")
	}
	if rec.Body.Len() == 0 {
		t.Fatal("client must receive translated stream events from the second account")
	}
}
