package demo

import (
	"testing"

	"github.com/hoophq/blueprint/internal/model"
)

// tipsFixture builds the storyboard with advice attached, the way a --costs run
// would produce it.
func tipsFixture(t *testing.T) *model.Snapshot {
	t.Helper()
	snap := Snapshot("test")
	AddRecommendations(snap)
	return snap
}

// allTips flattens the fixture's suggestions, keeping the resource each came
// from so a failure can name it.
func allTips(snap *model.Snapshot) []struct {
	res model.Resource
	rec model.Recommendation
} {
	var out []struct {
		res model.Resource
		rec model.Recommendation
	}
	for _, r := range snap.Resources {
		for _, rec := range r.Recommendations {
			out = append(out, struct {
				res model.Resource
				rec model.Recommendation
			}{r, rec})
		}
	}
	return out
}

// The fixture exists to be the thing renderers are built against, so the
// awkward states have to be in it. Each of these is a case a renderer gets
// wrong by default, and each is worth a named check rather than a count.
func TestFixtureHoldsEveryAwkwardSuggestionShape(t *testing.T) {
	tips := allTips(tipsFixture(t))
	if len(tips) == 0 {
		t.Fatal("fixture produced no suggestions at all")
	}

	var (
		zeroSaving   bool // AWS priced the change at nothing
		noCurrency   bool // an amount with no unit named
		noEffort     bool // effort unreported
		restartTrue  bool
		restartFalse bool
		restartNil   bool
		rollbackNil  bool
		noRollback   bool
		noRecommType bool // an action with no "after" shape
	)
	for _, tp := range tips {
		rec := tp.rec
		switch rec.EstimatedMonthlySavings {
		case "0.00":
			zeroSaving = true
		case "":
			t.Errorf("%s carries a suggestion with no savings string at all; the "+
				"fixture's unpriced case is meant to be the no-currency one", tp.res.Name)
		}
		if rec.EstimatedMonthlySavings != "" && rec.Currency == "" {
			noCurrency = true
		}
		if rec.ImplementationEffort == "" {
			noEffort = true
		}
		switch {
		case rec.RestartNeeded == nil:
			restartNil = true
		case *rec.RestartNeeded:
			restartTrue = true
		default:
			restartFalse = true
		}
		switch {
		case rec.RollbackPossible == nil:
			rollbackNil = true
		case !*rec.RollbackPossible:
			noRollback = true
		}
		if rec.RecommendedResourceType == "" {
			noRecommType = true
		}
	}

	for _, c := range []struct {
		got  bool
		what string
	}{
		{zeroSaving, "a suggestion AWS priced at exactly 0.00 — the case a " +
			"savings > 0 filter drops while reporting silence AWS did not give"},
		{noCurrency, "an amount with no currency, which needs its own bucket " +
			"rather than being summed with the dollars"},
		{noEffort, "a suggestion with no effort grade"},
		{restartTrue, "a suggestion that needs a restart"},
		{restartFalse, "a suggestion that explicitly needs none — a positive " +
			"statement, not an absence"},
		{restartNil, "a suggestion AWS said nothing about restarting"},
		{rollbackNil, "a suggestion AWS said nothing about rolling back"},
		{noRollback, "a suggestion that cannot be undone"},
		{noRecommType, "an action with no target shape, such as stopping or " +
			"deleting a resource"},
	} {
		if !c.got {
			t.Errorf("fixture is missing %s", c.what)
		}
	}
}

