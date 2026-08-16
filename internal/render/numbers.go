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
//   - > 100,000,000: "M" suffix, one decimal, trailing zeros removed ("123.5M")
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
	case u <= 100_000_000:
		return sign + trimTrailingZeros(strconv.FormatFloat(float64(u)/1_000_000, 'f', 2, 64)) + "M"
	default:
		return sign + trimTrailingZeros(strconv.FormatFloat(float64(u)/1_000_000, 'f', 1, 64)) + "M"
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
