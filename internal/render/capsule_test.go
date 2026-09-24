package render

import (
	"strings"
	"testing"
)

// The capsule tests live here because the capsule does: internal/render
// owns the glyphs, the per-cell ramp, the cell arithmetic and the
// equal-color run merging shared by the quota cards (internal/planusage)
// and the usage report's hit-rate column (internal/usage). Each caller's
// own layout invariants are asserted in that caller's package.

const (
	ansiGreen  = "\x1b[38;2;82;183;136m"
	ansiYellow = "\x1b[38;2;244;162;97m"
	ansiRed    = "\x1b[38;2;230;57;70m"
	ansiDim    = "\x1b[38;2;102;102;102m"
	ansiReset  = "\x1b[0m"
)

// TestCapsuleRampBoundaries locks the gradient stops: flat green in the
// healthy band, the #F4A261 yellow exactly at 60 %, the exhausted red at
// 100 %, and a monotone ramp in between. The level is a SEVERITY: low is
// healthy, high is exhausted.
func TestCapsuleRampBoundaries(t *testing.T) {
	cases := []struct {
		level int
		want  [3]uint8
	}{
		{-5, [3]uint8{82, 183, 136}},
		{0, [3]uint8{82, 183, 136}},
		{34, [3]uint8{82, 183, 136}},
		{40, [3]uint8{82, 183, 136}},
		{60, [3]uint8{244, 162, 97}},
		{100, [3]uint8{230, 57, 70}},
		{101, [3]uint8{230, 57, 70}},
	}
	for _, tc := range cases {
		if got := CapsuleRamp(tc.level); got != tc.want {
			t.Errorf("CapsuleRamp(%d) = %v, want %v", tc.level, got, tc.want)
		}
	}
	// Midpoints: green→yellow at 50 %, yellow→red at 80 %.
	if got := CapsuleRamp(50); got != [3]uint8{163, 173, 117} {
		t.Errorf("CapsuleRamp(50) = %v, want [163 173 117]", got)
	}
	if got := CapsuleRamp(80); got != [3]uint8{237, 110, 84} {
		t.Errorf("CapsuleRamp(80) = %v, want [237 110 84]", got)
	}
	// Monotone: green and blue channels never grow along the ramp.
	prev := CapsuleRamp(0)
	for level := 1; level <= 100; level++ {
		cur := CapsuleRamp(level)
		if cur[1] > prev[1] || cur[2] > prev[2] {
			t.Fatalf("ramp is not monotone at %d: %v after %v", level, cur, prev)
		}
		prev = cur
	}
}

// TestCapsuleRampDirectionIsSeverity documents the ONE knob each caller
// turns: the level passed in. The usage report inverts it, so the very
// same ramp yields "higher value = greener" there.
func TestCapsuleRampDirectionIsSeverity(t *testing.T) {
	// Quota direction: a consumed share of 95 % is red, 10 % is green.
	if got := CapsuleRamp(95); got != rgbRed && got[0] <= rgbGreen[0] {
		// (95 is between 60 and 100, so it is a warm red-orange; the point
		// is that it is far warmer than 10.)
		t.Logf("CapsuleRamp(95) = %v", got)
	}
	if CapsuleRamp(95) == rgbGreen {
		t.Error("a 95 % severity must not be the flat healthy green")
	}
	if CapsuleRamp(10) != rgbGreen {
		t.Error("a 10 % severity must be the flat healthy green")
	}
	// Usage direction (hit rate): the inverted level of a 95 % hit rate is
	// 5, which is green; a 5 % hit rate inverts to 95, which is warm.
	if CapsuleRamp(100-95) != rgbGreen {
		t.Error("inverted: a 95 % hit rate must read green")
	}
	if CapsuleRamp(100-5) == rgbGreen {
		t.Error("inverted: a 5 % hit rate must not read green")
	}
}

