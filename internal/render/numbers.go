package render

import (
	"math"
	"strconv"
	"strings"
)

// FormatInt formats n with thousands separators: 1783 -> "1,783".
func FormatInt(n int64) string {
	sign := ""
	u := uint64(n)
	if n < 0 {
		sign = "-"
		u = uint64(-(n + 1)) + 1
	}
	return sign + groupDigits(strconv.FormatUint(u, 10))
}

// FormatTokens formats a token count compactly:
//   - < 1000: plain digits ("340")
//   - 1000..999999: "k" suffix, no decimals, truncated ("340k", "999k")
//   - 1,000,000..100,000,000: "M" suffix, up to two decimals, trailing
//     zeros removed ("1M", "1.5M", "1.54M")
//   - > 100,000,000: "M" suffix, one decimal, trailing zeros removed
//     ("123.5M", "938.6M")
//
// The chain carries past M: the value is scaled into the largest unit that
// keeps it at 1 or more, and a render that rounds up to a full 1000 of its
// unit moves one unit up ("999.96M" -> "1B", "4557.8M" -> "4.6B"),
// repeating until the scaled value is below 1000 (M -> B -> T -> P -> E, so
// 1,234,567M reads "1.2T"). The units above M keep the single decimal the
// formatter already uses for its largest values.
//
// Integers never show a decimal point: 1000000 is "1M", not "1.00M", and
// 100000001 is "100M", not "100.0M".
func FormatTokens(n int64) string {
	sign := ""
	u := uint64(n)
	if n < 0 {
		sign = "-"
		u = uint64(-(n + 1)) + 1
	}
	switch {
	case u < 1000:
		return sign + strconv.FormatUint(u, 10)
	case u < 1_000_000:
		// k segment is truncated to whole thousands; there are no decimals
		// to trim, but the rule is identical: no trailing zeros.
		return sign + strconv.FormatUint(u/1000, 10) + "k"
	}
	return sign + formatMagnitude(u)
}

// tokenMagnitude is one step of the compact notation's carry chain: the
// divisor that scales a count into the unit and the suffix it renders with.
type tokenMagnitude struct {
	div    uint64
	suffix string
	// twoDecimalsUpTo keeps the formatter's original precision split for
	// the M segment: up to two decimals below 100M ("1.54M", "50.91M"),
	// one decimal above ("123.5M"). Zero means the unit always renders
	// with a single decimal, the style the formatter already uses for its
	// largest values.
	twoDecimalsUpTo uint64
}

// tokenMagnitudes lists the carry chain from the largest unit down to the
// smallest one above the truncated k segment. Each step is 1000x the next,
// so scaling into the first unit that fits always leaves a value below 1000
// once the carry rule below has been applied.
var tokenMagnitudes = []tokenMagnitude{
	{div: 1_000_000_000_000_000_000, suffix: "E"},
	{div: 1_000_000_000_000_000, suffix: "P"},
	{div: 1_000_000_000_000, suffix: "T"},
	{div: 1_000_000_000, suffix: "B"},
	{div: 1_000_000, suffix: "M", twoDecimalsUpTo: 100_000_000},
}

// formatMagnitude renders u (>= 1,000,000) with the compact suffix chain:
// the largest unit that keeps the value at 1 or more, carrying one unit up
// whenever the decimal render rounds up to 1000 of that unit (999.96M ->
// 1B) until the scaled value is below 1000.
func formatMagnitude(u uint64) string {
	i := len(tokenMagnitudes) - 1
	for j, m := range tokenMagnitudes {
		if u >= m.div {
			i = j
			break
		}
	}
	for {
		m := tokenMagnitudes[i]
		prec := 1
		if u <= m.twoDecimalsUpTo {
			prec = 2
		}
		text := trimTrailingZeros(strconv.FormatFloat(float64(u)/float64(m.div), 'f', prec, 64))
		// Rounding can fill the unit: 999.96M renders as 1000.0M, so it
		// carries and renders again one unit up as "1B".
		if v, err := strconv.ParseFloat(text, 64); err == nil && v >= 1000 && i > 0 {
			i--
			continue
		}
		return text + m.suffix
	}
}

// trimTrailingZeros removes trailing zeros from the fractional part of a
// fixed-point decimal and then the decimal point itself when the fraction is
// empty: "1.00" -> "1", "1.50" -> "1.5", "1.54" stays "1.54".
func trimTrailingZeros(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

// FormatCost formats a USD amount with a dollar sign and three decimals
// ("$0.836"). A nil pointer renders as "-": nil means the model has no unit
// price configured, which is different from a zero cost.
func FormatCost(v *float64) string {
	if v == nil {
		return "-"
	}
	s := strconv.FormatFloat(*v, 'f', 3, 64)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign = "-"
		s = s[1:]
	}
	intPart, frac, _ := strings.Cut(s, ".")
	return sign + "$" + groupDigits(intPart) + "." + frac
}

// FormatCostCompact formats a USD amount with the compact precision used
// by the usage detail table: amounts of $0.01 and above show at most two
// decimals ("$21.03", "$0.60"), amounts below $0.01 keep three decimals
// ("$0.005") so a small fee never collapses to "$0.00" or "$0". A nil
// pointer renders as "-": nil means the model has no unit price
// configured, which is different from a zero cost. Only the display is
// rounded — the underlying cost value is never modified.
func FormatCostCompact(v *float64) string {
	if v == nil {
		return "-"
	}
	prec := 2
	if a := math.Abs(*v); a < 0.01 {
		prec = 3
	}
	s := strconv.FormatFloat(*v, 'f', prec, 64)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign = "-"
		s = s[1:]
	}
	intPart, frac, _ := strings.Cut(s, ".")
	return sign + "$" + groupDigits(intPart) + "." + frac
}

// FormatPercent formats part/total*100 with one decimal and a "%" suffix
// ("0.7%"). A zero total renders as "-" instead of dividing by zero.
func FormatPercent(part, total float64) string {
	if total == 0 {
		return "-"
	}
	return strconv.FormatFloat(part/total*100, 'f', 1, 64) + "%"
}

// groupDigits inserts thousands separators into an ASCII digit string.
func groupDigits(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	b.Grow(len(digits) + len(digits)/3)
	for i := 0; i < len(digits); i++ {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(digits[i])
	}
	return b.String()
}
