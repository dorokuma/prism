package stream

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/usagemeta"
)

// flushWriter wraps an http.ResponseWriter with automatic flushing after every Write.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err == nil {
		fw.f.Flush()
	}
	return n, err
}

// captureWriter keeps the first headCap bytes and the last tailCap bytes of
// everything written to it, and nothing in between. It is the bounded-memory
// tee target for StreamResponseBody: SSE usage lives at the stream head
// (Anthropic message_start carries input_tokens) or at the stream tail
// (Anthropic message_delta carries output_tokens; OpenAI carries the final
// usage chunk), so head+tail capture is enough to extract usage for both
// wire formats without buffering the whole response.
//
// Write always succeeds (never returns an error) and memory stays bounded by
// headCap + tailCap regardless of stream length.
type captureWriter struct {
	head     []byte
	headFull bool
	headCap  int
	tail     []byte
	tailCap  int
}

func newCaptureWriter(headCap, tailCap int) *captureWriter {
	return &captureWriter{headCap: headCap, tailCap: tailCap}
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if !c.headFull {
		room := c.headCap - len(c.head)
		if room <= 0 {
			c.headFull = true
		} else if len(p) >= room {
			c.head = append(c.head, p[:room]...)
			c.headFull = true
		} else {
			c.head = append(c.head, p...)
		}
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > c.tailCap {
		c.tail = c.tail[len(c.tail)-c.tailCap:]
	}
	return len(p), nil
}

func (c *captureWriter) headBytes() []byte { return c.head }
func (c *captureWriter) tailBytes() []byte { return c.tail }

// sseDataPayloads returns the data: payloads of all SSE events in buf, in
// stream order. Per the SSE specification an event's data field may span
// multiple data: lines, which are joined with a single "\n" before the
// payload is returned (a fragment would not parse as JSON). An EMPTY data:
// line is still a data line: it contributes an empty fragment to the join
// (so `data: a` + `data:` + `data: b` yields "a\n\nb", never the silently
// altered "a\nb"). [DONE] sentinels, events without data lines, and events
// whose joined payload is empty are skipped. The field value is everything
// after "data:" with one optional leading space stripped (the spec
// delimiter); surrounding whitespace is trimmed so a payload with
// leading/trailing spaces still parses.
func sseDataPayloads(buf []byte) [][]byte {
	var out [][]byte
	for _, event := range bytes.Split(buf, []byte("\n\n")) {
		event = bytes.TrimSpace(event)
		if len(event) == 0 {
			continue
		}
		var lines [][]byte
		for _, line := range bytes.Split(event, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimPrefix(line[len("data:"):], []byte(" "))
			payload = bytes.TrimSpace(payload)
			// Keep empty data lines: they are part of the spec's join and
			// dropping them would change the merged payload (e.g. two JSON
			// fragments separated by an empty line must stay two "\n"s
			// apart, not one).
			lines = append(lines, payload)
		}
		if len(lines) == 0 {
			continue
		}
		payload := bytes.Join(lines, []byte("\n"))
		if len(payload) == 0 {
			continue
		}
		// The [DONE] sentinel is matched on the trimmed joined payload: an
		// empty trailing data line inside the sentinel event must not make
		// the sentinel undetectable.
		if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			continue
		}
		out = append(out, payload)
	}
	return out
}

// anthropicEvent is the shared shape of Anthropic SSE events that carry
// usage: message_start nests it under message.usage (input tokens),
// message_delta carries it at top level (output tokens).
type anthropicEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	Usage json.RawMessage `json:"usage"`
}

// isAnthropicMessageStart reports whether the stream head contains an
// Anthropic message_start event. The event type is read from the PARSED JSON
// of every data: payload, and ALL payloads are scanned: a preamble event
// before message_start (e.g. an Anthropic ping, a proxy keep-alive comment
// event, or an unparseable data line) must not cause an early false — the
// stream is Anthropic as soon as ANY event carries type=message_start. The
// parse-based check also defeats a `"type":"message_start"` byte search
// failure mode: an upstream that emits `"type": "message_start"` (space
// after the colon) or puts type after other fields would misroute the
// stream to the OpenAI parser.
func isAnthropicMessageStart(head []byte) bool {
	for _, payload := range sseDataPayloads(head) {
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			continue
		}
		if ev.Type == "message_start" {
			return true
		}
	}
	return false
}

// parseStreamUsage extracts token usage from a captured SSE stream prefix
// (head) and suffix (tail). The wire format is detected from the head:
// Anthropic streams open with a message_start event, OpenAI streams do not.
//
//   - OpenAI: the last data: chunk carrying a usage object (usually the
//     final content event before data: [DONE]) is parsed with the shared
//     OpenAI parser.
//   - Anthropic: usage is split across the stream — message_start at the
//     head carries input_tokens/cache_*_input_tokens, message_delta at the
//     tail carries output_tokens. Both ends are parsed with the shared
//     Anthropic parser and merged.
func parseStreamUsage(head, tail []byte) usagemeta.Usage {
	if isAnthropicMessageStart(head) {
		return parseAnthropicStreamUsage(head, tail)
	}
	return parseOpenAIStreamUsage(tail)
}

