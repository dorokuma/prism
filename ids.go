package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
)

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Printf("crypto/rand.Read failed: %v, using fallback", err)
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}