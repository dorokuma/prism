package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dorokuma/prism/internal/util"
)

// ErrEmptyUpstreamStream is returned when the chat completion stream has no model output.
var ErrEmptyUpstreamStream = errors.New("empty upstream chat completion stream")

// ErrResponsesStreamTooLarge is returned when an accumulation limit of the
// responses stream translator is exceeded: the PER-BUFFER cap
// (MaxResponsesStreamAccumulatedBytes — output text, reasoning text, or
// one tool call's arguments) or the TOTAL cap
// (MaxResponsesStreamTotalBytes — text + reasoning + every tool call's
// arguments combined). The error is explicit: the caller must surface it
// to the client (response.failed when the stream already started) instead
// of silently truncating the accumulated content, which would corrupt the
// final output_text.done / function_call arguments.
var ErrResponsesStreamTooLarge = errors.New("responses stream accumulated output exceeds the size limit")

// MaxResponsesStreamAccumulatedBytes is the per-buffer cap for the
// accumulated output of a /v1/responses stream translation: output text
// (content + refusal), reasoning text, and EACH tool call's arguments are
// each bounded by this value. 16 MiB is generous for any realistic model
// output (a 128k-token completion is roughly 0.5 MiB of text). A buffer
// that would exceed the cap aborts the translation with
// ErrResponsesStreamTooLarge (never silent truncation).
const MaxResponsesStreamAccumulatedBytes = 16 << 20

// MaxResponsesStreamTotalBytes is the cap for the SUM of ALL accumulated
// output of one /v1/responses stream translation: output text (content +
// refusal) + reasoning text + every tool call's arguments combined. 32 MiB
// (2× the per-buffer cap) bounds the worst-case translator memory
// regardless of how many tool calls the upstream emits — the per-buffer
// caps alone would let N tools hold N×16 MiB of arguments. A translation
// whose total would exceed the cap aborts with ErrResponsesStreamTooLarge
// (never silent truncation); normal streams (text or reasoning well under
// 32 MiB) are unaffected.
const MaxResponsesStreamTotalBytes = 32 << 20

// responsesStreamMaxAccumulated is the per-buffer cap used by
// TranslateChatStreamToResponses. It is a variable (not a const) so tests
// can shrink it to exercise the overflow path without allocating 16 MiB;
// production always uses MaxResponsesStreamAccumulatedBytes.
var responsesStreamMaxAccumulated = MaxResponsesStreamAccumulatedBytes

// responsesStreamMaxTotal is the total cap (text + reasoning + all tool
// args) used by TranslateChatStreamToResponses. Variable (not const) for
// the same reason as responsesStreamMaxAccumulated; production always uses
// MaxResponsesStreamTotalBytes.
var responsesStreamMaxTotal = MaxResponsesStreamTotalBytes

// SetResponsesStreamLimitsForTest overrides the accumulation caps used by
// TranslateChatStreamToResponses (per-buffer and total) for the duration
// of a test and returns a restore function. It exists so tests — including
// proxy-package tests that drive the production handler path — can
// exercise the over-limit behavior without allocating 16+ MiB per test;
// production limits are the constants above.
func SetResponsesStreamLimitsForTest(maxPerBuffer, maxTotal int) (restore func()) {
	oldPerBuffer := responsesStreamMaxAccumulated
	oldTotal := responsesStreamMaxTotal
	responsesStreamMaxAccumulated = maxPerBuffer
	responsesStreamMaxTotal = maxTotal
	return func() {
		responsesStreamMaxAccumulated = oldPerBuffer
		responsesStreamMaxTotal = oldTotal
	}
}

type reasoningPhase uint8

const (
	reasoningIdle     reasoningPhase = iota // 0: not started
	reasoningItemOpen                       // 1: output_item.added emitted
	reasoningPartOpen                       // 2: reasoning_summary_part.added emitted
)

type messagePhase uint8

const (
	messageIdle     messagePhase = iota // 0: not started
	messageItemOpen                     // 1: output_item.added emitted
	messagePartOpen                     // 2: content_part.added emitted
)

type streamToolState struct {
	itemID      string
	callID      string
	name        string
	namespace   string
	args        string
	added       bool
	outputIndex int
}

type responsesStreamTranslator struct {
	model              string
	respID             string
	msgItemID          string
	reasoningItemID    string
	nextOutputIdx      int
	reasoningOutputIdx int
	msgOutputIdx       int
	contentIdx         int
	tools              map[int]*streamToolState
	created            bool
	hadMessageContent  bool
	reasoningPhase     reasoningPhase
	messagePhase       messagePhase
	textBuf            strings.Builder
	reasoningBuf       strings.Builder
	// maxAccumulated is the per-buffer cap for textBuf, reasoningBuf and
	// each tool call's args (see MaxResponsesStreamAccumulatedBytes).
	maxAccumulated int
	// maxTotal is the cap for the SUM of all accumulated output (text +
	// reasoning + every tool call's args; see MaxResponsesStreamTotalBytes).
	maxTotal int
	// totalAccumulated counts the bytes accumulated into every buffer so
	// far (text + reasoning + all tool args). int64: the overflow-safe
	// check below compares int64(len(s)) against
	// int64(maxTotal)-totalAccumulated, so the counter can never wrap
	// regardless of platform int width.
	totalAccumulated int64
	// tool_search interception
	searchToolCache []map[string]any
	pendingSearchID string
	// wroteAny records whether at least one SSE event has been successfully
	// written to the client. It lets the failure paths distinguish a
	// mid-stream failure (an event was already delivered; the protocol
	// failure event response.failed must follow) from a pre-first-event
	// failure (nothing was written yet; the caller can still return a real
	// HTTP error instead of committing an empty 200).
	wroteAny bool
}

