package util_test

import (
	"testing"

	"github.com/dorokuma/prism/internal/util"
)

func TestMapThoughtLevel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"low to high", "low", "high"},
		{"medium to high", "medium", "high"},
		{"high to high", "high", "high"},
		{"xhigh to max", "xhigh", "max"},
		{"LOW uppercase to high", "LOW", "high"},
		{"HIGH uppercase to high", "HIGH", "high"},
		{"XHIGH uppercase to max", "XHIGH", "max"},
		{"unknown passes through", "unknown", "unknown"},
		{"empty string passes through", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := util.MapThoughtLevel(tt.input)
			if got != tt.want {
				t.Errorf("MapThoughtLevel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
