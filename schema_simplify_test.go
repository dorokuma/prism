package main

import (
	"encoding/json"
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

func TestSimplifyJSONSchema(t *testing.T) {
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

			got := simplifyJSONSchema(input)

			if tc.wantNil {
				if got != nil {
					t.Errorf("expected nil for nil input, got %v", got)
				}
				return
			}

			gotJSON := asJSON(t, got)
			wantJSON := asJSON(t, mustJSON(t, tc.want))

			if !jsonDeepEqual(t, gotJSON, wantJSON) {
				t.Errorf("simplifyJSONSchema() = %s, want %s", gotJSON, wantJSON)
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
