package policy

import (
	"fmt"
	"math"
)

// pct formats a fraction as a whole percent, clamped to [0,100], for
// human-readable deny reasons (e.g. 0.083 -> "8%").
func pct(f float64) string {
	if math.IsNaN(f) {
		f = 0
	}
	return fmt.Sprintf("%.0f%%", math.Max(0, math.Min(1, f))*100)
}