// The hub models a database's compute and its storage separately, so one ARN
// can carry two suggestions. They are complements, not rival answers, and a
// consumer that keeps one and drops the other loses advice AWS gave — which is
// only testable if the fixture actually has such a row.
func TestFixtureHoldsAResourceWithTwoSuggestions(t *testing.T) {
	snap := tipsFixture(t)
	var found *model.Resource
	for i := range snap.Resources {
		if len(snap.Resources[i].Recommendations) > 1 {
			found = &snap.Resources[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no fixture resource carries two suggestions")
	}
	a, b := found.Recommendations[0], found.Recommendations[1]
	if a.ID == b.ID {
		t.Errorf("%s carries the same recommendation twice (%s)", found.Name, a.ID)
	}
	if a.CurrentResourceType == b.CurrentResourceType {
		t.Errorf("%s's two suggestions are about the same thing (%s); they are "+
			"meant to be a compute tip and a storage tip",
			found.Name, a.CurrentResourceType)
	}
}

// Coverage is partial in both directions on purpose. The hub does not model
// every type — and a fixture where every row carried advice would let a
// renderer be built that has no silent row to handle, when silence is the
// common case.
func TestFixtureLeavesMostResourcesWithoutAdvice(t *testing.T) {
	snap := tipsFixture(t)
	tipped := 0
	for _, r := range snap.Resources {
		if r.Tipped() {
			tipped++
		}
		// The types AWS does not model at all must stay untouched, or the
		// fixture would be teaching a coverage claim the hub does not make.
		switch r.Type {
		case model.TypeS3Bucket, model.TypeLoadBalancerV2, model.TypeLoadBalancer,
			model.TypeDynamoDBTable, model.TypeElastiCacheReplicationGroup:
			if r.Tipped() {
				t.Errorf("%s (%s) carries advice; the hub does not model this type",
					r.Name, r.Type)
			}
		}
	}
	if tipped == 0 {
		t.Fatal("no resource got advice")
	}
	if tipped >= len(snap.Resources) {
		t.Errorf("%d of %d resources are tipped; the fixture needs silent rows",
			tipped, len(snap.Resources))
	}
}

// Every suggestion carries the window it was modelled over, because a monthly
// saving with no window attached cannot be judged for staleness — and a
// recommendation is a perishable thing, valid for about a day.
func TestFixtureSuggestionsAreWindowedAndIdentified(t *testing.T) {
	snap := tipsFixture(t)
	seen := map[string]bool{}
	for _, tp := range allTips(snap) {
		rec := tp.rec
		if rec.ID == "" {
			t.Errorf("%s carries a suggestion with no id, which is the only "+
				"field that can tell two identical rows apart", tp.res.Name)
			continue
		}
		if seen[rec.ID] {
			t.Errorf("recommendation id %q is used twice", rec.ID)
		}
		seen[rec.ID] = true
		if rec.ObservedFrom == nil || rec.ObservedTo == nil {
			t.Errorf("%s/%s has no usage window", tp.res.Name, rec.ID)
			continue
		}
		if !rec.ObservedFrom.Before(*rec.ObservedTo) {
			t.Errorf("%s/%s window runs backwards: %s → %s",
				tp.res.Name, rec.ID, rec.ObservedFrom, rec.ObservedTo)
		}
	}
}

// The fixture's one deliberate omission, pinned so it stays deliberate. The
// enricher attaches a caveat to a resource younger than the window its saving
// was modelled over; manufacturing that here would mean moving a creation date,
// which would make the census itself change shape depending on whether --costs
// was passed. internal/enrich covers the caveat against a fake instead.
func TestFixtureSuggestionsCarryNoCaveats(t *testing.T) {
	for _, tp := range allTips(tipsFixture(t)) {
		if len(tp.rec.Caveats) != 0 {
			t.Errorf("%s/%s carries caveats %v; the fixture's resources all predate "+
				"any plausible lookback", tp.res.Name, tp.rec.ID, tp.rec.Caveats)
		}
	}
}

// Advice is not a price. Nothing here may reach Resource.Costs, and the fixture
// is the one place a renderer could be handed a second money basis without any
// AWS call being involved — so the absence is checked, not assumed.
func TestFixtureAdviceNeverBecomesACostFigure(t *testing.T) {
	snap := Snapshot("test")
	before := map[string]int{}
	for _, r := range snap.Resources {
		before[r.ARN] = len(r.Costs)
	}
	AddRecommendations(snap)
	for _, r := range snap.Resources {
		if len(r.Costs) != before[r.ARN] {
			t.Errorf("%s gained a cost figure from the advice pass: %+v", r.Name, r.Costs)
		}
		for _, c := range r.Costs {
			if c.Method != model.CostMethodCE {
				t.Errorf("%s carries a %q figure; Cost Explorer is the only basis",
					r.Name, c.Method)
			}
		}
		if r.CostUnavailable != "" {
			t.Errorf("%s was marked unpriced by the advice pass: %q",
				r.Name, r.CostUnavailable)
		}
	}
}

// The fixture is meant to be indistinguishable from a real snapshot, and a real
// one has Finalize run after enrichment — so a resource's suggestions come back
// ordered, and two runs write the same bytes.
func TestAddRecommendationsLeavesTheSnapshotFinalized(t *testing.T) {
	snap := tipsFixture(t)
	for _, r := range snap.Resources {
		sorted := r
		sorted.Recommendations = append([]model.Recommendation(nil), r.Recommendations...)
		sorted.SortRecommendations()
		for i := range r.Recommendations {
			if r.Recommendations[i].ID != sorted.Recommendations[i].ID {
				t.Errorf("%s's suggestions are not in Finalize's order: %s before %s",
					r.Name, r.Recommendations[i].ID, sorted.Recommendations[i].ID)
				break
			}
		}
	}

	// And the same fixture built twice numbers the same way — the ids run in
	// census order, which is only fixed because Finalize sorted it first.
	other := tipsFixture(t)
	for i := range snap.Resources {
		a, b := snap.Resources[i], other.Resources[i]
		if len(a.Recommendations) != len(b.Recommendations) {
			t.Fatalf("%s got %d suggestions one run and %d the next",
				a.Name, len(a.Recommendations), len(b.Recommendations))
		}
		for j := range a.Recommendations {
			if a.Recommendations[j].ID != b.Recommendations[j].ID {
				t.Errorf("%s suggestion %d is %s one run and %s the next",
					a.Name, j, a.Recommendations[j].ID, b.Recommendations[j].ID)
			}
		}
	}
}

// A nil snapshot is what a --demo run hands the pass when nothing was built;
// panicking there would take the whole run out over an absence.
func TestAddRecommendationsToleratesNoSnapshot(t *testing.T) {
	AddRecommendations(nil)
}
