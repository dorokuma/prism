package main

import (
	"log"
	"os"
	"path/filepath"
)

var debugMode bool

func initDebugDumpDir() string {
	dir := filepath.Join(os.TempDir(), "prism-debug")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

func dumpDebugChatBody(chatBody []byte) {
	if !debugMode {
		return
	}
	dir := initDebugDumpDir()
	path := filepath.Join(dir, "last-chat-request.json")
	sanitized := []byte(redactBody(chatBody))
	if err := os.WriteFile(path, sanitized, 0o600); err != nil {
		log.Printf("debug: dump failed: %v", err)
		return
	}
	log.Printf("debug: wrote %s (%d bytes)", path, len(sanitized))
}

func dumpDebugResponsesBody(originalBody []byte) {
	if !debugMode {
		return
	}
	dir := initDebugDumpDir()
	path := filepath.Join(dir, "last-responses-request.json")
	sanitized := []byte(redactBody(originalBody))
	if err := os.WriteFile(path, sanitized, 0o600); err != nil {
		log.Printf("debug: dump failed: %v", err)
	}
}

func dumpDebugUpstreamResponse(rawBody []byte) {
	if !debugMode {
		return
	}
	dir := initDebugDumpDir()
	path := filepath.Join(dir, "last-upstream-response.json")
	sanitized := []byte(redactBody(rawBody))
	if err := os.WriteFile(path, sanitized, 0o600); err != nil {
		log.Printf("debug: dump failed: %v", err)
	}
}
