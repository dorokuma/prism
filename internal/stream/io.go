package stream

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/dorokuma/prism/internal/config"
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

// Usage-capture bounds (audit round 6, item 5): the stream is parsed SSE
// event by event, so memory stays bounded regardless of stream length while
// a huge single event can never evict the usage carriers (message_start at
// the head, message_delta / final OpenAI usage chunk at the tail).
const (
	// usageEventMaxBytes is the per-event cap. An event larger than this is
	// skipped (never cached, never parsed): it cannot be a usage carrier
	// (message_start / message_delta / a final OpenAI chunk are all tiny),
	// and skipping keeps the capture bounded without buffering the event.
	// It matches the stream scanner's single-line cap so the two layers
	// agree on what a legal SSE event looks like.
	usageEventMaxBytes = config.StreamScannerMaxBuf
	// usageHeadMaxBytes bounds the retained head-event window: the
	// Anthropic message_start (input-token carrier) is the first or second
	// event, so a 16 KiB head window always contains it (plus a small
	// preamble) while staying tiny.
	usageHeadMaxBytes = 16 << 10
	// usageRecentMaxBytes bounds the retained recent-event buffer: the
	// tail usage carriers are among the last events and are small, so a
	// 64 KiB window of the most recent complete events always contains
	// them while the buffer stays tiny.
	usageRecentMaxBytes = 64 << 10
)

// usageEventCapture is the bounded, SSE-event-aware tee target for
// StreamResponseBody. Unlike the old raw head/tail byte capture (16 KiB
// head + 8 KiB tail), it parses the stream into complete SSE events (an
// event ends at an empty line, "\n\n" after LF normalization; an event may
// span several Write calls) and retains:
//   - the HEAD events, bounded by usageHeadMaxBytes: the Anthropic
//     wire-format detector scans these for message_start (the usage
//     carrier may be the first or the second event behind a small
//     preamble);
//   - the most recent complete events, bounded by usageRecentMaxBytes
//     (the Anthropic message_delta and the OpenAI final usage chunk are
//     among the last events).
//
// A single event larger than usageEventMaxBytes is SKIPPED (dropped from
// the capture, never buffered): it cannot be a usage carrier and skipping
// keeps memory bounded — a huge content delta cannot evict the small usage
// events around it. Memory stays bounded by
// 2*usageEventMaxBytes (pending peak during one Write; the chunked fold
// never lets a single Write grow pending beyond the cap by more than one
// chunk) + usageHeadMaxBytes (head) + usageRecentMaxBytes (recent, plus
// one oversized event that cannot be split) regardless of stream length
// and regardless of how large a single Write is.
//
// CRLF input ("\r\n" line endings, "\r\n\r\n" blank lines) is normalized
// to LF at the input edge, including a "\r\n" pair split across two Write
// calls: a trailing '\r' is held back until the next chunk decides, and
// the pair becomes exactly one '\n' — never zero bytes. A lone '\r' is
// also a line ending per the SSE spec and normalizes to '\n' (it is never
// silently dropped). The passthrough stream is never touched (the capture
// is a TeeReader target).
//
// Finish MUST be called at EOF: an SSE stream is not required to end with
// an empty line, so the final event (often the OpenAI usage chunk) may
// arrive without its terminating blank line and would otherwise stay in
// pending forever.
//
// Write always succeeds (never returns an error).
type usageEventCapture struct {
	maxEventBytes int
	headBytes     int
	recentBytes   int

	// pending accumulates the current (possibly incomplete) event across
	// Write calls; pendingOversize marks it as over the cap so the whole
	// event is skipped once its boundary is found. pending never contains a
	// trailing '\r' that could be the first half of a split CRLF pair
	// (carryCR holds it instead).
	pending         []byte
	pendingOversize bool
	// carryCR remembers a trailing '\r' held back from the previous chunk:
	// its fate is decided by the next chunk's first byte — '\n' makes the
	// pair one '\n', anything else makes the lone CR a '\n' (SSE line
	// ending); Finish flushes it as '\n' at EOF.
	carryCR bool

	// head holds the complete head events joined with "\n\n" (the same
	// separator sseDataPayloads splits on), bounded by headBytes; headDone
	// stops further collection once the window is full.
	head     []byte
	headDone bool
	// recent holds the most recent complete events joined with "\n\n",
	// bounded by recentBytes: when a new event would overflow, the OLDEST
	// complete events are dropped first (never a partial event).
	recent []byte
}

