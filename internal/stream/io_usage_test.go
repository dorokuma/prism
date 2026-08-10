package stream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/usagemeta"
)

// ---------------------------------------------------------------------------
// captureWriter bounds
// ---------------------------------------------------------------------------

func TestCaptureWriter_BoundedHeadAndTail(t *testing.T) {
	c := newCaptureWriter(16, 8)
	// 40 bytes total: "0123456789abcdef" is 16 bytes, repeated.
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteByte(byte('a' + i%26))
	}
	data := []byte(sb.String())

	// Write in uneven chunks to exercise partial head fills.
	parts := [][]byte{data[:3], data[3:20], data[20:35], data[35:]}
	for _, p := range parts {
		if n, err := c.Write(p); err != nil || n != len(p) {
			t.Fatalf("Write(%d bytes) = %d, %v", len(p), n, err)
		}
	}

	head := string(c.headBytes())
	if head != string(data[:16]) {
		t.Errorf("head = %q, want first 16 bytes", head)
	}
	tail := string(c.tailBytes())
	if tail != string(data[len(data)-8:]) {
		t.Errorf("tail = %q, want last 8 bytes", tail)
	}
	// Bounded memory: head ≤ 16, tail ≤ 8 regardless of total writes.
	if len(c.headBytes()) > 16 || len(c.tailBytes()) > 8 {
		t.Errorf("capture buffers exceed caps: head=%d tail=%d", len(c.headBytes()), len(c.tailBytes()))
	}
}

func TestCaptureWriter_ShortStream(t *testing.T) {
	c := newCaptureWriter(16, 8)
	data := []byte("tiny")
	if _, err := c.Write(data); err != nil {
		t.Fatal(err)
	}
	if string(c.headBytes()) != "tiny" || string(c.tailBytes()) != "tiny" {
		t.Errorf("short stream: head=%q tail=%q, want both %q", c.headBytes(), c.tailBytes(), data)
	}
}

// ---------------------------------------------------------------------------
// parseStreamUsage unit tests
// ---------------------------------------------------------------------------

func TestParseStreamUsage_OpenAI(t *testing.T) {
	head := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	tail := []byte("data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":7,\"total_tokens\":22,\"prompt_cache_hit_tokens\":9,\"prompt_tokens_details\":{\"cached_tokens\":9},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\ndata: [DONE]\n\n")
	u := parseStreamUsage(head, tail)
	want := usagemeta.Usage{Prompt: 15, Completion: 7, Total: 22, Cached: 9, Reasoning: 3, Source: usagemeta.SourceOpenAI}
	if u != want {
		t.Errorf("parseStreamUsage(openai) = %+v, want %+v", u, want)
	}
}

func TestParseStreamUsage_Anthropic(t *testing.T) {
	// Real Anthropic stream shape: message_start at the head carries input
	// tokens, message_delta at the tail carries output tokens. The middle
	// is a long content delta (larger than the tail cap) to prove the two
	// ends are captured independently.
	head := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[],\"usage\":{\"input_tokens\":250,\"cache_creation_input_tokens\":50,\"cache_read_input_tokens\":200}}}\n\n")
	tail := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" + strings.Repeat("x", 9000) + "\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":40}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	u := parseStreamUsage(head, tail)
	want := usagemeta.Usage{Prompt: 250, Completion: 40, Total: 290, Cached: 200, CacheWrite: 50, Source: usagemeta.SourceAnthropic}
	if u != want {
		t.Errorf("parseStreamUsage(anthropic) = %+v, want %+v", u, want)
	}
}

func TestParseStreamUsage_NoUsage(t *testing.T) {
	head := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	tail := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"y\"}}]}\n\ndata: [DONE]\n\n")
	if u := parseStreamUsage(head, tail); u != (usagemeta.Usage{}) {
		t.Errorf("parseStreamUsage(no usage) = %+v, want zero", u)
	}
}

func TestParseStreamUsage_AnthropicDetectedByHead(t *testing.T) {
	// The head is what selects the Anthropic parser; a plain OpenAI tail
	// must not be misread even when it contains usage fields.
	head := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n")
	tail := []byte("data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":99,\"completion_tokens\":99}}\n\n")
	u := parseStreamUsage(head, tail)
	if u.Prompt != 10 || u.Completion != 0 || u.Source != usagemeta.SourceAnthropic {
		t.Errorf("parseStreamUsage = %+v, want Prompt=10 Completion=0 Source=anthropic (anthropic path, no message_delta)", u)
	}
}

// ---------------------------------------------------------------------------
// StreamResponseBody end-to-end (passthrough + audit capture)
// ---------------------------------------------------------------------------

func newAuditReq() (*http.Request, *middleware.RequestAudit) {
	a := &middleware.RequestAudit{}
	ctx := context.WithValue(context.Background(), middleware.AuditKey{}, a)
	req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	return req, a
}

