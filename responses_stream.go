package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// ErrEmptyUpstreamStream is returned when the chat completion stream has no model output.
var ErrEmptyUpstreamStream = errors.New("empty upstream chat completion stream")

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
	msgAdded           bool
	reasoningAdded     bool
	reasoningPartAdded bool
	contentPartAdded   bool
	hadMessageContent  bool
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
	if !t.reasoningAdded {
		t.reasoningAdded = true
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
	if !t.reasoningPartAdded {
		t.reasoningPartAdded = true
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
	if !t.msgAdded {
		t.msgAdded = true
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
	if !t.contentPartAdded {
		t.contentPartAdded = true
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

func translateChatStreamToResponses(w http.ResponseWriter, body io.Reader, model string, reqTools json.RawMessage, searchToolCache []map[string]any) error {
	flusher, _ := w.(http.Flusher)
	dst := io.Writer(w)
	if flusher != nil {
		dst = &flushWriter{w: w, f: flusher}
	}
	tr := newResponsesStreamTranslator(model, searchToolCache)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var usage map[string]any

	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimSpace(line[6:])
		if bytes.Equal(payload, []byte("[DONE]")) {
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
		// Upstream may stream reasoning_content (e.g. DeepSeek). Codex 0.142.5 expects
		// response.reasoning_summary_text.delta (not reasoning_summary.delta).
		if d.ReasoningContent != "" {
				if debugMode {
					log.Printf("stream: reasoning chunk: %q", d.ReasoningContent)
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
				log.Printf("stream: content chunk: %q", d.Content)
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
					log.Printf("stream: tool_call name=%s callID=%s", st.name, st.callID)
				}
				// Intercept tool_search for synthetic response
				if st.name == "tool_search" && len(tr.searchToolCache) > 0 {
					tr.pendingSearchID = st.itemID
					if debugMode {
						log.Printf("stream: tool_search intercepted, %d cached tools", len(tr.searchToolCache))
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
			log.Printf("stream: scanner done, err=%v", err)
		}
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
			log.Printf("stream: empty upstream, emitting response.failed")
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

	if debugMode {
		log.Printf("stream: ended, hadContent=%v, tools=%d", tr.hadMessageContent, len(tr.tools))
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
				log.Printf("stream: tool_search synthetic result emitted (%d tools)", len(searchTools))
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
	if tr.reasoningAdded {
		if err := tr.emit(dst, map[string]any{
			"type": "response.output_item.done", "output_index": tr.reasoningOutputIdx,
			"item": map[string]any{
				"type": "reasoning", "id": tr.reasoningItemID, "status": "completed",
			},
		}); err != nil {
			return err
		}
	}

	if tr.msgAdded || tr.hadMessageContent {
		if !tr.msgAdded {
			if err := tr.ensureMessageStream(dst); err != nil {
				return err
			}
		}
		if tr.contentPartAdded {
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

	resp := map[string]any{"id": tr.respID, "object": "response", "status": "completed", "model": tr.model}
	if len(reqTools) > 0 && string(reqTools) != "null" {
		resp["tools"] = jsonRawToAny(reqTools)
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return tr.emit(dst, map[string]any{"type": "response.completed", "response": resp})
}

type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
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