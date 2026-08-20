package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/dsml"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/pool"
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

func completionJSON(content, finish string) []byte {
	raw := map[string]any{
		"id": "c1", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": finish,
			},
		},
	}
	b, err := json.Marshal(raw)
	if err != nil {
		panic(err)
	}
	return b
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
	buf.WriteString(`data: {"id":"gen_abort","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n")
	mid := 80
	buf.Write(chatSSEContent("gen_abort", text[:mid]))
	buf.Write(chatSSEContent("gen_abort", text[mid:]))
	buf.WriteString(`data: {"id":"gen_abort","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

func recoveredChatSSE() []byte {
	block := []byte(officialDSMLWeatherBlock())
	var buf bytes.Buffer
	buf.WriteString(`data: {"id":"gen_rec","object":"chat.completion.chunk","created":9,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n")
	buf.Write(chatSSEContent("gen_rec", block))
	buf.WriteString(`data: {"id":"gen_rec","object":"chat.completion.chunk","created":9,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

func handleResponses(t *testing.T, resp *http.Response, w responseCommitWriter, opts ChatForwardOpts) *middleware.RequestAudit {
	t.Helper()
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "t", Key: "k", BaseURL: "http://upstream.invalid", Provider: "t"}}}
	p := pool.NewPool(cfg.Accounts)
	acc, slot, err := p.SelectByProvider(context.Background(), "gpt-4", 1, "t")
	if err != nil {
		t.Fatalf("select account: %v", err)
	}
	defer p.Release(slot)

	aud := &middleware.RequestAudit{Req: "dsml-resp-1", Method: "POST", Path: "/v1/responses", Model: "gpt-4"}
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r = r.WithContext(context.WithValue(r.Context(), middleware.AuditKey{}, aud))
	ctx, cancel := context.WithCancel(r.Context())
	done, _, class := handleUpstreamResponse(acc, w, r, resp, nil, time.Now(), opts, "dsml-resp-1", ctx, cancel)
	if !done {
		t.Fatalf("handleUpstreamResponse must report done, class=%d", class)
	}
	return aud
}

func responsesOutputText(t *testing.T, body []byte) string {
	t.Helper()
	var parsed struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("responses JSON: %v body=%s", err, body)
	}
	var b strings.Builder
	for _, item := range parsed.Output {
		for _, part := range item.Content {
			if part.Type == "output_text" {
				b.WriteString(part.Text)
			}
		}
	}
	return b.String()
}

func TestResponsesStream_DSMLGuardPassthrough(t *testing.T) {
	raw := dsmlTestdata(t, "clinepass-dsml-raw-stream.txt")
	recOff := httptest.NewRecorder()
	handleResponses(t, &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(raw)),
	}, &commitTrackWriter{inner: recOff}, ChatForwardOpts{ResponsesOut: true, Stream: true})
	recOn := httptest.NewRecorder()
	handleResponses(t, &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(raw)),
	}, &commitTrackWriter{inner: recOn}, ChatForwardOpts{ResponsesOut: true, Stream: true, DSMLGuard: true})

	off := streamEventSketch(t, recOff.Body.String())
	on := streamEventSketch(t, recOn.Body.String())
	if len(off) != len(on) {
		t.Fatalf("event count guard-on=%d guard-off=%d", len(on), len(off))
	}
	for i := range off {
		if off[i] != on[i] {
			t.Fatalf("event[%d] guard-on %s guard-off %s", i, on[i], off[i])
		}
	}
}

func TestResponsesStream_DSMLGuardAborted(t *testing.T) {
	sse := abortedChatSSE(t)
	rec := httptest.NewRecorder()
	handleResponses(t, &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(sse)),
	}, &commitTrackWriter{inner: rec}, ChatForwardOpts{ResponsesOut: true, Stream: true, DSMLGuard: true})
	body := rec.Body.String()
	text := streamCollectedText(t, body)
	if dsml.HasMarker(text) || strings.Contains(body, "\uFF5C"+"DSML") || strings.Contains(body, "toolpending") {
		t.Fatalf("Responses stream still has DSML: %s", truncateBody(body, 400))
	}
	if strings.Contains(body, "serializedRunner") || strings.Contains(body, "persistCacheStats") {
		t.Fatal("CoT body leaked into Responses stream")
	}
	if !strings.Contains(text, "[prism] removed ") || !strings.Contains(text, "aborted DSML output") {
		t.Fatalf("missing notice in output_text: %q", text)
	}
}

