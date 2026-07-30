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

// Move is the movement between two amounts of the same currency, held exactly.
//
// Comparing money is where a census most easily lies. Subtracting two decimal
// strings as floats can produce a non-zero delta for amounts that are the same
// money, and a percentage computed in floating point falls on either side of a
// threshold depending on how the division rounded — so whether a bill "moved"
// would become a property of the arithmetic rather than of the bill. Both
// operands here are exact rationals and both threshold tests are exact
// rational comparisons.
//
// It lives beside parseAmount rather than in the diff because a second notion
// of what an amount is, and of when two of them differ, is exactly the drift
// this file exists to prevent.
type Move struct {
	delta *big.Rat
	// base is |from|, the denominator of the relative test. Absolute because a
	// credit growing more negative is a decrease, and dividing by a negative
	// base would report it as an increase.
	base *big.Rat
	// decimals is the width the delta renders at — the wider of the two
	// inputs, so a delta neither truncates its operands nor claims precision
	// they did not have.
	decimals int
}

// Movement returns the move from one amount to another.
//
// It reports false when either string is not an amount this tool accepts from
// AWS, which is the caller's signal to report the pair as unreadable rather
// than to subtract it. Returning a zero Move instead would turn a figure
// nobody can parse into "this did not change".
func Movement(from, to string) (Move, bool) {
	a, err := parseAmount(from)
	if err != nil {
		return Move{}, false
	}
	b, err := parseAmount(to)
	if err != nil {
		return Move{}, false
	}
	return Move{
		delta:    new(big.Rat).Sub(b.rat, a.rat),
		base:     new(big.Rat).Abs(a.rat),
		decimals: max(minDecimals, max(a.decimals, b.decimals)),
	}, true
}

// Delta renders the movement as a signed decimal string.
func (m Move) Delta() string { return m.delta.FloatString(m.decimals) }

// Zero reports whether the two amounts were the same money, whatever width
// each was written at: "1.0" and "1.00" have not moved.
func (m Move) Zero() bool { return m.delta.Sign() == 0 }

// Sign reports the direction: +1 up, −1 down, 0 unchanged.
func (m Move) Sign() int { return m.delta.Sign() }

// AtLeast reports whether the move is at least abs in absolute size, with abs
// given as a decimal string in the same currency.
//
// A threshold that does not parse reports false. The alternative — treating an
// unreadable bound as no bound — would wave every move through on a typo.
func (m Move) AtLeast(abs string) bool {
	t, err := parseAmount(abs)
	if err != nil {
		return false
	}
	return new(big.Rat).Abs(m.delta).Cmp(new(big.Rat).Abs(t.rat)) >= 0
}

// AtLeastPercent reports whether the move is at least pct percent of the
// amount it started from.
//
// A move away from zero always qualifies: there is no percentage of nothing,
// and something that cost nothing and now costs money has moved by any
// reading. The test is |delta| × 100 ≥ pct × |from|, which is exact — no
// division, so no rounding decides whether a move clears its threshold.
func (m Move) AtLeastPercent(pct int) bool {
	if m.base.Sign() == 0 {
		return m.delta.Sign() != 0
	}
	lhs := new(big.Rat).Mul(new(big.Rat).Abs(m.delta), big.NewRat(100, 1))
	rhs := new(big.Rat).Mul(big.NewRat(int64(pct), 1), m.base)
	return lhs.Cmp(rhs) >= 0
}

// Percent renders the move as a signed percentage of the amount it started
// from, to one decimal place. It returns "" for a move away from zero, which
// has no percentage — the caller shows the amounts instead of inventing one.
func (m Move) Percent() string {
	if m.base.Sign() == 0 {
		return ""
	}
	p := new(big.Rat).Quo(new(big.Rat).Mul(m.delta, big.NewRat(100, 1)), m.base)
	return p.FloatString(1)
}

// Sum accumulates amounts exactly.
//
// Exported for the same reason Move is: netting spend across a diff is money
// arithmetic done by a caller that did not parse the strings itself, and it
// has to use this package's notion of a valid amount rather than a second one.
// The zero Sum is ready to use and renders as "0.00".
type Sum struct {
	total    *big.Rat
	decimals int
}

// Add adds one amount, reporting false when the string is not an amount. A
// rejected input leaves the total untouched, so a caller that ignores the
// result under-reports rather than fabricating.
func (s *Sum) Add(a string) bool { return s.accumulate(a, 1) }

// Sub subtracts one amount, with the same contract as Add.
func (s *Sum) Sub(a string) bool { return s.accumulate(a, -1) }

func (s *Sum) accumulate(a string, sign int64) bool {
	v, err := parseAmount(a)
	if err != nil {
		return false
	}
	s.grow(v.decimals)
	s.total.Add(s.total, new(big.Rat).Mul(v.rat, big.NewRat(sign, 1)))
	return true
}

// AddMove adds a movement's delta. It cannot fail: a Move only exists for
// amounts that already parsed.
func (s *Sum) AddMove(m Move) {
	s.grow(m.decimals)
	s.total.Add(s.total, m.delta)
}

func (s *Sum) grow(decimals int) {
	if s.total == nil {
		s.total = new(big.Rat)
		s.decimals = minDecimals
	}
	if decimals > s.decimals {
		s.decimals = decimals
	}
}

// String renders the running total at the precision of its most precise
// contributor.
func (s *Sum) String() string {
	if s.total == nil {
		return new(big.Rat).FloatString(minDecimals)
	}
	return s.total.FloatString(s.decimals)
}

// Sign reports the direction of the running total.
func (s *Sum) Sign() int {
	if s.total == nil {
		return 0
	}
	return s.total.Sign()
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