func newResponsesStreamTranslator(model string, searchToolCache []map[string]any) *responsesStreamTranslator {
	return &responsesStreamTranslator{
		model:           model,
		respID:          "resp_" + util.RandomID(),
		msgItemID:       "msg_" + util.RandomID(),
		reasoningItemID: "rs_" + util.RandomID(),
		tools:           make(map[int]*streamToolState),
		nextOutputIdx:   0,
		searchToolCache: searchToolCache,
		reasoningBuf:    strings.Builder{},
		maxAccumulated:  responsesStreamMaxAccumulated,
		maxTotal:        responsesStreamMaxTotal,
	}
}

// appendText accumulates one content/refusal delta into textBuf, failing
// with ErrResponsesStreamTooLarge when the per-buffer cap or the total
// cap would be exceeded (explicit error, never silent truncation). The
// wrapped context names the limit so the error is diagnosable.
func (tr *responsesStreamTranslator) appendText(s string) error {
	if tr.textBuf.Len()+len(s) > tr.maxAccumulated {
		return fmt.Errorf("%w (output text buffer)", ErrResponsesStreamTooLarge)
	}
	if err := tr.checkTotal(len(s)); err != nil {
		return err
	}
	tr.textBuf.WriteString(s)
	return nil
}

// appendReasoning accumulates one reasoning delta into reasoningBuf,
// failing with ErrResponsesStreamTooLarge when the per-buffer cap or the
// total cap would be exceeded.
func (tr *responsesStreamTranslator) appendReasoning(s string) error {
	if tr.reasoningBuf.Len()+len(s) > tr.maxAccumulated {
		return fmt.Errorf("%w (reasoning buffer)", ErrResponsesStreamTooLarge)
	}
	if err := tr.checkTotal(len(s)); err != nil {
		return err
	}
	tr.reasoningBuf.WriteString(s)
	return nil
}

// appendToolArgs accumulates one arguments delta into a tool call's args,
// failing with ErrResponsesStreamTooLarge when the per-buffer cap or the
// total cap would be exceeded.
func (tr *responsesStreamTranslator) appendToolArgs(st *streamToolState, s string) error {
	if len(st.args)+len(s) > tr.maxAccumulated {
		return fmt.Errorf("%w (tool call arguments buffer)", ErrResponsesStreamTooLarge)
	}
	if err := tr.checkTotal(len(s)); err != nil {
		return err
	}
	st.args += s
	return nil
}

// checkTotal fails when adding n more bytes would push the SUM of all
// accumulated output (text + reasoning + all tool args) past maxTotal. The
// comparison is overflow-safe: it is rewritten as
// int64(n) > int64(maxTotal)-totalAccumulated, so neither side can wrap
// (totalAccumulated is only ever advanced after passing this check, hence
// int64(maxTotal)-totalAccumulated >= 0 always). On success the counter is
// advanced. The wrapped context names the limit so the error is
// diagnosable.
func (tr *responsesStreamTranslator) checkTotal(n int) error {
	if int64(n) > int64(tr.maxTotal)-tr.totalAccumulated {
		return fmt.Errorf("%w (total accumulated output across text, reasoning and all tool call arguments)", ErrResponsesStreamTooLarge)
	}
	tr.totalAccumulated += int64(n)
	return nil
}

func (tr *responsesStreamTranslator) emit(w io.Writer, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	if err == nil {
		tr.wroteAny = true
	}
	return err
}

func (tr *responsesStreamTranslator) ensureCreated(w io.Writer) error {
	if tr.created {
		return nil
	}
	tr.created = true
	return tr.emit(w, map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": tr.respID, "object": "response", "status": "in_progress", "model": tr.model,
		},
	})
}

func (tr *responsesStreamTranslator) ensureReasoningStream(w io.Writer) error {
	if err := tr.ensureCreated(w); err != nil {
		return err
	}
	if tr.reasoningPhase == reasoningIdle {
		tr.reasoningPhase = reasoningItemOpen
		tr.reasoningOutputIdx = tr.nextOutputIdx
		tr.nextOutputIdx++
		if err := tr.emit(w, map[string]any{
			"type":         "response.output_item.added",
			"output_index": tr.reasoningOutputIdx,
			"item": map[string]any{
				"type": "reasoning", "id": tr.reasoningItemID, "status": "in_progress",
			},
		}); err != nil {
			return err
		}
	}
	if tr.reasoningPhase == reasoningItemOpen {
		tr.reasoningPhase = reasoningPartOpen
		return tr.emit(w, map[string]any{
			"type":          "response.reasoning_summary_part.added",
			"item_id":       tr.reasoningItemID,
			"output_index":  tr.reasoningOutputIdx,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": ""},
		})
	}
	return nil
}

