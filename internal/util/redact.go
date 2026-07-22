package util

import (
	"encoding/json"
	"regexp"
	"strings"
)

// TODO batch2: change to config.RedactJSONMaxDepth
const redactJSONMaxDepth = 20

var (
	reAPIKey = regexp.MustCompile(`sk-[A-Za-z0-9_-]{10,}`)
	reBearer = regexp.MustCompile(`Bearer [A-Za-z0-9_-]{10,}`)
)

// sensitiveJSONKeys names JSON object keys whose values should be redacted
// in debug logs and error responses. Compared after strings.ToLower.
var sensitiveJSONKeys = map[string]bool{
	"api_key":        true,
	"apikey":         true,
	"token":          true,
	"access_token":   true,
	"refresh_token":  true,
	"password":       true,
	"passwd":         true,
	"secret":         true,
	"authorization":  true,
	"key":            true,
	"client_key":     true,
	"session_key":    true,
}

// RedactBody masks common sensitive patterns in error/response bodies for safe
// logging. It first tries JSON-aware redaction (walk the object tree and replace
// sensitive-key values with "**"); if the body is not valid JSON it falls back
// to regex-based redaction of sk- and Bearer tokens.
func RedactBody(body []byte) string {
	return string(RedactBodyBytes(body))
}

// RedactBodyBytes is the []byte version of RedactBody, for direct use in
// response writing without an extra string allocation.
func RedactBodyBytes(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	// Try JSON-aware redaction first.
	var parsed any
	if err := json.Unmarshal(body, &parsed); err == nil && parsed != nil {
		parsed = RedactJSON(parsed, 0)
		if out, err := json.Marshal(parsed); err == nil {
			return out
		}
	}
	// Fall back to regex-based redaction.
	return []byte(RedactBodyRegex(string(body)))
}

// RedactBodyRegex applies regex-based redaction: sk- keys and Bearer tokens.
func RedactBodyRegex(s string) string {
	result := reAPIKey.ReplaceAllString(s, "sk-***")
	result = reBearer.ReplaceAllString(result, "Bearer ***")
	return result
}

// RedactJSON recursively walks a JSON value and replaces sensitive string
// values with "*". Arrays and nested objects are recursed into.  String
// leaf values also get regex redaction for embedded sk-/Bearer patterns.
// depth is capped at 20 to prevent stack overflow from malicious nesting.
// Returns the redacted value; when depth exceeds 20 the subtree is replaced
// with "<redacted:too deep>" instead of being silently passed through.
func RedactJSON(v any, depth int) any {
	if depth > redactJSONMaxDepth {
		return "<redacted:too deep>"
	}
	switch val := v.(type) {
	case map[string]any:
		for k, vv := range val {
			if sensitiveJSONKeys[strings.ToLower(k)] {
				val[k] = "***"
			} else if s, ok := vv.(string); ok {
				val[k] = RedactBodyRegex(s)
			} else {
				val[k] = RedactJSON(vv, depth+1)
			}
		}
		return val
	case []any:
		for i, item := range val {
			if s, ok := item.(string); ok {
				val[i] = RedactBodyRegex(s)
			} else {
				val[i] = RedactJSON(item, depth+1)
			}
		}
		return val
	}
	return v
}

// RedactBodyBytesWithKeys is like RedactBodyBytes but also scrubs each non-empty
// key from extraKeys as a literal substring inside string leaf values during
// JSON redaction.  Keys are NOT replaced in the raw bytes before JSON parsing
// to avoid corrupting JSON structure with short keys.
func RedactBodyBytesWithKeys(body []byte, extraKeys []string) []byte {
	if len(body) == 0 {
		return body
	}
	// Try JSON-aware redaction first (with key scrubbing inside string values).
	var parsed any
	if err := json.Unmarshal(body, &parsed); err == nil && parsed != nil {
		parsed = RedactJSONWithKeys(parsed, extraKeys, 0)
		if out, err := json.Marshal(parsed); err == nil {
			return out
		}
	}
	// Fall back to regex-based redaction + substring scrubbing on raw text.
	s := RedactBodyRegex(string(body))
	for _, k := range extraKeys {
		if k == "" {
			continue
		}
		s = strings.ReplaceAll(s, k, "***")
	}
	return []byte(s)
}

// RedactJSONWithKeys is like RedactJSON but additionally replaces any non-empty
// key from extraKeys as a literal substring inside string leaf values (on top
// of the existing regex redaction).
func RedactJSONWithKeys(v any, extraKeys []string, depth int) any {
	if depth > redactJSONMaxDepth {
		return "<redacted:too deep>"
	}
	switch val := v.(type) {
	case map[string]any:
		for k, vv := range val {
			if sensitiveJSONKeys[strings.ToLower(k)] {
				val[k] = "***"
			} else if s, ok := vv.(string); ok {
				val[k] = RedactStringWithKeys(s, extraKeys)
			} else {
				val[k] = RedactJSONWithKeys(vv, extraKeys, depth+1)
			}
		}
		return val
	case []any:
		for i, item := range val {
			if s, ok := item.(string); ok {
				val[i] = RedactStringWithKeys(s, extraKeys)
			} else {
				val[i] = RedactJSONWithKeys(item, extraKeys, depth+1)
			}
		}
		return val
	}
	return v
}

// RedactStringWithKeys applies regex redaction then replaces each non-empty
// extra key as a literal substring with "***".
func RedactStringWithKeys(s string, extraKeys []string) string {
	s = RedactBodyRegex(s)
	for _, k := range extraKeys {
		if k == "" {
			continue
		}
		s = strings.ReplaceAll(s, k, "***")
	}
	return s
}
