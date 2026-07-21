package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
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
	reasoningBuf        strings.Builder
	// tool_search interception
	searchToolCache    []map[string]any
	pendingSearchID    string
}

func newResponsesStreamTranslator(model string, searchToolCache []map[string]any) *responsesStreamTranslator {
	return &responsesStreamTranslator{
		model:           model,
		respID:          "resp_" + randomID(),
		msgItemID:       "msg_" + randomID(),
		reasoningItemID: "rs_" + randomID(),
		tools:           make(map[int]*streamToolState),
		nextOutputIdx:   0,
		searchToolCache: searchToolCache,
		reasoningBuf:       strings.Builder{},
	}
}


func (t *responsesStreamTranslator) emit(w io.Writer, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func (t *responsesStreamTranslator) ensureCreated(w io.Writer) error {
	if t.created {
		return nil
	}
	t.created = true
	return t.emit(w, map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": t.respID, "object": "response", "status": "in_progress", "model": t.model,
		},
	})
}

func (t *responsesStreamTranslator) ensureReasoningStream(w io.Writer) error {
	if err := t.ensureCreated(w); err != nil {
		return err
	}
	if t.reasoningPhase == reasoningIdle {
		t.reasoningPhase = reasoningItemOpen
		t.reasoningOutputIdx = t.nextOutputIdx
		t.nextOutputIdx++
		if err := t.emit(w, map[string]any{
			"type": "response.output_item.added",
			"output_index": t.reasoningOutputIdx,
			"item": map[string]any{
				"type": "reasoning", "id": t.reasoningItemID, "status": "in_progress",
			},
		}); err != nil {
			return err
		}
	}
	if t.reasoningPhase == reasoningItemOpen {
		t.reasoningPhase = reasoningPartOpen
		return t.emit(w, map[string]any{
			"type":          "response.reasoning_summary_part.added",
			"item_id":       t.reasoningItemID,
			"output_index":  t.reasoningOutputIdx,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": ""},
		})
	}
	return nil
}

func (t *responsesStreamTranslator) ensureMessageStream(w io.Writer) error {
	if err := t.ensureCreated(w); err != nil {
		return err
	}
	if t.messagePhase == messageIdle {
		t.messagePhase = messageItemOpen
		t.msgOutputIdx = t.nextOutputIdx
		t.nextOutputIdx++
		return t.emit(w, map[string]any{
			"type": "response.output_item.added",
			"output_index": t.msgOutputIdx,
			"item": map[string]any{
				"type": "message", "id": t.msgItemID, "role": "assistant", "status": "in_progress",
				"content": []any{},
			},
		})
	}
	return nil
}

