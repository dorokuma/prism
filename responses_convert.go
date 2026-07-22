package main

import (
	"github.com/dorokuma/prism/internal/convert"
	"github.com/dorokuma/prism/internal/sanitize"
	"github.com/dorokuma/prism/internal/util"
)

// responsesToChatCompletions converts a Responses API request body to a Chat Completions API request body.
var responsesToChatCompletions = convert.ResponsesToChatCompletions

// chatCompletionToResponse converts a Chat Completions API response body to a Responses API response body.
var chatCompletionToResponse = convert.ChatCompletionToResponse

// Existing shared utility shims (batch 1) — keep for backward compatibility.
var rawStringField = util.RawStringField
var rawBoolField = util.RawBoolField
var jsonRawToAny = util.JSONRawToAny
var isDeepSeekModel = util.IsDeepSeekModel
var mapThoughtLevel = util.MapThoughtLevel
var reasoningEffortFromRaw = util.ReasoningEffortFromRaw
var finishReasonToStatus = util.FinishReasonToStatus

// chatCompletionResponse type alias for proxy.go parseUsageFromChatCompletion.
type chatCompletionResponse = convert.ChatCompletionResponse

// Existing sanitize shim (batch 3) — keep for backward compatibility.
var mergeConsecutiveAssistantMessages = sanitize.MergeConsecutiveAssistantMessages
