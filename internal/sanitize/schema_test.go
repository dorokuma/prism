package sanitize

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func mustJSON(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("bad test fixture JSON: %v", err)
	}
	return v
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	return string(b)
}

func TestSimplifyJSONSchemaLimited(t *testing.T) {
	tests := []struct {
		name    string
		input   string // JSON input
		want    string // JSON expected output
		wantNil bool   // input is nil → output should be nil
	}{
		{
			name:    "nil input",
			input:   "",
			wantNil: true,
		},
		{
			name:  "string primitive passes through",
			input: `"hello"`,
			want:  `"hello"`,
		},
		{
			name:  "number primitive passes through",
			input: `42`,
			want:  `42`,
		},
		{
			name:  "bool primitive passes through",
			input: `true`,
			want:  `true`,
		},
		{
			name:  "empty object unchanged",
			input: `{}`,
			want:  `{}`,
		},
		{
			name:  "simple object no blacklisted properties",
			input: `{"type":"object","properties":{"query":{"type":"string"}}}`,
			want:  `{"type":"object","properties":{"query":{"type":"string"}}}`,
		},
		{
			name:  "strips justification from properties",
			input: `{"type":"object","properties":{"name":{"type":"string"},"justification":{"type":"string"}}}`,
			want:  `{"type":"object","properties":{"name":{"type":"string"}}}`,
		},
		{
			name:  "strips sandbox_permissions from properties",
			input: `{"type":"object","properties":{"sandbox_permissions":{"type":"object"},"x":{"type":"number"}}}`,
			want:  `{"type":"object","properties":{"x":{"type":"number"}}}`,
		},
		{
			name:  "strips prefix_rule from properties",
			input: `{"type":"object","properties":{"prefix_rule":{"type":"string"},"name":{"type":"string"}}}`,
			want:  `{"type":"object","properties":{"name":{"type":"string"}}}`,
		},
		{
			name:  "strips login from properties",
			input: `{"type":"object","properties":{"login":{"type":"string"},"email":{"type":"string"}}}`,
			want:  `{"type":"object","properties":{"email":{"type":"string"}}}`,
		},
		{
			name:  "strips yield_time_ms from properties",
			input: `{"type":"object","properties":{"yield_time_ms":{"type":"number"},"timeout":{"type":"number"}}}`,
			want:  `{"type":"object","properties":{"timeout":{"type":"number"}}}`,
		},
		{
			name:  "strips tty from properties",
			input: `{"type":"object","properties":{"tty":{"type":"boolean"},"command":{"type":"string"}}}`,
			want:  `{"type":"object","properties":{"command":{"type":"string"}}}`,
		},
		{
			name:  "strips all blacklisted keys from properties",
			input: `{"type":"object","properties":{"justification":{},"sandbox_permissions":{},"prefix_rule":{},"login":{},"yield_time_ms":{},"tty":{},"legit":{}}}`,
			want:  `{"type":"object","properties":{"legit":{}}}`,
		},
		{
			name:  "no properties key leaves object unchanged",
			input: `{"title":"test","description":"some desc"}`,
			want:  `{"title":"test","description":"some desc"}`,
		},
		{
			name:  "nested object with blacklisted props",
			input: `{"type":"object","properties":{"outer":{"type":"object","properties":{"inner":{"type":"object","properties":{"justification":{"type":"string"},"data":{"type":"string"}}}}}}}`,
			want:  `{"type":"object","properties":{"outer":{"type":"object","properties":{"inner":{"type":"object","properties":{"data":{"type":"string"}}}}}}}`,
		},
		{
			name:  "array of objects",
			input: `[{"type":"object","properties":{"justification":{"type":"string"},"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"string"}}}]`,
			want:  `[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"string"}}}]`,
		},
		{
			name:  "properties is not a map (edge case)",
			input: `{"type":"object","properties":"not-a-map"}`,
			want:  `{"type":"object","properties":"not-a-map"}`,
		},
		{
			name:  "deeply nested array inside object",
			input: `{"items":[{"properties":{"justification":{"type":"string"},"ok":{"type":"string"}}}]}`,
			want:  `{"items":[{"properties":{"ok":{"type":"string"}}}]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var input any
			if tc.input != "" {
				input = mustJSON(t, tc.input)
			}

			got, err := SimplifyJSONSchemaLimited(input, MaxJSONSchemaDepth)
			if err != nil {
				t.Fatalf("SimplifyJSONSchemaLimited() unexpected error: %v", err)
			}

			if tc.wantNil {
				if got != nil {
					t.Errorf("expected nil for nil input, got %v", got)
				}
				return
			}

			gotJSON := asJSON(t, got)
			wantJSON := asJSON(t, mustJSON(t, tc.want))

			if !jsonDeepEqual(t, gotJSON, wantJSON) {
				t.Errorf("SimplifyJSONSchemaLimited() = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// jsonDeepEqual compares two JSON strings by unmarshalling both into interface{}
// and using reflect.DeepEqual. This handles key ordering differences.
func jsonDeepEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		t.Fatalf("bad JSON in comparison 'a': %v\n%s", err, a)
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		t.Fatalf("bad JSON in comparison 'b': %v\n%s", err, b)
	}
	return reflect.DeepEqual(va, vb)
}

// TestSimplifyJSONSchemaDeprecatedWrapper_FailsSafeOnTooDeep pins the
// deprecated no-error wrapper's compatibility semantics: on a too-deep
// schema it returns nil (the deterministic fail-fast result mirroring
// SimplifyJSONSchemaLimited's (nil, ErrSchemaTooDeep)) — NEVER the unsafe
// original value — while in-depth inputs still simplify exactly as before.
func TestSimplifyJSONSchemaDeprecatedWrapper_FailsSafeOnTooDeep(t *testing.T) {
	// A tree deeper than the limit must fail safe: nil, never the input
	// unchanged (returning the input would propagate the attacker's deep
	// structure to the caller).
	if got := SimplifyJSONSchema(nestedSchema(MaxJSONSchemaDepth)); got != nil {
		t.Fatalf("deprecated wrapper on a too-deep schema must return nil, got %v", got)
	}
	if got := SimplifyJSONSchema(chainAtDepth(MaxJSONSchemaDepth + 1)); got != nil {
		t.Fatalf("deprecated wrapper on a too-deep chain must return nil, got %v", got)
	}

	// In-depth input still simplifies (compat behavior is preserved below
	// the limit).
	in := mustJSON(t, `{"type":"object","properties":{"justification":{"type":"string"},"ok":{"type":"string"}}}`)
	got := SimplifyJSONSchema(in)
	props, ok := got.(map[string]any)["properties"].(map[string]any)
	if !ok {
		t.Fatalf("deprecated wrapper must return a simplified object for in-depth input, got %v", got)
	}
	if _, ok := props["justification"]; ok {
		t.Error("blacklisted key must still be stripped by the deprecated wrapper")
	}
	if _, ok := props["ok"]; !ok {
		t.Error("legitimate key must survive the deprecated wrapper")
	}
}

// nestedSchema builds a schema nested `levels` levels deep (each level is
// two map hops: the properties wrapper plus the nested object).
func nestedSchema(levels int) any {
	root := map[string]any{"type": "object"}
	cur := root
	for i := 0; i < levels; i++ {
		nested := map[string]any{"type": "object"}
		cur["properties"] = map[string]any{"nested": nested}
		cur = nested
	}
	return root
}

// chainAtDepth returns a tree whose deepest MAP sits exactly at the given
// depth (each hop is one map level; the terminal value is a primitive).
func chainAtDepth(depth int) any {
	v := any("leaf")
	for i := 0; i < depth; i++ {
		v = map[string]any{"next": v}
	}
	return v
}

// TestSimplifyJSONSchemaLimited_TooDeep pins item 5: a schema deeper than
// the limit fails fast with ErrSchemaTooDeep and NO result — never a
// partial simplification, never the unsafe original value.
func TestSimplifyJSONSchemaLimited_TooDeep(t *testing.T) {
	_, err := SimplifyJSONSchemaLimited(nestedSchema(MaxJSONSchemaDepth), MaxJSONSchemaDepth)
	if !errors.Is(err, ErrSchemaTooDeep) {
		t.Fatalf("nestedSchema(MaxJSONSchemaDepth): expected ErrSchemaTooDeep, got %v", err)
	}

	// An explicit tiny limit rejects even modest nesting.
	_, err = SimplifyJSONSchemaLimited(nestedSchema(3), 2)
	if !errors.Is(err, ErrSchemaTooDeep) {
		t.Fatalf("maxDepth=2 vs 3 levels: expected ErrSchemaTooDeep, got %v", err)
	}

	// One level over the exact boundary fails.
	if _, err := SimplifyJSONSchemaLimited(chainAtDepth(MaxJSONSchemaDepth+1), MaxJSONSchemaDepth); !errors.Is(err, ErrSchemaTooDeep) {
		t.Fatalf("chain at depth+1: expected ErrSchemaTooDeep, got %v", err)
	}

	// A too-deep ARRAY nesting fails too (not only maps).
	deepArr := []any{"leaf"}
	for i := 0; i < MaxJSONSchemaDepth+2; i++ {
		deepArr = []any{deepArr}
	}
	if _, err := SimplifyJSONSchemaLimited(deepArr, MaxJSONSchemaDepth); !errors.Is(err, ErrSchemaTooDeep) {
		t.Fatalf("deep array: expected ErrSchemaTooDeep, got %v", err)
	}
}

// TestSimplifyJSONSchemaLimited_AtLimitPasses pins the boundary: a tree
// whose deepest map sits exactly at the limit still simplifies fine, and
// blacklist stripping still works at depth.
func TestSimplifyJSONSchemaLimited_AtLimitPasses(t *testing.T) {
	out, err := SimplifyJSONSchemaLimited(chainAtDepth(MaxJSONSchemaDepth), MaxJSONSchemaDepth)
	if err != nil {
		t.Fatalf("tree exactly at the limit must pass: %v", err)
	}
	if out == nil {
		t.Fatal("expected a result")
	}

	// Blacklist stripping still works deep inside the tree (the deepest
	// property values sit at depth MaxJSONSchemaDepth-1, within the limit).
	deep := chainAtDepth(MaxJSONSchemaDepth - 2)
	cur := deep.(map[string]any)
	for i := 0; i < MaxJSONSchemaDepth-3; i++ {
		cur = cur["next"].(map[string]any)
	}
	cur["properties"] = map[string]any{"justification": map[string]any{"type": "string"}, "ok": map[string]any{"type": "string"}}
	out, err = SimplifyJSONSchemaLimited(deep, MaxJSONSchemaDepth)
	if err != nil {
		t.Fatalf("schema within the limit must pass: %v", err)
	}
	leaf := out.(map[string]any)
	for i := 0; i < MaxJSONSchemaDepth-3; i++ {
		leaf = leaf["next"].(map[string]any)
	}
	props := leaf["properties"].(map[string]any)
	if _, ok := props["justification"]; ok {
		t.Error("blacklisted key must still be stripped at depth")
	}
	if _, ok := props["ok"]; !ok {
		t.Error("legitimate key must survive at depth")
	}
}