func TestStreamResponseBody_OpenAIUsageFullFields(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":7,\"total_tokens\":22,\"prompt_cache_hit_tokens\":9,\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(sse))
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("upstream request failed: %v", err)
	}
	defer resp.Body.Close()

	rec := httptest.NewRecorder()
	req, a := newAuditReq()
	if _, err := StreamResponseBody(rec, resp.Body, req, "test-account"); err != nil {
		t.Fatalf("StreamResponseBody failed: %v", err)
	}

	// Byte-for-byte passthrough.
	if rec.Body.String() != sse {
		t.Errorf("passthrough broken:\n got %q\nwant %q", rec.Body.String(), sse)
	}
	// Full field capture.
	if a.PromptTokens != 15 || a.TokensIn != 15 {
		t.Errorf("PromptTokens/TokensIn = %d/%d, want 15", a.PromptTokens, a.TokensIn)
	}
	if a.CompletionTokens != 7 || a.TokensOut != 7 {
		t.Errorf("CompletionTokens/TokensOut = %d/%d, want 7", a.CompletionTokens, a.TokensOut)
	}
	if a.TotalTokens != 22 {
		t.Errorf("TotalTokens = %d, want 22", a.TotalTokens)
	}
	if a.CachedTokens != 9 {
		t.Errorf("CachedTokens = %d, want 9", a.CachedTokens)
	}
	if a.ReasoningTokens != 3 {
		t.Errorf("ReasoningTokens = %d, want 3", a.ReasoningTokens)
	}
	if a.UsageSource != usagemeta.SourceOpenAI {
		t.Errorf("UsageSource = %q, want %q", a.UsageSource, usagemeta.SourceOpenAI)
	}
}

func TestStreamResponseBody_AnthropicUsage(t *testing.T) {
	sse := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[],\"usage\":{\"input_tokens\":250,\"cache_creation_input_tokens\":50,\"cache_read_input_tokens\":200}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":40}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(sse))
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("upstream request failed: %v", err)
	}
	defer resp.Body.Close()

	rec := httptest.NewRecorder()
	req, a := newAuditReq()
	if _, err := StreamResponseBody(rec, resp.Body, req, "test-account"); err != nil {
		t.Fatalf("StreamResponseBody failed: %v", err)
	}

	// Byte-for-byte passthrough.
	if rec.Body.String() != sse {
		t.Errorf("passthrough broken:\n got %q\nwant %q", rec.Body.String(), sse)
	}
	// Head (message_start) and tail (message_delta) merged.
	if a.PromptTokens != 250 {
		t.Errorf("PromptTokens = %d, want 250 (message_start input_tokens)", a.PromptTokens)
	}
	if a.CompletionTokens != 40 {
		t.Errorf("CompletionTokens = %d, want 40 (message_delta output_tokens)", a.CompletionTokens)
	}
	if a.TotalTokens != 290 {
		t.Errorf("TotalTokens = %d, want 290", a.TotalTokens)
	}
	if a.CachedTokens != 200 {
		t.Errorf("CachedTokens = %d, want 200 (cache_read_input_tokens)", a.CachedTokens)
	}
	if a.CacheWriteTokens != 50 {
		t.Errorf("CacheWriteTokens = %d, want 50 (cache_creation_input_tokens)", a.CacheWriteTokens)
	}
	if a.UsageSource != usagemeta.SourceAnthropic {
		t.Errorf("UsageSource = %q, want %q", a.UsageSource, usagemeta.SourceAnthropic)
	}
}

func TestStreamResponseBody_AnthropicLongStream(t *testing.T) {
	// A stream far larger than both caps (message_start at the head,
	// message_delta at the tail, ~100 KiB of content in between): proves
	// the head is not evicted by the content and the stream is not fully
	// buffered (memory stays at headCap+tailCap, see TestCaptureWriter).
	chunk := strings.Repeat("y", 4096)
	var sb strings.Builder
	sb.WriteString("event: message_start\n")
	sb.WriteString("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":111,\"cache_creation_input_tokens\":11,\"cache_read_input_tokens\":100}}}\n\n")
	for i := 0; i < 25; i++ {
		sb.WriteString("event: content_block_delta\n")
		sb.WriteString("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + chunk + "\"}}\n\n")
	}
	sb.WriteString("event: message_delta\n")
	sb.WriteString("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":222}}\n\n")
	sb.WriteString("event: message_stop\n")
	sb.WriteString("data: {\"type\":\"message_stop\"}\n\n")
	sse := sb.String()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(sse))
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("upstream request failed: %v", err)
	}
	defer resp.Body.Close()

	rec := httptest.NewRecorder()
	req, a := newAuditReq()
	n, err := StreamResponseBody(rec, resp.Body, req, "test-account")
	if err != nil {
		t.Fatalf("StreamResponseBody failed: %v", err)
	}

	if n != int64(len(sse)) || rec.Body.Len() != len(sse) {
		t.Errorf("written = %d, body len = %d, want %d (full passthrough)", n, rec.Body.Len(), len(sse))
	}
	if a.PromptTokens != 111 {
		t.Errorf("PromptTokens = %d, want 111", a.PromptTokens)
	}
	if a.CompletionTokens != 222 {
		t.Errorf("CompletionTokens = %d, want 222", a.CompletionTokens)
	}
	if a.TotalTokens != 333 {
		t.Errorf("TotalTokens = %d, want 333", a.TotalTokens)
	}
	if a.CachedTokens != 100 {
		t.Errorf("CachedTokens = %d, want 100", a.CachedTokens)
	}
	if a.UsageSource != usagemeta.SourceAnthropic {
		t.Errorf("UsageSource = %q, want %q", a.UsageSource, usagemeta.SourceAnthropic)
	}
}

func TestStreamResponseBody_NilClientRequest(t *testing.T) {
	// clientReq == nil must not panic (nil-safe audit path).
	body := io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
	rec := httptest.NewRecorder()
	if _, err := StreamResponseBody(rec, body, nil, "test-account"); err != nil {
		t.Fatalf("StreamResponseBody(nil req) failed: %v", err)
	}
}
