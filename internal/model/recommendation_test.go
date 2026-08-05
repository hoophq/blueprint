package model

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// boolPtr takes the address of a literal so a fixture can state "false" rather
// than leave the flag unsaid. The two are different answers, which is the whole
// reason RestartNeeded and RollbackPossible are pointers.
func boolPtr(b bool) *bool { return &b }

// AddCost replaces because two figures under one method would let a caller
// count the same spend twice. AddRecommendation appends because two suggestions
// for one resource are two different things to do — rightsizing its compute and
// rightsizing its storage — and keeping only the last would silently drop advice
// AWS gave.
func TestAddRecommendationAppendsWhereAddCostReplaces(t *testing.T) {
	r := Resource{ARN: "arn:aws:rds:us-east-1:1:db:orders"}
	if r.Tipped() {
		t.Error("Tipped = true before anything was added")
	}
	r.AddRecommendation(Recommendation{ID: "a", ActionType: "Rightsize"})
	r.AddRecommendation(Recommendation{ID: "b", ActionType: "Rightsize"})
	if len(r.Recommendations) != 2 {
		t.Fatalf("got %d recommendations, want both kept: %+v", len(r.Recommendations), r.Recommendations)
	}
	if !r.Tipped() {
		t.Error("Tipped = false with two recommendations attached")
	}

	// The contrast, stated in the same test so the asymmetry is deliberate
	// rather than an accident of two files.
	r.AddCost(ResourceCost{Amount: "1.00", Currency: "USD", Method: CostMethodCE})
	r.AddCost(ResourceCost{Amount: "2.00", Currency: "USD", Method: CostMethodCE})
	if len(r.Costs) != 1 || r.Costs[0].Amount != "2.00" {
		t.Errorf("costs = %+v, want one figure of 2.00", r.Costs)
	}
}

// sort.Slice is unstable, so every ordering in this package needs a final key
// no two rows can share. The hub can return two recommendations for one
// resource that agree on action, on both types and on the saving — a compute
// and a storage rightsizing of the same shape — and without the id tie-break
// they would swap places between two runs over one census, which the diff would
// then report as drift.
func TestSortRecommendationsIsStableDownToTheID(t *testing.T) {
	twins := func() []Recommendation {
		return []Recommendation{
			{ID: "zzz", ActionType: "Rightsize", CurrentResourceType: "RdsDbInstance",
				RecommendedResourceType: "RdsDbInstance", EstimatedMonthlySavings: "10.00"},
			{ID: "aaa", ActionType: "Rightsize", CurrentResourceType: "RdsDbInstance",
				RecommendedResourceType: "RdsDbInstance", EstimatedMonthlySavings: "10.00"},
		}
	}
	for _, order := range [][]int{{0, 1}, {1, 0}} {
		r := Resource{ARN: "arn:aws:rds:us-east-1:1:db:orders"}
		src := twins()
		for _, i := range order {
			r.AddRecommendation(src[i])
		}
		r.SortRecommendations()
		if got := []string{r.Recommendations[0].ID, r.Recommendations[1].ID}; !slices.Equal(got, []string{"aaa", "zzz"}) {
			t.Errorf("appended in %v, sorted to %v, want [aaa zzz]", order, got)
		}
	}
}

// The cascade above the id has to be exercised too, or a rule could be dropped
// without a test noticing. Each pair below differs on exactly one field, and the
// fields are checked in the order the comparator states them.
func TestSortRecommendationsOrdersByEachKeyInTurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first Recommendation
		last  Recommendation
	}{
		{
			name:  "action type",
			first: Recommendation{ID: "z", ActionType: "Delete"},
			last:  Recommendation{ID: "a", ActionType: "Rightsize"},
		},
		{
			name:  "current type",
			first: Recommendation{ID: "z", ActionType: "R", CurrentResourceType: "EbsVolume"},
			last:  Recommendation{ID: "a", ActionType: "R", CurrentResourceType: "Ec2Instance"},
		},
		{
			name: "recommended type",
			first: Recommendation{ID: "z", ActionType: "R", CurrentResourceType: "T",
				RecommendedResourceType: "A"},
			last: Recommendation{ID: "a", ActionType: "R", CurrentResourceType: "T",
				RecommendedResourceType: "B"},
		},
		{
			// A string compare, on purpose: this is a tie-break for a stable
			// artifact, not a ranking. "10.00" before "9.00" is wrong as money
			// and right as bytes, and the ranking a reader sees is built in the
			// renderer through big.Rat.
			name: "savings, lexically",
			first: Recommendation{ID: "z", ActionType: "R", CurrentResourceType: "T",
				RecommendedResourceType: "T", EstimatedMonthlySavings: "10.00"},
			last: Recommendation{ID: "a", ActionType: "R", CurrentResourceType: "T",
				RecommendedResourceType: "T", EstimatedMonthlySavings: "9.00"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Resource{ARN: "arn:aws:rds:us-east-1:1:db:orders"}
			r.AddRecommendation(tc.last)
			r.AddRecommendation(tc.first)
			r.SortRecommendations()
			if r.Recommendations[0].ID != tc.first.ID {
				t.Errorf("sorted to %+v, want %q first", r.Recommendations, tc.first.ID)
			}
		})
	}
}

