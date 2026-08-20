package dsml

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func officialBlock(name, city string, days int) string {
	return "<" + dsmlToken + "tool_calls>\n" +
		"<" + dsmlToken + "invoke name=\"" + name + "\">\n" +
		"<" + dsmlToken + "parameter name=\"city\" string=\"true\">" + city + "</" + dsmlToken + "parameter>\n" +
		"<" + dsmlToken + "parameter name=\"days\" string=\"false\">" + strconv.Itoa(days) + "</" + dsmlToken + "parameter>\n" +
		"</" + dsmlToken + "invoke>\n" +
		"</" + dsmlToken + "tool_calls>"
}

func TestQuickCheckAndFence(t *testing.T) {
	if QuickCheck("hello world") {
		t.Fatal("plain text must not trip QuickCheck")
	}
	sample := "<" + dsmlToken + "toolpending leftover"
	if !HasMarker(sample) {
		t.Fatal("fullwidth DSML tag must match")
	}
	if HasMarker("the DSML spec is unrelated") {
		t.Fatal("bare DSML without bars must not match")
	}
	fenced := "before\n```\n<" + dsmlToken + "invoke name=\"x\">\n```\nafter"
	if HasMarker(fenced) {
		t.Fatal("marker inside code fence must be skipped")
	}
	if !HasMarker("| DSML |toolpending") {
		t.Fatal("halfwidth spaced bars must match")
	}
	if !HasMarker("｜｜DSML｜｜tool_calls>") {
		t.Fatal("double fullwidth bars must match")
	}
	if !HasMarker("xxDSML｜yy") {
		t.Fatal("DSML｜ variant must match")
	}
}

func TestRecoverOfficial(t *testing.T) {
	block := officialBlock("get_weather", "Shanghai", 3)
	calls, ok := RecoverInvokes(block)
	if !ok {
		t.Fatalf("recover failed on official block:\n%s", block)
	}
	if len(calls) != 1 {
		t.Fatalf("calls=%d want 1", len(calls))
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("name=%q", calls[0].Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments json: %v %s", err, calls[0].Arguments)
	}
	if args["city"] != "Shanghai" {
		t.Errorf("city=%v", args["city"])
	}
	switch v := args["days"].(type) {
	case float64:
		if v != 3 {
			t.Errorf("days=%v", v)
		}
	default:
		t.Errorf("days type %T val %v", v, v)
	}
}

func TestRecoverAbortedSample(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "dsml-sample-0708-L59-prefix.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := RecoverInvokes(string(b)); ok {
		t.Fatal("aborted sample must not recover an invoke")
	}
}

func TestRewriteStreamPassthroughByteIdentical(t *testing.T) {
	in, err := os.ReadFile(filepath.Join("testdata", "clinepass-dsml-raw-stream.txt"))
	if err != nil {
		t.Fatal(err)
	}
	out := RewriteStream(in)
	if !bytes.Equal(in, out) {
		t.Fatalf("passthrough mutated stream: in=%d out=%d", len(in), len(out))
	}
}

