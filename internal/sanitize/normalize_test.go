package sanitize

import "testing"

func TestNormalizeMessagesMergesSystemAndFlattensParts(t *testing.T) {
	in := []map[string]any{
		{"role": "system", "content": "instr"},
		{"role": "system", "content": []any{map[string]any{"type": "text", "text": "skills"}}},
		{"role": "user", "content": "hi"},
	}
	out := NormalizeMessagesForChatAPI(in)
	if len(out) != 2 {
		t.Fatalf("len=%d want 2", len(out))
	}
	if out[0]["role"] != "system" {
		t.Fatalf("first role %v", out[0]["role"])
	}
	want := "instr\n\nskills"
	if out[0]["content"] != want {
		t.Fatalf("system=%q want %q", out[0]["content"], want)
	}
}
