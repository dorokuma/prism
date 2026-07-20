package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseEvent represents a single SSE event from the Responses API stream.
type sseEvent struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"`
}

// parseSSE parses the body of a Responses API SSE stream into events.
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if !strings.HasPrefix(block, "data: ") {
			t.Fatalf("unexpected non-SSE line: %q", block)
		}
		raw := strings.TrimPrefix(block, "data: ")
		var ev sseEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("parse SSE event: %v\nraw: %q", err, raw)
		}
		ev.Raw = json.RawMessage(raw)
		events = append(events, ev)
	}
	return events
}

// getStringField extracts a string field from a nested JSON RawMessage.
func getStringField(t *testing.T, raw json.RawMessage, path ...string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, key := range path {
		if i == len(path)-1 {
			v, _ := m[key].(string)
			return v
		}
		sub, _ := m[key].(map[string]any)
		m = sub
	}
	return ""
}

// getIntField extracts an int field from a nested JSON RawMessage.
func getIntField(t *testing.T, raw json.RawMessage, path ...string) int {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, key := range path {
		if i == len(path)-1 {
			v, _ := m[key].(float64)
			return int(v)
		}
		sub, _ := m[key].(map[string]any)
		m = sub
	}
	return 0
}

func TestTranslateStream_BasicText(t *testing.T) {
	input := `data: {"choices":[{"delta":{"content":"Hello"}}]}

data: {"choices":[{"delta":{"content":" world"}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}

data: [DONE]
`
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(input), "gpt-5.5", nil, nil, context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := parseSSE(t, rec.Body.String())

	// Expect: response.created → output_item.added → content_part.added →
	//         output_text.delta → output_text.delta → output_text.done →
	//         output_item.done → response.completed
	if len(events) != 8 {
		t.Fatalf("expected 8 events, got %d", len(events))
	}

	// Check event types
	types := make([]string, len(events))
	for i, ev := range events {
		types[i] = ev.Type
	}
	wantTypes := []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	for i, want := range wantTypes {
		if types[i] != want {
			t.Fatalf("event[%d].type = %q, want %q", i, types[i], want)
		}
	}

	// Verify response.created has id and model
	createdModel := getStringField(t, events[0].Raw, "response", "model")
	if createdModel != "gpt-5.5" {
		t.Fatalf("response.created model = %q, want gpt-5.5", createdModel)
	}
	createdID := getStringField(t, events[0].Raw, "response", "id")
	if createdID == "" {
		t.Fatal("response.created id is empty")
	}

	// Verify content deltas
	delta3 := getStringField(t, events[3].Raw, "delta")
	if delta3 != "Hello" {
		t.Fatalf("delta[3] = %q, want Hello", delta3)
	}
	delta4 := getStringField(t, events[4].Raw, "delta")
	if delta4 != " world" {
		t.Fatalf("delta[4] = %q, want  world", delta4)
	}

	// Verify completed text
	text5 := getStringField(t, events[5].Raw, "text")
	if text5 != "Hello world" {
		t.Fatalf("output_text.done text = %q, want 'Hello world'", text5)
	}

	// Verify usage
	usageInput := getIntField(t, events[7].Raw, "response", "usage", "input_tokens")
	usageOutput := getIntField(t, events[7].Raw, "response", "usage", "output_tokens")
	if usageInput != 5 {
		t.Fatalf("usage.input_tokens = %d, want 5", usageInput)
	}
	if usageOutput != 7 {
		t.Fatalf("usage.output_tokens = %d, want 7", usageOutput)
	}
}

