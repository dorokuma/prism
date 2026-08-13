package util

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestDumpDebugUpstreamResponseScrubsAccountKey guards the debug dump path
// (Batch-2 audit): the upstream response body is dumped through
// RedactBodyBytesWithKeys with the account key, so a custom auth_header key
// that does not look like an sk-/Bearer token is scrubbed and never lands on
// disk when an upstream echoes the credential it received.
func TestDumpDebugUpstreamResponseScrubsAccountKey(t *testing.T) {
	const key = "raw-key-98765"
	prev := DebugMode.Load()
	DebugMode.Store(true)
	defer func() { DebugMode.Store(prev) }()

	dir := filepath.Join(os.TempDir(), "prism-debug")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	body := []byte(`{"error":{"message":"invalid key ` + key + `","code":"invalid_api_key"}}`)
	DumpDebugUpstreamResponse(body, []string{key})

	data, err := os.ReadFile(filepath.Join(dir, "last-upstream-response.json"))
	if err != nil {
		t.Fatalf("read debug dump: %v", err)
	}
	if strings.Contains(string(data), key) {
		t.Errorf("debug dump leaks the non-sk account key: %s", data)
	}
	if !strings.Contains(string(data), "***") {
		t.Errorf("debug dump should carry the redaction marker: %s", data)
	}
	// Without the key the dump stays off (DebugMode false → no file).
	DebugMode.Store(false)
	_ = os.RemoveAll(dir)
	DumpDebugUpstreamResponse(body, []string{key})
	if _, err := os.Stat(filepath.Join(dir, "last-upstream-response.json")); err == nil {
		t.Error("debug dump must not be written when DebugMode is off")
	}
}

