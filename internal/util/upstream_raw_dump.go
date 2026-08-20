package util

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Environment switches for the optional upstream raw-body dump. Both are
// off by default: when PRISM_DUMP_UPSTREAM_SSE is unset/false the dump
// helpers return nil and the proxy path is identical to the pre-dump code.
const (
	EnvDumpUpstreamSSE     = "PRISM_DUMP_UPSTREAM_SSE"
	EnvDumpUpstreamSSEPath = "PRISM_DUMP_UPSTREAM_SSE_PATH"
)

// UpstreamRawDumpBodyMarker precedes the verbatim upstream body in a dump
// file so headers can sit above the payload without rewriting it.
const UpstreamRawDumpBodyMarker = "-----BEGIN PRISM UPSTREAM RESPONSE BODY-----\n"

var dumpSeq atomic.Uint64

// DumpUpstreamSSEEnabled reports whether the raw-upstream dump is on.
// Accepted truthy values: 1, true, yes, on (any case, surrounding space ignored).
func DumpUpstreamSSEEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvDumpUpstreamSSE)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// isSensitiveHeaderName reports whether an HTTP header name is a credential
// field that must be replaced with REDACTED in a dump.
func isSensitiveHeaderName(name string) bool {
	canon := textproto.CanonicalMIMEHeaderKey(name)
	switch canon {
	case "Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
		"X-Api-Key", "Api-Key", "X-Auth-Token", "X-Access-Token":
		return true
	}
	lower := strings.ToLower(canon)
	if strings.Contains(lower, "authorization") {
		return true
	}
	if strings.Contains(lower, "api-key") || strings.Contains(lower, "apikey") {
		return true
	}
	if strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "passwd") {
		return true
	}
	if strings.Contains(lower, "token") {
		return true
	}
	if strings.Contains(lower, "credential") {
		return true
	}
	return false
}

// RedactHTTPHeaders copies h with credential header values replaced by REDACTED.
// The input header is not mutated. A nil input yields an empty header.
func RedactHTTPHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	if h == nil {
		return out
	}
	for k, vs := range h {
		if isSensitiveHeaderName(k) {
			out[k] = []string{"REDACTED"}
			continue
		}
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// UpstreamRawDumpMeta is the non-body metadata written at the top of a dump.
type UpstreamRawDumpMeta struct {
	RequestID string
	Status    int
	URL       string
	Header    http.Header
}

// UpstreamRawDump is an open dump file. Write appends verbatim upstream body
// bytes; Close flushes and closes the file.
type UpstreamRawDump struct {
	f    *os.File
	path string
}

// Path returns the dump file path.
func (d *UpstreamRawDump) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

// Write appends raw upstream body bytes. It is safe to call with a nil dump.
func (d *UpstreamRawDump) Write(p []byte) (int, error) {
	if d == nil || d.f == nil {
		return len(p), nil
	}
	return d.f.Write(p)
}

// Close closes the dump file. A nil dump is a no-op.
func (d *UpstreamRawDump) Close() error {
	if d == nil || d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

func dumpEnabledEnv() bool {
	return DumpUpstreamSSEEnabled()
}

func dumpFilePath() string {
	if p := strings.TrimSpace(os.Getenv(EnvDumpUpstreamSSEPath)); p != "" {
		return p
	}
	n := dumpSeq.Add(1)
	return filepath.Join(os.TempDir(), fmt.Sprintf("prism-upstream-raw-%d-%d.txt", time.Now().UnixNano(), n))
}

func writeDumpPreamble(w io.Writer, meta UpstreamRawDumpMeta) error {
	if _, err := io.WriteString(w, "# prism upstream raw dump\n"); err != nil {
		return err
	}
	if meta.RequestID != "" {
		if _, err := fmt.Fprintf(w, "# request_id: %s\n", meta.RequestID); err != nil {
			return err
		}
	}
	if meta.Status != 0 {
		if _, err := fmt.Fprintf(w, "# upstream_status: %d\n", meta.Status); err != nil {
			return err
		}
	}
	if meta.URL != "" {
		if _, err := fmt.Fprintf(w, "# upstream_url: %s\n", meta.URL); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "# request headers (credentials redacted)\n\n"); err != nil {
		return err
	}
	redacted := RedactHTTPHeaders(meta.Header)
	if err := redacted.Write(w); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, UpstreamRawDumpBodyMarker); err != nil {
		return err
	}
	return nil
}

// StartUpstreamRawDump opens a dump file and writes the redacted header
// preamble. When the env switch is off it returns nil and the caller must
// treat the body path as unchanged. File-open or preamble errors are logged
// and return nil so the request path never fails because of the dump.
func StartUpstreamRawDump(meta UpstreamRawDumpMeta) *UpstreamRawDump {
	if !dumpEnabledEnv() {
		return nil
	}
	path := dumpFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Warn("upstream raw dump mkdir failed", "error", err)
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		slog.Warn("upstream raw dump open failed", "error", err)
		return nil
	}
	if err := writeDumpPreamble(f, meta); err != nil {
		_ = f.Close()
		slog.Warn("upstream raw dump preamble failed", "error", err)
		return nil
	}
	slog.Info("upstream raw dump started", "path", path)
	return &UpstreamRawDump{f: f, path: path}
}

// DumpUpstreamRawBytes writes a complete (already-buffered) upstream body
// to a dump file. No-op when the env switch is off.
func DumpUpstreamRawBytes(meta UpstreamRawDumpMeta, body []byte) {
	dump := StartUpstreamRawDump(meta)
	if dump == nil {
		return
	}
	if len(body) > 0 {
		if _, err := dump.Write(body); err != nil {
			slog.Warn("upstream raw dump write failed", "error", err)
		}
	}
	if err := dump.Close(); err != nil {
		slog.Warn("upstream raw dump close failed", "error", err)
	}
}