// parseOpenAIStreamUsage scans an SSE stream tail for the last data: chunk
// that contains a usage object and returns its full token usage.
func parseOpenAIStreamUsage(tail []byte) usagemeta.Usage {
	payloads := sseDataPayloads(tail)
	// Walk backwards to find the usage chunk faster (it's usually the last
	// content event before data: [DONE]).
	for i := len(payloads) - 1; i >= 0; i-- {
		var chunk struct {
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(payloads[i], &chunk) != nil || len(chunk.Usage) == 0 {
			continue
		}
		u := usagemeta.ParseOpenAI(chunk.Usage)
		if u.Prompt > 0 || u.Completion > 0 {
			return u
		}
	}
	return usagemeta.Usage{}
}

// parseAnthropicStreamUsage extracts Anthropic stream usage: input tokens
// from the message_start event (stream head) and output tokens from the
// message_delta event (stream tail). The buffers are bounded (see
// captureWriter), so a long content stream cannot evict either end: the
// message_start event is the first SSE event (a few hundred bytes, well
// inside the head cap) and message_delta is among the last events (well
// inside the tail cap).
func parseAnthropicStreamUsage(head, tail []byte) usagemeta.Usage {
	var u usagemeta.Usage
	// message_start: input_tokens + cache counts, nested under message.usage.
	for _, payload := range sseDataPayloads(head) {
		var ev anthropicEvent
		if json.Unmarshal(payload, &ev) != nil || ev.Type != "message_start" || ev.Message == nil || len(ev.Message.Usage) == 0 {
			continue
		}
		u = usagemeta.ParseAnthropic(ev.Message.Usage)
		break
	}
	// message_delta: output_tokens at top level; take the last one.
	payloads := sseDataPayloads(tail)
	for i := len(payloads) - 1; i >= 0; i-- {
		var ev anthropicEvent
		if json.Unmarshal(payloads[i], &ev) != nil || ev.Type != "message_delta" || len(ev.Usage) == 0 {
			continue
		}
		d := usagemeta.ParseAnthropic(ev.Usage)
		u.Completion = d.Completion
		break
	}
	u.Total = u.Prompt + u.Completion
	// Pin the wire format explicitly: the merged usage is Anthropic-shaped
	// even when the head parse produced nothing (e.g. the message_start
	// event fell outside the capture buffers), so the cost layer must not
	// apply the OpenAI cached-subtraction formula.
	u.Source = usagemeta.SourceAnthropic
	return u
}

// StreamResponseBody copies the upstream body (SSE) to the client http.ResponseWriter,
// capturing token usage from the head and tail of the stream for audit logging.
func StreamResponseBody(w http.ResponseWriter, body io.ReadCloser, clientReq *http.Request, account string) (int64, error) {
	dst := io.Writer(w)
	if flusher, ok := w.(http.Flusher); ok {
		dst = &flushWriter{w: w, f: flusher}
	}

	// Tee the upstream body through a bounded head+tail capture so we can
	// extract token usage from the SSE stream after the fact without
	// buffering the entire response in memory. Bounds:
	//   head: 16 KiB — holds the first SSE event(s); the Anthropic
	//         message_start usage carrier is the very first event and is a
	//         few hundred bytes, so 16 KiB covers it plus any small
	//         preamble (e.g. a ping event) with a wide margin.
	//   tail: 8 KiB — holds the last SSE events; the Anthropic
	//         message_delta usage carrier and the OpenAI final usage chunk
	//         are both among the last events and are small.
	// The stream body itself is never buffered — only these two fixed
	// slices are retained, so a long response cannot blow up memory.
	capture := newCaptureWriter(16<<10, 8192)
	teeReader := io.TeeReader(body, capture)

	n, err := io.Copy(dst, teeReader)

	// Capture token usage for audit (nil-safe; legacy streaming path).
	if clientReq != nil {
		if a := middleware.AuditFromCtx(clientReq.Context()); a != nil {
			if u := parseStreamUsage(capture.headBytes(), capture.tailBytes()); u.Prompt > 0 || u.Completion > 0 {
				a.ApplyUsage(u)
			}
		}
	}

	if err != nil {
		if clientReq != nil && clientReq.Context().Err() != nil {
			slog.Warn("client disconnected during stream", "account", account, "written", n, "error", err)
		} else {
			slog.Error("upstream stream error", "account", account, "written", n, "error", err)
		}
		// Drain the upstream body so the account connection is released cleanly
		// even when the downstream client has already gone away.
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			slog.Warn("drain upstream body error", "account", account, "error", drainErr)
		}
		return n, err
	}
	return n, nil
}