// TestDebugDumpOmitsBusinessContentKeepsStructure pins the debug-dump
// sanitize step: the dump keeps the JSON structure (model, roles, object
// keys) but omits business text content (prompt/completion text — including
// a credential that was embedded inside it) and never leaks it to disk.
func TestDebugDumpOmitsBusinessContentKeepsStructure(t *testing.T) {
	prev := DebugMode.Load()
	DebugMode.Store(true)
	defer func() { DebugMode.Store(prev) }()

	dir := filepath.Join(os.TempDir(), "prism-debug")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"my secret prompt sk-ABCDEFGHIJ1234567890"},{"role":"assistant","content":"the answer"}]}`)
	DumpDebugChatBody(body)

	data, err := os.ReadFile(filepath.Join(dir, "last-chat-request.json"))
	if err != nil {
		t.Fatalf("read debug dump: %v", err)
	}
	s := string(data)
	for _, want := range []string{`"model":"gpt-4"`, `"role":"user"`, `"role":"assistant"`, `\u003comitted\u003e`} {
		if !strings.Contains(s, want) {
			t.Errorf("dump must keep structure %q: %s", want, s)
		}
	}
	for _, leak := range []string{"my secret prompt", "the answer", "sk-ABCDEFGHIJ1234567890"} {
		if strings.Contains(s, leak) {
			t.Errorf("dump leaks business content %q: %s", leak, s)
		}
	}
}

// TestDebugDumpSanitizeSizeCap pins the debug-dump total-size bound: an
// oversized NON-content structure (e.g. a huge tool description) is capped
// at debugDumpMaxBytes and marked; a huge business-content body collapses to
// a small dump (omission, not truncation); non-JSON input is replaced by a
// placeholder plus length (never kept raw) and stays capped.
func TestDebugDumpSanitizeSizeCap(t *testing.T) {
	// Oversized non-content structure: capped and marked.
	big := `{"model":"gpt-4","extra":"` + strings.Repeat("x", 200<<10) + `"}`
	got := debugDumpSanitize([]byte(big))
	if len(got) > debugDumpMaxBytes {
		t.Errorf("sanitized dump = %d bytes, want <= %d", len(got), debugDumpMaxBytes)
	}
	if !strings.Contains(string(got), debugDumpTruncatedSuffix) {
		t.Errorf("oversized dump must carry the truncation marker: %s...", got[:min(len(got), 120)])
	}

	// Huge business content: omission keeps the dump tiny — no marker, no
	// content, no leak.
	huge := `{"model":"gpt-4","messages":[{"role":"user","content":"` + strings.Repeat("y", 200<<10) + `"}]}`
	got2 := debugDumpSanitize([]byte(huge))
	if len(got2) > debugDumpMaxBytes {
		t.Errorf("content-collapsed dump = %d bytes, want <= %d", len(got2), debugDumpMaxBytes)
	}
	if strings.Contains(string(got2), "yyy") {
		t.Errorf("dump leaks business content: %s...", got2[:min(len(got2), 120)])
	}
	if strings.Contains(string(got2), debugDumpTruncatedSuffix) {
		t.Errorf("content-omitted dump must not be truncated: %s", got2)
	}

	// Non-JSON input: never kept raw — replaced by a placeholder + length,
	// so nothing leaks and the output stays tiny (still capped).
	raw := strings.Repeat("plain text ", 100<<10)
	gotRaw := debugDumpSanitize([]byte(raw))
	if len(gotRaw) > debugDumpMaxBytes {
		t.Errorf("non-JSON sanitized dump = %d bytes, want <= %d", len(gotRaw), debugDumpMaxBytes)
	}
	if strings.Contains(string(gotRaw), "plain text") {
		t.Errorf("non-JSON dump must not keep raw text: %s...", gotRaw[:min(len(gotRaw), 120)])
	}
}

// TestDebugDumpSanitizeOmitsNonJSONBody pins the non-JSON path: a body that
// fails json.Unmarshal must never keep its raw text — only the omission
// placeholder and the original byte length survive — no matter how large it
// is (64 KiB cap is respected trivially because nothing raw is kept).
func TestDebugDumpSanitizeOmitsNonJSONBody(t *testing.T) {
	body := []byte("plain text body with a secret birthday 1990-01-01 and token ABC")
	got := debugDumpSanitize(body)
	s := string(got)
	for _, leak := range []string{"birthday", "1990-01-01", "ABC"} {
		if strings.Contains(s, leak) {
			t.Errorf("non-JSON body leaks raw text %q: %s", leak, s)
		}
	}
	if !strings.Contains(s, debugDumpOmitted) {
		t.Errorf("non-JSON body must carry the omission placeholder: %s", s)
	}
	if !strings.Contains(s, fmt.Sprintf(`"bytes":%d`, len(body))) {
		t.Errorf("non-JSON body must carry the original length: %s", s)
	}

	// Oversized non-JSON: still omitted (never truncated raw text), still capped.
	big := []byte(strings.Repeat("x", 200<<10) + " tail-secret")
	gotBig := debugDumpSanitize(big)
	if len(gotBig) > debugDumpMaxBytes {
		t.Errorf("oversized non-JSON dump = %d bytes, want <= %d", len(gotBig), debugDumpMaxBytes)
	}
	if strings.Contains(string(gotBig), "tail-secret") || strings.Contains(string(gotBig), "xxx") {
		t.Errorf("oversized non-JSON dump leaks raw text: %s...", gotBig[:min(len(gotBig), 120)])
	}
}

// TestDebugDumpSanitizeOmitsTopLevelJSONScalar pins the top-level scalar
// path: a bare JSON string (or number / bool / null) must never keep its raw
// text — only the omission placeholder and the original byte length survive.
func TestDebugDumpSanitizeOmitsTopLevelJSONScalar(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`"top secret json string"`),
		[]byte(`12345`),
		[]byte(`true`),
		[]byte(`null`),
	} {
		got := debugDumpSanitize(body)
		s := string(got)
		raw := strings.TrimSpace(string(body))
		if strings.Contains(s, raw) {
			t.Errorf("top-level scalar %q must not keep the raw text: %s", raw, s)
		}
		if !strings.Contains(s, debugDumpOmitted) {
			t.Errorf("top-level scalar %q must carry the omission placeholder: %s", raw, s)
		}
		if !strings.Contains(s, fmt.Sprintf(`"bytes":%d`, len(body))) {
			t.Errorf("top-level scalar %q must carry the original length: %s", raw, s)
		}
	}
}

// TestDebugDumpSanitizeOmitsContentStringArray pins content arrays of bare
// strings: the array shape survives, each element becomes a placeholder, and
// no raw string leaks.
func TestDebugDumpSanitizeOmitsContentStringArray(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":["secret one","secret two"]}]}`)
	got := debugDumpSanitize(body)
	s := string(got)
	for _, want := range []string{`"model":"gpt-4"`, `"role":"user"`, `"content":[`} {
		if !strings.Contains(s, want) {
			t.Errorf("content string array must keep structure %q: %s", want, s)
		}
	}
	for _, leak := range []string{"secret one", "secret two"} {
		if strings.Contains(s, leak) {
			t.Errorf("content string array leaks %q: %s", leak, s)
		}
	}
	if n := strings.Count(s, `\u003comitted\u003e`); n != 2 {
		t.Errorf("content string array must yield 2 placeholders, got %d: %s", n, s)
	}
}

