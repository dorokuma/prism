package main

import (
	"log"
	"os"
	"path/filepath"
)

func dumpDebugChatBody(chatBody []byte) {
	dir := filepath.Join(os.TempDir(), "reasonix-lb-debug")
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "last-chat-request.json")
	if err := os.WriteFile(path, chatBody, 0o600); err != nil {
		log.Printf("proxy: debug dump failed: %v", err)
		return
	}
	log.Printf("proxy: debug wrote %s (%d bytes)", path, len(chatBody))
}