// TestCapsuleGlyphWidth guards the capsule's cell arithmetic: ▰ and ▱ are
// both exactly one display column, whatever the surrounding font does.
func TestCapsuleGlyphWidth(t *testing.T) {
	if w := DisplayWidth(CapUsed); w != 1 {
		t.Errorf("DisplayWidth(%q) = %d, want 1", CapUsed, w)
	}
	if w := DisplayWidth(CapEmpty); w != 1 {
		t.Errorf("DisplayWidth(%q) = %d, want 1", CapEmpty, w)
	}
	if w := DisplayWidth(strings.Repeat(CapUsed, 51)); w != 51 {
		t.Errorf("51 capsule cells = %d columns, want 51", w)
	}
}

// TestCapsuleUsedCells locks ceil(pct/100 * cells) with clamping.
func TestCapsuleUsedCells(t *testing.T) {
	cases := []struct{ pct, cells, want int }{
		{-10, 10, 0}, {0, 10, 0}, {1, 10, 1}, {5, 10, 1}, {6, 10, 1},
		{50, 10, 5}, {51, 10, 6}, {94, 10, 10}, {100, 10, 10}, {140, 10, 10},
		// An odd-cell example: the quota card's geometry before it narrowed to 56 columns (51 cells; the card's own tests now pin 47).
		{0, 51, 0}, {34, 51, 18}, {59, 51, 31}, {99, 51, 51}, {100, 51, 51},
	}
	for _, tc := range cases {
		if got := CapsuleUsedCells(tc.pct, tc.cells); got != tc.want {
			t.Errorf("CapsuleUsedCells(%d, %d) = %d, want %d", tc.pct, tc.cells, got, tc.want)
		}
	}
	if got := CapsuleUsedCells(50, 0); got != 0 {
		t.Errorf("CapsuleUsedCells(50, 0) = %d, want 0 (no cells, no division blow-up)", got)
	}
}

// TestCapsuleLevel covers the per-cell level the ramp is evaluated at: the
// leading cell is one hundredth of the bar, the last is 100 %.
func TestCapsuleLevel(t *testing.T) {
	if got := CapsuleLevel(0, 51); got != 1 {
		t.Errorf("CapsuleLevel(0, 51) = %d, want 1 (100/51)", got)
	}
	if got := CapsuleLevel(50, 51); got != 100 {
		t.Errorf("CapsuleLevel(last, 51) = %d, want 100", got)
	}
	if got := CapsuleLevel(0, 10); got != 10 {
		t.Errorf("CapsuleLevel(0, 10) = %d, want 10", got)
	}
	if got := CapsuleLevel(0, 0); got != 100 {
		t.Errorf("CapsuleLevel(0, 0) = %d, want 100 (no cells: no ramp index)", got)
	}
}

// TestCapsuleBarQuotaDirection locks the quota direction of the shared
// bar: a low share is one flat green run, the remainder is dim hollow
// cells, and a nearly full bar warms from green to red cell by cell.
func TestCapsuleBarQuotaDirection(t *testing.T) {
	low := CapsuleBar(10, 3, nil, true)
	if want := ansiGreen + strings.Repeat(CapUsed, 3) + ansiReset + ansiDim + strings.Repeat(CapEmpty, 7) + ansiReset; low != want {
		t.Fatalf("low severity bar:\ngot  %q\nwant %q", low, want)
	}
	if strings.Count(low, "\x1b[38;2;") != 2 {
		t.Errorf("a two-run bar must cost two color runs, got %d:\n%q", strings.Count(low, "\x1b[38;2;"), low)
	}

	high := CapsuleBar(10, 10, nil, true)
	if !strings.HasPrefix(high, ansiGreen+CapUsed) {
		t.Errorf("a full bar must start green:\n%q", high)
	}
	if !strings.HasSuffix(high, ansiRed+CapUsed+ansiReset) {
		t.Errorf("a full bar must end red:\n%q", high)
	}
	if strings.Count(high, "\x1b[38;2;") < 5 {
		t.Errorf("a full bar must be a per-cell gradient, got %d runs:\n%q", strings.Count(high, "\x1b[38;2;"), high)
	}
	if strings.Contains(high, ansiDim) {
		t.Errorf("a full bar has no hollow remainder:\n%q", high)
	}

	// Zero share: every cell hollow dim, no ramp color at all.
	zero := CapsuleBar(10, 0, nil, true)
	if want := ansiDim + strings.Repeat(CapEmpty, 10) + ansiReset; zero != want {
		t.Fatalf("zero bar:\ngot  %q\nwant %q", zero, want)
	}
}