func (tr *responsesStreamTranslator) ensureMessageStream(w io.Writer) error {
	if err := tr.ensureCreated(w); err != nil {
		return err
	}
	if tr.messagePhase == messageIdle {
		tr.messagePhase = messageItemOpen
		tr.msgOutputIdx = tr.nextOutputIdx
		tr.nextOutputIdx++
		return tr.emit(w, map[string]any{
			"type":         "response.output_item.added",
			"output_index": tr.msgOutputIdx,
			"item": map[string]any{
				"type": "message", "id": tr.msgItemID, "role": "assistant", "status": "in_progress",
				"content": []any{},
			},
		})
	}
	return nil
}

func (tr *responsesStreamTranslator) ensureContentPart(w io.Writer) error {
	if err := tr.ensureMessageStream(w); err != nil {
		return err
	}
	if tr.messagePhase == messageItemOpen {
		tr.messagePhase = messagePartOpen
		return tr.emit(w, map[string]any{
			"type":          "response.content_part.added",
			"item_id":       tr.msgItemID,
			"output_index":  tr.msgOutputIdx,
			"content_index": tr.contentIdx,
			"part":          map[string]any{"type": "output_text", "text": ""},
		})
	}
	return nil
}

// ctxReader wraps an io.Reader so that Read calls return promptly when ctx
// is cancelled. Data is copied from r into an io.Pipe in a background goroutine.
// When ctx is done, the pipe's read end is closed, unblocking any pending Read.
//
// A persistent goroutine (readLoop) reads from the pipe into an internal buffer
// and delivers results via a channel. This avoids creating a new goroutine per
// Read call and decouples pr.Read from the caller's p slice: when ctx is
// cancelled, p is guaranteed untouched (io.Reader contract safety).
//
// A ctx watcher goroutine closes the pipe write end on cancellation as a
// backstop: if nobody is calling Read (e.g. translate exited due to a write
// error), the watcher unblocks io.Copy, which would otherwise be stuck on
// pw.Write with a full pipe buffer.
func ctxReader(ctx context.Context, r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	// Watcher: ctx cancel forces the write end closed, unblocking io.Copy
	// even when nobody is reading from pr. CloseWithError is idempotent.
	go func() {
		<-ctx.Done()
		pw.CloseWithError(ctx.Err())
	}()
	go func() {
		defer pw.Close()
		_, err := io.Copy(pw, r)
		if err != nil {
			pw.CloseWithError(err)
		}
	}()
	cpr := &ctxPipeReader{
		ctx: ctx,
		pr:  pr,
		ch:  make(chan prResult, 1),
	}
	go cpr.readLoop()
	return cpr
}

// prResult carries the outcome of a single pipe read performed by the
// persistent readLoop goroutine.
type prResult struct {
	n   int
	err error
	buf []byte // copy of the data read (owned by the receiver)
}

type ctxPipeReader struct {
	ctx      context.Context
	pr       *io.PipeReader
	ch       chan prResult // results from the persistent readLoop goroutine
	leftover []byte        // unconsumed data from the previous prResult
	lerr     error         // error attached to leftover (delivered once leftover is drained)
}

// readLoop is the persistent goroutine that reads from the pipe into an
// internal buffer and delivers results to Read via ch. It exits when the
// pipe returns an error or ctx is cancelled.
func (c *ctxPipeReader) readLoop() {
	defer close(c.ch)
	buf := make([]byte, 32*1024)
	for {
		n, err := c.pr.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case c.ch <- prResult{n: n, err: err, buf: data}:
			case <-c.ctx.Done():
				return
			}
		}
		if err != nil {
			if n == 0 {
				select {
				case c.ch <- prResult{err: err}:
				case <-c.ctx.Done():
				}
			}
			return
		}
	}
}

func (c *ctxPipeReader) Read(p []byte) (int, error) {
	// Serve leftover data from the previous internal read first.
	if len(c.leftover) > 0 {
		n := copy(p, c.leftover)
		c.leftover = c.leftover[n:]
		if len(c.leftover) == 0 && c.lerr != nil {
			err := c.lerr
			c.lerr = nil
			return n, err
		}
		return n, nil
	}

	select {
	case <-c.ctx.Done():
		c.pr.CloseWithError(c.ctx.Err())
		return 0, c.ctx.Err()
	case res, ok := <-c.ch:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, res.buf)
		if n < len(res.buf) {
			c.leftover = res.buf[n:]
			c.lerr = res.err
			return n, nil
		}
		return n, res.err
	}
}
