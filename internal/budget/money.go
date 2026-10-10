package budget

// Money for USD budgets is exact integer micro-USD: one USD is 1_000_000
// micros, held in an int64. Nothing here converts a *configured* or stored
// budget amount through float64. The single float64 entry point,
// ReportedUSDToMicros, exists only to fold an already-reported provider cost
// (a float64 that came off the wire) into the same integer unit, and it does so
// conservatively: see its doc comment.
//
// This file owns no types and defines no package-level identifiers beyond the
// two exported functions below, so it can live beside the budget store without
// clashing with it.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseUSD parses an exact nonnegative decimal dollar amount into integer
// micro-USD (1 USD = 1_000_000 micros).
//
// Grammar (after trimming leading and trailing whitespace):
//
//	[+]? DIGIT+ ( "." DIGIT+ )?
//
// Rules:
//   - The value must be nonnegative: a leading '-' is always rejected, and a
//     single leading '+' is accepted and has no effect on the value.
//   - The result is exact; parsing never goes through float64.
//   - At least one digit is required on each side of the decimal point, so
//     ".5" and "5." are rejected.
//   - At most six fractional digits are representable exactly. A seventh or
//     later fractional digit is allowed only when every one of those extra
//     digits is '0'; a nonzero digit below one micro is rejected (it cannot be
//     represented exactly).
//   - Exponent notation, NaN/Inf spellings, digit grouping, underscores, hex,
//     and internal whitespace are all rejected.
//   - A value whose micro-USD magnitude exceeds math.MaxInt64 is rejected.
//
// "0", "0.0", "5", "5.25", "1.234567" and " 5.25 " are valid.
func ParseUSD(s string) (int64, error) {
	const fractionDigits = 6

	isDigits := func(v string) bool {
		for i := 0; i < len(v); i++ {
			if v[i] < '0' || v[i] > '9' {
				return false
			}
		}
		return true
	}
	invalid := func() error {
		return fmt.Errorf("budget: ParseUSD: %q is not a valid nonnegative decimal", s)
	}

	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("budget: ParseUSD: empty string")
	}
	body := t
	if body[0] == '+' {
		body = body[1:]
		if body == "" {
			return 0, invalid()
		}
	}
	if body[0] == '-' {
		return 0, fmt.Errorf("budget: ParseUSD: negative value %q", s)
	}

	intPart := body
	fracPart := ""
	if i := strings.IndexByte(body, '.'); i >= 0 {
		intPart, fracPart = body[:i], body[i+1:]
		if intPart == "" || fracPart == "" {
			return 0, invalid()
		}
		if strings.IndexByte(fracPart, '.') >= 0 {
			return 0, invalid()
		}
	}
	if !isDigits(intPart) || !isDigits(fracPart) {
		return 0, invalid()
	}

	// Fractional digits beyond six are representable only if they are all '0'.
	if len(fracPart) > fractionDigits {
		for i := fractionDigits; i < len(fracPart); i++ {
			if fracPart[i] != '0' {
				return 0, fmt.Errorf("budget: ParseUSD: %q has nonzero digits beyond micro-USD precision", s)
			}
		}
		fracPart = fracPart[:fractionDigits]
	}
	scaled := intPart + fracPart + strings.Repeat("0", fractionDigits-len(fracPart))
	micros, err := strconv.ParseInt(scaled, 10, 64)
	if err != nil {
		// Digits are already validated, so the only possible failure is range.
		return 0, fmt.Errorf("budget: ParseUSD: %q overflows int64 micro-USD", s)
	}
	return micros, nil
}

// ReportedUSDToMicros converts a provider-reported USD cost that is already a
// float64 into integer micro-USD (1 USD = 1_000_000 micros).
//
// It accepts any finite value >= 0 (an explicit 0, including negative zero, is
// valid) and rejects NaN, ±Inf, and negative values. Any fractional micro is
// rounded UP, so the result never understates the reported cost; a strictly
// positive value always maps to at least 1 micro. Overflow of int64 micros is
// reported as an error rather than saturating or wrapping.
//
// This is a conservative mapping of the float64 already carried by the ledger,
// not a claim to reproduce the exact decimal the provider sent: the float64 it
// receives may already have rounded the original JSON number. It is intended
// for folding reported cost into the integer budget ledger, never for
// reconstructing provider JSON.
func ReportedUSDToMicros(usd float64) (int64, error) {
	const microsPerUSD = 1_000_000
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0, fmt.Errorf("budget: ReportedUSDToMicros: non-finite value %v", usd)
	}
	if usd < 0 {
		return 0, fmt.Errorf("budget: ReportedUSDToMicros: negative value %v", usd)
	}
	scaled := usd * microsPerUSD
	micros := math.Ceil(scaled)
	// 2^63 is the first float64 at or above the int64 range; anything at or
	// above it cannot be represented as a nonnegative int64.
	if math.IsInf(micros, 0) || micros >= 9223372036854775808.0 {
		return 0, fmt.Errorf("budget: ReportedUSDToMicros: %v overflows int64 micro-USD", usd)
	}
	return int64(micros), nil
}
