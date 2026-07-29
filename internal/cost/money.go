package cost

import (
	"fmt"
	"math/big"
	"strings"
)

// minDecimals is the floor on how many decimal places a computed sum is
// rendered with, so money reads like money ("3.00", not "3").
const minDecimals = 2

// amount is a validated decimal amount from Cost Explorer, kept as an exact
// rational plus the raw string AWS sent.
type amount struct {
	rat *big.Rat
	// raw is exactly what Cost Explorer returned. Leaf values are reported
	// verbatim so the artifact can be checked against the AWS console without
	// arguing about reformatting.
	raw string
	// decimals is the number of digits after the point in raw, tracked so a
	// sum is rendered at the precision of its most precise input rather than
	// at some arbitrary fixed width.
	decimals int
}

// parseAmount validates and parses a Cost Explorer amount.
//
// The validation is deliberately strict — an optional sign, digits, an
// optional fractional part, nothing else. Cost Explorer amounts are the one
// place in this tool where an AWS-controlled string is treated as a number,
// and everything downstream (summing, and eventually a spreadsheet cell that
// must not be read as a formula) depends on that string provably being a
// plain decimal. Rejecting here means no other layer has to guess.
func parseAmount(s string) (amount, error) {
	if !ValidDecimal(s) {
		return amount{}, fmt.Errorf("not a decimal number: %q", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		// Unreachable for a string ValidDecimal accepts, but a silent zero
		// here would be a fabricated amount.
		return amount{}, fmt.Errorf("not a decimal number: %q", s)
	}
	decimals := 0
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		decimals = len(s) - dot - 1
	}
	return amount{rat: r, raw: s, decimals: decimals}, nil
}

// ValidDecimal reports whether s matches ^-?\d+(\.\d+)?$.
//
// Written out rather than done with regexp so the accepted grammar is
// obvious: big.Rat.SetString on its own is far more permissive (it accepts
// "1/3", "1e9", and leading "+"), and those forms must not reach an artifact
// that claims to quote AWS verbatim.
//
// It is exported because Cost Explorer is no longer the only source of money
// in the census: Cost Optimization Hub models its amounts as doubles, so that
// path has to convert to a decimal string itself and then prove the result is
// one this tool would have accepted from AWS directly. Two definitions of
// "amount" would drift; there is one, and it lives here.
func ValidDecimal(s string) bool {
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return false
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if !allDigits(intPart) {
		return false
	}
	if hasDot && !allDigits(fracPart) {
		return false
	}
	return true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// sum adds amounts exactly and renders the result as a decimal string.
//
// The result is exact: every input is a finite decimal, so their sum is a
// finite decimal with at most as many places as the most precise input, and
// FloatString at that width truncates nothing.
func sum(amounts []amount) string {
	decimals := minDecimals
	for _, a := range amounts {
		if a.decimals > decimals {
			decimals = a.decimals
		}
	}
	return total(amounts).FloatString(decimals)
}

// total adds amounts exactly without rendering them.
//
// Comparisons go through this rather than through sum's string, so whether
// two figures agree never depends on the width they happened to be formatted
// at: "1.0" and "1.00" are the same money and must compare equal.
func total(amounts []amount) *big.Rat {
	t := new(big.Rat)
	for _, a := range amounts {
		t.Add(t, a.rat)
	}
	return t
}

// ChargeUSD renders n Cost Explorer requests as the dollar amount AWS charges
// for them. At $0.01 each the arithmetic is integer cents, so this is exact
// rather than a float multiplication.
//
// It is exported so the CLI can quote the same price in its pre-flight notice
// that the artifact's meter reports afterwards — one formula, in one place.
func ChargeUSD(n int) string {
	neg := ""
	if n < 0 {
		// Not reachable from a request counter, but formatting a negative as
		// "-0.-3" would be worse than handling it.
		neg, n = "-", -n
	}
	return fmt.Sprintf("%s%d.%02d", neg, n/100, n%100)
}