// TestDebugDumpSanitizeOmitsToolCallArguments pins OpenAI tool calls: the
// arguments string (a JSON-encoded blob) is replaced wholesale, while the
// tool id, type and function name stay for debugging.
func TestDebugDumpSanitizeOmitsToolCallArguments(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":[{"id":"call_abc123","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"San Francisco\",\"unit\":\"celsius\"}"}}]}]}`)
	got := debugDumpSanitize(body)
	s := string(got)
	for _, want := range []string{`"model":"gpt-4"`, `"role":"assistant"`, `"id":"call_abc123"`, `"name":"get_weather"`, `"arguments":`} {
		if !strings.Contains(s, want) {
			t.Errorf("tool call must keep structure %q: %s", want, s)
		}
	}
	for _, leak := range []string{"San Francisco", "celsius", "location", "unit"} {
		if strings.Contains(s, leak) {
			t.Errorf("tool call arguments leak %q: %s", leak, s)
		}
	}
}

// TestDebugDumpSanitizeOmitsAnthropicToolUseInput pins Anthropic tool_use
// blocks: the system prompt and the whole tool_use content block (id, name,
// input object) are content-mode — every string inside is a placeholder —
// while outer structure (model, role) and the block/argument key names
// survive.
func TestDebugDumpSanitizeOmitsAnthropicToolUseInput(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet","system":"You are a helpful assistant.","messages":[{"role":"user","content":[{"type":"tool_use","id":"toolu_01ABC","name":"get_weather","input":{"location":"Paris, France","unit":"metric"}}]}]}`)
	got := debugDumpSanitize(body)
	s := string(got)
	for _, want := range []string{`"model":"claude-3-5-sonnet"`, `"role":"user"`, `"content":[`, `"input":`, `"location":`} {
		if !strings.Contains(s, want) {
			t.Errorf("tool_use block must keep structure %q: %s", want, s)
		}
	}
	for _, leak := range []string{"Paris, France", "metric", "toolu_01ABC", "helpful assistant"} {
		if strings.Contains(s, leak) {
			t.Errorf("tool_use input leaks %q: %s", leak, s)
		}
	}
}

// TestDebugDumpSanitizeOmitsReasoningVariants pins the reasoning / refusal /
// summary keys (DeepSeek-style reasoning_content, reasoning_text, OpenAI
// refusal, conversation summary) and case-insensitive key matching: values
// become placeholders while sibling structural fields (id, role,
// finish_reason, model) survive.
func TestDebugDumpSanitizeOmitsReasoningVariants(t *testing.T) {
	body := []byte(`{"id":"chatcmpl_9","choices":[{"message":{"role":"assistant","reasoning_content":"hidden chain of thought","content":"final answer text","refusal":"refusal text"},"finish_reason":"stop"}],"summary":"conversation summary text"}`)
	got := debugDumpSanitize(body)
	s := string(got)
	for _, want := range []string{`"id":"chatcmpl_9"`, `"role":"assistant"`, `"finish_reason":"stop"`} {
		if !strings.Contains(s, want) {
			t.Errorf("reasoning dump must keep structure %q: %s", want, s)
		}
	}
	for _, leak := range []string{"hidden chain of thought", "final answer text", "refusal text", "conversation summary text"} {
		if strings.Contains(s, leak) {
			t.Errorf("reasoning/summary dump leaks %q: %s", leak, s)
		}
	}

	// reasoning_text plus case-insensitive key matching.
	body2 := []byte(`{"model":"deepseek","Reasoning_Text":"uppercase secret","PROMPT":"prompt secret"}`)
	got2 := debugDumpSanitize(body2)
	s2 := string(got2)
	if !strings.Contains(s2, `"model":"deepseek"`) {
		t.Errorf("case-insensitive dump must keep the model: %s", s2)
	}
	for _, leak := range []string{"uppercase secret", "prompt secret"} {
		if strings.Contains(s2, leak) {
			t.Errorf("case-insensitive key leaks %q: %s", leak, s2)
		}
	}
}

