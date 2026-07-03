package main

import (
	"log"
	"os"
	"path/filepath"
)

var debugMode bool

func initDebugDumpDir() string {
	dir := filepath.Join(os.TempDir(), "reasonix-lb-debug")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

func dumpDebugChatBody(chatBody []byte) {
	if !debugMode {
		return
	}
	dir := initDebugDumpDir()
	path := filepath.Join(dir, "last-chat-request.json")
	if err := os.WriteFile(path, chatBody, 0o600); err != nil {
		log.Printf("debug: dump failed: %v", err)
		return
	}
	log.Printf("debug: wrote %s (%d bytes)", path, len(chatBody))
}

func dumpDebugResponsesBody(originalBody []byte) {
	if !debugMode {
		return
	}
	dir := initDebugDumpDir()
	path := filepath.Join(dir, "last-responses-request.json")
	if err := os.WriteFile(path, originalBody, 0o600); err != nil {
		log.Printf("debug: dump failed: %v", err)
	}
}

func dumpDebugUpstreamResponse(rawBody []byte) {
	if !debugMode {
		return
	}
	dir := initDebugDumpDir()
	path := filepath.Join(dir, "last-upstream-response.json")
	if err := os.WriteFile(path, rawBody, 0o600); err != nil {
		log.Printf("debug: dump failed: %v", err)
	}
}
