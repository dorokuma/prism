package render

import (
	"strconv"
)

// ANSI 24-bit true color sequences (ESC [ 38;2;R;G;B m).
// Raw ESC bytes are used; the sequences cost 0 display columns.

// colorBrand is the cyan used for the provider/service name in card titles.
const colorBrand = "\x1b[38;2;0;180;216m"

// colorRed is used exhausted / used-up quota.
const colorRed = "\x1b[38;2;230;57;70m"

// colorYellow is the warning color (#F4A261): the 已达限额
// footer and other exhausted/warning accents. The spec palette has no
// separate orange tier, so no ≥80 % orange exists.
const colorYellow = "\x1b[38;2;244;162;97m"

// colorDim is used for secondary text: plan type, account id, borders.
const colorDim = "\x1b[38;2;102;102;102m"

// colorReset returns to the terminal default foreground color.
const colorReset = "\x1b[0m"

// Brand wraps s in the brand (cyan) ANSI sequence.
func Brand(s string) string {
	return colorBrand + s + colorReset
}

// Red wraps s in the red ANSI sequence.
func Red(s string) string {
	return colorRed + s + colorReset
}

// Yellow wraps s in the yellow (#F4A261) ANSI sequence.
func Yellow(s string) string {
	return colorYellow + s + colorReset
}

// Dim wraps s in the dim (dark gray) ANSI sequence.
func Dim(s string) string {
	return colorDim + s + colorReset
}

// colorBold is the increased-intensity attribute (ESC [ 1 m). It is combined
// with colorBrand for a bold brand-colored label; the usage report's table
// header used to be exactly that, but since v0.31.2 the header is plain
// text, so no caller is left.
const colorBold = "\x1b[1m"

// BrandBold wraps s in bold + brand cyan (ESC [ 1 m + ESC [ 38;2;0;180;216 m).
// A single reset (ESC [ 0 m) returns to the terminal defaults for both
// attributes, so the sequence costs zero display columns like the plain
// wrappers. It is the generic "emphasise this label" primitive: the usage
// card's column titles were its consumer until v0.31.2 made them plain
// text, and it stays available (implementation untouched) for callers that
// do want a bold brand label.
func BrandBold(s string) string {
	return colorBold + colorBrand + s + colorReset
}

// Fg wraps s in a 24-bit true-color sequence for the RGB triple
// (ESC [ 38;2;R;G;B m). It is the generic form of Brand/Red/Yellow/Dim,
// for renders that need interpolated colors: the quota capsule bar ramps
// green → yellow → red one cell at a time (the green stop has no named
// wrapper because the capsule only ever emits it interpolated). Like the
// named colors the sequence costs 0 display columns.
func Fg(r, g, b uint8, s string) string {
	return colorStart(r, g, b) + s + colorReset
}

// colorStart is the bare foreground sequence (no reset), so callers that
// emit many cells can share it across runs.
func colorStart(r, g, b uint8) string {
	return "\x1b[38;2;" +
		strconv.Itoa(int(r)) + ";" +
		strconv.Itoa(int(g)) + ";" +
		strconv.Itoa(int(b)) + "m"
}
