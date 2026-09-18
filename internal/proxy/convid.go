package proxy

import (
	"net/http"
	"strings"

	"github.com/dorokuma/prism/internal/pool"
)

// convIDHeaders defines candidate header names in descending priority order.
var convIDHeaders = []string{
	"x-grok-conv-id",
	"session_id",
	"session-id",
	"x-session-affinity",
}

// isUUID reports whether s is a canonical 36-character 8-4-4-4-12 hex UUID.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			c := s[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

// stripLaneSuffix extracts the base conversation ID when the value matches
// <uuid>:<suffix> (e.g. pi lane / subagent ids), so all lanes in the same
// session share the parent conversation cache. Non-UUID prefixes with colons
// are preserved as-is.
func stripLaneSuffix(v string) string {
	idx := strings.IndexByte(v, ':')
	if idx == -1 {
		return v
	}
	prefix := v[:idx]
	suffix := v[idx+1:]
	if len(suffix) > 0 && isUUID(prefix) {
		return prefix
	}
	return v
}

// sanitizeConvID cleans the conversation ID:
// - strips leading and trailing whitespace
// - keeps only [A-Za-z0-9._:-]
// - truncates to at most 128 characters
// Returns empty string if no valid characters remain.
func sanitizeConvID(s string) string {
	var sb strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == ':' || r == '-' {
			sb.WriteRune(r)
			if sb.Len() == 128 {
				break
			}
		}
	}
	return sb.String()
}

// getHeaderIgnoreCase searches h for the given target key case-insensitively.
func getHeaderIgnoreCase(h http.Header, target string) string {
	if h == nil {
		return ""
	}
	if v := strings.TrimSpace(h.Get(target)); v != "" {
		return v
	}
	for k, vs := range h {
		if strings.EqualFold(k, target) {
			for _, v := range vs {
				if tv := strings.TrimSpace(v); tv != "" {
					return tv
				}
			}
		}
	}
	return ""
}

// extractConvID checks candidate headers in priority order and returns the first
// valid sanitized conversation ID, or "" if none are found.
func extractConvID(src http.Header) string {
	for _, key := range convIDHeaders {
		val := getHeaderIgnoreCase(src, key)
		if val == "" {
			continue
		}
		if s := sanitizeConvID(val); s != "" {
			return stripLaneSuffix(s)
		}
	}
	return ""
}

// applyXaiConvID maps the client's session affinity header to x-grok-conv-id
// for xAI upstreams only. Priority: explicit client x-grok-conv-id >
// session_id/session-id > x-session-affinity.
func applyXaiConvID(dst http.Header, src http.Header, acc *pool.Account) {
	if acc == nil || acc.Provider() != "xai" {
		return
	}
	if dst == nil || src == nil {
		return
	}
	// If dst already contains X-Grok-Conv-Id (e.g. explicitly supplied by client
	// and copied by copyClientHeaders), do not overwrite it.
	if getHeaderIgnoreCase(dst, "x-grok-conv-id") != "" {
		return
	}
	if v := extractConvID(src); v != "" {
		dst.Set("X-Grok-Conv-Id", v)
		dst.Del("session_id")
		dst.Del("session-id")
		for k := range dst {
			if strings.EqualFold(k, "session_id") || strings.EqualFold(k, "session-id") {
				delete(dst, k)
			}
		}
	}
}
