package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/prism/internal/dsml"
)

func dsmlTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "dsml", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func officialDSMLWeatherBlock() string {
	bar := "\uFF5C"
	tok := bar + "DSML" + bar
	return "<" + tok + "tool_calls>\n" +
		"<" + tok + "invoke name=\"get_weather\">\n" +
		"<" + tok + "parameter name=\"city\" string=\"true\">Shanghai</" + tok + "parameter>\n" +
		"<" + tok + "parameter name=\"days\" string=\"false\">3</" + tok + "parameter>\n" +
		"</" + tok + "invoke>\n" +
		"</" + tok + "tool_calls>"
}

func chatSSEJSON(obj any) []byte {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return append(append([]byte("data: "), b...), []byte("\n\n")...)
}

func jsonStringRaw(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		panic(err)
	}
	return bytes.TrimSpace(buf.Bytes())
}

func chatSSEContent(id string, content []byte) []byte {
	payload := jsonStringRaw(string(content))
	var b bytes.Buffer
	b.WriteString(`data: {"id":"`)
	b.WriteString(id)
	b.WriteString(`","object":"chat.completion.chunk","created":9,"model":"m","choices":[{"index":0,"delta":{"content":`)
	b.Write(payload)
	b.WriteString(`},"finish_reason":null}]}` + "\n\n")
	return b.Bytes()
}

