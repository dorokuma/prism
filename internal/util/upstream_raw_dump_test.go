package util

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDumpUpstreamSSEEnabled_DefaultOff(t *testing.T) {
	t.Setenv(EnvDumpUpstreamSSE, "")
	if DumpUpstreamSSEEnabled() {
		t.Fatal("dump must be off when env is empty")
	}
	t.Setenv(EnvDumpUpstreamSSE, "0")
	if DumpUpstreamSSEEnabled() {
		t.Fatal("dump must be off when env is 0")
	}
	t.Setenv(EnvDumpUpstreamSSE, "false")
	if DumpUpstreamSSEEnabled() {
		t.Fatal("dump must be off when env is false")
	}
	t.Setenv(EnvDumpUpstreamSSE, "off")
	if DumpUpstreamSSEEnabled() {
		t.Fatal("dump must be off when env is off")
	}
}

func TestDumpUpstreamSSEEnabled_Truthy(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", " yes ", "On"} {
		t.Setenv(EnvDumpUpstreamSSE, v)
		if !DumpUpstreamSSEEnabled() {
			t.Fatalf("dump must be on for %q", v)
		}
	}
}

func TestStartUpstreamRawDump_DisabledIsNil(t *testing.T) {
	t.Setenv(EnvDumpUpstreamSSE, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "must-not-exist.txt")
	t.Setenv(EnvDumpUpstreamSSEPath, path)

	if got := StartUpstreamRawDump(UpstreamRawDumpMeta{RequestID: "r1"}); got != nil {
		t.Fatalf("Start must return nil when dump is off, got %#v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("dump file must not be created when off, stat err=%v", err)
	}
}

func TestRedactHTTPHeaders_AuthorizationAndKeys(t *testing.T) {
	h := make(http.Header)
	h.Set("Authorization", "placeholder-auth-value")
	h.Set("X-Api-Key", "placeholder-api-key-value")
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Request-Id", "abc")
	h.Add("Cookie", "session=leak")
	h.Set("X-Custom-Token", "tok-leak")

	out := RedactHTTPHeaders(h)
	if got := out.Get("Authorization"); got != "REDACTED" {
		t.Fatalf("Authorization = %q, want REDACTED", got)
	}
	if got := out.Get("X-Api-Key"); got != "REDACTED" {
		t.Fatalf("X-Api-Key = %q, want REDACTED", got)
	}
	if got := out.Get("Cookie"); got != "REDACTED" {
		t.Fatalf("Cookie = %q, want REDACTED", got)
	}
	if got := out.Get("X-Custom-Token"); got != "REDACTED" {
		t.Fatalf("X-Custom-Token = %q, want REDACTED", got)
	}
	if got := out.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type rewritten: %q", got)
	}
	if got := out.Get("X-Request-Id"); got != "abc" {
		t.Fatalf("X-Request-Id rewritten: %q", got)
	}
	if h.Get("Authorization") != "placeholder-auth-value" {
		t.Fatal("RedactHTTPHeaders must not mutate the input header")
	}
}

func TestStartUpstreamRawDump_WritesRedactedHeadersAndVerbatimBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raw.sse")
	t.Setenv(EnvDumpUpstreamSSE, "1")
	t.Setenv(EnvDumpUpstreamSSEPath, path)

	authVal := "placeholder-auth-value"
	h := make(http.Header)
	h.Set("Authorization", authVal)
	h.Set("Content-Type", "application/json")

	body := "data: {\"choices\":[{\"delta\":{\"content\":\"<｜tool▁calls▁begin｜>\"}}]}\n\ndata: [DONE]\n\n"

	dump := StartUpstreamRawDump(UpstreamRawDumpMeta{
		RequestID: "req-test-1",
		Status:    200,
		URL:       "https://api.cline.bot/api/v1/chat/completions",
		Header:    h,
	})
	if dump == nil {
		t.Fatal("Start must return a dump when env is on")
	}
	if dump.Path() != path {
		t.Fatalf("path = %q, want %q", dump.Path(), path)
	}
	n, err := dump.Write([]byte(body))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(body) {
		t.Fatalf("Write n=%d want %d", n, len(body))
	}
	if err := dump.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	s := string(raw)
	if strings.Contains(s, authVal) {
		t.Fatalf("dump leaked credential: %s", s)
	}
	if !strings.Contains(s, "Authorization: REDACTED") {
		t.Fatalf("dump missing redacted Authorization:\n%s", s)
	}
	if !strings.Contains(s, "Content-Type: application/json") {
		t.Fatalf("dump missing non-secret header:\n%s", s)
	}
	idx := strings.Index(s, UpstreamRawDumpBodyMarker)
	if idx < 0 {
		t.Fatalf("dump missing body marker:\n%s", s)
	}
	gotBody := s[idx+len(UpstreamRawDumpBodyMarker):]
	if gotBody != body {
		t.Fatalf("body mismatch\ngot:  %q\nwant: %q", gotBody, body)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("dump mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestDumpUpstreamRawBytes_OffDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope.txt")
	t.Setenv(EnvDumpUpstreamSSE, "")
	t.Setenv(EnvDumpUpstreamSSEPath, path)
	DumpUpstreamRawBytes(UpstreamRawDumpMeta{RequestID: "x"}, []byte("data: hi\n\n"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("DumpUpstreamRawBytes must be a no-op when dump is off")
	}
}

func TestTeeReader_DumpDoesNotAlterCopiedBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tee.sse")
	t.Setenv(EnvDumpUpstreamSSE, "true")
	t.Setenv(EnvDumpUpstreamSSEPath, path)

	payload := []byte("event: chunk\ndata: {\"id\":\"1\"}\n\n")
	dump := StartUpstreamRawDump(UpstreamRawDumpMeta{Status: 200})
	if dump == nil {
		t.Fatal("expected dump")
	}
	var dst bytes.Buffer
	n, err := io.Copy(&dst, io.TeeReader(bytes.NewReader(payload), dump))
	if err != nil {
		t.Fatal(err)
	}
	if err := dump.Close(); err != nil {
		t.Fatal(err)
	}
	if n != int64(len(payload)) || !bytes.Equal(dst.Bytes(), payload) {
		t.Fatalf("tee altered stream: n=%d dst=%q", n, dst.Bytes())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	idx := strings.Index(got, UpstreamRawDumpBodyMarker)
	if idx < 0 {
		t.Fatalf("missing marker: %s", got)
	}
	if got[idx+len(UpstreamRawDumpBodyMarker):] != string(payload) {
		t.Fatalf("dumped body != payload: %q", got)
	}
}