func newUsageEventCapture(maxEventBytes, headBytes, recentBytes int) *usageEventCapture {
	return &usageEventCapture{maxEventBytes: maxEventBytes, headBytes: headBytes, recentBytes: recentBytes}
}

// Write feeds stream bytes into the capture, extracting complete SSE events
// and updating head/recent. It is the io.Writer side of io.TeeReader; the
// bytes are also forwarded untouched by the TeeReader, so the capture never
// alters the passthrough stream.
func (c *usageEventCapture) Write(p []byte) (int, error) {
	n := len(p)
	// Chunked incremental consumption: p is folded into pending in slices
	// of at most maxEventBytes, and complete events are consumed and the
	// oversize tail trimmed after EVERY slice, so the peak retained memory
	// is bounded by 2*maxEventBytes regardless of len(p). The old
	// implementation appended the whole p first (peak maxEventBytes+len(p))
	// and trimmed only afterwards, so a single huge Write (a multi-megabyte
	// content chunk) ballooned pending linearly with the input.
	for len(p) > 0 {
		chunk := p
		if len(chunk) > c.maxEventBytes {
			chunk = chunk[:c.maxEventBytes]
		}
		p = p[len(chunk):]
		// CR/LF normalization at the input edge (see appendNormalized).
		c.pending = c.appendNormalized(c.pending, chunk)
		// Consume complete events BEFORE the oversize trim: a boundary that
		// arrived inside this chunk must never be trimmed away (the trim
		// keeps only the newest bytes, and a huge single chunk could
		// otherwise push the boundary out of the kept window).
		c.consumeEvents()
		c.trimPending()
	}
	return n, nil
}

// appendNormalized appends the CR/LF-normalized form of src to dst and
// returns the extended slice. Per the SSE spec a line ends with CR, LF, or
// CRLF, so:
//   - "\r\n" becomes one '\n' — including a pair SPLIT across two chunks
//     (a trailing '\r' is held in carryCR until the next chunk's first
//     byte decides its fate);
//   - a lone '\r' becomes '\n' (it is a line ending, never silently
//     dropped);
//   - '\n' stays '\n'.
//
// The held-back '\r' is what keeps a split CRLF pair from being deleted:
// the old carry dropped BOTH bytes of a pair split as "...\r" | "\n...",
// so a blank line split at the boundary vanished ("\n\n" collapsed toward
// a single '\n') and two events merged into one unparseable event.
func (c *usageEventCapture) appendNormalized(dst, src []byte) []byte {
	if c.carryCR {
		if len(src) == 0 {
			// No byte to decide the carried '\r' against yet: keep carrying
			// (defensive — Write never feeds an empty chunk).
			return dst
		}
		c.carryCR = false
		if src[0] == '\n' {
			// The carried '\r' + this '\n' is one CRLF pair → one '\n'.
			dst = append(dst, '\n')
			src = src[1:]
		} else {
			// The carried '\r' was a lone CR → a line ending → '\n'.
			dst = append(dst, '\n')
		}
	}
	for {
		i := bytes.IndexAny(src, "\r\n")
		if i < 0 {
			return append(dst, src...)
		}
		dst = append(dst, src[:i]...)
		switch src[i] {
		case '\n':
			dst = append(dst, '\n')
			src = src[i+1:]
		case '\r':
			if i == len(src)-1 {
				// Trailing '\r': hold it until the next chunk decides whether
				// it was the first half of a CRLF pair.
				c.carryCR = true
				return dst
			}
			if src[i+1] == '\n' {
				dst = append(dst, '\n')
				src = src[i+2:]
			} else {
				// Lone CR: per the SSE spec it terminates the line.
				dst = append(dst, '\n')
				src = src[i+1:]
			}
		}
	}
}

