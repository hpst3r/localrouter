package policy

import (
	"fmt"
	"math"
	"strings"
)

// pct formats a fraction as a percent for human-readable deny reasons,
// clamped to [0,100], with one decimal only when it is not a whole number
// (0.10 -> "10%", 0.999 -> "99.9%", 0.0834 -> "8.3%").
func pct(f float64) string {
	if math.IsNaN(f) {
		f = 0
	}
	s := fmt.Sprintf("%.1f", math.Max(0, math.Min(1, f))*100)
	return strings.TrimSuffix(s, ".0") + "%"
}

// usd formats signed dollars to cents: -0.076 -> "-$0.08", 5 -> "$5.00".
// Matching the dashboard's signed formatter, any non-zero magnitude below one
// cent renders as a sub-cent form ("<$0.01" / "-<$0.01") rather than rounding
// to a flat "$0.00" that would erase the balance's direction; exact zero and
// amounts at one cent and above round to two decimals as usual.
func usd(f float64) string {
	a := math.Abs(f)
	if f != 0 && a < 0.01 {
		if f < 0 {
			return "-<$0.01"
		}
		return "<$0.01"
	}
	s := fmt.Sprintf("%.2f", a)
	if f < 0 {
		return "-$" + s
	}
	return "$" + s
}