func TestTranslateStream_ReasoningContent(t *testing.T) {
	input := `data: {"choices":[{"delta":{"reasoning_content":"Step 1: think"}}]}

data: {"choices":[{"delta":{"reasoning_content":"Step 2: more"}}]}

data: {"choices":[{"delta":{"content":"Final answer"}}]}

data: [DONE]
`
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(input), "deepseek", nil, nil, context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := parseSSE(t, rec.Body.String())

	// reasoning_summary_text.delta (Codex 0.142.5); no reasoning_summary.done
	if len(events) < 9 {
		t.Fatalf("expected >= 9 events, got %d", len(events))
	}

	foundReasoningDeltas := 0
	for _, ev := range events {
		if ev.Type == "response.reasoning_summary_text.delta" {
			foundReasoningDeltas++
		}
		if ev.Type == "response.reasoning_summary.delta" || ev.Type == "response.reasoning_summary.done" {
			t.Fatalf("unexpected legacy reasoning event %q", ev.Type)
		}
	}
	if foundReasoningDeltas != 2 {
		t.Fatalf("expected 2 reasoning_summary_text.delta events, got %d", foundReasoningDeltas)
	}

	// Verify final content
	var hasContentDelta bool
	for _, ev := range events {
		if ev.Type == "response.output_text.delta" {
			hasContentDelta = true
			delta := getStringField(t, ev.Raw, "delta")
			if delta != "Final answer" {
				t.Fatalf("content delta = %q, want 'Final answer'", delta)
			}
		}
	}
	if !hasContentDelta {
		t.Fatal("expected output_text.delta event")
	}
}

func TestTranslateStream_ToolCalls(t *testing.T) {
	input := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ls -la"}}]}}]}

data: [DONE]
`
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(input), "gpt-5.5", nil, nil, context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := parseSSE(t, rec.Body.String())

	// Expect: created → output_item.added (function_call) → output_item.done → completed
	// (no function_call_arguments.delta — Codex does not handle it)
	if len(events) < 4 {
		t.Fatalf("expected >= 4 events, got %d", len(events))
	}

	// Find the output_item.added event for function_call
	var fnItemAdded, fnItemDone bool
	for _, ev := range events {
		if ev.Type == "response.output_item.added" {
			itemType := getStringField(t, ev.Raw, "item", "type")
			if itemType == "function_call" {
				fnItemAdded = true
				name := getStringField(t, ev.Raw, "item", "name")
				if name != "exec_command" {
					t.Fatalf("function_call name = %q, want 'exec_command'", name)
				}
			}
		}
		if ev.Type == "response.function_call_arguments.delta" {
			t.Fatal("unexpected function_call_arguments.delta event")
		}
		if ev.Type == "response.output_item.done" {
			itemType := getStringField(t, ev.Raw, "item", "type")
			if itemType == "function_call" {
				fnItemDone = true
				args := getStringField(t, ev.Raw, "item", "arguments")
				if args != "ls -la" {
					t.Fatalf("done arguments = %q, want 'ls -la'", args)
				}
			}
		}
	}
	if !fnItemAdded {
		t.Fatal("expected function_call output_item.added")
	}
	if !fnItemDone {
		t.Fatal("expected function_call output_item.done")
	}
}

func TestTranslateStream_ToolSearchInterception(t *testing.T) {
	// Simulate cached MCP tools
	cachedTools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "mcp__codegraph__search", "description": "Search code"}},
		{"type": "function", "function": map[string]any{"name": "mcp__tavily__tavily_search", "description": "Search web"}},
	}

	input := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"ts_call_1","type":"function","function":{"name":"tool_search","arguments":""}}]}}]}

data: [DONE]
`
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(input), "gpt-5.5", nil, cachedTools, context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := parseSSE(t, rec.Body.String())

	// Expected: created → function_call output_item.added →
	//           tool_search_output output_item.added → tool_search_output output_item.done →
	//           completed
	if len(events) < 5 {
		t.Fatalf("expected >= 5 events, got %d", len(events))
	}

	// Check that tool_search_output was emitted with cached tools
	var foundToolSearchOutput bool
	for _, ev := range events {
		if ev.Type == "response.output_item.added" {
			itemType := getStringField(t, ev.Raw, "item", "type")
			if itemType == "tool_search_output" {
				foundToolSearchOutput = true
				callID := getStringField(t, ev.Raw, "item", "call_id")
				if callID != "ts_call_1" {
					t.Fatalf("tool_search_output call_id = %q, want 'ts_call_1'", callID)
				}
				execution := getStringField(t, ev.Raw, "item", "execution")
				if execution != "client" {
					t.Fatalf("tool_search_output execution = %q, want 'client'", execution)
				}
			}
		}
	}
	if !foundToolSearchOutput {
		t.Fatal("expected tool_search_output event")
	}

}

func TestTranslateStream_ToolSearchInterception_NoCache(t *testing.T) {
	// When tool_search is called but no cached tools → no synthetic output
	input := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"tool_search","arguments":""}}]}}]}

