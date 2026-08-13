package util

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// WriteJSON writes a JSON response with the given status code. The payload is
// marshaled first: a marshal failure becomes HTTP 500 before any header is
// written. Encode/write errors after the header (typically a disconnected
// client) are logged but not returned.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Warn("writeJSON marshal failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"internal error","code":"internal_error"}}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := w.Write(append(body, '\n')); err != nil {
		slog.Warn("writeJSON encode/write failed", "error", err)
	}
}
