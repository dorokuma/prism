package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/pool"
	"github.com/dorokuma/prism/internal/util"
)

// proxyMessages forwards an anthropic /v1/messages request to the upstream
// unchanged (pure passthrough): the body is NOT sanitized (it is not a chat
// completion body — remap/effort/strip would corrupt it) and NO responses
// translation is applied. Streaming SSE is passed through byte-for-byte by
// the legacy stream path in handleUpstreamResponse.
func proxyMessages(p *pool.Pool, w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	start := time.Now()
	defer r.Body.Close()
	const maxBodySize = 10 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("messages body read error", "error", err)
		http.Error(w, "failed to read body", 500)
		return
	}
	tenantID := getTenantID(r)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(bodyBytes, &raw)
	stream := util.RawBoolField(raw, "stream")
	model, _ := util.RawStringField(raw, "model")
	proxyChatWithBody(p, w, r, bodyBytes, start, ChatForwardOpts{
		Stream:       stream,
		Model:        model,
		TenantID:     tenantID,
		ResponsesOut: false, // never translate anthropic messages → responses
		UpstreamPath: "/v1/messages",
		SkipSanitize: true, // anthropic body must pass through byte-for-byte
	}, cfg)
}