data: [DONE]
`
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(input), "gpt-5.5", nil, nil, context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := parseSSE(t, rec.Body.String())

	// No tool_search_output when cache is empty
	for _, ev := range events {
		if ev.Type == "response.output_item.added" {
			itemType := getStringField(t, ev.Raw, "item", "type")
			if itemType == "tool_search_output" {
				t.Fatal("unexpected tool_search_output when cache is empty")
			}
		}
	}
}

func TestTranslateStream_MixedContent(t *testing.T) {
	// reasoning_content → content → tool_calls (all in one stream)
	input := `data: {"choices":[{"delta":{"reasoning_content":"thinking..."}}]}

data: {"choices":[{"delta":{"content":"Answer:"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"test\"}"}}]}}]}

data: [DONE]
`
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(input), "gpt-5.5", nil, nil, context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := parseSSE(t, rec.Body.String())

	// Must have: reasoning events + content events + tool_call events
	var hasReasoning, hasContent, hasToolCallDone bool
	for _, ev := range events {
		switch ev.Type {
		case "response.reasoning_summary_text.delta":
			hasReasoning = true
		case "response.output_text.delta":
			hasContent = true
		case "response.output_item.done":
			if getStringField(t, ev.Raw, "item", "type") == "function_call" {
				hasToolCallDone = true
			}
		case "response.function_call_arguments.delta":
			t.Fatal("unexpected function_call_arguments.delta event")
		}
	}
	if !hasReasoning {
		t.Fatal("expected reasoning_summary_text.delta events")
	}
	if !hasContent {
		t.Fatal("expected content delta events")
	}
	if !hasToolCallDone {
		t.Fatal("expected function_call output_item.done")
	}
}

func TestTranslateStream_EmptyInput(t *testing.T) {
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(""), "gpt-5.5", nil, nil, context.Background())
	if err != ErrEmptyUpstreamStream {
		t.Fatalf("expected ErrEmptyUpstreamStream, got %v", err)
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) != 2 {
		t.Fatalf("expected 2 events (created + failed), got %d", len(events))
	}
	if events[0].Type != "response.created" {
		t.Fatalf("event[0].type = %q, want response.created", events[0].Type)
	}
	if events[1].Type != "response.failed" {
		t.Fatalf("event[1].type = %q, want response.failed", events[1].Type)
	}
}

func TestStreamUsageConversion(t *testing.T) {
	// Stream with detailed usage including cache and reasoning fields.
	input := `data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":200,"completion_tokens":50,"total_tokens":250,"prompt_cache_hit_tokens":100,"prompt_cache_miss_tokens":50,"prompt_tokens_details":{"cached_tokens":100},"completion_tokens_details":{"reasoning_tokens":30}}}

data: [DONE]
`
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(input), "gpt-5.5", nil, nil, context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := parseSSE(t, rec.Body.String())

	// Verify usage in completed event
	for _, ev := range events {
		if ev.Type == "response.completed" {
			// Check prompt_cache_hit_tokens
			hit := getIntField(t, ev.Raw, "response", "usage", "prompt_cache_hit_tokens")
			if hit != 100 {
				t.Fatalf("prompt_cache_hit_tokens = %d, want 100", hit)
			}
			// Check prompt_cache_miss_tokens
			miss := getIntField(t, ev.Raw, "response", "usage", "prompt_cache_miss_tokens")
			if miss != 50 {
				t.Fatalf("prompt_cache_miss_tokens = %d, want 50", miss)
			}
			// Check completion_tokens_details.reasoning_tokens
			rt := getIntField(t, ev.Raw, "response", "usage", "completion_tokens_details", "reasoning_tokens")
			if rt != 30 {
				t.Fatalf("reasoning_tokens = %d, want 30", rt)
			}
			// Check prompt_tokens
			pt := getIntField(t, ev.Raw, "response", "usage", "prompt_tokens")
			if pt != 200 {
				t.Fatalf("prompt_tokens = %d, want 200", pt)
			}
			// Check input_tokens
			inputTok := getIntField(t, ev.Raw, "response", "usage", "input_tokens")
			if inputTok != 200 {
				t.Fatalf("input_tokens = %d, want 200", inputTok)
			}
		}
	}
}

func TestTranslateStream_UsagePassThrough(t *testing.T) {
	input := `data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}