// TestCapsuleBarInvertedDirection locks the usage direction: the caller
// passes the inverted level, so a high hit rate fills green cells while a
// cold cache reads red. The glyph run is identical either way.
func TestCapsuleBarInvertedDirection(t *testing.T) {
	inverted := func(i int) int { return 100 - CapsuleLevel(i, 10) }
	hit := CapsuleBar(10, 9, inverted, true)
	if !strings.Contains(hit, ansiGreen) {
		t.Errorf("a high hit rate must reach green:\n%q", hit)
	}
	// The green run reaches the hollow remainder, i.e. the last filled
	// cell (the frontier of the hit rate) is green.
	if !strings.Contains(hit, ansiGreen+strings.Repeat(CapUsed, 4)+ansiReset+ansiDim) {
		t.Errorf("the last filled cell of a high hit rate must be green:\n%q", hit)
	}
	// Only the leading CELL is cold: the bar warms up, exactly like quota
	// warms down.
	cold := CapsuleBar(10, 2, inverted, true)
	if n := strings.Count(cold, CapUsed); n != 2 {
		t.Errorf("a 2-cell hit rate must fill 2 cells, got %d:\n%q", n, cold)
	}
	if strings.Contains(cold, ansiGreen) {
		t.Errorf("a low hit rate must not reach green:\n%q", cold)
	}
	if strings.HasPrefix(cold, ansiDim) {
		t.Errorf("a low hit rate must not start on the dim (hollow) color:\n%q", cold)
	}
}

// TestCapsuleBarNoColor pins the palette contract: with color off the
// bar is the colored render minus its escapes, byte for byte.
func TestCapsuleBarNoColor(t *testing.T) {
	for _, used := range []int{0, 1, 3, 10} {
		colored := CapsuleBar(10, used, nil, true)
		plain := CapsuleBar(10, used, nil, false)
		if strings.Contains(plain, "\x1b") {
			t.Fatalf("used=%d: no-color bar still carries escapes:\n%q", used, plain)
		}
		if StripANSI(colored) != plain {
			t.Fatalf("used=%d: no-color bar is not the colored bar minus escapes:\ngot  %q\nwant %q", used, plain, StripANSI(colored))
		}
	}
	// Glyph counts never depend on the color decision.
	if plain, colored := CapsuleBar(10, 4, nil, false), CapsuleBar(10, 4, nil, true); strings.Count(plain, CapUsed) != 4 ||
		strings.Count(colored, CapUsed) != 4 || strings.Count(plain, CapEmpty) != 6 {
		t.Fatalf("cell counts drifted: plain %q colored %q", plain, colored)
	}
}

// TestCapsuleBarEdgeCells guards the destructive inputs: a non-positive
// cell count renders nothing and an out-of-range share is clamped instead
// of panicking or over-drawing.
func TestCapsuleBarEdgeCells(t *testing.T) {
	if got := CapsuleBar(0, 5, nil, true); got != "" {
		t.Errorf("CapsuleBar(0, 5) = %q, want empty", got)
	}
	if got := CapsuleBar(-1, 5, nil, true); got != "" {
		t.Errorf("CapsuleBar(-1, 5) = %q, want empty", got)
	}
	if got := CapsuleBar(10, -3, nil, true); strings.Count(got, CapUsed) != 0 || strings.Count(got, CapEmpty) != 10 {
		t.Errorf("negative share must clamp to 0 filled cells:\n%q", got)
	}
	if got := CapsuleBar(10, 99, nil, true); strings.Count(got, CapUsed) != 10 {
		t.Errorf("oversized share must clamp to a full bar:\n%q", got)
	}
	// A nil level defaults to the quota direction (higher = worse).
	if got := CapsuleBar(10, 10, nil, true); !strings.HasSuffix(got, ansiRed+CapUsed+ansiReset) {
		t.Errorf("nil level must default to the quota direction:\n%q", got)
	}
}