// consumeEvents extracts every complete event ("\n\n"-terminated) from
// pending into head/recent, skipping oversize events. Runs BEFORE the
// oversize trim so a boundary that arrived in the latest chunk is never
// trimmed away.
func (c *usageEventCapture) consumeEvents() {
	for {
		idx := bytes.Index(c.pending, []byte("\n\n"))
		if idx < 0 {
			return
		}
		event := c.pending[:idx]
		c.pending = c.pending[idx+2:]
		oversize := c.pendingOversize
		c.pendingOversize = false
		// A strict length check backs up the oversize flag: an event can
		// exceed the cap by up to one chunk's worth before the flag is set
		// (the trim runs after the scan), so the flag alone would let a
		// marginally-over-cap event through.
		if oversize || len(event) > c.maxEventBytes {
			// The event exceeded the cap: skip it entirely. It cannot be a
			// usage carrier, and caching a truncated fragment would corrupt
			// the SSE parse of the retained buffer.
			continue
		}
		c.addHead(event)
		c.addRecent(event)
	}
}

// trimPending enforces the per-event cap: once pending exceeds
// maxEventBytes, the oldest bytes (the doomed oversize event's head) are
// dropped and the event is marked oversize so nothing of it is cached.
// After the trim pending is at most maxEventBytes, which is what keeps the
// retained memory bounded under chunked writes.
func (c *usageEventCapture) trimPending() {
	if len(c.pending) > c.maxEventBytes {
		c.pending = c.pending[len(c.pending)-c.maxEventBytes:]
		c.pendingOversize = true
	}
}

// Finish flushes the final partial event at EOF: an SSE stream is not
// required to end with an empty line, so the last event (often the OpenAI
// final usage chunk) may arrive without its terminating "\n\n". Without
// this flush it would stay in pending and never be captured. An empty or
// oversize pending is a no-op (nothing complete to capture).
func (c *usageEventCapture) Finish() {
	// A trailing '\r' held back from the last Write is a lone CR at EOF:
	// per the SSE spec it terminates the line, so normalize it to '\n'
	// before the flush (TrimRight below strips trailing newlines, so the
	// lone CR cannot leak into the captured event).
	if c.carryCR {
		c.carryCR = false
		c.pending = append(c.pending, '\n')
	}
	if len(c.pending) == 0 || c.pendingOversize {
		return
	}
	event := bytes.TrimRight(c.pending, "\n")
	if len(event) == 0 {
		return
	}
	if len(event) > c.maxEventBytes {
		return
	}
	c.addHead(event)
	c.addRecent(event)
	c.pending = nil
}

// addHead appends a complete event to the head window until the window is
// full (headDone); the head holds the first events the Anthropic detector
// scans. A single event larger than the head window is SKIPPED (not
// cached): it cannot be message_start (the usage carrier is tiny), and
// skipping lets a later small message_start still enter the window — the
// audit round 6 item 5 property that a huge preamble cannot defeat
// Anthropic detection.
func (c *usageEventCapture) addHead(event []byte) {
	if c.headDone {
		return
	}
	if len(event) > c.headBytes {
		// Oversized head event: skip it entirely so it cannot fill the
		// window ahead of the real message_start.
		return
	}
	need := len(event) + 2 // event + the "\n\n" separator
	if len(c.head) > 0 && len(c.head)+need > c.headBytes {
		// The window is full: drop every later head event (they are not
		// needed — message_start is among the first events).
		c.headDone = true
		return
	}
	if len(c.head) > 0 {
		c.head = append(c.head, '\n', '\n')
	}
	c.head = append(c.head, event...)
}

