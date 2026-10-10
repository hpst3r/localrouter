package budget

// TDD tests for the exact integer micro-USD money helpers.
//
// Unit of account: 1 USD = 1_000_000 micro-USD ("micros"), stored as int64.
// ParseUSD is exact: it never converts through float64.
// ReportedUSDToMicros maps an already-reported float64 cost to micros with a
// conservative round-up; it is deliberately NOT an assertion that the original
// provider JSON carried that exact decimal.

import (
	"math"
	"strings"
	"testing"
)

func TestParseUSDExact(t *testing.T) {
	const maxMicros = int64(math.MaxInt64)
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{"zero", "0", 0},
		{"zero-decimal", "0.0", 0},
		{"zero-six", "0.000000", 0},
		{"zero-leading", "00.000000", 0},
		{"whole", "5", 5_000_000},
		{"whole-decimal", "5.0", 5_000_000},
		{"cents", "5.25", 5_250_000},
		{"one-micro", "0.000001", 1},
		{"max-fraction", "0.999999", 999_999},
		{"six-digits", "1.234567", 1_234_567},
		{"mixed", "123456789.123456", 123_456_789_123_456},
		{"trim-spaces", " 5.25 ", 5_250_000},
		{"trim-tabs-newline", "\t1.5\n", 1_500_000},
		{"trim-nbsp", "\u00a01.5\u00a0", 1_500_000},
		{"max-int64-micros", "9223372036854.775807", maxMicros},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseUSD(tc.in)
			if err != nil {
				t.Fatalf("ParseUSD(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseUSD(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseUSDLeadingPlusPolicy pins the explicit policy: a single leading '+'
// is accepted (the value is nonnegative by definition of the grammar); a '-'
// is always rejected.
func TestParseUSDLeadingPlusPolicy(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"+5", 5_000_000},
		{"+0", 0},
		{"+0.000001", 1},
		{" +5.25 ", 5_250_000},
	} {
		got, err := ParseUSD(tc.in)
		if err != nil {
			t.Fatalf("ParseUSD(%q) unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseUSD(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// A lone or repeated sign is not a number.
	for _, in := range []string{"+", "++5", "+-5", "+ 5"} {
		if _, err := ParseUSD(in); err == nil {
			t.Fatalf("ParseUSD(%q) = nil error, want rejection", in)
		}
	}
}

// TestParseUSDFractionExtraDigitsZeroOnly covers the exactness rule: more than
// six fractional digits are allowed only when every extra digit is '0'.
func TestParseUSDFractionExtraDigitsZeroOnly(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"1.0000000", 1_000_000},
		{"1.000000000000", 1_000_000},
		{"0.5000000", 500_000},
		{"0.0000010", 1},
	} {
		got, err := ParseUSD(tc.in)
		if err != nil {
			t.Fatalf("ParseUSD(%q) unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseUSD(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, in := range []string{"1.0000001", "1.2345678", "0.0000005", "0.00000001"} {
		_, err := ParseUSD(in)
		if err == nil {
			t.Fatalf("ParseUSD(%q) = nil error, want sub-micro precision rejection", in)
		}
		if !strings.Contains(err.Error(), "precision") {
			t.Fatalf("ParseUSD(%q) error = %q, want it to mention precision", in, err)
		}
	}
}

func TestParseUSDRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
		sub  string
	}{
		{"empty", "", "empty"},
		{"blank", "   ", "empty"},
		{"dot-only", ".", "not a valid"},
		{"leading-dot", ".5", "not a valid"},
		{"trailing-dot", "5.", "not a valid"},
		{"double-dot", "5..0", "not a valid"},
		{"internal-space", "5 25", "not a valid"},
		{"internal-dot-space", "5 .25", "not a valid"},
		{"comma-group", "1,000", "not a valid"},
		{"underscore", "1_000", "not a valid"},
		{"hex", "0x10", "not a valid"},
		{"dollar", "$5", "not a valid"},
		{"trailing-plus", "5+", "not a valid"},
		{"exponent", "1e6", "not a valid"},
		{"exponent-upper", "1E6", "not a valid"},
		{"exponent-negative", "1e-6", "not a valid"},
		{"exponent-on-fraction", "1.0000000e2", "not a valid"},
		{"nan", "NaN", "not a valid"},
		{"nan-lower", "nan", "not a valid"},
		{"inf", "Inf", "not a valid"},
		{"inf-plus", "+Inf", "not a valid"},
		{"infinity", "Infinity", "not a valid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseUSD(tc.in)
			if err == nil {
				t.Fatalf("ParseUSD(%q) = %d, nil error; want rejection", tc.in, got)
			}
			if !strings.Contains(err.Error(), tc.sub) {
				t.Fatalf("ParseUSD(%q) error = %q, want it to contain %q", tc.in, err, tc.sub)
			}
		})
	}
}

func TestParseUSDRejectsNegative(t *testing.T) {
	for _, in := range []string{"-1", "-0", "-0.0", "-0.000001", "-9223372036854.775807"} {
		got, err := ParseUSD(in)
		if err == nil {
			t.Fatalf("ParseUSD(%q) = %d, nil error; want rejection", in, got)
		}
		if !strings.Contains(err.Error(), "negative") {
			t.Fatalf("ParseUSD(%q) error = %q, want it to mention negative", in, err)
		}
	}
}

// TestParseUSDOverflowBoundary pins the exact int64 micro-USD ceiling.
func TestParseUSDOverflowBoundary(t *testing.T) {
	const maxMicros = int64(math.MaxInt64)
	got, err := ParseUSD("9223372036854.775807")
	if err != nil {
		t.Fatalf("ParseUSD at ceiling: unexpected error: %v", err)
	}
	if got != maxMicros {
		t.Fatalf("ParseUSD at ceiling = %d, want %d", got, maxMicros)
	}
	for _, in := range []string{
		"9223372036854.775808", // one micro above the ceiling
		"9223372036854775808",  // 2^63 whole dollars
		"99999999999999999999",
	} {
		got, err := ParseUSD(in)
		if err == nil {
			t.Fatalf("ParseUSD(%q) = %d, nil error; want overflow", in, got)
		}
		if !strings.Contains(err.Error(), "overflow") {
			t.Fatalf("ParseUSD(%q) error = %q, want it to mention overflow", in, err)
		}
	}
}

func TestReportedUSDToMicrosRoundUp(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want int64
	}{
		{"zero", 0, 0},
		{"whole", 5.0, 5_000_000},
		{"cents", 5.25, 5_250_000},
		{"half", 0.5, 500_000},
		{"tenth", 0.1, 100_000},
		{"one-point-one", 1.1, 1_100_000},
		{"one-micro", 0.000001, 1},
		{"tiny-positive", 0.0000001, 1},
		{"half-micro", 0.0000005, 1},
		{"sub-micro", 1e-9, 1},
		{"rounded-up-fraction", 0.0012345, 1235},
		{"mixed", 1234567.891234, 1_234_567_891_234},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReportedUSDToMicros(tc.in)
			if err != nil {
				t.Fatalf("ReportedUSDToMicros(%v) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ReportedUSDToMicros(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestReportedUSDToMicrosTinyPositiveIsOne asserts the conservative floor of
// the conversion: any strictly positive reported cost maps to at least 1 micro.
func TestReportedUSDToMicrosTinyPositiveIsOne(t *testing.T) {
	for _, in := range []float64{
		math.SmallestNonzeroFloat64,
		math.Nextafter(0, 1),
		1e-300,
		1e-12,
		0.0000001, // 0.1 micro
	} {
		got, err := ReportedUSDToMicros(in)
		if err != nil {
			t.Fatalf("ReportedUSDToMicros(%v) unexpected error: %v", in, err)
		}
		if got != 1 {
			t.Fatalf("ReportedUSDToMicros(%v) = %d, want 1", in, got)
		}
	}
}

// TestReportedUSDToMicrosZeroExplicit covers +0 and the negative-zero bit
// pattern (both mean zero, and zero is explicitly valid).
func TestReportedUSDToMicrosZeroExplicit(t *testing.T) {
	for _, in := range []float64{0, math.Copysign(0, -1)} {
		got, err := ReportedUSDToMicros(in)
		if err != nil {
			t.Fatalf("ReportedUSDToMicros(%v) unexpected error: %v", in, err)
		}
		if got != 0 {
			t.Fatalf("ReportedUSDToMicros(%v) = %d, want 0", in, got)
		}
	}
}

func TestReportedUSDToMicrosRejectsNonFinite(t *testing.T) {
	for _, in := range []float64{
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
	} {
		got, err := ReportedUSDToMicros(in)
		if err == nil {
			t.Fatalf("ReportedUSDToMicros(%v) = %d, nil error; want rejection", in, got)
		}
		if !strings.Contains(err.Error(), "non-finite") {
			t.Fatalf("ReportedUSDToMicros(%v) error = %q, want it to mention non-finite", in, err)
		}
	}
}

func TestReportedUSDToMicrosRejectsNegative(t *testing.T) {
	for _, in := range []float64{-1, -0.4, -1e-9, -math.SmallestNonzeroFloat64} {
		got, err := ReportedUSDToMicros(in)
		if err == nil {
			t.Fatalf("ReportedUSDToMicros(%v) = %d, nil error; want rejection", in, got)
		}
		if !strings.Contains(err.Error(), "negative") {
			t.Fatalf("ReportedUSDToMicros(%v) error = %q, want it to mention negative", in, err)
		}
	}
}

// TestReportedUSDToMicrosOverflowBoundary pins the checked-overflow ceiling.
// The largest in-range product is 2^63-2048 micros, so the USD value whose
// scaled product first reaches 2^63 must be rejected.
func TestReportedUSDToMicrosOverflowBoundary(t *testing.T) {
	const maxInRange = int64(9223372036854773760) // 2^63 - 2048, the largest float64-expressible micros < 2^63
	got, err := ReportedUSDToMicros(9223372036854.773)
	if err != nil {
		t.Fatalf("ReportedUSDToMicros near ceiling: unexpected error: %v", err)
	}
	if got != maxInRange {
		t.Fatalf("ReportedUSDToMicros near ceiling = %d, want %d", got, maxInRange)
	}
	for _, in := range []float64{
		9223372036854.775, // scales to exactly 2^63
		1e13,
		1e300,
		math.MaxFloat64,
	} {
		got, err := ReportedUSDToMicros(in)
		if err == nil {
			t.Fatalf("ReportedUSDToMicros(%v) = %d, nil error; want overflow", in, got)
		}
		if !strings.Contains(err.Error(), "overflow") {
			t.Fatalf("ReportedUSDToMicros(%v) error = %q, want it to mention overflow", in, err)
		}
	}
}
