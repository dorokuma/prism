package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sync/atomic"
)

var idCounter atomic.Int64

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Printf("crypto/rand.Read failed: %v, using fallback", err)
		return fmt.Sprintf("fallback-%d", idCounter.Add(1))
	}
	return hex.EncodeToString(b[:])
}