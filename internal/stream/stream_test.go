package stream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testResponseWriter wraps httptest.ResponseRecorder and implements http.Flusher.
type testResponseWriter struct {
	*httptest.ResponseRecorder
	flushed int
}

func (w *testResponseWriter) Flush() {
	w.flushed++
}

func TestFlushWriter_Write(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := &testResponseWriter{ResponseRecorder: rec}

	fw := &flushWriter{w: tw, f: tw}

	// Single write
	n, err := fw.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != 5 {
		t.Errorf("Write() n = %d, want 5", n)
	}
	if tw.flushed != 1 {
		t.Errorf("expected 1 flush after Write, got %d", tw.flushed)
	}
	if rec.Body.String() != "hello" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "hello")
	}
}

func TestFlushWriter_MultipleWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := &testResponseWriter{ResponseRecorder: rec}

	fw := &flushWriter{w: tw, f: tw}

	parts := []string{"Hello", " ", "World", "!"}
	for _, p := range parts {
		_, err := fw.Write([]byte(p))
		if err != nil {
			t.Fatalf("Write(%q) failed: %v", p, err)
		}
	}

	if tw.flushed != len(parts) {
		t.Errorf("expected %d flushes, got %d", len(parts), tw.flushed)
	}
	if rec.Body.String() != "Hello World!" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "Hello World!")
	}
}

func TestStreamResponseBody_Normal(t *testing.T) {
	body := "data: {\"text\":\"hello world\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(body))
	}))
	defer upstream.Close()

	// Use http.NewRequest for client requests (not httptest.NewRequest)
	req, _ := http.NewRequest("GET", upstream.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upstream request failed: %v", err)
	}
	defer resp.Body.Close()

	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest("POST", "/", nil)

	n, err := StreamResponseBody(rec, resp.Body, clientReq, "test-account")
	if err != nil {
		t.Fatalf("StreamResponseBody failed: %v", err)
	}

	if n != int64(len(body)) {
		t.Errorf("written bytes = %d, want %d", n, len(body))
	}
	if rec.Body.String() != body {
		t.Errorf("body = %q, want %q", rec.Body.String(), body)
	}
}

// failResponseWriter succeeds for the first failAfter bytes, then returns an error
// on every subsequent Write (simulating a client disconnect).
type failResponseWriter struct {
	*httptest.ResponseRecorder
	failAfter int
	written   int
}

func (w *failResponseWriter) Write(p []byte) (int, error) {
	if w.written >= w.failAfter {
		return 0, errors.New("client disconnected")
	}
	avail := w.failAfter - w.written
	n := len(p)
	if n > avail {
		n = avail
	}
	nw, _ := w.ResponseRecorder.Write(p[:n])
	w.written += nw
	if nw < len(p) {
		return nw, errors.New("client disconnected")
	}
	return nw, nil
}

func TestStreamResponseBody_ClientDisconnect(t *testing.T) {
	ctx := context.Background()
	clientReq := httptest.NewRequest("POST", "/", nil).WithContext(ctx)

	// Use an io.Pipe as the upstream body so we can verify the drain goroutine
	// actually empties the pipe after the downstream write fails.
	pr, pw := io.Pipe()

	// failAfter is set to exactly the length of the first chunk so that the
	// first Write succeeds cleanly and the second Write returns an error.
	rec := &failResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		failAfter:        len("chunk1\n"),
	}

	// The pipe writer goroutine is synchronised with the reader: each Write
	// blocks until the data has been consumed.  When io.Copy in
	// StreamResponseBody hits the Write error it will call
	// io.Copy(io.Discard, body) to drain the remaining data, which unblocks
	// the next pipe Write.  We use a done channel to prove drain happened.
	done := make(chan struct{})
	go func() {
		defer pw.Close()
		defer close(done)
		pw.Write([]byte("chunk1\n"))
		pw.Write([]byte("chunk2\n"))
		pw.Write([]byte("chunk3_after_disconnect\n"))
	}()

	_, err := StreamResponseBody(rec, pr, clientReq, "test-account")
	if err == nil {
		t.Error("expected error after writer failure, got nil")
	}

	if rec.Body.String() != "chunk1\n" {
		t.Errorf("expected 'chunk1\\n' in body, got %q", rec.Body.String())
	}

	// Prove the drain branch ran: if io.Copy(io.Discard, body) does not drain,
	// the pipe writer blocks forever on its second Write and the done channel
	// never closes.
	select {
	case <-done:
		// Drain succeeded — pipe writer finished.
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for drain: io.Copy(io.Discard, body) did not drain the upstream body")
	}
}

func TestStreamResponseBody_LargeBody(t *testing.T) {
	// Build a large body
	largeChunk := strings.Repeat("x", 64*1024) // 64KB chunk
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		// Write 10 chunks = 640KB
		for i := 0; i < 10; i++ {
			w.Write([]byte(largeChunk))
		}
	}))
	defer upstream.Close()

	req, _ := http.NewRequest("GET", upstream.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upstream request failed: %v", err)
	}
	defer resp.Body.Close()

	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest("POST", "/", nil)

	n, err := StreamResponseBody(rec, resp.Body, clientReq, "test-account")
	if err != nil {
		t.Fatalf("StreamResponseBody failed: %v", err)
	}

	expectedLen := int64(len(largeChunk) * 10)
	if n != expectedLen {
		t.Errorf("written bytes = %d, want %d", n, expectedLen)
	}
}
