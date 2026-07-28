package cost

import (
	"math/big"
	"testing"
)

func TestParseAmountAccepts(t *testing.T) {
	for _, tc := range []struct {
		in       string
		decimals int
	}{
		{"0", 0},
		{"0.00", 2},
		{"12", 0},
		{"12.34", 2},
		{"-450.00", 2},
		{"-0.0000000001", 10},
		// Cost Explorer really does return this many places.
		{"1234.5678901234", 10},
		{"0.0000000001", 10},
	} {
		got, err := parseAmount(tc.in)
		if err != nil {
			t.Errorf("parseAmount(%q) error: %v", tc.in, err)
			continue
		}
		if got.raw != tc.in {
			t.Errorf("parseAmount(%q).raw = %q, want the input verbatim", tc.in, got.raw)
		}
		if got.decimals != tc.decimals {
			t.Errorf("parseAmount(%q).decimals = %d, want %d", tc.in, got.decimals, tc.decimals)
		}
	}
}

// The rejected forms are the point of hand-rolling the validator: big.Rat
// accepts all of them, and any one of them reaching an artifact would break
// the promise that amounts are AWS's own decimal strings.
func TestParseAmountRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"-",
		".",
		".5",
		"5.",
		"1/3",
		"1e9",
		"1E9",
		"+1",
		"1_000",
		"1,000.00",
		" 1.00",
		"1.00 ",
		"USD 1.00",
		"NaN",
		"Inf",
		"0x10",
		"--1",
		"1.2.3",
		"=1+1",
	} {
		if _, err := parseAmount(in); err == nil {
			t.Errorf("parseAmount(%q) accepted a non-decimal", in)
		}
	}
}

func TestSumIsExact(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want string
	}{
		{"empty is zero, not blank", nil, "0.00"},
		{"whole numbers keep two places", []string{"1", "2"}, "3.00"},
		{"cents", []string{"0.01", "0.02"}, "0.03"},
		{"negatives subtract", []string{"1058.27", "-450.00", "200.00"}, "808.27"},
		{"cancels to zero", []string{"10.00", "-10.00"}, "0.00"},
		// float64 cannot represent 0.1 or 0.2; 0.1+0.2 == 0.30000000000000004
		// in binary floating point. This is the case the whole big.Rat
		// apparatus exists for.
		{"no float drift", []string{"0.1", "0.2"}, "0.30"},
		{"widest input sets the precision", []string{"0.0000000001", "1.00"}, "1.0000000001"},
		{"long CE amounts", []string{"1234.5678901234", "1.0000000001"}, "1235.5678901235"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			amounts := make([]amount, 0, len(tc.in))
			for _, s := range tc.in {
				a, err := parseAmount(s)
				if err != nil {
					t.Fatalf("parseAmount(%q): %v", s, err)
				}
				amounts = append(amounts, a)
			}
			if got := sum(amounts); got != tc.want {
				t.Errorf("sum(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A thousand hundredths must be exactly ten dollars. Accumulating 0.01 a
// thousand times in float64 lands on 9.999999999999831.
func TestSumDoesNotAccumulateError(t *testing.T) {
	a, err := parseAmount("0.01")
	if err != nil {
		t.Fatal(err)
	}
	amounts := make([]amount, 1000)
	for i := range amounts {
		amounts[i] = a
	}
	if got, want := sum(amounts), "10.00"; got != want {
		t.Errorf("sum of 1000×0.01 = %q, want %q", got, want)
	}
}

// sum's output must itself parse, or a total could not be re-read from a
// saved artifact and summed again.
func TestSumOutputRoundTrips(t *testing.T) {
	a, _ := parseAmount("1234.5678901234")
	b, _ := parseAmount("-1.1")
	total := sum([]amount{a, b})
	parsed, err := parseAmount(total)
	if err != nil {
		t.Fatalf("sum output %q does not parse: %v", total, err)
	}
	want := new(big.Rat).Add(a.rat, b.rat)
	if parsed.rat.Cmp(want) != 0 {
		t.Errorf("round trip changed the value: %v != %v", parsed.rat, want)
	}
}

func TestChargeUSD(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{2, "0.02"},
		{20, "0.20"},
		{99, "0.99"},
		{100, "1.00"},
		{101, "1.01"},
		{1234, "12.34"},
	} {
		if got := ChargeUSD(tc.n); got != tc.want {
			t.Errorf("ChargeUSD(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
