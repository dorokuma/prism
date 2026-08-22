package pool

import "testing"

func TestIsQuotaErrorBody_AgentRouterPreConsume(t *testing.T) {
	// Live Anthropic /v1/messages 403: no error.code, fullwidth dollar sign.
	ant := []byte(`{"error":{"message":"pre-consume quota failed, user quota: ＄0.031792, need quota: ＄0.545150 (request id: 20260822200214517664032m5lgqVkUakoRB)","type":"new_api_error"},"type":"error"}`)
	if !IsQuotaErrorBody(ant) {
		t.Fatal("Anthropic pre-consume envelope (no code) must be a quota error")
	}

	// Live OpenAI /v1/chat/completions 403: includes insufficient_user_quota.
	oai := []byte(`{"error":{"code":"insufficient_user_quota","message":"pre-consume quota failed, user quota: ＄0.090214, need quota: ＄0.913616 (request id: 20260821230327400780460rh8pcBdDhZFwa)","param":"","type":"new_api_error"}}`)
	if !IsQuotaErrorBody(oai) {
		t.Fatal("OpenAI pre-consume envelope with insufficient_user_quota must be a quota error")
	}
}

func TestIsQuotaErrorBody_DoesNotMatchUnrelated(t *testing.T) {
	if IsQuotaErrorBody([]byte(`{"error":{"code":"some_other_error","type":"new_api_error"}}`)) {
		t.Error("unrelated new_api_error code must not match")
	}
	if IsQuotaErrorBody([]byte(`quota exceeded`)) {
		t.Error("plain-text quota exceeded must not match")
	}
	if IsQuotaErrorBody([]byte(`{"error":{"message":"model not found","type":"new_api_error"}}`)) {
		t.Error("new_api_error without pre-consume message must not match")
	}
	if IsQuotaErrorBody([]byte(`pre-consume quota failed`)) {
		t.Error("bare phrase outside a JSON error envelope must not match")
	}
	if IsQuotaErrorBody(nil) {
		t.Error("nil body must not match")
	}
}

func TestIsPermanentCredentialBody(t *testing.T) {
	if !IsPermanentCredentialBody([]byte(`{"error":{"code":"invalid_api_key"}}`)) {
		t.Error("invalid_api_key must match")
	}
	if IsPermanentCredentialBody([]byte(`{"error":{"code":"insufficient_quota"}}`)) {
		t.Error("insufficient_quota is quota, not credential")
	}
}
