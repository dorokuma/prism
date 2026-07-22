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

type reasoningPhase uint8

const (
	reasoningIdle      reasoningPhase = iota // 0: not started
	reasoningItemOpen                         // 1: output_item.added emitted
	reasoningPartOpen                         // 2: reasoning_summary_part.added emitted
)

type messagePhase uint8

const (
	messageIdle      messagePhase = iota // 0: not started
	messageItemOpen                        // 1: output_item.added emitted
	messagePartOpen                        // 2: content_part.added emitted
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
	// tool_search interception
	searchToolCache []map[string]any
	pendingSearchID string
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
	}
}

func (tr *responsesStreamTranslator) emit(w io.Writer, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
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
