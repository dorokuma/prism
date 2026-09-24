package render

import (
	"math"
	"strings"
)

// Capsule glyphs shared by the quota cards (internal/planusage) and the
// usage report's hit-rate mini capsule (internal/usage). ▰ is the filled
// share (solid), ▱ the remaining share (hollow). Both are exactly one
// display column (see DisplayWidth), so a capsule is counted in cells —
// never in bytes or runes.
const (
	// CapUsed is the filled capsule cell (▰).
	CapUsed = "▰"
	// CapEmpty is the remaining capsule cell (▱).
	CapEmpty = "▱"
)

// Ramp stops of the capsule gradient, in percent. The healthy band stays
// flat green up to rampGreen, warms to the #F4A261 warning yellow at
// rampYellow and reaches the exhausted red at 100 %.
const (
	rampGreen  = 40
	rampYellow = 60
)

// The palette stops the capsule ramp interpolates between: the spec green
// (82,183,136), the #F4A261 warning yellow (244,162,97) and the exhausted
// red (230,57,70).
var (
	rgbGreen  = [3]uint8{82, 183, 136}
	rgbYellow = [3]uint8{244, 162, 97}
	rgbRed    = [3]uint8{230, 57, 70}
)

// CapsuleRamp maps a level (0..100) to a capsule color: flat palette green
// up to rampGreen, linearly interpolated to the #F4A261 yellow at
// rampYellow and on to the exhausted red at 100 %. Piecewise linear and
// clamped, so a capsule reads as one smooth gradient instead of hard color
// tiers.
//
// DIRECTION: the level is a SEVERITY — low is healthy (green), high is
// exhausted (red). Both callers feed it a severity:
//
//   - quota (internal/planusage) passes the CONSUMED share: the more
//     quota is used, the warmer the capsule (CapsuleLevel, higher = worse).
//   - usage (internal/usage) passes the INVERTED cache-hit level
//     (100 - CapsuleLevel): the higher the hit rate, the greener the
//     capsule, so a cold cache reads red.
//
// The inversion is the only place the two reports differ; the ramp,
// glyphs, cell arithmetic and segment merging are shared.
func CapsuleRamp(level int) [3]uint8 {
	switch {
	case level <= rampGreen:
		return rgbGreen
	case level >= 100:
		return rgbRed
	case level < rampYellow:
		return lerpRGB(level, rampGreen, rampYellow, rgbGreen, rgbYellow)
	default:
		return lerpRGB(level, rampYellow, 100, rgbYellow, rgbRed)
	}
}

// lerpRGB linearly interpolates from → to over the level interval
// [lo, hi], rounding to the nearest channel value.
func lerpRGB(level, lo, hi int, from, to [3]uint8) [3]uint8 {
	t := float64(level-lo) / float64(hi-lo)
	var out [3]uint8
	for i := range out {
		v := float64(from[i]) + (float64(to[i])-float64(from[i]))*t
		out[i] = uint8(v + 0.5)
	}
	return out
}

// CapsuleLevel is the severity level (0..100) that capsule cell i of a
// cells-long bar stands for: the leading cell is 100/cells (about 2 % for
// a 47-cell quota capsule), the last is 100 %. Coloring a bar per cell by
// that level makes it warm up as it grows instead of switching tiers in
// one step.
func CapsuleLevel(i, cells int) int {
	if cells <= 0 {
		return 100
	}
	return (i + 1) * 100 / cells
}

// CapsuleUsedCells is the number of filled cells for a share pct
// (0..100): ceil(pct/100 * cells), clamped into [0, cells]. The filled
// LENGTH is the share itself, so the value is readable from the capsule
// alone — 0 % draws no ▰ at all, 99 % nearly all of them.
func CapsuleUsedCells(pct, cells int) int {
	if cells <= 0 {
		return 0
	}
	n := int(math.Ceil(float64(pct) / 100 * float64(cells)))
	if n > cells {
		n = cells
	}
	if n < 0 {
		n = 0
	}
	return n
}

// CapsuleSeg is one run of equal-colored capsule cells; equal runs are
// emitted under a single ANSI sequence (an all-dim bar costs one). It is
// comparable, so runs are merged by direct equality.
type CapsuleSeg struct {
	// RGB is the ramp color of a filled run.
	RGB [3]uint8
	// Dim marks the remaining (hollow) run, rendered in dim gray.
	Dim bool
}

// CapsuleBar renders a cells-long capsule whose first used cells are solid
// ▰ and whose remaining cells are hollow ▱. Filled cells are colored per
// cell by CapsuleRamp(level(i)) — level is the SEVERITY of cell i, low =
// green and high = red (see CapsuleRamp for the direction each caller
// uses); a nil level defaults to CapsuleLevel, i.e. the quota direction.
// The remaining cells are dim gray (#666666).
//
// color selects ANSI true-color output: with color false the very same
// glyph run is returned without escapes, so the two renders are byte
// identical once the escapes are stripped. Adjacent equal-color runs are
// merged into one ANSI sequence, so a flat bar never costs one escape per
// cell.
func CapsuleBar(cells, used int, level func(i int) int, color bool) string {
	if cells <= 0 {
		return ""
	}
	if used < 0 {
		used = 0
	}
	if used > cells {
		used = cells
	}
	if level == nil {
		level = func(i int) int { return CapsuleLevel(i, cells) }
	}
	segs := make([]CapsuleSeg, 0, cells)
	for i := 0; i < cells; i++ {
		if i < used {
			segs = append(segs, CapsuleSeg{RGB: CapsuleRamp(level(i))})
		} else {
			segs = append(segs, CapsuleSeg{Dim: true})
		}
	}
	var b strings.Builder
	for i := 0; i < len(segs); {
		j := i + 1
		for j < len(segs) && segs[j] == segs[i] {
			j++
		}
		glyph := CapEmpty
		if i < used {
			glyph = CapUsed
		}
		b.WriteString(capsuleRun(segs[i], strings.Repeat(glyph, j-i), color))
		i = j
	}
	return b.String()
}

// capsuleRun colors one run of equal capsule cells: a ramp color for the
// filled share, dim gray for the remaining share. With color off the text
// is returned unchanged — the glyph run (and therefore the layout) was
// decided before this point.
func capsuleRun(seg CapsuleSeg, text string, color bool) string {
	if !color {
		return text
	}
	if seg.Dim {
		return Dim(text)
	}
	return Fg(seg.RGB[0], seg.RGB[1], seg.RGB[2], text)
}
