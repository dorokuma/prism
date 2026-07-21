package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON writes a JSON response with the given status code. Errors during
// encoding/writing (typically caused by a disconnected client) are logged but
// not returned, since the connection is already lost.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("writeJSON encode/write failed", "error", err)
	}
}