func TestRewriteStreamAbortedNoDSMLNoCoT(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "dsml-sample-0708-L59-prefix.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 2000 {
		t.Fatalf("prefix testdata too short: %d", len(raw))
	}
	text := raw[:2000]
	var buf bytes.Buffer
	buf.Write(sseJSON(`{"id":"gen_abort","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	buf.Write(sseJSON(`{"id":"gen_abort","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"reasoning":"plan the call"},"finish_reason":null}]}`))
	// Split the leak so hold-back and cross-chunk detection both run.
	mid := 80
	buf.Write(sseContent("gen_abort", text[:mid]))
	buf.Write(sseContent("gen_abort", text[mid:]))
	buf.Write(sseJSON(`{"id":"gen_abort","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	buf.WriteString("data: [DONE]\n\n")

	out := RewriteStream(buf.Bytes())
	outs := string(out)
	if HasMarker(outs) || strings.Contains(strings.ToLower(outs), "dsml") && strings.Contains(outs, "toolpending") {
		t.Fatalf("output still has DSML: %s", truncate(outs, 400))
	}
	if bytes.Contains(out, []byte("serializedRunner")) || bytes.Contains(out, []byte("persistCacheStats")) {
		t.Fatal("CoT body leaked into output")
	}
	if !strings.Contains(outs, "[prism] removed ") || !strings.Contains(outs, "aborted DSML output") {
		t.Fatalf("missing notice: %s", truncate(outs, 400))
	}
	if !strings.Contains(outs, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason not preserved: %s", truncate(outs, 800))
	}
	if !strings.Contains(outs, `"reasoning":"plan the call"`) {
		t.Fatal("reasoning delta must pass through in guard mode")
	}
	if !bytes.Contains(out, []byte("data: [DONE]")) {
		t.Fatal("missing [DONE]")
	}
}

func TestRewriteStreamRecoverUTF8Cuts(t *testing.T) {
	block := []byte(officialBlock("get_weather", "Shanghai", 3))
	bar := []byte(fullwidthBar)
	cuts := []int{1}
	for i := 0; i+len(bar) <= len(block); i++ {
		if bytes.Equal(block[i:i+len(bar)], bar) {
			cuts = append(cuts, i, i+1, i+2)
		}
	}
	// Every byte boundary, including the first fullwidth bar's 3 bytes.
	for i := 1; i < len(block); i++ {
		cuts = append(cuts, i)
	}

	seen := map[int]struct{}{}
	for _, cut := range cuts {
		if cut <= 0 || cut >= len(block) {
			continue
		}
		if _, ok := seen[cut]; ok {
			continue
		}
		seen[cut] = struct{}{}
		left, right := block[:cut], block[cut:]
		var buf bytes.Buffer
		buf.Write(sseJSON(`{"id":"gen_rec","object":"chat.completion.chunk","created":9,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
		buf.Write(sseContent("gen_rec", left))
		buf.Write(sseContent("gen_rec", right))
		buf.Write(sseJSON(`{"id":"gen_rec","object":"chat.completion.chunk","created":9,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
		buf.WriteString("data: [DONE]\n\n")
		out := RewriteStream(buf.Bytes())
		assertRecoveredWeather(t, out, cut)
	}
}

func TestRewriteStreamRecoverSingleBytes(t *testing.T) {
	block := []byte(officialBlock("get_weather", "Shanghai", 3))
	var buf bytes.Buffer
	buf.Write(sseJSON(`{"id":"gen_b","object":"chat.completion.chunk","created":9,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`))
	for i := 0; i < len(block); i++ {
		buf.Write(sseContent("gen_b", block[i:i+1]))
	}
	buf.Write(sseJSON(`{"id":"gen_b","object":"chat.completion.chunk","created":9,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	out := RewriteStream(buf.Bytes())
	assertRecoveredWeather(t, out, -1)
}

func TestRewriteCompletionAborted(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "dsml-sample-0708-L59-prefix.txt"))
	if err != nil {
		t.Fatal(err)
	}
	body := completionJSON(string(raw[:2000]), "length")
	out := RewriteCompletion(body)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	ch := parsed["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	content, _ := msg["content"].(string)
	if HasMarker(content) {
		t.Fatal("non-stream abort still has DSML")
	}
	if !strings.Contains(content, "[prism] removed ") {
		t.Fatalf("content=%q", content)
	}
	if ch["finish_reason"] != "length" {
		t.Fatalf("finish_reason=%v want length", ch["finish_reason"])
	}
}

func TestRewriteCompletionWrappedDataEnvelope(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "dsml-sample-0708-L59-prefix.txt"))
	if err != nil {
		t.Fatal(err)
	}
	inner := completionJSON(string(raw[:2000]), "length")
	wrapped, err := json.Marshal(map[string]any{
		"success": true,
		"data":    json.RawMessage(inner),
	})
	if err != nil {
		t.Fatal(err)
	}
	out := RewriteCompletion(wrapped)
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.Success {
		t.Fatal("wrapper success flag must be kept")
	}
	var parsed map[string]any
	if err := json.Unmarshal(env.Data, &parsed); err != nil {
		t.Fatal(err)
	}
	ch := parsed["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	content, _ := msg["content"].(string)
	if HasMarker(content) || strings.Contains(content, "toolpending") {
		t.Fatal("wrapped abort still has DSML")
	}
	if !strings.Contains(content, "[prism] removed ") {
		t.Fatalf("content=%q", content)
	}
	if ch["finish_reason"] != "length" {
		t.Fatalf("finish_reason=%v", ch["finish_reason"])
	}
}

func TestRewriteCompletionRecover(t *testing.T) {
	block := officialBlock("get_weather", "Shanghai", 3)
	body := completionJSON(block, "stop")
	out := RewriteCompletion(body)
	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason=%q", parsed.Choices[0].FinishReason)
	}
	tc := parsed.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].Function.Name != "get_weather" {
		t.Fatalf("tool_calls=%+v", tc)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc[0].Function.Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["city"] != "Shanghai" {
		t.Errorf("city=%v", args["city"])
	}
}

func TestRewriteCompletionPassthrough(t *testing.T) {
	body := []byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	out := RewriteCompletion(body)
	if !bytes.Equal(body, out) {
		t.Fatalf("mutated clean body:\n%s\n%s", body, out)
	}
}

func TestHoldLenUTF8Bar(t *testing.T) {
	bar := fullwidthBar
	if utf8.RuneCountInString(bar) != 1 || len(bar) != 3 {
		t.Fatalf("fullwidth bar encoding: %q len=%d", bar, len(bar))
	}
	if holdLen(bar[:1]) != 1 || holdLen(bar[:2]) != 2 {
		t.Fatalf("truncated bar must be held: %d %d", holdLen(bar[:1]), holdLen(bar[:2]))
	}
	if holdLen("hello") != 0 {
		t.Fatalf("hello hold=%d", holdLen("hello"))
	}
	if holdLen("hello"+bar[:1]) != 1 {
		t.Fatalf("suffix truncated bar")
	}
}

func assertRecoveredWeather(t *testing.T, out []byte, cut int) {
	t.Helper()
	outs := string(out)
	if HasMarker(outs) && strings.Contains(outs, "tool_calls") && strings.Contains(outs, dsmlToken) {
		// tool_calls in OpenAI JSON is fine; DSML token must not remain.
	}
	if strings.Contains(outs, dsmlToken) || strings.Contains(outs, "toolpending") {
		t.Fatalf("cut=%d DSML leaked: %s", cut, truncate(outs, 300))
	}
	if !strings.Contains(outs, `"finish_reason":"tool_calls"`) {
		t.Fatalf("cut=%d finish_reason not tool_calls: %s", cut, truncate(outs, 800))
	}
	if !strings.Contains(outs, `"name":"get_weather"`) {
		t.Fatalf("cut=%d missing name: %s", cut, truncate(outs, 800))
	}
	if !strings.Contains(outs, `Shanghai`) || !strings.Contains(outs, `days`) {
		t.Fatalf("cut=%d missing arguments: %s", cut, truncate(outs, 800))
	}
	var argsOK bool
	for _, line := range strings.Split(outs, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
			continue
		}
		if len(chunk.Choices) == 0 || len(chunk.Choices[0].Delta.ToolCalls) == 0 {
			continue
		}
		fn := chunk.Choices[0].Delta.ToolCalls[0].Function
		if fn.Name != "get_weather" {
			continue
		}
		var args map[string]any
		if json.Unmarshal([]byte(fn.Arguments), &args) != nil {
			t.Fatalf("cut=%d arguments not JSON: %s", cut, fn.Arguments)
		}
		if args["city"] != "Shanghai" {
			t.Fatalf("cut=%d city=%v", cut, args["city"])
		}
		days, _ := args["days"].(float64)
		if days != 3 {
			t.Fatalf("cut=%d days=%v", cut, args["days"])
		}
		argsOK = true
	}
	if !argsOK {
		t.Fatalf("cut=%d no recovered tool_calls arguments", cut)
	}
}

func sseJSON(s string) []byte {
	return []byte("data: " + s + "\n\n")
}

func sseContent(id string, content []byte) []byte {
	var b bytes.Buffer
	b.WriteString(`data: {"id":"`)
	b.WriteString(id)
	b.WriteString(`","object":"chat.completion.chunk","created":9,"model":"m","choices":[{"index":0,"delta":{"content":`)
	b.Write(encodeJSONString(content))
	b.WriteString(`},"finish_reason":null}]}` + "\n\n")
	return b.Bytes()
}

func completionJSON(content, finish string) []byte {
	raw := map[string]any{
		"id":      "c1",
		"object":  "chat.completion",
		"created": 1,
		"model":   "m",
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
	b, _ := json.Marshal(raw)
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
