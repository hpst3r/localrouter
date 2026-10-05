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