func TestResponsesStream_DSMLGuardRecover(t *testing.T) {
	sse := recoveredChatSSE()
	rec := httptest.NewRecorder()
	handleResponses(t, &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(sse)),
	}, &commitTrackWriter{inner: rec}, ChatForwardOpts{ResponsesOut: true, Stream: true, DSMLGuard: true})
	body := rec.Body.String()
	if strings.Contains(body, "\uFF5C"+"DSML") || strings.Contains(body, "toolpending") {
		t.Fatalf("DSML leaked: %s", truncateBody(body, 400))
	}
	var sawFn bool
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev["type"] != "response.output_item.done" {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		if item["type"] != "function_call" {
			continue
		}
		sawFn = true
		if item["name"] != "get_weather" {
			t.Fatalf("name=%v", item["name"])
		}
		args, _ := item["arguments"].(string)
		var parsed map[string]any
		if err := json.Unmarshal([]byte(args), &parsed); err != nil {
			t.Fatalf("arguments: %v %s", err, args)
		}
		if parsed["city"] != "Shanghai" {
			t.Fatalf("city=%v", parsed["city"])
		}
	}
	if !sawFn {
		t.Fatalf("no function_call in Responses stream: %s", truncateBody(body, 800))
	}
}

func TestResponsesNonStream_DSMLGuardPassthrough(t *testing.T) {
	body := []byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	recOff := httptest.NewRecorder()
	directNonStreamResp(t, &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(body)),
	}, &commitTrackWriter{inner: recOff}, ChatForwardOpts{ResponsesOut: true})
	recOn := httptest.NewRecorder()
	directNonStreamResp(t, &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(body)),
	}, &commitTrackWriter{inner: recOn}, ChatForwardOpts{ResponsesOut: true, DSMLGuard: true})
	off := responsesOutputText(t, recOff.Body.Bytes())
	on := responsesOutputText(t, recOn.Body.Bytes())
	if off != "hello" || on != "hello" {
		t.Fatalf("output_text off=%q on=%q", off, on)
	}
}

func TestResponsesNonStream_DSMLGuardAborted(t *testing.T) {
	raw := dsmlTestdata(t, "dsml-sample-0708-L59-prefix.txt")
	body := completionJSON(string(raw[:2000]), "length")
	rec := httptest.NewRecorder()
	directNonStreamResp(t, &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(body)),
	}, &commitTrackWriter{inner: rec}, ChatForwardOpts{ResponsesOut: true, DSMLGuard: true})
	text := responsesOutputText(t, rec.Body.Bytes())
	if dsml.HasMarker(text) || strings.Contains(text, "toolpending") {
		t.Fatalf("non-stream still has DSML: %q", text)
	}
	if !strings.Contains(text, "[prism] removed ") {
		t.Fatalf("missing notice: %q", text)
	}
}

func TestResponsesNonStream_DSMLGuardRecover(t *testing.T) {
	body := completionJSON(officialDSMLWeatherBlock(), "stop")
	rec := httptest.NewRecorder()
	directNonStreamResp(t, &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(body)),
	}, &commitTrackWriter{inner: rec}, ChatForwardOpts{ResponsesOut: true, DSMLGuard: true})
	var parsed struct {
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, item := range parsed.Output {
		if item.Type != "function_call" {
			continue
		}
		saw = true
		if item.Name != "get_weather" {
			t.Fatalf("name=%q", item.Name)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(item.Arguments), &args); err != nil {
			t.Fatal(err)
		}
		if args["city"] != "Shanghai" {
			t.Fatalf("city=%v", args["city"])
		}
	}
	if !saw {
		t.Fatalf("no function_call: %s", rec.Body.String())
	}
}

func streamEventSketch(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" || !strings.HasPrefix(block, "data: ") {
			continue
		}
		raw := strings.TrimPrefix(block, "data: ")
		var ev map[string]any
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("parse SSE: %v raw=%q", err, raw)
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "response.output_text.delta":
			d, _ := ev["delta"].(string)
			out = append(out, typ+"|"+d)
		case "response.output_text.done":
			d, _ := ev["text"].(string)
			out = append(out, typ+"|"+d)
		case "response.output_item.added", "response.output_item.done":
			item, _ := ev["item"].(map[string]any)
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			itype, _ := item["type"].(string)
			out = append(out, typ+"|"+itype+"|"+name+"|"+args)
		default:
			out = append(out, typ)
		}
	}
	return out
}

func truncateBody(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func streamCollectedText(t *testing.T, body string) string {
	t.Helper()
	var b strings.Builder
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if !strings.HasPrefix(block, "data: ") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(block, "data: ")), &ev) != nil {
			continue
		}
		if ev["type"] == "response.output_text.delta" {
			d, _ := ev["delta"].(string)
			b.WriteString(d)
		}
	}
	return b.String()
}
