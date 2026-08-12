package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDumpDebugUpstreamResponseScrubsAccountKey guards the debug dump path
// (Batch-2 audit): the upstream response body is dumped through
// RedactBodyBytesWithKeys with the account key, so a custom auth_header key
// that does not look like an sk-/Bearer token is scrubbed and never lands on
// disk when an upstream echoes the credential it received.
func TestDumpDebugUpstreamResponseScrubsAccountKey(t *testing.T) {
	const key = "raw-key-98765"
	prev := DebugMode
	DebugMode = true
	defer func() { DebugMode = prev }()

	dir := filepath.Join(os.TempDir(), "prism-debug")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	body := []byte(`{"error":{"message":"invalid key ` + key + `","code":"invalid_api_key"}}`)
	DumpDebugUpstreamResponse(body, []string{key})

	data, err := os.ReadFile(filepath.Join(dir, "last-upstream-response.json"))
	if err != nil {
		t.Fatalf("read debug dump: %v", err)
	}
	if strings.Contains(string(data), key) {
		t.Errorf("debug dump leaks the non-sk account key: %s", data)
	}
	if !strings.Contains(string(data), "***") {
		t.Errorf("debug dump should carry the redaction marker: %s", data)
	}
	// Without the key the dump stays off (DebugMode false → no file).
	DebugMode = false
	_ = os.RemoveAll(dir)
	DumpDebugUpstreamResponse(body, []string{key})
	if _, err := os.Stat(filepath.Join(dir, "last-upstream-response.json")); err == nil {
		t.Error("debug dump must not be written when DebugMode is off")
	}
}
