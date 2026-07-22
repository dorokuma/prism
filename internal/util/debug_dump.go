package util

import (
	"log/slog"
	"os"
	"path/filepath"
)

// DebugMode controls whether debug dumps are written.
var DebugMode bool

func initDebugDumpDir() string {
	dir := filepath.Join(os.TempDir(), "prism-debug")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// DumpDebugChatBody dumps the chat request body to a temp file for debugging.
func DumpDebugChatBody(chatBody []byte) {
	if !DebugMode {
		return
	}
	dir := initDebugDumpDir()
	path := filepath.Join(dir, "last-chat-request.json")
	sanitized := []byte(RedactBody(chatBody))
	if err := os.WriteFile(path, sanitized, 0o600); err != nil {
		slog.Debug("debug dump failed", "error", err)
		return
	}
	slog.Debug("debug wrote dump", "path", path, "bytes", len(sanitized))
}

// DumpDebugResponsesBody dumps the responses body to a temp file for debugging.
func DumpDebugResponsesBody(originalBody []byte) {
	if !DebugMode {
		return
	}
	dir := initDebugDumpDir()
	path := filepath.Join(dir, "last-responses-request.json")
	sanitized := []byte(RedactBody(originalBody))
	if err := os.WriteFile(path, sanitized, 0o600); err != nil {
		slog.Debug("debug responses dump failed", "error", err)
	}
}

// DumpDebugUpstreamResponse dumps the upstream response to a temp file for debugging.
func DumpDebugUpstreamResponse(rawBody []byte) {
	if !DebugMode {
		return
	}
	dir := initDebugDumpDir()
	path := filepath.Join(dir, "last-upstream-response.json")
	sanitized := []byte(RedactBody(rawBody))
	if err := os.WriteFile(path, sanitized, 0o600); err != nil {
		slog.Debug("debug upstream response dump failed", "error", err)
	}
}