// addRecent appends a complete event to the recent buffer, dropping the
// OLDEST complete events when the byte bound would be exceeded (an event is
// never split).
func (c *usageEventCapture) addRecent(event []byte) {
	need := len(event) + 2 // event + the "\n\n" separator
	if len(c.recent)+need <= c.recentBytes || len(c.recent) == 0 {
		if len(c.recent) > 0 {
			c.recent = append(c.recent, '\n', '\n')
		}
		c.recent = append(c.recent, event...)
		return
	}
	// Drop the oldest event(s) until the new one fits. Events in recent are
	// joined by "\n\n": the first event ends at the first "\n\n". A
	// single-event recent has no separator: drop it entirely (the new event
	// takes its place — oldest first).
	for len(c.recent)+need > c.recentBytes {
		sep := bytes.Index(c.recent, []byte("\n\n"))
		if sep < 0 {
			c.recent = c.recent[:0]
			break
		}
		c.recent = c.recent[sep+2:]
	}
	if len(c.recent) > 0 {
		c.recent = append(c.recent, '\n', '\n')
	}
	c.recent = append(c.recent, event...)
}

// headEvents returns the retained head events, joined with "\n\n",
// bounded by usageHeadMaxBytes (plus one oversized event).
func (c *usageEventCapture) headEvents() []byte { return c.head }

// recentEvents returns the retained recent complete events, joined with
// "\n\n", bounded by usageRecentMaxBytes (plus one oversized event).
func (c *usageEventCapture) recentEvents() []byte { return c.recent }

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

// parseStreamUsage extracts token usage from the captured SSE stream
// prefix (head: the first events) and suffix (tail: the last events). The
// wire format is detected from the head: Anthropic streams open with a
// message_start event, OpenAI streams do not.
//
//   - OpenAI: the last data: chunk carrying a usage object (usually the
//     final content event before data: [DONE]) is parsed with the shared
//     OpenAI parser.
//   - Anthropic: usage is split across the stream — message_start at the
//     head carries input_tokens/cache_*_input_tokens, message_delta at the
//     tail carries output_tokens. Both ends are parsed with the shared
//     Anthropic parser and merged.
//
// Both buffers are EVENT-aligned (see usageEventCapture): a huge content
// event cannot evict the small usage carriers at either end (audit round 6,
// item 5).
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
		if u.HasTokens() {
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
// capturing token usage from the stream for audit logging.
func StreamResponseBody(w http.ResponseWriter, body io.ReadCloser, clientReq *http.Request, account string) (int64, error) {
	dst := io.Writer(w)
	if flusher, ok := w.(http.Flusher); ok {
		dst = &flushWriter{w: w, f: flusher}
	}

	// Tee the upstream body through a bounded SSE-EVENT-aware capture so we
	// can extract token usage after the fact without buffering the entire
	// response in memory. The capture keeps the FIRST complete event (the
	// Anthropic message_start usage carrier) and the most recent complete
	// events (the Anthropic message_delta and the OpenAI final usage chunk)
	// — parsed per SSE event, so a huge content event can never evict the
	// small usage events around it (the old raw head/tail byte capture
	// missed usage when a single event exceeded the 16 KiB head or 8 KiB
	// tail window). Memory stays bounded by usageEventMaxBytes +
	// usageRecentMaxBytes regardless of stream length; the stream body
	// itself is never buffered.
	capture := newUsageEventCapture(usageEventMaxBytes, usageHeadMaxBytes, usageRecentMaxBytes)
	teeReader := io.TeeReader(body, capture)

	n, err := io.Copy(dst, teeReader)
	// EOF flush: the last SSE event is not required to end with an empty
	// line, so the capture must be finished before parsing — without it the
	// final event (often the OpenAI usage chunk) would never be captured.
	// Runs on the error path too: a dropped connection may still have
	// delivered the usage carrier as the final partial event.
	capture.Finish()

	// Capture token usage for audit (nil-safe; legacy streaming path). The
	// gate accepts any usage with at least one non-zero token count —
	// including a usage carrying ONLY cache tokens — via the shared
	// usagemeta.HasTokens gate (item: ApplyUsage gate consistency).
	if clientReq != nil {
		if a := middleware.AuditFromCtx(clientReq.Context()); a != nil {
			if u := parseStreamUsage(capture.headEvents(), capture.recentEvents()); u.HasTokens() {
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
