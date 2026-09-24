package render

import "testing"

func TestFg(t *testing.T) {
	got := Fg(82, 183, 136, "▰")
	want := "\x1b[38;2;82;183;136m▰\x1b[0m"
	if got != want {
		t.Fatalf("Fg = %q, want %q", got, want)
	}
	// A truecolor sequence costs no display columns, so a gradient render
	// can be padded and measured like plain text.
	if dw := DisplayWidth(got); dw != 1 {
		t.Fatalf("DisplayWidth(Fg(...)) = %d, want 1", dw)
	}
}

func TestNamedColorsCostNoWidth(t *testing.T) {
	for _, s := range []string{
		Brand("svc"), Red("▰"), Yellow("已达限额"), Dim("│ "), Fg(82, 183, 136, "▰"),
	} {
		if dw := DisplayWidth(s); dw != DisplayWidth(StripANSI(s)) {
			t.Fatalf("color wrapper changed the display width of %q", s)
		}
		if !contains(s, colorReset) {
			t.Fatalf("color wrapper must end with a reset: %q", s)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
