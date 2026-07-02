package main

import "testing"

func TestParseWireAPIMode(t *testing.T) {
	cases := []struct {
		in   string
		want WireAPIMode
	}{
		{"", WireAPIBoth},
		{"both", WireAPIBoth},
		{"legacy", WireAPILegacy},
		{"chat", WireAPILegacy},
		{"responses", WireAPIResponses},
	}
	for _, c := range cases {
		got, err := ParseWireAPIMode(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q: got %q want %q", c.in, got, c.want)
		}
	}
}