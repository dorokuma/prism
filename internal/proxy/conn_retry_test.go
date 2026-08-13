package proxy

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
)

func startResetListener(t *testing.T) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 4096)
			_, _ = c.Read(buf)
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), func() {
		close(done)
		_ = ln.Close()
	}
}

func TestDoUpstreamRequest_ResetNotRetryable(t *testing.T) {
	addr, closeFn := startResetListener(t)
	defer closeFn()

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "rst", Key: "k", BaseURL: "http://" + addr, Provider: "p"},
	}}
	p := pool.NewPool(cfg.Accounts)
	acc := p.AllAccounts()[0]
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("X-Prism-Provider", "p")

	res := doUpstreamRequest(acc, r, body, ChatForwardOpts{Model: "gpt-4"}, "req-rst-1")
	if res.retry {
		t.Fatal("connection reset must not be retryable (request may have reached upstream)")
	}
	if res.fatalErr == nil {
		t.Fatal("non-retryable connection error must set fatalErr")
	}
}

func TestProxyChat_ConnRefusedFailsOver(t *testing.T) {
	restoreCooldown := SetUpstreamCooldownForTest(5 * time.Second)
	defer restoreCooldown()
	restoreSelect := SetAccountSelectTimeoutForTest(500 * time.Millisecond)
	defer restoreSelect()

	var secondHits int32
	okUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer okUp.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "dead", Key: "k1", BaseURL: "http://127.0.0.1:1", Provider: "p"},
		{Name: "ok", Key: "k2", BaseURL: okUp.URL, Provider: "p"},
	}}
	p := pool.NewPool(cfg.Accounts)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"gpt-4"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "p")

	proxyChatWithBody(p, rec, r, body, time.Now(), ChatForwardOpts{Model: "gpt-4"}, cfg)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after refused failover; body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&secondHits) < 1 {
		t.Fatal("connection refused must still switch to the second account")
	}
}

func TestProxyChat_ConnResetDoesNotFailOver(t *testing.T) {
	restoreCooldown := SetUpstreamCooldownForTest(10 * time.Millisecond)
	defer restoreCooldown()
	restoreSelect := SetAccountSelectTimeoutForTest(200 * time.Millisecond)
	defer restoreSelect()

	addr, closeFn := startResetListener(t)
	defer closeFn()

	var secondHits int32
	okUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer okUp.Close()

	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "rst", Key: "k1", BaseURL: "http://" + addr, Provider: "p"},
		{Name: "ok", Key: "k2", BaseURL: okUp.URL, Provider: "p"},
	}}
	p := pool.NewPool(cfg.Accounts)
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"gpt-4"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Prism-Provider", "p")

	proxyChatWithBody(p, rec, r, body, time.Now(), ChatForwardOpts{Model: "gpt-4"}, cfg)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (reset must not switch accounts); body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Fatal("non-retryable connection error must write a 502 JSON body, not an empty response")
	}
	if atomic.LoadInt32(&secondHits) != 0 {
		t.Fatal("connection reset must not switch to the second account")
	}
}