func abortedChatSSE(t *testing.T) []byte {
	t.Helper()
	raw := dsmlTestdata(t, "dsml-sample-0708-L59-prefix.txt")
	if len(raw) < 2000 {
		t.Fatalf("prefix testdata too short: %d", len(raw))
	}
	text := raw[:2000]
	var buf bytes.Buffer
	buf.Write(chatSSEJSON(map[string]any{
		"id": "gen_abort", "object": "chat.completion.chunk", "created": 1, "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	}))
	buf.Write(chatSSEJSON(map[string]any{
		"id": "gen_abort", "object": "chat.completion.chunk", "created": 1, "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning": "plan the call"}, "finish_reason": nil}},
	}))
	mid := 80
	buf.Write(chatSSEContent("gen_abort", text[:mid]))
	buf.Write(chatSSEContent("gen_abort", text[mid:]))
	buf.Write(chatSSEJSON(map[string]any{
		"id": "gen_abort", "object": "chat.completion.chunk", "created": 1, "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 2},
	}))
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

func recoveredChatSSE() []byte {
	block := []byte(officialDSMLWeatherBlock())
	var buf bytes.Buffer
	buf.Write(chatSSEJSON(map[string]any{
		"id": "gen_rec", "object": "chat.completion.chunk", "created": 9, "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	}))
	buf.Write(chatSSEContent("gen_rec", block))
	buf.Write(chatSSEJSON(map[string]any{
		"id": "gen_rec", "object": "chat.completion.chunk", "created": 9, "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}))
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

func translateChatToResponses(t *testing.T, body io.Reader) string {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := TranslateChatStreamToResponses(rec, body, "gpt-5.5", nil, nil, context.Background()); err != nil {
		t.Fatalf("translate: %v", err)
	}
	return rec.Body.String()
}

func eventSketch(t *testing.T, body string) []string {
	t.Helper()
	events := parseSSE(t, body)
	out := make([]string, 0, len(events))
	for _, ev := range events {
		switch ev.Type {
		case "response.output_text.delta":
			out = append(out, ev.Type+"|"+getStringField(t, ev.Raw, "delta"))
		case "response.output_text.done":
			out = append(out, ev.Type+"|"+getStringField(t, ev.Raw, "text"))
		case "response.reasoning_summary_text.delta":
			out = append(out, ev.Type+"|"+getStringField(t, ev.Raw, "delta"))
		case "response.output_item.added", "response.output_item.done":
			out = append(out, ev.Type+"|"+getStringField(t, ev.Raw, "item", "type")+"|"+getStringField(t, ev.Raw, "item", "name")+"|"+getStringField(t, ev.Raw, "item", "arguments"))
		default:
			out = append(out, ev.Type)
		}
	}
	return out
}

func collectedOutputText(t *testing.T, body string) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range parseSSE(t, body) {
		if ev.Type == "response.output_text.delta" {
			b.WriteString(getStringField(t, ev.Raw, "delta"))
		}
	}
	return b.String()
}

func TestTranslateStream_DSMLGuardPassthrough(t *testing.T) {
	raw := dsmlTestdata(t, "clinepass-dsml-raw-stream.txt")
	guardedSSE, err := io.ReadAll(dsml.NewGuardReader(io.NopCloser(bytes.NewReader(raw))))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, guardedSSE) {
		t.Fatalf("guard mutated clean chat SSE: in=%d out=%d", len(raw), len(guardedSSE))
	}

	ungarded := translateChatToResponses(t, bytes.NewReader(raw))
	guarded := translateChatToResponses(t, dsml.NewGuardReader(io.NopCloser(bytes.NewReader(raw))))
	want := eventSketch(t, ungarded)
	got := eventSketch(t, guarded)
	if len(want) != len(got) {
		t.Fatalf("event count %d vs %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d]=\n  got  %s\n  want %s", i, got[i], want[i])
		}
	}
}

func TestTranslateStream_DSMLGuardAborted(t *testing.T) {
	sse := abortedChatSSE(t)
	out := translateChatToResponses(t, dsml.NewGuardReader(io.NopCloser(bytes.NewReader(sse))))
	text := collectedOutputText(t, out)
	if dsml.HasMarker(text) || strings.Contains(out, "\uFF5C"+"DSML") || strings.Contains(out, "toolpending") {
		t.Fatalf("Responses stream still has DSML: %s", truncateForTest(out, 400))
	}
	if strings.Contains(out, "serializedRunner") || strings.Contains(out, "persistCacheStats") {
		t.Fatal("CoT body leaked into Responses stream")
	}
	if !strings.Contains(text, "[prism] removed ") || !strings.Contains(text, "aborted DSML output") {
		t.Fatalf("missing notice in output_text: %q", text)
	}
}

func TestTranslateStream_DSMLGuardRecover(t *testing.T) {
	sse := recoveredChatSSE()
	out := translateChatToResponses(t, dsml.NewGuardReader(io.NopCloser(bytes.NewReader(sse))))
	if strings.Contains(out, "\uFF5C"+"DSML") || strings.Contains(out, "toolpending") {
		t.Fatalf("DSML leaked into Responses stream: %s", truncateForTest(out, 400))
	}
	events := parseSSE(t, out)
	var sawAdded, sawDone bool
	for _, ev := range events {
		if ev.Type == "response.output_item.added" && getStringField(t, ev.Raw, "item", "type") == "function_call" {
			sawAdded = true
			if getStringField(t, ev.Raw, "item", "name") != "get_weather" {
				t.Fatalf("function_call name=%q", getStringField(t, ev.Raw, "item", "name"))
			}
		}
		if ev.Type == "response.output_item.done" && getStringField(t, ev.Raw, "item", "type") == "function_call" {
			sawDone = true
			args := getStringField(t, ev.Raw, "item", "arguments")
			var parsed map[string]any
			if err := json.Unmarshal([]byte(args), &parsed); err != nil {
				t.Fatalf("arguments not JSON: %s", args)
			}
			if parsed["city"] != "Shanghai" {
				t.Fatalf("city=%v", parsed["city"])
			}
			days, _ := parsed["days"].(float64)
			if days != 3 {
				t.Fatalf("days=%v", parsed["days"])
			}
		}
		if ev.Type == "response.output_text.delta" {
			t.Fatalf("recover path must not emit output_text, got %q", getStringField(t, ev.Raw, "delta"))
		}
	}
	if !sawAdded || !sawDone {
		t.Fatalf("missing function_call events added=%v done=%v body=%s", sawAdded, sawDone, truncateForTest(out, 800))
	}
}

func truncateForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
