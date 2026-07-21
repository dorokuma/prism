package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err == nil {
		fw.f.Flush()
	}
	return n, err
}

// tailWriter keeps the last N bytes written to it, bounded to maxSize.
// Write always succeeds (never returns an error) and the internal buffer
// is kept to at most maxSize bytes by dropping the oldest data.
type tailWriter struct {
	buf     []byte
	maxSize int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.maxSize {
		// Keep only the last maxSize bytes.
		t.buf = t.buf[len(t.buf)-t.maxSize:]
	}
	return len(p), nil
}

func (t *tailWriter) bytes() []byte {
	return t.buf
}

// parseStreamUsage scans an SSE stream tail buffer for the last data: chunk
// that contains a usage object and returns prompt_tokens/completion_tokens.
func parseStreamUsage(data []byte) (tokensIn, tokensOut int) {
	// Split on double newline — SSE event boundaries.
	events := bytes.Split(data, []byte("\n\n"))
	// Walk backwards to find the usage chunk faster (it's usually the last
	// content event before data: [DONE]).
	for i := len(events) - 1; i >= 0; i-- {
		event := bytes.TrimSpace(events[i])
		if len(event) == 0 {
			continue
		}
		lines := bytes.Split(event, []byte("\n"))
		for _, line := range lines {
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}
			payload := bytes.TrimSpace(line[6:])
			if bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			var chunk struct {
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(payload, &chunk) == nil && chunk.Usage != nil {
				return chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
			}
		}
	}
	return 0, 0
}

func streamResponseBody(w http.ResponseWriter, body io.ReadCloser, clientReq *http.Request, account string) (int64, error) {
	dst := io.Writer(w)
	if flusher, ok := w.(http.Flusher); ok {
		dst = &flushWriter{w: w, f: flusher}
	}

	// Tee the upstream body through a bounded tail writer so we can
	// extract token usage from the final SSE chunks after the stream
	// without buffering the entire response in memory.
	tail := &tailWriter{maxSize: 8192}
	teeReader := io.TeeReader(body, tail)

	n, err := io.Copy(dst, teeReader)

	// Capture token usage for audit (nil-safe; legacy streaming path).
	if clientReq != nil {
		if a := auditFromCtx(clientReq.Context()); a != nil {
			if tokensIn, tokensOut := parseStreamUsage(tail.bytes()); tokensIn > 0 || tokensOut > 0 {
				a.tokensIn = tokensIn
				a.tokensOut = tokensOut
			}
		}
	}

	if err != nil {
		if clientReq != nil && clientReq.Context().Err() != nil {
			slog.Warn("client disconnected during stream", "account", account, "written", n, "error", err)
		} else {
			slog.Error("upstream stream error", "account", account, "written", n, "error", err)
		}
		// Drain the upstream body so the account connection is released cleanly
		// even when the downstream client has already gone away.
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			slog.Warn("drain upstream body error", "account", account, "error", drainErr)
		}
		return n, err
	}
	return n, nil
}
