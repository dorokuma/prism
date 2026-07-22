package util

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync/atomic"
)

var idCounter atomic.Int64

// RandomID generates a random hex string suitable for use as a request or response ID.
func RandomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		slog.Error("crypto/rand.Read failed, using fallback", "error", err)
		return fmt.Sprintf("fallback-%d", idCounter.Add(1))
	}
	return hex.EncodeToString(b[:])
}
