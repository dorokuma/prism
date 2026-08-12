package sanitize

import (
	"errors"
)

// Only Codex-internal properties that have no meaning for upstream models.
var codexParamPropertyBlacklist = map[string]bool{
	"justification":       true,
	"sandbox_permissions": true,
	"prefix_rule":         true,
	"login":               true,
	"yield_time_ms":       true,
	"tty":                 true,
}

// ErrSchemaTooDeep is returned by SimplifyJSONSchemaLimited when a JSON
// Schema value tree exceeds the maximum recursion depth. The recursion is
// depth-bounded so a pathological schema (deeply nested maps/arrays from
// untrusted client input) fails fast with this sentinel instead of
// exhausting the goroutine stack or burning CPU. Callers handling untrusted
// input MUST surface this error to the client (the proxy maps it to a 400);
// it must never be swallowed or replaced by the unsafe original value.
var ErrSchemaTooDeep = errors.New("json schema exceeds maximum recursion depth")

// MaxJSONSchemaDepth is the maximum nesting depth SimplifyJSONSchema walks.
// 32 levels is far beyond any real tool parameter schema (the deepest
// production schema is ~6 levels), while the depth cap keeps the walk
// O(depth) in stack and O(nodes) in work.
const MaxJSONSchemaDepth = 32

// SimplifyJSONSchemaLimited is the depth-bounded, error-reporting
// SimplifyJSONSchema: it deletes blacklisted keys from "properties" maps in
// a JSON Schema value tree, walking at most maxDepth nested levels. When
// the limit is exceeded it returns (nil, ErrSchemaTooDeep) — fail-fast with
// no partial result and no fallback to the unsafe original value. maxDepth
// <= 0 rejects every non-empty tree.
func SimplifyJSONSchemaLimited(v any, maxDepth int) (any, error) {
	return simplifyJSONSchema(v, 0, maxDepth)
}

// SimplifyJSONSchema recursively deletes blacklisted keys from "properties"
// maps in a JSON Schema value tree.
//
// Deprecated: use SimplifyJSONSchemaLimited. This is the legacy
// compatibility wrapper: its plain signature cannot report a depth error, so
// on a too-deep schema it fails SAFE and deterministic — it returns nil, the
// same fail-fast result SimplifyJSONSchemaLimited reports as (nil,
// ErrSchemaTooDeep) — instead of the input unchanged. Returning the input
// would propagate an attacker-controlled deep structure to the caller;
// nil is the fixed failure value and cannot be mistaken for a sanitized
// schema. Callers handling untrusted input MUST use
// SimplifyJSONSchemaLimited and surface ErrSchemaTooDeep; this wrapper is
// kept only for API compatibility and must not be used for untrusted
// schemas.
func SimplifyJSONSchema(v any) any {
	out, err := simplifyJSONSchema(v, 0, MaxJSONSchemaDepth)
	if err != nil {
		return nil
	}
	return out
}

func simplifyJSONSchema(v any, depth, maxDepth int) (any, error) {
	if depth > maxDepth {
		return nil, ErrSchemaTooDeep
	}
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, vv := range val {
			nv, err := simplifyJSONSchema(vv, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		if props, ok := out["properties"].(map[string]any); ok {
			for bad := range codexParamPropertyBlacklist {
				delete(props, bad)
			}
			out["properties"] = props
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(val))
		for _, item := range val {
			nv, err := simplifyJSONSchema(item, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			out = append(out, nv)
		}
		return out, nil
	default:
		return val, nil
	}
}
