package cost

import "testing"

// The reason Move exists rather than a float subtraction: every one of these
// deltas is a value float64 gets wrong, and each wrong answer is a bill the
// census would misreport.
func TestMovementSubtractsExactly(t *testing.T) {
	for _, tc := range []struct {
		from, to, want string
	}{
		{"0.00", "0.00", "0.00"},
		{"100.00", "141.00", "41.00"},
		{"141.00", "100.00", "-41.00"},
		// 1234.56 − 1234.55 is 0.010000000000047748 in float64.
		{"1234.55", "1234.56", "0.01"},
		// 0.3 − 0.1 is 0.19999999999999998 in float64.
		{"0.1", "0.3", "0.20"},
		// The delta renders at the width of its widest operand, never wider
		// and never narrower — truncating would drop money.
		{"0.0000000001", "0.0000000002", "0.0000000001"},
		{"12", "12.5", "0.50"},
		// A credit growing more negative is a decrease.
		{"-10.00", "-20.00", "-10.00"},
	} {
		m, ok := Movement(tc.from, tc.to)
		if !ok {
			t.Errorf("Movement(%q, %q) rejected valid amounts", tc.from, tc.to)
			continue
		}
		if got := m.Delta(); got != tc.want {
			t.Errorf("Movement(%q, %q).Delta() = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

// A figure this package would not accept from AWS is not silently treated as
// zero. Reporting "no movement" for a number nobody can read is the failure
// mode the caller has to be able to see.
func TestMovementRejectsNonAmounts(t *testing.T) {
	for _, tc := range [][2]string{
		{"", "1.00"},
		{"1.00", ""},
		{"1e9", "1.00"},
		{"1.00", "1/3"},
		{"+1.00", "2.00"},
		{"USD 1.00", "2.00"},
		{"1.00", "NaN"},
	} {
		if _, ok := Movement(tc[0], tc[1]); ok {
			t.Errorf("Movement(%q, %q) accepted a non-amount", tc[0], tc[1])
		}
	}
}

// Cost Explorer and Cost Optimization Hub do not agree on how many places to
// write, and the same money written at two widths has not moved.
func TestMovementIgnoresWidth(t *testing.T) {
	for _, tc := range [][2]string{
		{"1.0", "1.00"},
		{"1", "1.000000"},
		{"0", "0.00"},
		{"-0.0", "0.000"},
	} {
		m, ok := Movement(tc[0], tc[1])
		if !ok {
			t.Fatalf("Movement(%q, %q) rejected valid amounts", tc[0], tc[1])
		}
		if !m.Zero() || m.Sign() != 0 {
			t.Errorf("Movement(%q, %q) reports a move; the amounts are the same money", tc[0], tc[1])
		}
	}
}

// Both thresholds are required, and both are exact. The boundary cases are the
// point: a move that lands exactly on 5% or exactly on one unit qualifies, and
// a float computation of the same comparison lands on the other side of it.
func TestMoveThresholds(t *testing.T) {
	for _, tc := range []struct {
		name           string
		from, to       string
		wantAbs, wantP bool
	}{
		{"large bill, small move", "10000.00", "10000.50", false, false},
		{"small bill, large percentage", "0.50", "0.60", false, true},
		{"clears both", "100.00", "141.00", true, true},
		{"exactly one unit", "100.00", "101.00", true, false},
		{"a hair under one unit", "100.00", "100.99", false, false},
		{"exactly five percent", "100.00", "105.00", true, true},
		{"a hair under five percent", "100.00", "104.99", true, false},
		// 0.0735 − 0.07 is exactly 5% of 0.07; in float64 the subtraction
		// yields 0.0034999999999999975 and the test flips to false.
		{"five percent that float64 gets wrong", "0.07", "0.0735", false, true},
		// There is no percentage of nothing, so any move away from zero
		// qualifies on the relative test and the absolute one decides.
		{"away from zero", "0.00", "5.00", true, true},
		{"away from zero, tiny", "0.00", "0.01", false, true},
		{"zero to zero", "0.00", "0.000", false, false},
		{"down to zero", "5.00", "0.00", true, true},
		// A credit deepening is a decrease of the same size as a charge rising.
		{"credit deepens", "-10.00", "-20.00", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := Movement(tc.from, tc.to)
			if !ok {
				t.Fatalf("Movement(%q, %q) rejected valid amounts", tc.from, tc.to)
			}
			if got := m.AtLeast("1.00"); got != tc.wantAbs {
				t.Errorf("AtLeast(1.00) = %v, want %v", got, tc.wantAbs)
			}
			if got := m.AtLeastPercent(5); got != tc.wantP {
				t.Errorf("AtLeastPercent(5) = %v, want %v", got, tc.wantP)
			}
		})
	}
}

// An unreadable threshold blocks rather than waves through. The alternative is
// that a typo in a constant silently turns the materiality filter off and every
// cent of billing jitter starts printing.
func TestMoveAtLeastRejectsUnreadableThreshold(t *testing.T) {
	m, ok := Movement("0.00", "1000000.00")
	if !ok {
		t.Fatal("Movement rejected valid amounts")
	}
	for _, bad := range []string{"", "one dollar", "1e2", "$1.00"} {
		if m.AtLeast(bad) {
			t.Errorf("AtLeast(%q) passed on an unreadable threshold", bad)
		}
	}
	// A negative bound is still a size, so it is read as one rather than
	// letting a stray sign disable the filter.
	if !m.AtLeast("-1.00") {
		t.Error("AtLeast(-1.00) should compare against the magnitude")
	}
}

func TestMovePercent(t *testing.T) {
	for _, tc := range []struct {
		from, to, want string
	}{
		{"100.00", "141.00", "41.0"},
		{"100.00", "59.00", "-41.0"},
		{"88.00", "12.00", "-86.4"},
		{"3.00", "4.00", "33.3"},
		// A move away from zero has no percentage, and inventing one (∞, or a
		// silent 0) would be worse than showing the amounts.
		{"0.00", "5.00", ""},
		{"0.00", "0.00", ""},
		// Percentages are taken against the magnitude, so a deepening credit
		// reads as the decrease in spend that it is.
		{"-10.00", "-20.00", "-100.0"},
	} {
		m, ok := Movement(tc.from, tc.to)
		if !ok {
			t.Fatalf("Movement(%q, %q) rejected valid amounts", tc.from, tc.to)
		}
		if got := m.Percent(); got != tc.want {
			t.Errorf("Movement(%q, %q).Percent() = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestSumIsExactAndKeepsPrecision(t *testing.T) {
	var s Sum
	if got := s.String(); got != "0.00" {
		t.Errorf("the zero Sum renders %q, want %q", got, "0.00")
	}
	if s.Sign() != 0 {
		t.Error("the zero Sum should have no sign")
	}
	// 0.1 + 0.2 is 0.30000000000000004 in float64.
	for _, a := range []string{"0.1", "0.2"} {
		if !s.Add(a) {
			t.Fatalf("Add(%q) rejected a valid amount", a)
		}
	}
	if got := s.String(); got != "0.30" {
		t.Errorf("0.1 + 0.2 = %q, want %q", got, "0.30")
	}
	// A more precise input widens the total rather than rounding to it.
	s.Add("0.0000000001")
	if got := s.String(); got != "0.3000000001" {
		t.Errorf("after a ten-place input the total is %q, want %q", got, "0.3000000001")
	}
	if s.Sign() != 1 {
		t.Error("a positive total should report a positive sign")
	}
	s.Sub("0.3000000001")
	if got := s.String(); got != "0.0000000000" {
		t.Errorf("subtracting the total leaves %q, want a zero at the same width", got)
	}
	if s.Sign() != 0 {
		t.Error("a total subtracted to nothing should have no sign")
	}
}

// A rejected input leaves the running total exactly as it was, so a caller
// that ignores the result under-reports instead of adding a fabricated number.
func TestSumRejectsNonAmounts(t *testing.T) {
	var s Sum
	s.Add("10.00")
	for _, bad := range []string{"", "ten", "1e1", "-"} {
		if s.Add(bad) {
			t.Errorf("Add(%q) accepted a non-amount", bad)
		}
		if s.Sub(bad) {
			t.Errorf("Sub(%q) accepted a non-amount", bad)
		}
	}
	if got := s.String(); got != "10.00" {
		t.Errorf("rejected inputs changed the total to %q, want %q", got, "10.00")
	}
}

func TestSumAddMove(t *testing.T) {
	up, _ := Movement("100.00", "141.00")
	down, _ := Movement("88.00", "12.00")
	var s Sum
	s.AddMove(up)
	s.AddMove(down)
	if got := s.String(); got != "-35.00" {
		t.Errorf("netting +41.00 and −76.00 gives %q, want %q", got, "-35.00")
	}
	if s.Sign() != -1 {
		t.Error("a net decrease should report a negative sign")
	}
	// Offsetting moves net to zero rather than to either operand.
	var off Sum
	off.AddMove(up)
	back, _ := Movement("141.00", "100.00")
	off.AddMove(back)
	if got := off.String(); got != "0.00" || off.Sign() != 0 {
		t.Errorf("offsetting moves net to %q (sign %d), want 0.00 and no sign", got, off.Sign())
	}
}

// The materiality constant the diff compares against has to be an amount this
// package accepts, or AtLeast silently blocks every move and the spend section
// goes quiet without anyone noticing.
func TestMaterialThresholdIsAnAmount(t *testing.T) {
	if !ValidDecimal("1.00") {
		t.Fatal("the absolute materiality threshold is not a valid amount")
	}
}
