package pool

import (
	"encoding/json"
	"strings"
)

// preConsumeQuotaFailed is the AgentRouter new-api relay message when the
// remaining credit cannot cover one request. Live HTTP 403 bodies:
//
//	{"error":{"message":"pre-consume quota failed, user quota: ＄0.03, need quota: ＄0.54 (request id: …)","type":"new_api_error"},"type":"error"}
//
// Anthropic /v1/messages omits error.code; OpenAI /v1/chat/completions
// sometimes includes code "insufficient_user_quota". Matching this phrase
// on the structured error.message field covers both. Plain-text bodies
// that merely contain the words "quota" are not matched.
const preConsumeQuotaFailed = "pre-consume quota failed"

type upstreamErrorEnvelope struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// IsPermanentCredentialBody reports a structured permanent credential
// error: error.code invalid_api_key / revoked / account_deactivated.
func IsPermanentCredentialBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var errResp upstreamErrorEnvelope
	if err := json.Unmarshal(body, &errResp); err != nil || errResp.Error.Code == "" {
		return false
	}
	code := strings.ToLower(errResp.Error.Code)
	return code == "invalid_api_key" || code == "revoked" || code == "account_deactivated"
}

// IsQuotaErrorBody reports a structured permanent quota/balance error:
//
//   - error.type gousagelimiterror
//   - error.code insufficient_quota / insufficient_user_quota
//   - error.message contains "pre-consume quota failed" (AgentRouter
//     new_api_error; Anthropic envelopes often omit error.code)
//
// Matching is on the JSON error envelope only. A plain-text "quota
// exceeded" body is not a permanent quota error.
func IsQuotaErrorBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var errResp upstreamErrorEnvelope
	if err := json.Unmarshal(body, &errResp); err != nil {
		return false
	}
	if errResp.Error.Type != "" && strings.ToLower(errResp.Error.Type) == "gousagelimiterror" {
		return true
	}
	if errResp.Error.Code != "" {
		code := strings.ToLower(errResp.Error.Code)
		if code == "insufficient_quota" || code == "insufficient_user_quota" {
			return true
		}
	}
	if strings.Contains(strings.ToLower(errResp.Error.Message), preConsumeQuotaFailed) {
		return true
	}
	return false
}