data: [DONE]
`
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(input), "gpt-5.5", nil, nil, context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := parseSSE(t, rec.Body.String())

	// Find completed event and check usage
	for _, ev := range events {
		if ev.Type == "response.completed" {
			inputTok := getIntField(t, ev.Raw, "response", "usage", "input_tokens")
			outputTok := getIntField(t, ev.Raw, "response", "usage", "output_tokens")
			totalTok := getIntField(t, ev.Raw, "response", "usage", "total_tokens")
			if inputTok != 10 {
				t.Fatalf("usage.input_tokens = %d, want 10", inputTok)
			}
			if outputTok != 20 {
				t.Fatalf("usage.output_tokens = %d, want 20", outputTok)
			}
			if totalTok != 30 {
				t.Fatalf("usage.total_tokens = %d, want 30", totalTok)
			}
		}
	}
}

func TestTranslateStream_ReqToolsPropagated(t *testing.T) {
	reqTools := json.RawMessage(`[{"type":"function","name":"exec_command"}]`)
	input := `data: {"choices":[{"delta":{"content":"hello"}}]}

data: [DONE]
`
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(input), "gpt-5.5", reqTools, nil, context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := parseSSE(t, rec.Body.String())

	// response.completed should have tools field
	for _, ev := range events {
		if ev.Type == "response.completed" {
			var m map[string]any
			if err := json.Unmarshal(ev.Raw, &m); err != nil {
				t.Fatal(err)
			}
			resp, ok := m["response"].(map[string]any)
			if !ok {
				t.Fatal("response.completed missing response field")
			}
			tools, ok := resp["tools"]
			if !ok {
				t.Fatal("response.completed missing tools field")
			}
			toolsArr, ok := tools.([]any)
			if !ok || len(toolsArr) == 0 {
				t.Fatal("tools should be non-empty array")
			}
		}
	}
}

func TestTranslateStream_SkipsNonDataLines(t *testing.T) {
	// Lines without "data: " prefix should be ignored
	input := `:comment

data: {"choices":[{"delta":{"content":"hi"}}]}

:heartbeat

data: [DONE]
`
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(input), "gpt-5.5", nil, nil, context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := parseSSE(t, rec.Body.String())
	// Should produce valid events (created, added, part, delta, done, item_done, completed)
	if len(events) < 7 {
		t.Fatalf("expected >= 7 events, got %d (input had non-data lines that should be skipped)", len(events))
	}
}

func TestTranslateStream_NamespacePrefixedToolName(t *testing.T) {
	input := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"nc_1","type":"function","function":{"name":"mcp__codegraph__search","arguments":""}}]}}]}

data: [DONE]
`
	rec := httptest.NewRecorder()
	err := translateChatStreamToResponses(rec, strings.NewReader(input), "gpt-5.5", nil, nil, context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := parseSSE(t, rec.Body.String())

	for _, ev := range events {
		if ev.Type == "response.output_item.added" {
			itemType := getStringField(t, ev.Raw, "item", "type")
			if itemType == "function_call" {
				name := getStringField(t, ev.Raw, "item", "name")
				if name != "search" {
					t.Fatalf("resolved name = %q, want 'search' (stripped namespace prefix)", name)
				}
				ns := getStringField(t, ev.Raw, "item", "namespace")
				if ns != "mcp__codegraph" {
					t.Fatalf("namespace = %q, want 'mcp__codegraph'", ns)
				}
			}
		}
	}
}

func TestTranslateStreamCancelBlockedReader(t *testing.T) {
	// A pipe with no writer blocks forever – this simulates an upstream
	// that is silent (e.g. long reasoning pause). After ctx cancel the
	// ctxReader wrapper must unblock and return context.Canceled promptly.
	pr, pw := io.Pipe()
	// Don't write anything; just close pw when the test ends so the
	// background io.Copy goroutine in ctxReader can exit.
	defer pw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()

	errCh := make(chan error, 1)
	go func() {
		errCh <- translateChatStreamToResponses(rec, pr, "gpt-5.5", nil, nil, ctx)
	}()

	// Let translate start reading; it should block on the pipe.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: translate did not return after cancel on blocked reader")
	}
}

