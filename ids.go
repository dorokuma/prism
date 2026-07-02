package main

import (
	"crypto/rand"
	"encoding/hex"
)

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}