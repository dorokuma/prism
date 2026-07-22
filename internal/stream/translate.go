package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/mcp"
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/util"
)

// chatStreamChunk is the SSE chunk shape for a chat completion stream.
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

// TranslateChatStreamToResponses reads a chat completion SSE stream from body,
// translates each chunk into Responses API SSE events, and writes them to w.
// This is the streaming counterpart of convert.ChatCompletionToResponse.
func TranslateChatStreamToResponses(w http.ResponseWriter, body io.Reader, model string, reqTools json.RawMessage, searchToolCache []map[string]any, ctx context.Context) error {
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
	sc.Buffer(make([]byte, 0, config.StreamScannerInitialBuf), config.StreamScannerMaxBuf)
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
				"input_tokens":             chunk.Usage.PromptTokens,
				"output_tokens":            chunk.Usage.CompletionTokens,
				"total_tokens":             chunk.Usage.TotalTokens,
				"prompt_tokens":            chunk.Usage.PromptTokens,
				"completion_tokens":        chunk.Usage.CompletionTokens,
				"prompt_cache_hit_tokens":  hit,
				"prompt_cache_miss_tokens": miss,
			}
			if chunk.Usage.CompletionTokensDetails != nil {
				usage["completion_tokens_details"] = map[string]any{
					"reasoning_tokens": chunk.Usage.CompletionTokensDetails.ReasoningTokens,
				}
			}
			// Capture tokens for audit (nil-safe; ctx without audit yields nil).
			if a := middleware.AuditFromCtx(ctx); a != nil {
				a.TokensIn = chunk.Usage.PromptTokens
				a.TokensOut = chunk.Usage.CompletionTokens
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
			if util.DebugMode {
				slog.Debug("stream reasoning chunk", "req", util.RequestIDFromCtx(ctx), "content", d.ReasoningContent)
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
			if util.DebugMode {
				slog.Debug("stream content chunk", "req", util.RequestIDFromCtx(ctx), "content", d.Content)
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
			if util.DebugMode {
				slog.Debug("stream refusal chunk", "req", util.RequestIDFromCtx(ctx), "content", d.Refusal)
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
					itemID: "fc_" + util.RandomID(),
					callID: tc.ID,
					name:   mcp.ResolveNamespaceTool(tc.Function.Name),
				}
				tr.tools[idx] = st
			}
			if tc.ID != "" {
				st.callID = tc.ID
			}
			if tc.Function.Name != "" {
				st.name = mcp.ResolveNamespaceTool(tc.Function.Name)
				st.namespace = mcp.NamespaceForTool(tc.Function.Name)
			}
			if !st.added && st.name != "" {
				if util.DebugMode {
					slog.Debug("stream tool_call", "req", util.RequestIDFromCtx(ctx), "name", st.name, "call_id", st.callID)
				}
				// Intercept tool_search for synthetic response
				if st.name == "tool_search" && len(tr.searchToolCache) > 0 {
					tr.pendingSearchID = st.itemID
					if util.DebugMode {
						slog.Debug("stream tool_search intercepted", "req", util.RequestIDFromCtx(ctx), "cached_tools", len(tr.searchToolCache))
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
		if util.DebugMode {
			slog.Debug("stream scanner done", "req", util.RequestIDFromCtx(ctx), "error", err)
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
		if util.DebugMode {
			slog.Debug("stream empty upstream, emitting response.failed", "req", util.RequestIDFromCtx(ctx))
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
		if util.DebugMode {
			slog.Debug("stream clean EOF without completion event", "req", util.RequestIDFromCtx(ctx))
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

	if util.DebugMode {
		slog.Debug("stream ended", "req", util.RequestIDFromCtx(ctx), "had_content", tr.hadMessageContent, "tools", len(tr.tools))
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
			searchResultID := "ts_" + util.RandomID()
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
			if util.DebugMode {
				slog.Debug("stream tool_search synthetic result emitted", "req", util.RequestIDFromCtx(ctx), "tools", len(searchTools))
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

	resp := map[string]any{"id": tr.respID, "object": "response", "status": util.FinishReasonToStatus(lastFinishReason), "model": tr.model, "created_at": time.Now().Unix()}
	if len(reqTools) > 0 && string(reqTools) != "null" {
		resp["tools"] = util.JSONRawToAny(reqTools)
	}
	if usage != nil {
		resp["usage"] = usage
	}
	if lastLogprobs != nil {
		resp["logprobs"] = lastLogprobs
	}
	return tr.emit(dst, map[string]any{"type": "response.completed", "response": resp})
}
