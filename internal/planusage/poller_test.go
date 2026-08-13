package planusage

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollerStopIdempotent(t *testing.T) {
	p := NewPoller(DefaultFetchers(), NewCache(), 30*time.Second, time.Second)
	p.Start()
	p.Stop()
	p.Stop()
}

func TestPollerStopBeforeStart(t *testing.T) {
	p := NewPoller(DefaultFetchers(), NewCache(), 30*time.Second, time.Second)
	p.Stop()
	p.Stop()
}

func TestPollerDisabledSkipsFetch(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, `{"usage":{"rolling":{"status":"ok","percent":1}}}`)
	}))
	defer srv.Close()

	p := NewPoller([]Fetcher{GoFetcher{}}, NewCache(), 30*time.Second, time.Second)
	p.SetAccounts([]AccountView{fakeAcc{
		name: "a", provider: "opencode-go", base: srv.URL + "/v1", key: "k",
		client: srv.Client(),
	}})
	p.SetOptions(false, 30*time.Second, time.Second)
	p.Refresh()
	if hits.Load() != 0 {
		t.Fatalf("disabled poller hit upstream %d times", hits.Load())
	}
}

func TestPollerStopCancelsInFlight(t *testing.T) {
	started := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := NewPoller([]Fetcher{GoFetcher{}}, NewCache(), 30*time.Second, 5*time.Second)
	p.SetAccounts([]AccountView{
		fakeAcc{name: "a", provider: "opencode-go", base: srv.URL + "/v1", key: "k1", client: srv.Client()},
		fakeAcc{name: "b", provider: "opencode-go", base: srv.URL + "/v1", key: "k2", client: srv.Client()},
	})
	done := make(chan struct{})
	go func() {
		p.Refresh()
		close(done)
	}()
	<-started
	<-started
	begin := time.Now()
	p.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Refresh did not abort after Stop")
	}
	if time.Since(begin) > 2*time.Second {
		t.Fatalf("Stop waited %v, want well under 2s", time.Since(begin))
	}
}

func TestErrorCode(t *testing.T) {
	if ErrorCode(ErrUnauthorized) != "unauthorized" {
		t.Fatal(ErrorCode(ErrUnauthorized))
	}
	if ErrorCode(io.EOF) != "fetch_failed" {
		t.Fatal(ErrorCode(io.EOF))
	}
}