// Finalize is what the scan calls, and it has to reach the recommendations too
// — an ordering only SortRecommendations' own callers get is an ordering the
// JSON artifact does not have.
func TestFinalizeSortsRecommendations(t *testing.T) {
	r := Resource{ARN: "arn:aws:rds:us-east-1:1:db:orders", Name: "orders", Service: "rds"}
	r.AddRecommendation(Recommendation{ID: "b", ActionType: "Stop"})
	r.AddRecommendation(Recommendation{ID: "a", ActionType: "Delete"})
	snap := &Snapshot{Resources: []Resource{r}}
	snap.Finalize()
	if got := snap.Resources[0].Recommendations[0].ID; got != "a" {
		t.Errorf("first recommendation after Finalize = %q, want a", got)
	}
}

// A resource the hub said nothing about carries no key at all, for the same
// reason an unpriced one carries no cost: a null or an empty list in the
// artifact invites "there is nothing to save here", and what the hub actually
// said is nothing.
func TestRecommendationsAbsentRatherThanEmpty(t *testing.T) {
	data, err := json.Marshal(Resource{ARN: "arn:aws:s3:::logs"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "recommendations") {
		t.Errorf("resource with no advice carries a recommendations key: %s", data)
	}
}

// Every tri-state and every absence has to survive the round trip, because each
// one is a distinct thing AWS did or did not say. A saving reported as zero is
// the case that breaks first: it is a change worth making that saves nothing
// this month, and it must not come back as "AWS reported no figure".
func TestRecommendationRoundTripsEveryStateAWSCanReport(t *testing.T) {
	r := Resource{ARN: "arn:aws:rds:us-east-1:1:db:orders"}
	r.AddRecommendation(Recommendation{
		ID: "zero", ActionType: "MigrateToGraviton",
		EstimatedMonthlySavings: "0.00", Currency: "USD",
		EstimatedSavingsPercentage: "0",
		RestartNeeded:              boolPtr(false), RollbackPossible: boolPtr(false),
	})
	r.AddRecommendation(Recommendation{ID: "silent", ActionType: "Upgrade"})

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{
		`"estimated_monthly_savings":"0.00"`,
		`"estimated_savings_percentage":"0"`,
		`"restart_needed":false`,
		`"rollback_possible":false`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %s in: %s", want, data)
		}
	}

	var back Resource
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	zero, silent := back.Recommendations[0], back.Recommendations[1]
	if zero.EstimatedMonthlySavings != "0.00" {
		t.Errorf("reported zero came back as %q", zero.EstimatedMonthlySavings)
	}
	if zero.RestartNeeded == nil || *zero.RestartNeeded {
		t.Errorf("a stated 'no restart' came back as %v", zero.RestartNeeded)
	}
	if zero.RollbackPossible == nil || *zero.RollbackPossible {
		t.Errorf("a stated 'not reversible' came back as %v", zero.RollbackPossible)
	}
	// The other half of the same rule: the suggestion AWS priced at nothing and
	// the one it did not price at all stay distinguishable.
	if silent.EstimatedMonthlySavings != "" {
		t.Errorf("unpriced suggestion gained an amount: %q", silent.EstimatedMonthlySavings)
	}
	if silent.RestartNeeded != nil {
		t.Errorf("unsaid restart flag came back as %v, want nil", *silent.RestartNeeded)
	}
}

// The schema version gates --compare, and this change is exactly what it gates:
// schema 3 wrote a per-resource `coh` cost on every hub-modelled resource and
// schema 4 writes none, so a diff across the boundary would report every one of
// them as having lost its price — spend drift that never happened. Bumping is
// the whole mechanism, so the constant is pinned rather than left to be quietly
// reverted.
func TestSchemaVersionIsBumpedForTheWithdrawnCostMethod(t *testing.T) {
	if SchemaVersion != 4 {
		t.Errorf("SchemaVersion = %d, want 4: withdrawing the coh cost entries "+
			"changed how a costed resource is represented", SchemaVersion)
	}
	// And the method it withdrew is gone from the list the CSV and the diff
	// build their columns from — a method still advertised but never written
	// produces a column of blanks that reads as "nothing cost anything".
	for _, m := range CostMethods() {
		if m == "coh" {
			t.Error("CostMethods still advertises coh, which no source writes")
		}
	}
}
