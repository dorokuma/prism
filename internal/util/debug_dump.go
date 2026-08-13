package util

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
)

// DebugMode controls whether debug dumps are written. It is an atomic.Bool:
// the SIGHUP config reload in cmd/prism writes it (Store) while request
// goroutines read it (Load) on every proxied request — a plain bool would be
// a data race (audit round 6, item 4).
var DebugMode atomic.Bool

// debugDumpMaxBytes caps the total size of a debug dump file: dumps are for
// debugging request STRUCTURE (model, ids, headers-relevant metadata), never
// for business content or unbounded bodies, so a 64 KiB cap is always
// enough — and keeps a misbehaving upstream from filling the disk with one
// dump.
const debugDumpMaxBytes = 64 << 10

// debugDumpTruncatedSuffix marks a dump that was cut at debugDumpMaxBytes.
const debugDumpTruncatedSuffix = "\n... [debug dump truncated]"

// debugDumpOmitted is the placeholder replacing business text values
// (prompt/completion content) in a debug dump.
const debugDumpOmitted = "<omitted>"

// debugDumpContentKeys names JSON object keys whose values are business
// content (prompts, completions, tool arguments, reasoning text) that must
// never land in a debug dump, even in debug mode. Compared after
// strings.ToLower, like sensitiveJSONKeys. A string value is replaced with
// debugDumpOmitted; any other value (string arrays, text blocks, tool
// arguments objects) is recursed into in "content mode" where every string
// is replaced but the object keys, numbers, booleans and the array / object
// shape are preserved — no raw text under these keys ever survives.
var debugDumpContentKeys = map[string]bool{
	"arguments":         true,
	"content":           true,
	"input":             true,
	"instructions":      true,
	"output":            true,
	"prompt":            true,
	"reasoning":         true,
	"reasoning_content": true,
	"reasoning_text":    true,
	"refusal":           true,
	"summary":           true,
	"system":            true,
	"text":              true,
}

// debugDumpOmittedBody is the safe placeholder produced when the whole body
// cannot be structurally sanitized — it is not valid JSON, or it is a
// top-level JSON scalar (string / number / bool / null). The original text
// is never kept; only the placeholder and the original byte length survive,
// so the dump still carries a hint of what was dropped. The result is tiny
// and always stays far below debugDumpMaxBytes.
func debugDumpOmittedBody(originalLen int) []byte {
	return []byte(fmt.Sprintf(`{"omitted":%q,"bytes":%d}`, debugDumpOmitted, originalLen))
}

// debugDumpSanitize prepares a redacted body for a debug dump: it keeps the
// JSON structure (object keys, arrays, non-business values such as model /
// role / usage / ids) but omits business text content and caps the total
// size at debugDumpMaxBytes. Credential redaction is NOT part of this
// function — callers redact first (RedactBody / RedactBodyBytesWithKeys),
// so the credential-redaction semantics stay exactly where they are and the
// sanitize step only removes business body and bounds the size. When the
// body is not valid JSON, or it parses to a top-level scalar, the original
// text is NEVER kept: it is replaced with debugDumpOmittedBody (placeholder
// + original byte length). A truncated dump is marked with
// debugDumpTruncatedSuffix.
func debugDumpSanitize(body []byte) []byte {
	out := body
	var parsed any
	if err := json.Unmarshal(body, &parsed); err == nil && parsed != nil {
		switch parsed.(type) {
		case map[string]any, []any:
			parsed = omitDebugContent(parsed, false)
			if marshaled, err := json.Marshal(parsed); err == nil {
				out = marshaled
			}
		default:
			// Top-level JSON scalar (e.g. a bare JSON string): the raw
			// text must not survive.
			out = debugDumpOmittedBody(len(body))
		}
	} else {
		// Not valid JSON (or JSON null): the raw text must not survive.
		out = debugDumpOmittedBody(len(body))
	}
	if len(out) > debugDumpMaxBytes {
		keep := debugDumpMaxBytes - len(debugDumpTruncatedSuffix)
		if keep < 0 {
			keep = 0
		}
		out = append(out[:keep], debugDumpTruncatedSuffix...)
	}
	return out
}

// omitDebugContent walks a parsed JSON value. inContent marks values that
// live under a business-content key (see debugDumpContentKeys): inside such a
// subtree every string — regardless of its key — is replaced with
// debugDumpOmitted, because a sensitive value may be a string, a list of
// strings, a text block or a tool-arguments object whose strings are all
// business content. The keys, numbers, booleans and the array / object shape
// are kept so the surrounding structure stays debuggable. Outside content
// subtrees only the string values of business-content keys are replaced;
// structural strings (model, role, ids, function names) pass through.
func omitDebugContent(v any, inContent bool) any {
	switch val := v.(type) {
	case map[string]any:
		for k, vv := range val {
			if debugDumpContentKeys[strings.ToLower(k)] {
				val[k] = omitDebugContent(vv, true)
				continue
			}
			val[k] = omitDebugContent(vv, inContent)
		}
		return val
	case []any:
		for i, item := range val {
			val[i] = omitDebugContent(item, inContent)
		}
		return val
	case string:
		if inContent {
			return debugDumpOmitted
		}
		return val
	}
	return v
}

func debugDumpDir() string {
	return filepath.Join(os.TempDir(), "prism-debug")
}

func initDebugDumpDir() (string, error) {
	dir := debugDumpDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("debug dump path is not a directory")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("debug dump dir stat unsupported")
	}
	if int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("debug dump dir not owned by current user")
	}
	return dir, nil
}

func writeDebugDumpFile(name string, data []byte) error {
	dir, err := initDebugDumpDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	cerr := f.Close()
	if err != nil {
		return err
	}
	return cerr
}

// DumpDebugChatBody dumps the chat request body to a temp file for debugging.
func DumpDebugChatBody(chatBody []byte) {
	if !DebugMode.Load() {
		return
	}
	sanitized := debugDumpSanitize([]byte(RedactBody(chatBody)))
	if err := writeDebugDumpFile("last-chat-request.json", sanitized); err != nil {
		slog.Debug("debug dump failed", "error", err)
		return
	}
	slog.Debug("debug wrote dump", "path", filepath.Join(debugDumpDir(), "last-chat-request.json"), "bytes", len(sanitized))
}

// DumpDebugResponsesBody dumps the responses body to a temp file for debugging.
func DumpDebugResponsesBody(originalBody []byte) {
	if !DebugMode.Load() {
		return
	}
	sanitized := debugDumpSanitize([]byte(RedactBody(originalBody)))
	if err := writeDebugDumpFile("last-responses-request.json", sanitized); err != nil {
		slog.Debug("debug responses dump failed", "error", err)
	}
}

// DumpDebugUpstreamResponse dumps the upstream response body to a temp file
// for debugging. extraKeys (the account key) are scrubbed as literal
// substrings on top of the sk-/Bearer regex redaction, so a custom
// auth_header key that does not look like an sk-/Bearer token never lands on
// disk when an upstream echoes the credential it received.
func DumpDebugUpstreamResponse(rawBody []byte, extraKeys []string) {
	if !DebugMode.Load() {
		return
	}
	sanitized := debugDumpSanitize(RedactBodyBytesWithKeys(rawBody, extraKeys))
	if err := writeDebugDumpFile("last-upstream-response.json", sanitized); err != nil {
		slog.Debug("debug upstream response dump failed", "error", err)
		return
	}
	slog.Debug("debug wrote upstream response dump", "path", filepath.Join(debugDumpDir(), "last-upstream-response.json"), "bytes", len(sanitized))
}
