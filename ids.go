package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync/atomic"
)

var idCounter atomic.Int64

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		slog.Error("crypto/rand.Read failed, using fallback", "error", err)
		return fmt.Sprintf("fallback-%d", idCounter.Add(1))
	}
	return hex.EncodeToString(b[:])
}