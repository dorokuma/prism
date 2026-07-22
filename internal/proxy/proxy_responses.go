package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/convert"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

func proxyResponses(p *pool.Pool, w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	start := time.Now()
	defer r.Body.Close()
	const maxBodySize = 10 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("responses body read error", "error", err)
		http.Error(w, "failed to read body", 500)
		return
	}
	tenantID := getTenantID(r)

	// Extract model early for logging (before conversion in case it fails).
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(bodyBytes, &raw)
	virtualModel, _ := util.RawStringField(raw, "model")
	requestID := util.RequestIDFromCtx(r.Context())

	chatBody, stream, reqTools, err := convert.ResponsesToChatCompletions(bodyBytes, tenantID)
	util.DumpDebugResponsesBody(bodyBytes)
	util.DumpDebugChatBody(chatBody)
	if err != nil {
		slog.Error("responses convert error", "error", err)
		// Rejection logging: classify known rejection reasons.
		errStr := err.Error()
		if strings.Contains(errStr, "image") ||
			strings.Contains(errStr, "previous_response_id") ||
			strings.Contains(errStr, "not supported") {
			reason := "unsupported_input"
			if strings.Contains(errStr, "image") {
				reason = "multimodal_input"
			} else if strings.Contains(errStr, "previous_response_id") {
				reason = "previous_response_id"
			}
			slog.Warn("request_rejected", "req", requestID, "reason", reason, "model", virtualModel)
		}
		errBody, marshalErr := json.Marshal(map[string]any{
			"error": map[string]any{"message": err.Error(), "code": "invalid_request"},
		})
		if marshalErr != nil {
			errBody = []byte(`{"error":{"message":"internal error","code":"internal"}}`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write(errBody)
		return
	}

	slog.Debug("responses request", "remote_addr", r.RemoteAddr, "model", virtualModel, "stream", stream, "chat_body_bytes", len(chatBody))
	proxyChatWithBody(p, w, r, chatBody, start, ChatForwardOpts{
		ResponsesOut: true,
		Stream:       stream,
		Model:        virtualModel,
		ReqTools:     reqTools,
		TenantID:     tenantID,
	}, cfg)
}