// Test TranslateStream with partial input (no [DONE]) to exercise scanner
// EOF handling through the ctxReader wrapper.
func TestTranslateStreamCancel(t *testing.T) {
	// Pipe that continuously produces SSE data lines.
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, err := pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
			if err != nil {
				return
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	// started signals that the translate goroutine has begun reading the pipe.
	started := make(chan struct{})
	sr := &signalReader{r: pr, started: started}

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()

	errCh := make(chan error, 1)
	go func() {
		errCh <- translateChatStreamToResponses(rec, sr, "gpt-5.5", nil, nil, ctx)
	}()

	// Wait until translate has started reading, then cancel.
	<-started
	cancel()

	var err error
	select {
	case err = <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for translate to return after cancel")
	}

	if err == nil {
		t.Fatal("expected error after context cancel, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}

	pw.Close()
	<-done
}

// signalReader wraps an io.Reader and closes the started channel on the first
// Read call, so tests can synchronize with the reader starting.
type signalReader struct {
	r       io.Reader
	started chan struct{}
	once    sync.Once
}

func (s *signalReader) Read(p []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	return s.r.Read(p)
}

// failWriter is an http.ResponseWriter whose Write always returns an error,
// simulating a client disconnect mid-stream.
type failWriter struct {
	header http.Header
}

func (w *failWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failWriter) Write(p []byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func (w *failWriter) WriteHeader(statusCode int) {}

// TestTranslateStreamEmitFailureCancelsCtxReader verifies that when translate
// exits due to a write error (e.g. client disconnected) and the ctxReader's
// io.Copy goroutine might be stuck on the inner pipe write, cancelling the
// context causes the watcher goroutine to close the pipe write end, unblocking
// io.Copy and preventing a goroutine + pipe leak.
//
// Without the watcher goroutine in ctxReader, this scenario would leak:
//  1. translate emits SSE events (w.Write) — client disconnects, Write returns
//     err, translate returns.
//  2. At that point sc.Scan is not running, so nobody calls ctxPipeReader.Read.
//  3. The io.Copy goroutine inside ctxReader may be blocked on pw.Write (inner
//     pipe full). Cancelling ctx alone won't help because io.Copy does not
//     watch ctx; the existing cancel path in ctxPipeReader.Read never fires
//     because nobody is calling Read.
//  4. The watcher goroutine added to ctxReader closes pw on ctx.Done, forcing
//     io.Copy's pw.Write to fail, causing io.Copy to exit.
//
// This test uses goroutine counting to assert that both ctxReader internal
// goroutines (io.Copy and watcher) exit after context cancellation. The old
// writer-goroutine-exit approach did not actually prove ctxReader cleanup.
func TestTranslateStreamEmitFailureCancelsCtxReader(t *testing.T) {
	// Take a goroutine baseline before creating any extra goroutines for
	// this test (producer, ctxReader's io.Copy, watcher). All of these must
	// exit for the count to return to baseline.
	baseline := runtime.NumGoroutine()

	// Producer: continuously write SSE data as fast as possible so that
	// io.Copy's srcPr.Read never blocks. After translate returns (nobody
	// reads pr), io.Copy will reliably block on pw.Write — exactly the
	// scenario the watcher goroutine guards against.
	srcPr, srcPw := io.Pipe()
	go func() {
		for i := 0; ; i++ {
			_, err := fmt.Fprintf(srcPw, "data: {\"choices\":[{\"delta\":{\"content\":\"x%d\"}}]}\n\n", i)
			if err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	fw := &failWriter{}

	// translate returns on the first emit because failWriter.Write fails.
	translateErr := translateChatStreamToResponses(fw, srcPr, "gpt-5.5", nil, nil, ctx)
	if translateErr == nil {
		cancel()
		srcPw.Close()
		t.Fatal("expected translate to fail due to write error")
	}

	// Cancel the context. The watcher goroutine inside ctxReader closes the
	// inner pipe write end, unblocking io.Copy. Both ctxReader goroutines
	// (io.Copy and watcher) should exit.
	cancel()

	// Close the upstream writer so the producer goroutine exits too.
	srcPw.Close()

	// Poll until goroutine count returns to baseline.
	runtime.GC()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline {
		buf := make([]byte, 1<<16)
		stackLen := runtime.Stack(buf, true)
		t.Fatalf("goroutine leak: ctxReader io.Copy/watcher did not exit (count=%d, baseline=%d, delta=%d):\n%s",
			n, baseline, n-baseline, buf[:stackLen])
	}
}