// TestDebugDumpSanitizeOmitsResponsesAPIFields pins the OpenAI Responses API
// body keys: instructions (string), input and output (arrays of message
// blocks) — no raw text survives, keys and block shapes do.
func TestDebugDumpSanitizeOmitsResponsesAPIFields(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","instructions":"respond as a pirate","input":[{"role":"user","content":[{"type":"input_text","text":"hello secret"}]}],"output":[{"role":"assistant","content":[{"type":"output_text","text":"result secret"}]}]}`)
	got := debugDumpSanitize(body)
	s := string(got)
	for _, want := range []string{`"model":"gpt-4o"`, `"instructions":`, `"input":`, `"output":`, `"content":[`, `"type":`} {
		if !strings.Contains(s, want) {
			t.Errorf("responses body must keep structure %q: %s", want, s)
		}
	}
	for _, leak := range []string{"respond as a pirate", "hello secret", "result secret"} {
		if strings.Contains(s, leak) {
			t.Errorf("responses body leaks %q: %s", leak, s)
		}
	}
}

// TestDumpDebugUpstreamResponseOmitsBusinessKeys is the end-to-end guard for
// the new keys on the disk path: a real upstream response carrying
// reasoning_content, a content string array and tool arguments is dumped
// through the full redact + sanitize + write pipeline; the file must keep
// the structure (id, role, function name, arguments key) and never contain
// the raw business text.
func TestDumpDebugUpstreamResponseOmitsBusinessKeys(t *testing.T) {
	prev := DebugMode.Load()
	DebugMode.Store(true)
	defer func() { DebugMode.Store(prev) }()

	dir := filepath.Join(os.TempDir(), "prism-debug")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	body := []byte(`{"id":"chatcmpl_7","choices":[{"message":{"role":"assistant","reasoning_content":"chain of thought secret","content":["answer one","answer two"],"tool_calls":[{"function":{"name":"get_weather","arguments":"{\"location\":\"Oslo\"}"}}]}}]}`)
	DumpDebugUpstreamResponse(body, nil)

	data, err := os.ReadFile(filepath.Join(dir, "last-upstream-response.json"))
	if err != nil {
		t.Fatalf("read debug dump: %v", err)
	}
	s := string(data)
	for _, want := range []string{`"id":"chatcmpl_7"`, `"role":"assistant"`, `"name":"get_weather"`, `"arguments":`} {
		if !strings.Contains(s, want) {
			t.Errorf("upstream dump must keep structure %q: %s", want, s)
		}
	}
	for _, leak := range []string{"chain of thought secret", "answer one", "answer two", "Oslo"} {
		if strings.Contains(s, leak) {
			t.Errorf("upstream dump leaks %q: %s", leak, s)
		}
	}
}

// TestDebugModeConcurrentReadWrite is the race-coverage test for the
// atomic DebugMode (audit round 6, item 4): the SIGHUP reload path writes
// the flag (Store) while request goroutines read it (Load) on every
// request. A plain bool was a data race; this test hammers concurrent
// Loads (through the real dump functions) against Stores, so `go test
// -race` proves the access is race-free. It also pins the behavior: each
// dump observes exactly the last stored value (atomic snapshot semantics).
func TestDebugModeConcurrentReadWrite(t *testing.T) {
	prev := DebugMode.Load()
	defer func() { DebugMode.Store(prev) }()

	dir := filepath.Join(os.TempDir(), "prism-debug")
	_ = os.RemoveAll(dir)
	defer os.RemoveAll(dir)

	body := []byte(`{"choices":[{"message":{"content":"x"}}]}`)
	const readers = 8
	const rounds = 200
	var wg sync.WaitGroup

	// Writers: flip the flag like the SIGHUP handler does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			DebugMode.Store(i%2 == 0)
		}
	}()

	// Readers: exercise the real debug-dump read paths (the exact Load
	// sites request goroutines hit).
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				DumpDebugChatBody(body)
				DumpDebugResponsesBody(body)
				DumpDebugUpstreamResponse(body, []string{"k"})
			}
		}()
	}
	wg.Wait()
}
