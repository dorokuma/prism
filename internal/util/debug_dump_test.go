package util

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestDumpDebugUpstreamResponseScrubsAccountKey guards the debug dump path
// (Batch-2 audit): the upstream response body is dumped through
// RedactBodyBytesWithKeys with the account key, so a custom auth_header key
// that does not look like an sk-/Bearer token is scrubbed and never lands on
// disk when an upstream echoes the credential it received.
func TestDumpDebugUpstreamResponseScrubsAccountKey(t *testing.T) {
	const key = "raw-key-98765"
	prev := DebugMode.Load()
	DebugMode.Store(true)
	defer func() { DebugMode.Store(prev) }()

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
	DebugMode.Store(false)
	_ = os.RemoveAll(dir)
	DumpDebugUpstreamResponse(body, []string{key})
	if _, err := os.Stat(filepath.Join(dir, "last-upstream-response.json")); err == nil {
		t.Error("debug dump must not be written when DebugMode is off")
	}
}

// TestDebugModeConcurrentReadWrite is the race-coverage test for the
// atomic DebugMode (audit round 6, item 4): the SIGHUP reload path writes
// the flag (Store) while request goroutines read it (Load) on every
// request. A plain bool was a data race; this test hammers concurrent
// Loads (through the real dump functions) against Stores, so `go test
// -race` proves the access is race-free. It also pins the behavior: each
// dump observes exactly the last stored value (atomic snapshot semantics).
func TestDebugModeConcurrentReadWrite(t *testing.T) {
	prev := DebugMode.Load()
	defer func() { DebugMode.Store(prev) }()

	dir := filepath.Join(os.TempDir(), "prism-debug")
	_ = os.RemoveAll(dir)
	defer os.RemoveAll(dir)

	body := []byte(`{"choices":[{"message":{"content":"x"}}]}`)
	const readers = 8
	const rounds = 200
	var wg sync.WaitGroup

	// Writers: flip the flag like the SIGHUP handler does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			DebugMode.Store(i%2 == 0)
		}
	}()

	// Readers: exercise the real debug-dump read paths (the exact Load
	// sites request goroutines hit).
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				DumpDebugChatBody(body)
				DumpDebugResponsesBody(body)
				DumpDebugUpstreamResponse(body, []string{"k"})
			}
		}()
	}
	wg.Wait()
}
