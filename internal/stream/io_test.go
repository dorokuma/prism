package stream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dorokuma/prism/internal/middleware"
)

type failAfterFirstChunkWriter struct {
	header http.Header
	writes int
}

func (f *failAfterFirstChunkWriter) Header() http.Header {
	if f.header == nil {
		f.header = make(http.Header)
	}
	return f.header
}

func (f *failAfterFirstChunkWriter) Write(p []byte) (int, error) {
	f.writes++
	if f.writes > 1 {
		return 0, errors.New("client connection broken")
	}
	return len(p), nil
}

func (f *failAfterFirstChunkWriter) WriteHeader(statusCode int) {}

type chunkReader struct {
	chunks [][]byte
	idx    int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.idx >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.idx])
	c.idx++
	return n, nil
}

func TestStreamResponseBody_DrainCapturesUsage(t *testing.T) {
	// Upstream sends 3 chunks: first chunk succeeds, second chunk fails to client,
	// third chunk contains usage and is drained.
	chunks := [][]byte{
		[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"),
		[]byte("data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n"),
		[]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":60,\"total_tokens\":180}}\n\n"),
	}

	body := io.NopCloser(&chunkReader{chunks: chunks})
	rec := &failAfterFirstChunkWriter{}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	aud := &middleware.RequestAudit{Req: "test-req"}
	req = req.WithContext(context.WithValue(req.Context(), middleware.AuditKey{}, aud))

	n, err := StreamResponseBody(rec, body, req, "acc-1")
	if err == nil {
		t.Fatal("expected error due to client broken connection, got nil")
	}
	if n <= 0 {
		t.Errorf("written bytes = %d, want > 0", n)
	}

	if aud.PromptTokens != 120 {
		t.Errorf("aud.PromptTokens = %d, want 120", aud.PromptTokens)
	}
	if aud.CompletionTokens != 60 {
		t.Errorf("aud.CompletionTokens = %d, want 60", aud.CompletionTokens)
	}
	if aud.TotalTokens != 180 {
		t.Errorf("aud.TotalTokens = %d, want 180", aud.TotalTokens)
	}
}

type infiniteReader struct{}

func (r *infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestStreamResponseBody_DrainBoundedByLimitReader(t *testing.T) {
	// Infinite upstream body, client breaks on 2nd write.
	// Drain must terminate due to LimitReader (16MB) rather than hanging forever.
	body := io.NopCloser(&infiniteReader{})
	rec := &failAfterFirstChunkWriter{}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	aud := &middleware.RequestAudit{Req: "test-req-infinite"}
	req = req.WithContext(context.WithValue(req.Context(), middleware.AuditKey{}, aud))

	n, err := StreamResponseBody(rec, body, req, "acc-infinite")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if n <= 0 {
		t.Errorf("written bytes = %d, want > 0", n)
	}
}
