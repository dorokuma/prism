package main

import (
	"io"
	"log/slog"
	"net/http"
)

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err == nil {
		fw.f.Flush()
	}
	return n, err
}

func streamResponseBody(w http.ResponseWriter, body io.ReadCloser, clientReq *http.Request, account string) (int64, error) {
	dst := io.Writer(w)
	if flusher, ok := w.(http.Flusher); ok {
		dst = &flushWriter{w: w, f: flusher}
	}
	n, err := io.Copy(dst, body)
	if err != nil {
		if clientReq != nil && clientReq.Context().Err() != nil {
			slog.Warn("client disconnected during stream", "account", account, "written", n, "error", err)
		} else {
			slog.Error("upstream stream error", "account", account, "written", n, "error", err)
		}
		// Drain the upstream body so the account connection is released cleanly
		// even when the downstream client has already gone away.
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			slog.Warn("drain upstream body error", "account", account, "error", drainErr)
		}
		return n, err
	}
	return n, nil
}
