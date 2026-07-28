package demo

import (
	"math/big"
	"testing"

	"github.com/hoophq/blueprint/internal/model"
)

// rat parses a fixture amount, failing on anything that is not a plain
// decimal — the same grammar the real collector enforces.
func rat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("fixture amount %q is not a number", s)
	}
	return r
}

// The fixture's numbers are invented, but they must still be internally
// consistent: a demo whose breakdowns did not add up to its totals would
// make every renderer look correct while hiding arithmetic bugs, and would
// be the first thing a reader tried to reconcile.
func TestCostReportArithmetic(t *testing.T) {
	r := CostReport()
	if len(r.Currencies) != 1 {
		t.Fatalf("got %d currencies, want 1", len(r.Currencies))
	}
	cur := r.Currencies[0]

	total := rat(t, cur.Total)
	attributed := rat(t, cur.Attributed)
	unattributed := rat(t, cur.Unattributed)

	if got := new(big.Rat).Add(attributed, unattributed); got.Cmp(total) != 0 {
		t.Errorf("Attributed + Unattributed = %s, want Total %s",
			got.FloatString(2), cur.Total)
	}

	for _, tc := range []struct {
		field string
		want  *big.Rat
		parts []model.NamedAmount
	}{
		{"Services", attributed, cur.Services},
		{"UnattributedRecords", unattributed, cur.UnattributedRecords},
		{"Accounts", total, cur.Accounts},
	} {
		sum := new(big.Rat)
		for _, p := range tc.parts {
			sum.Add(sum, rat(t, p.Amount))
		}
		if sum.Cmp(tc.want) != 0 {
			t.Errorf("%s sum to %s, want %s", tc.field, sum.FloatString(2), tc.want.FloatString(2))
		}
	}
}

// A fixture makes no AWS calls, so the meter must say exactly that. Inventing
// a charge the user was never billed would be the one lie a demo cannot tell.
func TestCostReportMeterIsHonest(t *testing.T) {
	m := CostReport().Meter
	if m.Requests != 0 {
		t.Errorf("Requests = %d, want 0 — --demo makes no AWS calls", m.Requests)
	}
	if m.EstimatedChargeUSD != "0.00" {
		t.Errorf("EstimatedChargeUSD = %q, want 0.00", m.EstimatedChargeUSD)
	}
	if m.Capped {
		t.Error("Capped is true for a run that issued no requests")
	}
}

// The window comes from the same helper the real phase uses, so the demo
// always shows a closed month rather than a hardcoded one that goes stale.
func TestCostReportWindow(t *testing.T) {
	w := CostReport().Window
	if w.Start == "" || w.End == "" || w.Label == "" {
		t.Fatalf("window is incomplete: %+v", w)
	}
	if w.Start >= w.End {
		t.Errorf("window start %q is not before end %q", w.Start, w.End)
	}
	if got, want := w.Start[:7], w.Label; got != want {
		t.Errorf("label %q does not match start %q", want, got)
	}
}

// A credit reduces a bill. It has to survive as a negative number, because
// dropping the sign would overstate what the estate cost.
func TestCostReportKeepsNegativeCredit(t *testing.T) {
	for _, n := range CostReport().Currencies[0].UnattributedRecords {
		if n.Name == "Credit" {
			if rat(t, n.Amount).Sign() >= 0 {
				t.Errorf("Credit = %q, want a negative amount", n.Amount)
			}
			return
		}
	}
	t.Error("fixture has no Credit record; the negative-amount path is uncovered")
}

func TestCostReportIsSorted(t *testing.T) {
	cur := CostReport().Currencies[0]
	for _, tc := range []struct {
		field string
		parts []model.NamedAmount
	}{
		{"Services", cur.Services},
		{"UnattributedRecords", cur.UnattributedRecords},
		{"Accounts", cur.Accounts},
	} {
		for i := 1; i < len(tc.parts); i++ {
			if tc.parts[i-1].Name > tc.parts[i].Name {
				t.Errorf("%s is not sorted: %q before %q", tc.field, tc.parts[i-1].Name, tc.parts[i].Name)
			}
		}
	}
}
