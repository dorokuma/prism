package sanitize

// Only Codex-internal properties that have no meaning for upstream models.
var codexParamPropertyBlacklist = map[string]bool{
	"justification":        true,
	"sandbox_permissions":  true,
	"prefix_rule":         true,
	"login":               true,
	"yield_time_ms":       true,
	"tty":                 true,
}

// SimplifyJSONSchema recursively deletes blacklisted keys from "properties"
// maps in a JSON Schema value tree.
func SimplifyJSONSchema(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, vv := range val {
			out[k] = SimplifyJSONSchema(vv)
		}
		if props, ok := out["properties"].(map[string]any); ok {
			for bad := range codexParamPropertyBlacklist {
				delete(props, bad)
			}
			out["properties"] = props
		}
		return out
	case []any:
		out := make([]any, 0, len(val))
		for _, item := range val {
			out = append(out, SimplifyJSONSchema(item))
		}
		return out
	default:
		return val
	}
}
