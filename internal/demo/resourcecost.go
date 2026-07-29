package demo

import (
	"fmt"
	"slices"
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

// AddResourceCosts attaches Cost Optimization Hub-style per-resource estimates
// to the fixture, the way `--costs` would after a real scan.
//
// Only RDS instances get one. Cost Optimization Hub models a couple of dozen
// resource types and databases are among them, but the fixture's DynamoDB
// tables and ElastiCache clusters come away unpriced on purpose: partial
// coverage is the normal state of this data, and a fixture where every row
// carries a price would let a renderer be built that has no unpriced case to
// handle.
//
// Two shapes cycle over those instances — a plain whole-resource figure and
// one that covers storage only, so the per-resource caveat path is exercised.
// The enricher's other caveat, for a resource younger than the window its
// figure was modelled over, has no fixture: every resource here was created
// years before any plausible lookback, and moving one of their creation dates
// to manufacture the case would make the census itself change shape depending
// on whether --costs was passed. internal/enrich covers it against a fake.
//
// Nothing here is reconciled against CostReport. The account rollup is billed
// spend and these are modelled monthly rates; adding the two is exactly the
// mistake ResourceCost.Method exists to prevent.
func AddResourceCosts(snap *model.Snapshot) {
	if snap == nil {
		return
	}
	// The window ends at the scan and runs back over a lookback, matching what
	// the enricher derives from a recommendation's LastRefreshTimestamp and
	// RecommendationLookbackPeriodInDays.
	const lookback = 14 * 24 * time.Hour
	to := snap.GeneratedAt.UTC()
	from := to.Add(-lookback)

	shapes := []struct {
		// monthly derives the figure from the resource's own size, so the
		// fixture never prices a small database above a large one.
		monthly func(sizeBytes int64) string
		caveats []string
	}{
		{monthly: func(size int64) string { return dollars(120 + gib(size)*3) }},
		{
			monthly: func(size int64) string { return dollars(gib(size) * 2) },
			caveats: []string{"covers storage only (Cost Optimization Hub resource type RdsDbInstanceStorage); " +
				"other charges for this resource are not included"},
		},
	}

	n := 0
	for i := range snap.Resources {
		r := &snap.Resources[i]
		if r.Type != model.TypeRDSInstance {
			continue
		}
		size, ok := r.Measure(model.MeasureSizeBytes)
		if !ok {
			continue
		}
		s := shapes[n%len(shapes)]
		n++
		r.Cost = &model.ResourceCost{
			Amount:       s.monthly(size),
			Currency:     "USD",
			Method:       model.CostMethodCOH,
			Estimated:    true,
			ObservedFrom: &from,
			ObservedTo:   &to,
			// Cloned so two fixture resources never share one backing array —
			// a consumer that sorts or appends in place would otherwise reach
			// across resources.
			Caveats: slices.Clone(s.caveats),
		}
	}

	// Re-finalize: the fixture is meant to be indistinguishable from a real
	// snapshot, and a real one has every derivation run after enrichment.
	snap.Finalize()
}

// gib renders a byte count as whole gibibytes, floored. The fixture prices
// from it rather than reporting it, so losing the remainder costs nothing.
func gib(bytes int64) int64 { return bytes >> 30 }

// dollars renders a whole-dollar figure as the decimal string every amount in
// the census is carried as.
func dollars(n int64) string { return fmt.Sprintf("%d.00", n) }
