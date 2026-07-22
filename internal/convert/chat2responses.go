package convert

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/mcp"
	"github.com/dorokuma/prism/internal/util"
)

// ChatCompletionToResponse converts a Chat Completions API response body to a Responses API response body.
func ChatCompletionToResponse(body []byte, model string, reqTools json.RawMessage) ([]byte, error) {
	var comp ChatCompletionResponse
	if err := json.Unmarshal(body, &comp); err != nil {
		return nil, err
	}
	if len(comp.Choices) == 0 {
		return nil, fmt.Errorf("chat completion: no choices")
	}
	ch := comp.Choices[0]
	respID := "resp_" + util.RandomID()
	output := make([]map[string]any, 0)
	if ch.Message.ReasoningContent != "" {
		output = append(output, map[string]any{
			"type": "reasoning", "id": "rs_" + util.RandomID(), "status": "completed",
			"summary": []map[string]any{{"type": "summary_text", "text": ch.Message.ReasoningContent}},
		})
	}
	if ch.Message.Refusal != "" {
		msg := map[string]any{
			"type": "message", "id": "msg_" + util.RandomID(), "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": ch.Message.Refusal}},
		}
		if ch.Message.Annotations != nil {
			msg["annotations"] = ch.Message.Annotations
		}
		output = append(output, msg)
	} else if ch.Message.Content != nil && contentString(ch.Message.Content) != "" {
		msg := map[string]any{
			"type": "message", "id": "msg_" + util.RandomID(), "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": contentString(ch.Message.Content)}},
		}
		if ch.Message.Annotations != nil {
			msg["annotations"] = ch.Message.Annotations
		}
		output = append(output, msg)
	}
	for _, tc := range ch.Message.ToolCalls {
		name := mcp.ResolveNamespaceTool(tc.Function.Name)
		ns := mcp.NamespaceForTool(tc.Function.Name)
		item := map[string]any{
			"type": "function_call", "id": "fc_" + util.RandomID(), "call_id": tc.ID,
			"name": name, "arguments": tc.Function.Arguments, "status": "completed",
		}
		if ns != "" {
			item["namespace"] = ns
		}
		output = append(output, item)
	}
	usage := map[string]any{}
	if comp.Usage != nil {
		hit := comp.Usage.PromptCacheHitTokens
		miss := comp.Usage.PromptCacheMissTokens
		if hit == 0 && comp.Usage.PromptTokensDetails != nil {
			hit = comp.Usage.PromptTokensDetails.CachedTokens
		}
		if miss == 0 && hit > 0 && comp.Usage.PromptTokens > hit {
			miss = comp.Usage.PromptTokens - hit
		}
		usage = map[string]any{
			"input_tokens": comp.Usage.PromptTokens, "output_tokens": comp.Usage.CompletionTokens,
			"total_tokens": comp.Usage.TotalTokens,
			"prompt_tokens": comp.Usage.PromptTokens,
			"completion_tokens": comp.Usage.CompletionTokens,
			"prompt_cache_hit_tokens": hit,
			"prompt_cache_miss_tokens": miss,
		}
		if comp.Usage.CompletionTokensDetails != nil {
			usage["completion_tokens_details"] = map[string]any{
				"reasoning_tokens": comp.Usage.CompletionTokensDetails.ReasoningTokens,
			}
		}
	}
	createdAt := int64(comp.Created)
	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}
	resp := map[string]any{
		"id": respID, "object": "response", "status": util.FinishReasonToStatus(ch.FinishReason),
		"model": model, "output": output, "usage": usage, "created_at": createdAt,
	}
	if ch.Logprobs != nil {
		resp["logprobs"] = ch.Logprobs
	}
	if len(reqTools) > 0 && string(reqTools) != "null" {
		resp["tools"] = util.JSONRawToAny(reqTools)
	}
	return json.Marshal(resp)
}

type ChatCompletionResponse struct {
	Model   string `json:"model"`
	Created int    `json:"created"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Logprobs     any    `json:"logprobs"`
		Message      struct {
			Role             string `json:"role"`
			Content          any    `json:"content"`
			Refusal          string `json:"refusal"`
			Annotations      any    `json:"annotations"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
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

func contentString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case []any:
		// Multimodal content parts: extract text portions
		var b strings.Builder
		for _, part := range t {
			if m, ok := part.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