func (t *responsesStreamTranslator) ensureContentPart(w io.Writer) error {
	if err := t.ensureMessageStream(w); err != nil {
		return err
	}
	if t.messagePhase == messageItemOpen {
		t.messagePhase = messagePartOpen
		return t.emit(w, map[string]any{
			"type":          "response.content_part.added",
			"item_id":       t.msgItemID,
			"output_index":  t.msgOutputIdx,
			"content_index": t.contentIdx,
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

func translateChatStreamToResponses(w http.ResponseWriter, body io.Reader, model string, reqTools json.RawMessage, searchToolCache []map[string]any, ctx context.Context) error {
	flusher, _ := w.(http.Flusher)
	dst := io.Writer(w)
	if flusher != nil {
		dst = &flushWriter{w: w, f: flusher}
	}
	tr := newResponsesStreamTranslator(model, searchToolCache)
	// Wrap body with a ctx-aware reader so that a blocked Read (e.g.
	// upstream silent during long reasoning) returns promptly when the
	// context is cancelled.
	sc := bufio.NewScanner(ctxReader(ctx, body))
	sc.Buffer(make([]byte, 0, streamScannerInitialBuf), streamScannerMaxBuf)
	var usage map[string]any
	completed := false
	lastFinishReason := ""
	var lastLogprobs any

	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimSpace(line[6:])
		if bytes.Equal(payload, []byte("[DONE]")) {
			completed = true
			break
		}
		var chunk chatStreamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			hit := chunk.Usage.PromptCacheHitTokens
			miss := chunk.Usage.PromptCacheMissTokens
			if hit == 0 && chunk.Usage.PromptTokensDetails != nil {
				hit = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			if miss == 0 && hit > 0 && chunk.Usage.PromptTokens > hit {
				miss = chunk.Usage.PromptTokens - hit
			}
			usage = map[string]any{
				"input_tokens":              chunk.Usage.PromptTokens,
				"output_tokens":             chunk.Usage.CompletionTokens,
				"total_tokens":              chunk.Usage.TotalTokens,
				"prompt_tokens":             chunk.Usage.PromptTokens,
				"completion_tokens":         chunk.Usage.CompletionTokens,
				"prompt_cache_hit_tokens":   hit,
				"prompt_cache_miss_tokens":  miss,
			}
			if chunk.Usage.CompletionTokensDetails != nil {
				usage["completion_tokens_details"] = map[string]any{
					"reasoning_tokens": chunk.Usage.CompletionTokensDetails.ReasoningTokens,
				}
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if chunk.Choices[0].FinishReason != "" {
			lastFinishReason = chunk.Choices[0].FinishReason
		}
		if chunk.Choices[0].Logprobs != nil {
			lastLogprobs = chunk.Choices[0].Logprobs
		}
		// Upstream may stream reasoning_content (e.g. DeepSeek). Codex 0.142.5 expects
		// response.reasoning_summary_text.delta (not reasoning_summary.delta).
		if d.ReasoningContent != "" {
				if debugMode {
					slog.Debug("stream reasoning chunk", "content", d.ReasoningContent)
				}
				tr.reasoningBuf.WriteString(d.ReasoningContent)
				if err := tr.ensureReasoningStream(dst); err != nil {
					return err
				}
				if err := tr.emit(dst, map[string]any{
					"type":          "response.reasoning_summary_text.delta",
					"item_id":       tr.reasoningItemID,
					"output_index":  tr.reasoningOutputIdx,
					"summary_index": 0,
					"delta":         d.ReasoningContent,
				}); err != nil {
					return err
				}
			}
		if d.Content != "" {
			if debugMode {
				slog.Debug("stream content chunk", "content", d.Content)
			}
			tr.hadMessageContent = true
			tr.textBuf.WriteString(d.Content)
			if err := tr.ensureContentPart(dst); err != nil {
				return err
			}
			if err := tr.emit(dst, map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       tr.msgItemID,
				"output_index":  tr.msgOutputIdx,
				"content_index": tr.contentIdx,
				"delta":         d.Content,
			}); err != nil {
				return err
			}
		}
		if d.Refusal != "" {
			if debugMode {
				slog.Debug("stream refusal chunk", "content", d.Refusal)
			}
			tr.hadMessageContent = true
			tr.textBuf.WriteString(d.Refusal)
			if err := tr.ensureContentPart(dst); err != nil {
				return err
			}
			if err := tr.emit(dst, map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       tr.msgItemID,
				"output_index":  tr.msgOutputIdx,
				"content_index": tr.contentIdx,
				"delta":         d.Refusal,
			}); err != nil {
				return err
			}
		}
		for _, tc := range d.ToolCalls {
			idx := tc.Index
			st, ok := tr.tools[idx]
			if !ok {
				st = &streamToolState{
					itemID: "fc_" + randomID(),
					callID: tc.ID,
					name:   ResolveNamespaceTool(tc.Function.Name),
				}
				tr.tools[idx] = st
			}
			if tc.ID != "" {
				st.callID = tc.ID
			}
			if tc.Function.Name != "" {
				st.name = ResolveNamespaceTool(tc.Function.Name)
				st.namespace = NamespaceForTool(tc.Function.Name)
			}
			if !st.added && st.name != "" {
				if debugMode {
					slog.Debug("stream tool_call", "name", st.name, "call_id", st.callID)
				}
				// Intercept tool_search for synthetic response
				if st.name == "tool_search" && len(tr.searchToolCache) > 0 {
					tr.pendingSearchID = st.itemID
					if debugMode {
						slog.Debug("stream tool_search intercepted", "cached_tools", len(tr.searchToolCache))
					}
				}
				st.added = true
				st.outputIndex = tr.nextOutputIdx
				tr.nextOutputIdx++
				if err := tr.ensureCreated(dst); err != nil {
					return err
				}
				item := map[string]any{
					"type": "function_call", "id": st.itemID, "call_id": st.callID,
					"name": st.name, "status": "in_progress",
				}
				if st.namespace != "" {
					item["namespace"] = st.namespace
				}
				if err := tr.emit(dst, map[string]any{
					"type": "response.output_item.added", "output_index": st.outputIndex,
					"item": item,
				}); err != nil {
					return err
				}
			}
			if tc.Function.Arguments != "" {
				st.args += tc.Function.Arguments
				// Codex 0.142.5 does not handle function_call_arguments.delta; arguments
				// are delivered in response.output_item.done for the function_call item.
			}
		}
	}
	if err := sc.Err(); err != nil {
		if debugMode {
			slog.Debug("stream scanner done", "error", err)
		}
		// If the context is already cancelled, the client closed the
		// connection — don't try to write to a dead connection.
		if ctx.Err() != nil {
			return err
		}
		// Real upstream stream error: emit a response.failed frame so the
		// client sees an explicit error instead of a silent connection drop.
		_ = tr.ensureCreated(dst)
		_ = tr.emit(dst, map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": tr.respID, "object": "response", "status": "failed", "model": tr.model,
				"error": map[string]any{
					"code":    "upstream_stream_error",
					"message": "upstream stream ended unexpectedly",
				},
			},
		})
		return err
	}

	if err := tr.ensureCreated(dst); err != nil {
		return err
	}

	hasSubstantive := tr.reasoningBuf.Len() > 0 || tr.hadMessageContent
	if !hasSubstantive {
		for _, st := range tr.tools {
			if st.added {
				hasSubstantive = true
				break
			}
		}
	}

	if !hasSubstantive {
		if debugMode {
			slog.Debug("stream empty upstream, emitting response.failed")
		}
		if err := tr.emit(dst, map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": tr.respID, "object": "response", "status": "failed", "model": tr.model,
				"error": map[string]any{
					"code":    "empty_upstream_stream",
					"message": "upstream chat completion stream contained no model output",
				},
			},
		}); err != nil {
			return err
		}
		return ErrEmptyUpstreamStream
	}

	// Clean EOF (sc.Err() == nil) with content but without a [DONE]
	// completion event: upstream disconnected before finishing the stream.
	if !completed {
		if debugMode {
			slog.Debug("stream clean EOF without completion event")
		}
		_ = tr.emit(dst, map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": tr.respID, "object": "response", "status": "failed", "model": tr.model,
				"error": map[string]any{
					"code":    "upstream_stream_incomplete",
					"message": "upstream stream ended without completion event",
				},
			},
		})
		return fmt.Errorf("upstream stream ended without completion event")
	}

	if debugMode {
		slog.Debug("stream ended", "had_content", tr.hadMessageContent, "tools", len(tr.tools))
	}

	for _, st := range tr.tools {
		if !st.added {
			continue
		}
		// If this is the intercepted tool_search, emit synthetic result
		if st.name == "tool_search" && tr.pendingSearchID == st.itemID && len(tr.searchToolCache) > 0 {
			searchTools := make([]any, 0, len(tr.searchToolCache))
			for _, t := range tr.searchToolCache {
				searchTools = append(searchTools, t)
			}
			outputIdx := tr.nextOutputIdx
			tr.nextOutputIdx++
			searchResultID := "ts_" + randomID()
			if err := tr.ensureCreated(dst); err != nil {
				return err
			}
			if err := tr.emit(dst, map[string]any{
				"type": "response.output_item.added", "output_index": outputIdx,
				"item": map[string]any{
					"type": "tool_search_output", "id": searchResultID,
					"call_id": st.callID, "status": "completed", "execution": "client",
					"tools": searchTools,
				},
			}); err != nil {
				return err
			}
			if err := tr.emit(dst, map[string]any{
				"type": "response.output_item.done", "output_index": outputIdx,
				"item": map[string]any{
					"type": "tool_search_output", "id": searchResultID,
					"call_id": st.callID, "status": "completed", "execution": "client",
					"tools": searchTools,
				},
			}); err != nil {
				return err
			}
			if debugMode {
				slog.Debug("stream tool_search synthetic result emitted", "tools", len(searchTools))
			}
			continue
		}
		item := map[string]any{
			"type": "function_call", "id": st.itemID, "call_id": st.callID,
			"name": st.name, "arguments": st.args, "status": "completed",
		}
		if st.namespace != "" {
			item["namespace"] = st.namespace
		}
		if err := tr.emit(dst, map[string]any{
			"type": "response.output_item.done", "output_index": st.outputIndex,
			"item": item,
		}); err != nil {
			return err
		}
	}

	// Complete reasoning output item if it was started (no reasoning_summary.done — Codex ignores it)
	if tr.reasoningPhase != reasoningIdle {
		if err := tr.emit(dst, map[string]any{
			"type": "response.output_item.done", "output_index": tr.reasoningOutputIdx,
			"item": map[string]any{
				"type": "reasoning", "id": tr.reasoningItemID, "status": "completed",
			},
		}); err != nil {
			return err
		}
	}

	if tr.messagePhase != messageIdle || tr.hadMessageContent {
		if tr.messagePhase == messageIdle {
			if err := tr.ensureMessageStream(dst); err != nil {
				return err
			}
		}
		if tr.messagePhase == messagePartOpen {
			if err := tr.emit(dst, map[string]any{
				"type":          "response.output_text.done",
				"item_id":       tr.msgItemID,
				"output_index":  tr.msgOutputIdx,
				"content_index": tr.contentIdx,
				"text":          tr.textBuf.String(),
			}); err != nil {
				return err
			}
		}
		msgDone := map[string]any{
			"type": "message", "id": tr.msgItemID, "role": "assistant", "status": "completed",
		}
		if tr.textBuf.Len() > 0 {
			msgDone["content"] = []map[string]any{
				{"type": "output_text", "text": tr.textBuf.String()},
			}
		}
		if err := tr.emit(dst, map[string]any{
			"type": "response.output_item.done", "output_index": tr.msgOutputIdx,
			"item": msgDone,
		}); err != nil {
			return err
		}
	}

	resp := map[string]any{"id": tr.respID, "object": "response", "status": finishReasonToStatus(lastFinishReason), "model": tr.model, "created_at": time.Now().Unix()}
	if len(reqTools) > 0 && string(reqTools) != "null" {
		resp["tools"] = jsonRawToAny(reqTools)
	}
	if usage != nil {
		resp["usage"] = usage
	}
	if lastLogprobs != nil {
		resp["logprobs"] = lastLogprobs
	}
	return tr.emit(dst, map[string]any{"type": "response.completed", "response": resp})
}

type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Refusal          string `json:"refusal"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
		Logprobs     any    `json:"logprobs"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens            int `json:"prompt_tokens"`
		CompletionTokens        int `json:"completion_tokens"`
		TotalTokens             int `json:"total_tokens"`
		PromptCacheHitTokens    int `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens   int `json:"prompt_cache_miss_tokens"`
		PromptTokensDetails     *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}