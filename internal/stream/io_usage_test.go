package stream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/usagemeta"
)

// ---------------------------------------------------------------------------
// usageEventCapture bounds
// ---------------------------------------------------------------------------

// TestUsageEventCapture_EventBounded exercises the SSE-event-aware capture:
// events split across Write boundaries are assembled, the HEAD events and
// the RECENT events are kept, and the recent buffer never exceeds its byte
// bound (oldest events dropped first, never a partial event).
func TestUsageEventCapture_EventBounded(t *testing.T) {
	c := newUsageEventCapture(64, 128, 50)

	// Three events; the second is written in two fragments (crosses a Write
	// boundary), the first and third are each written whole.
	part1 := "event: e1\ndata: {\"a\":1}\n\nevent: e2\ndata: {\"b\":"
	part2 := "2}\n\nevent: e3\ndata: {\"c\":3}\n\n"
	if _, err := c.Write([]byte(part1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte(part2)); err != nil {
		t.Fatal(err)
	}

	head := string(c.headEvents())
	if !strings.Contains(head, "\"a\":1") || !strings.Contains(head, "\"b\":2") || !strings.Contains(head, "\"c\":3") {
		t.Errorf("head = %q, want all three events (head window large enough)", head)
	}
	got := string(c.recentEvents())
	// recent bound is 50 bytes: e1+e2 (44 bytes) fit, adding e3 (67 total)
	// overflows → e1 is dropped, e2+e3 remain.
	if !strings.Contains(got, "\"b\":2") || !strings.Contains(got, "\"c\":3") {
		t.Errorf("recent = %q, want e2+e3 (oldest dropped under the bound)", got)
	}
	if strings.Contains(got, "\"a\":1") {
		t.Errorf("recent = %q, must drop the oldest event under the byte bound", got)
	}
}

// TestUsageEventCapture_ShortStream: a stream shorter than one event
// captures nothing until Finish (pending is not a complete event), and a
// single tiny event becomes both head and recent. After Finish the final
// partial event IS captured — an SSE stream is not required to end with an
// empty line (EOF-without-terminator semantics).
func TestUsageEventCapture_ShortStream(t *testing.T) {
	c := newUsageEventCapture(64, 128, 40)
	if _, err := c.Write([]byte("tiny")); err != nil {
		t.Fatal(err)
	}
	if len(c.headEvents()) != 0 || len(c.recentEvents()) != 0 {
		t.Errorf("incomplete trailing event must not be captured before Finish: head=%q recent=%q", c.headEvents(), c.recentEvents())
	}
	// EOF: the partial event is the final event and must be captured.
	c.Finish()
	if got := string(c.headEvents()); got != "tiny" {
		t.Errorf("head after Finish = %q, want the final partial event", got)
	}
	if got := string(c.recentEvents()); got != "tiny" {
		t.Errorf("recent after Finish = %q, want the same event", got)
	}

	c2 := newUsageEventCapture(64, 128, 40)
	if _, err := c2.Write([]byte("data: {\"a\":1}\n\n")); err != nil {
		t.Fatal(err)
	}
	if got := string(c2.headEvents()); got != "data: {\"a\":1}" {
		t.Errorf("head = %q, want the complete event", got)
	}
	if got := string(c2.recentEvents()); got != "data: {\"a\":1}" {
		t.Errorf("recent = %q, want the same event", got)
	}
	// Finish on an empty pending is a no-op (already terminated stream).
	c2.Finish()
	if got := string(c2.headEvents()); got != "data: {\"a\":1}" {
		t.Errorf("head after Finish on terminated stream = %q, want unchanged", got)
	}
}

// TestUsageEventCapture_CRLFNormalized: CRLF streams ("\r\n" line endings,
// "\r\n\r\n" blank lines) must be captured exactly like LF streams — the
// old capture split on "\n\n" only, so a CRLF stream never terminated an
// event and the whole stream was skipped as one oversize event. Also
// covers a "\r\n" pair SPLIT across two Write calls (chunk ends with '\r').
func TestUsageEventCapture_CRLFNormalized(t *testing.T) {
	c := newUsageEventCapture(64, 128, 64)
	// CRLF events; the first event's blank line is split across Writes
	// ("...\r" | "\n\r\n...").
	if _, err := c.Write([]byte("data: {\"a\":1}\r")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("\n\r\ndata: {\"b\":2}\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	head := string(c.headEvents())
	if !strings.Contains(head, "\"a\":1") || !strings.Contains(head, "\"b\":2") {
		t.Errorf("head = %q, want both CRLF events (split pair included)", head)
	}
	// No raw CR may survive into the retained buffers (they would corrupt
	// the JSON parse / boundary split).
	if strings.Contains(head, "\r") {
		t.Errorf("head = %q, must be LF-normalized (no raw CR)", head)
	}
	// A pure-CRLF stream ending WITHOUT a blank line: the final event is
	// captured at Finish.
	c2 := newUsageEventCapture(64, 128, 64)
	if _, err := c2.Write([]byte("data: {\"a\":1}\r\n")); err != nil {
		t.Fatal(err)
	}
	c2.Finish()
	if got := string(c2.headEvents()); got != "data: {\"a\":1}" {
		t.Errorf("head after Finish on CRLF EOF = %q, want the final event", got)
	}
}

// TestUsageEventCapture_EOFWithoutBlankLine: the final SSE event is not
// required to end with an empty line — the last event (often the OpenAI
// usage chunk) must be captured by Finish instead of staying in pending
// forever.
func TestUsageEventCapture_EOFWithoutBlankLine(t *testing.T) {
	c := newUsageEventCapture(64, 128, 64)
	if _, err := c.Write([]byte("data: {\"a\":1}\n\ndata: {\"b\":2}\n")); err != nil {
		t.Fatal(err)
	}
	// The first event is complete; the second is pending.
	if got := string(c.headEvents()); got != "data: {\"a\":1}" {
		t.Errorf("head before Finish = %q, want only the complete event", got)
	}
	c.Finish()
	if got := string(c.headEvents()); got != "data: {\"a\":1}\n\ndata: {\"b\":2}" {
		t.Errorf("head after Finish = %q, want both events (final partial captured)", got)
	}
}

// TestUsageEventCapture_ChunkSplitEverywhere is a property test: the SSE
// stream is fed through the capture at EVERY possible single split point
// and in per-byte chunks; the retained head/recent must be identical in
// every case — arbitrary chunk cutting must not change the capture.
//
// The event cap must exceed every event in the stream (~79-105 bytes) and
// the head/recent windows must hold several events: with the previous
// parameters (64/128/64) every event was oversize and skipped, so the
// whole-stream reference was EMPTY and every split compared ""=="" — the
// test passed without exercising chunk splitting at all (vacuous pass).
func TestUsageEventCapture_ChunkSplitEverywhere(t *testing.T) {
	stream := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n"

	reference := func(c *usageEventCapture) (string, string) {
		c.Finish()
		return string(c.headEvents()), string(c.recentEvents())
	}

	// Whole stream in one Write.
	cWhole := newUsageEventCapture(128, 256, 256)
	if _, err := cWhole.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	wantHead, wantRecent := reference(cWhole)

	// Precondition: the reference capture must be NON-EMPTY and correct.
	// With an event cap of 64 bytes every event in this stream (~79-105
	// bytes) is oversize and skipped, so head/recent are empty and every
	// split-point comparison below compares ""=="" — the test would pass
	// without exercising chunk splitting at all.
	if wantHead == "" || wantRecent == "" {
		t.Fatalf("reference capture is empty (head=%q recent=%q): every stream event exceeds the event cap, all split comparisons would be vacuous", wantHead, wantRecent)
	}
	// The retained head/recent must carry the expected payloads: two in
	// each window (head: message_start + first content delta; recent: last
	// content delta + message_delta).
	wantHeadPayloads := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
	}
	headPayloads := sseDataPayloads([]byte(wantHead))
	if len(headPayloads) != len(wantHeadPayloads) {
		t.Fatalf("head payloads = %d (%q), want %d", len(headPayloads), headPayloads, len(wantHeadPayloads))
	}
	for i := range wantHeadPayloads {
		if string(headPayloads[i]) != wantHeadPayloads[i] {
			t.Errorf("head payload[%d] = %q, want %q", i, headPayloads[i], wantHeadPayloads[i])
		}
	}
	wantRecentPayloads := []string{
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"message_delta","usage":{"output_tokens":3}}`,
	}
	recentPayloads := sseDataPayloads([]byte(wantRecent))
	if len(recentPayloads) != len(wantRecentPayloads) {
		t.Fatalf("recent payloads = %d (%q), want %d", len(recentPayloads), recentPayloads, len(wantRecentPayloads))
	}
	for i := range wantRecentPayloads {
		if string(recentPayloads[i]) != wantRecentPayloads[i] {
			t.Errorf("recent payload[%d] = %q, want %q", i, recentPayloads[i], wantRecentPayloads[i])
		}
	}
	// The reference must yield the stream's usage when parsed.
	u := parseStreamUsage([]byte(wantHead), []byte(wantRecent))
	wantUsage := usagemeta.Usage{Prompt: 10, Completion: 3, Total: 13, Source: usagemeta.SourceAnthropic}
	if u != wantUsage {
		t.Fatalf("reference usage = %+v, want %+v", u, wantUsage)
	}

	// Every single split point.
	for i := 0; i < len(stream); i++ {
		c := newUsageEventCapture(128, 256, 256)
		if _, err := c.Write([]byte(stream[:i])); err != nil {
			t.Fatalf("split %d first half: %v", i, err)
		}
		if _, err := c.Write([]byte(stream[i:])); err != nil {
			t.Fatalf("split %d second half: %v", i, err)
		}
		h, r := reference(c)
		if h != wantHead || r != wantRecent {
			t.Fatalf("split %d changed the capture:\n head  want %q got %q\n recent want %q got %q",
				i, wantHead, h, wantRecent, r)
		}
	}

	// One byte per Write.
	c := newUsageEventCapture(128, 256, 256)
	for i := 0; i < len(stream); i++ {
		if _, err := c.Write([]byte(stream[i : i+1])); err != nil {
			t.Fatal(err)
		}
	}
	h, r := reference(c)
	if h != wantHead || r != wantRecent {
		t.Errorf("per-byte feeding changed the capture:\n head  want %q got %q\n recent want %q got %q", wantHead, h, wantRecent, r)
	}

	// CRLF variant: same stream with "\r\n" line endings and "\r\n\r\n"
	// blank lines, one byte per Write (exercises the split-CRLF carry). A
	// SINGLE "\n"→"\r\n" replacement converts both at once; the old
	// two-step replacement ("\n\n"→"\r\n\r\n" first, then "\n"→"\r\n")
	// re-replaced the "\n" inside the freshly inserted "\r\n" pairs and
	// turned every blank line into "\r\r\n\r\r\n" (four newlines after
	// normalization → spurious empty events) — masked by the empty
	// reference.
	crlf := strings.ReplaceAll(stream, "\n", "\r\n")
	cCrlf := newUsageEventCapture(128, 256, 256)
	for i := 0; i < len(crlf); i++ {
		if _, err := cCrlf.Write([]byte(crlf[i : i+1])); err != nil {
			t.Fatal(err)
		}
	}
	h, r = reference(cCrlf)
	if h != wantHead || r != wantRecent {
		t.Errorf("per-byte CRLF feeding changed the capture:\n head  want %q got %q\n recent want %q got %q", wantHead, h, wantRecent, r)
	}
}

// TestUsageEventCapture_OversizeThenRecovery: after an over-cap event the
// capture must recover — later normal events are still cached — and the
// oversize event's bytes must never reach head/recent. Also covers the
// loop-before-trim ordering: a huge single Write whose boundary sits deep
// inside the chunk must still split correctly (the old trim-first order
// could discard the boundary and mislabel the following event).
func TestUsageEventCapture_OversizeThenRecovery(t *testing.T) {
	c := newUsageEventCapture(64, 128, 4096)
	big := "data: {\"content\":\"" + strings.Repeat("x", 200) + "\"}\n\n"
	if _, err := c.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if len(c.headEvents()) != 0 {
		t.Errorf("oversize first event must be skipped, got head=%q", c.headEvents())
	}
	if _, err := c.Write([]byte("data: {\"usage\":{\"prompt_tokens\":1}}\n\n")); err != nil {
		t.Fatal(err)
	}
	if got := string(c.headEvents()); !strings.Contains(got, "usage") {
		t.Errorf("event after an oversize event must be captured, got %q", got)
	}
	if strings.Contains(string(c.recentEvents()), "xxxxx") {
		t.Errorf("oversize event bytes must never reach the recent buffer, got %q", c.recentEvents())
	}

	// A huge single Write containing a boundary DEEP inside: the boundary
	// must be consumed before the oversize trim (trim-first would cut the
	// boundary away and mislabel the trailing normal event as oversize).
	c2 := newUsageEventCapture(64, 128, 4096)
	huge := strings.Repeat("y", 5000)
	// One Write: oversize event's tail + boundary + small normal event.
	chunk := huge + "\n\ndata: {\"usage\":{\"prompt_tokens\":7}}\n\n"
	if _, err := c2.Write([]byte(chunk)); err != nil {
		t.Fatal(err)
	}
	// The oversize event must be skipped; the small event after the deep
	// boundary must be captured.
	if got := string(c2.recentEvents()); !strings.Contains(got, "prompt_tokens\":7") {
		t.Errorf("event after a deep boundary inside a huge Write must be captured, got %q", c2.recentEvents())
	}
	if strings.Contains(string(c2.recentEvents()), "yyyyy") {
		t.Errorf("oversize bytes must never reach the recent buffer, got %q", c2.recentEvents())
	}

	// Bounds hold under pathological input: pending is trimmed to the cap,
	// head/recent stay within their windows (recent may hold one oversized
	// event that cannot be split).
	if len(c2.pending) > c2.maxEventBytes {
		t.Errorf("pending = %d bytes, want <= maxEventBytes %d", len(c2.pending), c2.maxEventBytes)
	}
	if len(c2.head) > c2.headBytes {
		t.Errorf("head = %d bytes, want <= headBytes %d", len(c2.head), c2.headBytes)
	}
	if len(c2.recent) > c2.recentBytes+c2.maxEventBytes {
		t.Errorf("recent = %d bytes, want <= recentBytes+maxEventBytes %d", len(c2.recent), c2.recentBytes+c2.maxEventBytes)
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

// TestParseStreamUsage_AnthropicDetectedWithSpacesAndFieldOrder guards the
// wire-format detection against formatting: the message_start type is read
// from the PARSED data JSON, so `"type": "message_start"` (space after the
// colon) and reordered fields are detected exactly like the compact form — a
// raw byte-substring match for `"type":"message_start"` would miss both
// and misroute the stream to the OpenAI parser (zero usage).
func TestParseStreamUsage_AnthropicDetectedWithSpacesAndFieldOrder(t *testing.T) {
	tail := []byte("event: message_delta\ndata: {\"type\": \"message_delta\", \"usage\": {\"output_tokens\": 3}}\n\n")
	cases := [][]byte{
		// Space after the colon (valid JSON, defeats a no-space byte match).
		[]byte("event: message_start\ndata: {\"type\": \"message_start\", \"message\": {\"usage\": {\"input_tokens\": 10}}}\n\n"),
		// Field order: type after message (also defeats a prefix byte match).
		[]byte("event: message_start\ndata: {\"message\": {\"usage\": {\"input_tokens\": 10}}, \"type\": \"message_start\"}\n\n"),
	}
	for i, head := range cases {
		u := parseStreamUsage(head, tail)
		want := usagemeta.Usage{Prompt: 10, Completion: 3, Total: 13, Source: usagemeta.SourceAnthropic}
		if u != want {
			t.Errorf("head %d: parseStreamUsage = %+v, want %+v (anthropic path with whitespace/reordered fields)", i, u, want)
		}
	}
}

// TestParseStreamUsage_AnthropicWithPreamble guards the wire-format
// detection against events that precede message_start: a proxy/upstream may
// emit a ping event (or any other parseable event) before the Anthropic
// message_start, and the detector must scan ALL data payloads instead of
// returning false on the first parseable non-message_start event.
func TestParseStreamUsage_AnthropicWithPreamble(t *testing.T) {
	tail := []byte("event: message_delta\ndata: {\"type\": \"message_delta\", \"usage\": {\"output_tokens\": 3}}\n\n")
	heads := [][]byte{
		// Parseable preamble: Anthropic ping event before message_start.
		[]byte("event: ping\ndata: {\"type\": \"ping\"}\n\nevent: message_start\ndata: {\"type\": \"message_start\", \"message\": {\"usage\": {\"input_tokens\": 10}}}\n\n"),
		// Unparseable preamble: a data line that is not valid JSON must be
		// skipped, not treated as the answer.
		[]byte("data: keepalive-not-json\n\nevent: message_start\ndata: {\"type\": \"message_start\", \"message\": {\"usage\": {\"input_tokens\": 10}}}\n\n"),
		// Non-Anthropic first event: an OpenAI-shaped chunk before the
		// message_start must not misroute the stream.
		[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\nevent: message_start\ndata: {\"type\": \"message_start\", \"message\": {\"usage\": {\"input_tokens\": 10}}}\n\n"),
	}
	for i, head := range heads {
		u := parseStreamUsage(head, tail)
		want := usagemeta.Usage{Prompt: 10, Completion: 3, Total: 13, Source: usagemeta.SourceAnthropic}
		if u != want {
			t.Errorf("head %d: parseStreamUsage = %+v, want %+v (anthropic path despite preamble)", i, u, want)
		}
	}
}

// TestParseStreamUsage_MultiLineData guards the SSE multi-line data rule: a
// single event whose data field spans several "data:" lines is joined with
// "\n" (per the SSE spec) BEFORE JSON parsing, so a message_start split
// across lines is still detected, and a message_delta split across lines
// still yields output tokens.
func TestParseStreamUsage_MultiLineData(t *testing.T) {
	// message_start split across three data: lines (valid JSON once joined).
	head := []byte("event: message_start\n" +
		"data: {\"type\": \"message_start\",\n" +
		"data: \"message\": {\"usage\": {\"input_tokens\": 10}},\n" +
		"data: \"id\": \"msg_1\"}\n\n")
	// message_delta split across two data: lines.
	tail := []byte("event: message_delta\n" +
		"data: {\"type\": \"message_delta\",\n" +
		"data: \"usage\": {\"output_tokens\": 3}}\n\n")
	u := parseStreamUsage(head, tail)
	want := usagemeta.Usage{Prompt: 10, Completion: 3, Total: 13, Source: usagemeta.SourceAnthropic}
	if u != want {
		t.Errorf("parseStreamUsage(multi-line data) = %+v, want %+v", u, want)
	}
}

// TestSSEDataPayloads_MergesMultiLineData is a direct unit test of the SSE
// data: extraction: multi-line data within one event is merged into a single
// payload (joined with "\n"), each event yields at most one payload, and
// events without data lines are skipped.
func TestSSEDataPayloads_MergesMultiLineData(t *testing.T) {
	buf := []byte("event: ping\ndata: {\"type\": \"ping\"}\n\n" +
		"event: message_start\ndata: {\"type\": \"message_start\",\ndata: \"message\": {}}\n\n" +
		": keep-alive comment\n\n" +
		"data: [DONE]\n\n")
	payloads := sseDataPayloads(buf)
	if len(payloads) != 2 {
		t.Fatalf("sseDataPayloads returned %d payloads, want 2", len(payloads))
	}
	if string(payloads[0]) != `{"type": "ping"}` {
		t.Errorf("payload[0] = %q, want the ping JSON", payloads[0])
	}
	if string(payloads[1]) != "{\"type\": \"message_start\",\n\"message\": {}}" {
		t.Errorf("payload[1] = %q, want the merged multi-line data", payloads[1])
	}
}

// TestSSEDataPayloads_EmptyDataLinesPerSpec pins the SSE empty-data-line
// rule: per the spec an empty data: line is still a data line and
// contributes an empty fragment to the joined payload (data lines are
// joined with "\n"), so `data: a` + `data:` + `data: b` yields "a\n\nb" —
// the parser must not silently drop the empty line and produce "a\nb"
// (which would change the JSON). Events whose ONLY data line is empty still
// produce no payload.
func TestSSEDataPayloads_EmptyDataLinesPerSpec(t *testing.T) {
	buf := []byte(
		"event: e1\n" +
			"data: {\"a\":1,\n" +
			"data:\n" +
			"data: \"b\":2}\n\n" +
			"data:\n\n" + // event with only an empty data line → no payload
			"data: [DONE]\n\n")
	payloads := sseDataPayloads(buf)
	if len(payloads) != 1 {
		t.Fatalf("sseDataPayloads returned %d payloads, want 1", len(payloads))
	}
	want := "{\"a\":1,\n\n\"b\":2}"
	if string(payloads[0]) != want {
		t.Errorf("payload = %q, want %q (empty data line must join as an empty fragment, not be dropped)", payloads[0], want)
	}
}

// TestParseStreamUsage_AnthropicWithEmptyDataLine verifies empty data lines
// inside an event do not break usage parsing: the message_start payload
// joined per spec (with the empty fragment) still parses and yields the
// input tokens.
func TestParseStreamUsage_AnthropicWithEmptyDataLine(t *testing.T) {
	head := []byte("event: message_start\n" +
		"data: {\"type\": \"message_start\",\n" +
		"data:\n" +
		"data: \"message\": {\"usage\": {\"input_tokens\": 10}}}\n\n")
	tail := []byte("event: message_delta\ndata: {\"type\": \"message_delta\", \"usage\": {\"output_tokens\": 3}}\n\n")
	u := parseStreamUsage(head, tail)
	want := usagemeta.Usage{Prompt: 10, Completion: 3, Total: 13, Source: usagemeta.SourceAnthropic}
	if u != want {
		t.Errorf("parseStreamUsage(empty data line) = %+v, want %+v", u, want)
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

// TestStreamResponseBody_OpenAIHugeContentEventKeepsUsage is the audit
// round 6 item 5 regression: a single content event far larger than the
// old 8 KiB tail window sits between the deltas and the final usage chunk.
// The old raw-byte tail capture lost the usage chunk (it was pushed out of
// the 8 KiB window by the huge event); the event-aware capture keeps the
// usage chunk because it parses events, not bytes.
func TestStreamResponseBody_OpenAIHugeContentEventKeepsUsage(t *testing.T) {
	huge := strings.Repeat("x", 100<<10) // 100 KiB in ONE SSE event
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"" + huge + "\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":7,\"total_tokens\":22}}\n\n" +
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
	// Byte-for-byte passthrough (the huge event must survive untouched).
	if rec.Body.Len() != len(sse) {
		t.Errorf("passthrough length = %d, want %d", rec.Body.Len(), len(sse))
	}
	// The usage chunk after the huge event must still be captured.
	if a.PromptTokens != 15 || a.CompletionTokens != 7 || a.TotalTokens != 22 {
		t.Errorf("usage after huge event = %+v, want prompt=15 completion=7 total=22 (huge event must not evict the usage carrier)", a)
	}
}

// TestStreamResponseBody_AnthropicHugeContentEventKeepsUsage: the same
// property on the Anthropic wire format — the message_delta usage carrier
// at the tail must survive huge content events between it and message_start.
func TestStreamResponseBody_AnthropicHugeContentEventKeepsUsage(t *testing.T) {
	huge := strings.Repeat("y", 100<<10)
	var sb strings.Builder
	sb.WriteString("event: message_start\n")
	sb.WriteString("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":111,\"cache_read_input_tokens\":100}}}\n\n")
	// 25 huge content events (2.5 MiB total) between the two usage carriers.
	for i := 0; i < 25; i++ {
		sb.WriteString("event: content_block_delta\n")
		sb.WriteString("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + huge + "\"}}\n\n")
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
	if _, err := StreamResponseBody(rec, resp.Body, req, "test-account"); err != nil {
		t.Fatalf("StreamResponseBody failed: %v", err)
	}
	if rec.Body.Len() != len(sse) {
		t.Errorf("passthrough length = %d, want %d", rec.Body.Len(), len(sse))
	}
	if a.PromptTokens != 111 {
		t.Errorf("PromptTokens = %d, want 111 (message_start input_tokens)", a.PromptTokens)
	}
	if a.CompletionTokens != 222 {
		t.Errorf("CompletionTokens = %d, want 222 (message_delta output_tokens)", a.CompletionTokens)
	}
	if a.TotalTokens != 333 {
		t.Errorf("TotalTokens = %d, want 333", a.TotalTokens)
	}
	if a.CachedTokens != 100 {
		t.Errorf("CachedTokens = %d, want 100", a.CachedTokens)
	}
	if a.UsageSource != usagemeta.SourceAnthropic {
		t.Errorf("UsageSource = %q, want anthropic", a.UsageSource)
	}
}

// TestStreamResponseBody_CRLF: a CRLF-delimited OpenAI stream must be
// captured like the LF form — the old capture only split on "\n\n" and
// never terminated events on CRLF streams (all usage lost).
func TestStreamResponseBody_CRLF(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":7,\"total_tokens\":22}}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// Split the write mid-CRLF-pair to exercise the cross-chunk carry.
		w.Write([]byte(sse[:40]))
		w.Write([]byte(sse[40:]))
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
	if rec.Body.String() != sse {
		t.Errorf("passthrough broken:\n got %q\nwant %q", rec.Body.String(), sse)
	}
	if a.PromptTokens != 15 || a.CompletionTokens != 7 || a.TotalTokens != 22 {
		t.Errorf("usage = prompt=%d completion=%d total=%d, want 15/7/22 (CRLF stream must be captured)", a.PromptTokens, a.CompletionTokens, a.TotalTokens)
	}
}

// TestStreamResponseBody_EOFWithoutBlankLine: the upstream ends WITHOUT the
// terminating empty line after the final event (the OpenAI usage chunk is
// the last event). The capture's Finish must flush it — before the fix the
// usage chunk stayed in pending and the audit recorded zero tokens.
func TestStreamResponseBody_EOFWithoutBlankLine(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":7,\"total_tokens\":22}}\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(sse)) // final event NOT terminated by an empty line
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
	if rec.Body.String() != sse {
		t.Errorf("passthrough broken:\n got %q\nwant %q", rec.Body.String(), sse)
	}
	if a.PromptTokens != 15 || a.CompletionTokens != 7 || a.TotalTokens != 22 {
		t.Errorf("usage = prompt=%d completion=%d total=%d, want 15/7/22 (EOF without blank line must still capture the final usage chunk)", a.PromptTokens, a.CompletionTokens, a.TotalTokens)
	}
}

// TestStreamResponseBody_EOFWithoutBlankLineAnthropic: the same EOF
// property on the Anthropic wire format — a stream ending with
// message_delta without a trailing blank line must still yield output
// tokens.
func TestStreamResponseBody_EOFWithoutBlankLineAnthropic(t *testing.T) {
	sse := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":250}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n"
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
	if a.PromptTokens != 250 || a.CompletionTokens != 40 || a.TotalTokens != 290 {
		t.Errorf("usage = prompt=%d completion=%d total=%d, want 250/40/290 (Anthropic EOF without blank line)", a.PromptTokens, a.CompletionTokens, a.TotalTokens)
	}
	if a.UsageSource != usagemeta.SourceAnthropic {
		t.Errorf("UsageSource = %q, want anthropic", a.UsageSource)
	}
}

// TestStreamResponseBody_AnthropicHugePreambleKeepsDetection: the wire-
// format detector reads the FIRST complete event, so a huge preamble event
// (far larger than the old 16 KiB head window) before message_start must
// not defeat Anthropic detection — the old raw-head capture was evicted by
// the preamble and misrouted the stream to the OpenAI parser (zero usage).
func TestStreamResponseBody_AnthropicHugePreambleKeepsDetection(t *testing.T) {
	huge := strings.Repeat("z", 100<<10)
	sse := "event: ping\ndata: {\"type\":\"ping\",\"payload\":\"" + huge + "\"}\n\n" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

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
	if a.PromptTokens != 10 || a.CompletionTokens != 3 || a.TotalTokens != 13 {
		t.Errorf("usage = prompt=%d completion=%d total=%d, want 10/3/13 (huge preamble must not defeat Anthropic detection)", a.PromptTokens, a.CompletionTokens, a.TotalTokens)
	}
	if a.UsageSource != usagemeta.SourceAnthropic {
		t.Errorf("UsageSource = %q, want anthropic", a.UsageSource)
	}
}

// ---------------------------------------------------------------------------
// CR/LF streaming normalization regression tests
// ---------------------------------------------------------------------------
//
// The old Write normalized CRLF with a cross-chunk carry that DROPPED both
// bytes of a CRLF pair split across two Writes ("...\r" | "\n..."): the
// blank line vanished and two events merged into one unparseable event
// (usage lost). A lone '\r' was also not treated as the line ending the
// SSE spec defines. The tests below pin the correct streaming behavior and
// fail on the old implementation.

// TestUsageEventCapture_CRLFSplitEveryPosition feeds a CRLF stream through
// the capture at EVERY possible two-chunk split point and byte-by-byte;
// head/recent must be identical to the whole-stream reference in every
// case. The old carry dropped a CRLF pair split at the chunk boundary, so
// every split inside a "\r\n" pair lost the line ending and corrupted the
// event boundaries.
func TestUsageEventCapture_CRLFSplitEveryPosition(t *testing.T) {
	// Three small events (each well under the event cap), CRLF line
	// endings, CRLF blank lines; the final event has NO trailing blank
	// line (EOF-flush case).
	stream := "event: e1\r\n" +
		"data: {\"a\":1}\r\n\r\n" +
		"event: e2\r\n" +
		"data: {\"b\":2}\r\n\r\n" +
		"event: e3\r\n" +
		"data: {\"c\":3}\r\n"

	reference := func(c *usageEventCapture) (string, string) {
		c.Finish()
		return string(c.headEvents()), string(c.recentEvents())
	}
	check := func(t *testing.T, what string, c *usageEventCapture, wantHead, wantRecent string) {
		t.Helper()
		h, r := reference(c)
		if h != wantHead || r != wantRecent {
			t.Errorf("%s changed the capture:\n head  want %q got %q\n recent want %q got %q", what, wantHead, h, wantRecent, r)
		}
		// The retained events must be exactly the three payloads in order:
		// a lost boundary merges events and breaks this.
		payloads := sseDataPayloads(c.recentEvents())
		want := []string{"{\"a\":1}", "{\"b\":2}", "{\"c\":3}"}
		if len(payloads) != len(want) {
			t.Errorf("%s: recent yields %d payloads, want 3 (%q)", what, len(payloads), payloads)
			return
		}
		for i := range want {
			if string(payloads[i]) != want[i] {
				t.Errorf("%s: payload[%d] = %q, want %q", what, i, payloads[i], want[i])
			}
		}
	}

	// Whole stream in one Write: the reference the splits must match.
	cWhole := newUsageEventCapture(64, 128, 128)
	if _, err := cWhole.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	wantHead, wantRecent := reference(cWhole)

	// Every possible two-chunk split point.
	for i := 0; i < len(stream); i++ {
		c := newUsageEventCapture(64, 128, 128)
		if _, err := c.Write([]byte(stream[:i])); err != nil {
			t.Fatalf("split %d first half: %v", i, err)
		}
		if _, err := c.Write([]byte(stream[i:])); err != nil {
			t.Fatalf("split %d second half: %v", i, err)
		}
		check(t, fmt.Sprintf("split at byte %d", i), c, wantHead, wantRecent)
	}

	// One byte per Write: every CRLF pair is split across Writes.
	c := newUsageEventCapture(64, 128, 128)
	for i := 0; i < len(stream); i++ {
		if _, err := c.Write([]byte(stream[i : i+1])); err != nil {
			t.Fatal(err)
		}
	}
	check(t, "per-byte CRLF", c, wantHead, wantRecent)
}

// TestUsageEventCapture_CRLFPairSplitAcrossWrites is the exact defect from
// the audit: Write1 ends with '\r', Write2 starts with '\n'. The old carry
// dropped BOTH bytes, so the blank line between the two OpenAI events
// vanished, the events merged into one unparseable payload and the usage
// chunk was lost. The pair must normalize to a single '\n'.
func TestUsageEventCapture_CRLFPairSplitAcrossWrites(t *testing.T) {
	c := newUsageEventCapture(4096, 8192, 8192)
	part1 := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r"
	part2 := "\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":7,\"total_tokens\":22}}\r\n\r\n"
	if _, err := c.Write([]byte(part1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte(part2)); err != nil {
		t.Fatal(err)
	}
	c.Finish()
	u := parseStreamUsage(c.headEvents(), c.recentEvents())
	want := usagemeta.Usage{Prompt: 15, Completion: 7, Total: 22, Source: usagemeta.SourceOpenAI}
	if u != want {
		t.Errorf("parseStreamUsage = %+v, want %+v (split CRLF pair must normalize to '\\n', not vanish)", u, want)
	}
}

// TestUsageEventCapture_CRLFPairSplitAcrossWritesAnthropic: the same split
// CRLF pair on the Anthropic wire format — the blank line between
// message_start and message_delta is split, so the old code merged the two
// events, the Anthropic detector found no message_start and usage was
// routed to the OpenAI parser (zero tokens).
func TestUsageEventCapture_CRLFPairSplitAcrossWritesAnthropic(t *testing.T) {
	c := newUsageEventCapture(4096, 8192, 8192)
	part1 := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\r\n\r"
	part2 := "\nevent: message_delta\r\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\r\n"
	if _, err := c.Write([]byte(part1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte(part2)); err != nil {
		t.Fatal(err)
	}
	c.Finish()
	u := parseStreamUsage(c.headEvents(), c.recentEvents())
	want := usagemeta.Usage{Prompt: 10, Completion: 3, Total: 13, Source: usagemeta.SourceAnthropic}
	if u != want {
		t.Errorf("parseStreamUsage = %+v, want %+v (split CRLF pair must keep the Anthropic wire format detectable)", u, want)
	}
}

// TestUsageEventCapture_LoneCRIsLineEnding pins the SSE spec rule: a lone
// '\r' terminates a line, so "\r\r" between two data lines is a complete
// blank line (event separator). The old code left the raw '\r' in pending
// (ReplaceAll only matched "\r\n"), the two events stayed merged into one
// unparseable payload and the usage was lost.
func TestUsageEventCapture_LoneCRIsLineEnding(t *testing.T) {
	c := newUsageEventCapture(4096, 8192, 8192)
	if _, err := c.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\r\r")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("data: [DONE]\r")); err != nil {
		t.Fatal(err)
	}
	c.Finish()
	u := parseStreamUsage(c.headEvents(), c.recentEvents())
	want := usagemeta.Usage{Prompt: 5, Completion: 2, Total: 7, Source: usagemeta.SourceOpenAI}
	if u != want {
		t.Errorf("parseStreamUsage = %+v, want %+v (lone CRs must be line endings per the SSE spec, not raw bytes)", u, want)
	}
}

// TestUsageEventCapture_CRLFEndsWithLoneCR: a stream may end with a lone
// '\r' (a valid SSE line ending at EOF). The final event must be captured
// and the trailing CR must never leak into the retained payload.
func TestUsageEventCapture_CRLFEndsWithLoneCR(t *testing.T) {
	c := newUsageEventCapture(64, 128, 64)
	if _, err := c.Write([]byte("data: {\"a\":1}\r")); err != nil {
		t.Fatal(err)
	}
	c.Finish()
	if got := string(c.headEvents()); got != "data: {\"a\":1}" {
		t.Errorf("head after lone-CR EOF = %q, want the final event without the CR", got)
	}
	if strings.Contains(string(c.headEvents()), "\r") {
		t.Errorf("head = %q, must contain no raw CR", c.headEvents())
	}
}

// TestStreamResponseBody_CRLFPairSplitAcrossReads is the end-to-end form of
// the split-pair defect: the upstream body is delivered in two reads that
// split the first blank line as "...\r" | "\n...". The passthrough must
// stay byte-for-byte and the usage chunk must still be captured.
func TestStreamResponseBody_CRLFPairSplitAcrossReads(t *testing.T) {
	want := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":7,\"total_tokens\":22}}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	// Split exactly inside the first blank line: read1 ends with "\r\n\r",
	// read2 starts with "\n".
	split := strings.Index(want, "\r\n\r\n") + 3
	body := io.NopCloser(io.MultiReader(
		strings.NewReader(want[:split]),
		strings.NewReader(want[split:]),
	))
	rec := httptest.NewRecorder()
	req, a := newAuditReq()
	if _, err := StreamResponseBody(rec, body, req, "test-account"); err != nil {
		t.Fatalf("StreamResponseBody failed: %v", err)
	}
	if rec.Body.String() != want {
		t.Errorf("passthrough broken:\n got %q\nwant %q", rec.Body.String(), want)
	}
	if a.PromptTokens != 15 || a.CompletionTokens != 7 || a.TotalTokens != 22 {
		t.Errorf("usage = prompt=%d completion=%d total=%d, want 15/7/22 (split CRLF pair must not lose the event boundary)", a.PromptTokens, a.CompletionTokens, a.TotalTokens)
	}
}

// ---------------------------------------------------------------------------
// Bounded-memory regression tests
// ---------------------------------------------------------------------------
//
// The old Write appended the WHOLE p to pending before trimming, so a
// single huge Write peaked at maxEventBytes+len(p) — retained memory grew
// linearly with the input. The chunked rewrite must keep both the pending
// length and its backing array (cap — the actually retained allocation)
// within a small constant multiple of maxEventBytes regardless of len(p).

// TestUsageEventCapture_SingleHugeWriteMemoryBounded: one Write far larger
// than the event cap (1 MiB vs 64 bytes), then a 4x larger one. The
// retained pending length AND backing-array capacity must stay bounded; the
// old implementation's cap followed len(p) (>= 1 MiB).
func TestUsageEventCapture_SingleHugeWriteMemoryBounded(t *testing.T) {
	const maxEv = 64
	c := newUsageEventCapture(maxEv, 128, 4096)

	huge := bytes.Repeat([]byte("x"), 1<<20) // 1 MiB in a single Write
	if _, err := c.Write(huge); err != nil {
		t.Fatal(err)
	}
	if got := len(c.pending); got > 2*maxEv {
		t.Errorf("pending len = %d after a %d-byte single Write, want <= %d", got, len(huge), 2*maxEv)
	}
	if got := cap(c.pending); got > 2*maxEv {
		t.Errorf("pending cap = %d after a %d-byte single Write, want <= %d (backing array must not scale with the Write)", got, len(huge), 2*maxEv)
	}

	huge2 := bytes.Repeat([]byte("y"), 4<<20) // 4 MiB: 4x larger, same bound
	if _, err := c.Write(huge2); err != nil {
		t.Fatal(err)
	}
	if got := cap(c.pending); got > 2*maxEv {
		t.Errorf("pending cap = %d after a %d-byte single Write, want <= %d (retention must stay flat)", got, len(huge2), 2*maxEv)
	}
	if got := len(c.pending); got > 2*maxEv {
		t.Errorf("pending len = %d after a %d-byte single Write, want <= %d", got, len(huge2), 2*maxEv)
	}

	// The capture still recovers: terminate the oversize event, then a
	// usage event (small enough to fit the 64-byte event cap) written
	// afterwards is parsed normally.
	if _, err := c.Write([]byte("\n\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3}}\n\n")); err != nil {
		t.Fatal(err)
	}
	c.Finish()
	u := parseStreamUsage(c.headEvents(), c.recentEvents())
	want := usagemeta.Usage{Prompt: 5, Completion: 3, Total: 8, Source: usagemeta.SourceOpenAI}
	if u != want {
		t.Errorf("usage after huge writes = %+v, want %+v (capture must recover after an oversize single Write)", u, want)
	}
}

// TestUsageEventCapture_ProductionConstantsHugeWriteBounded: the same
// property at production scale (usageEventMaxBytes = 4 MiB): a single
// 20 MiB Write must keep the retained backing array within 2x the cap, and
// a usage event written after the huge event is still recovered.
func TestUsageEventCapture_ProductionConstantsHugeWriteBounded(t *testing.T) {
	c := newUsageEventCapture(usageEventMaxBytes, usageHeadMaxBytes, usageRecentMaxBytes)
	huge := bytes.Repeat([]byte("x"), 20<<20) // 20 MiB in a single Write
	if _, err := c.Write(huge); err != nil {
		t.Fatal(err)
	}
	if got := cap(c.pending); got > 2*usageEventMaxBytes {
		t.Errorf("pending cap = %d after a %d-byte single Write, want <= %d", got, len(huge), 2*usageEventMaxBytes)
	}
	if got := len(c.pending); got > 2*usageEventMaxBytes {
		t.Errorf("pending len = %d, want <= %d", got, 2*usageEventMaxBytes)
	}

	if _, err := c.Write([]byte("\n\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":7,\"total_tokens\":22}}\n\n")); err != nil {
		t.Fatal(err)
	}
	c.Finish()
	u := parseStreamUsage(c.headEvents(), c.recentEvents())
	want := usagemeta.Usage{Prompt: 15, Completion: 7, Total: 22, Source: usagemeta.SourceOpenAI}
	if u != want {
		t.Errorf("usage after huge production-scale write = %+v, want %+v", u, want)
	}
}
