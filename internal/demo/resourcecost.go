package demo

import (
	"fmt"
	"slices"
	"time"

	"github.com/hoophq/blueprint/internal/cost"
	"github.com/hoophq/blueprint/internal/model"
)

// AddResourceCosts attaches Cost Optimization Hub-style per-resource estimates
// to the fixture, the way `--costs` would after a real scan.
//
// Only RDS instances and NAT gateways get one. Cost Optimization Hub models a
// couple of dozen resource types and both of these are among them, but the
// fixture's DynamoDB tables and ElastiCache clusters come away unpriced on
// purpose: partial coverage is the normal state of this data, and a fixture
// where every row carries a price would let a renderer be built that has no
// unpriced case to handle.
//
// Two shapes cycle over the instances — a plain whole-resource figure and
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

	// Cloned so two fixture resources never share one backing array — a
	// consumer that sorts or appends in place would otherwise reach across
	// resources.
	priced := func(amount string, caveats []string) model.ResourceCost {
		return model.ResourceCost{
			Amount:       amount,
			Currency:     "USD",
			Method:       model.CostMethodCOH,
			Estimated:    true,
			ObservedFrom: &from,
			ObservedTo:   &to,
			Caveats:      slices.Clone(caveats),
			// No MatchKey: the hub reports ARNs, so there is no join to disclose.
		}
	}

	n := 0
	for i := range snap.Resources {
		r := &snap.Resources[i]
		size, sized := r.Measure(model.MeasureSizeBytes)
		switch {
		case r.Type == model.TypeRDSInstance && sized:
			s := shapes[n%len(shapes)]
			n++
			r.AddCost(priced(s.monthly(size), s.caveats))
		case r.Type == model.TypeNATGateway:
			// Flat, and derived from nothing on the row, because that is how a
			// gateway bills: an hourly charge for existing, the same whether it
			// holds a public address or routes to on-premises, and the reason
			// the census gives one a row at all. The odd cents are deliberate —
			// every other figure in the fixture is a round dollar, and a
			// renderer that only ever sees those is untested against the ones a
			// real hub returns.
			r.AddCost(priced("32.85", nil))
		default:
			// The fixture stands in for a completed hub read, so everything it
			// does not price carries the reason it was not priced — the same
			// named absence the real stage writes. A demo whose unpriced
			// resources came away blank would hide the coverage caveat, which
			// is the part of the cost output a reader most needs to see.
			r.CostUnavailable = unpricedReason
		}
	}

	// Re-finalize: the fixture is meant to be indistinguishable from a real
	// snapshot, and a real one has every derivation run after enrichment.
	snap.Finalize()
}

// AddResourceCostOverlay attaches Cost Explorer resource-level figures and the
// probe report, the way `--costs --cost-resources` would.
//
// It runs after AddResourceCosts and deliberately overlaps with it: the RDS
// instances the hub already priced get a second figure here, because two figures
// on one resource is the case every consumer gets wrong — the CSV needs two
// column blocks, the terminal needs two ranked groups, the differ needs to match
// on (ARN, method) rather than ARN. A fixture where no resource carried two
// would let all three be built with a bug that only shows up against a real
// account.
//
// The DynamoDB tables get a Cost Explorer figure and keep the
// CostUnavailable string the hub stage left on them. That combination is not a
// contradiction and is the second case worth fixturing: "Cost Optimization Hub
// does not model this resource type" stays true after Cost Explorer bills it,
// and a renderer that blanks the reason on the strength of an unrelated figure
// is hiding a coverage gap behind an answer to a different question.
//
// The probe outcomes are invented; the meter is not. This mirrors CostReport:
// a fixture may make up an estate, and it may make up what AWS said about that
// estate, but it may not make up a bill the user was never sent. A demo run
// issues no Cost Explorer requests, so Requests is 0 and the charge is $0.00.
//
// Two of the seven outcomes have no fixture on purpose. ProbeDenied and
// ProbeSkipped are run-wide conditions — a missing permission or opt-in denies
// every service, an exhausted budget skips every service after the one that
// spent the last request — so pairing either with the successful probes below
// would make the fixture contradict itself, and a self-contradicting fixture
// teaches a renderer something untrue. internal/render covers both lines
// directly.
func AddResourceCostOverlay(snap *model.Snapshot) {
	if snap == nil {
		return
	}
	window := cost.ResourceWindow(snap.GeneratedAt)
	from, to, ok := windowBounds(window)
	if !ok {
		return
	}

	// Billed, not modelled: Estimated on the figure answers "is this a model",
	// and Cost Explorer's answer is no. Whether AWS has finished restating the
	// window is a different question, answered once on the report below.
	figure := func(amount, matchKey string) model.ResourceCost {
		return model.ResourceCost{
			Amount:       amount,
			Currency:     "USD",
			Method:       model.CostMethodCE,
			Estimated:    false,
			ObservedFrom: &from,
			ObservedTo:   &to,
			MatchKey:     matchKey,
		}
	}

	// The probe covers the two curated accounts and says so on the report
	// below, so it may only price rows in them. --demo-scale generates rows in
	// accounts the probe never named, and pricing those would make the fixture
	// contradict its own coverage statement — a renderer built against it would
	// learn that a figure can appear under an account the probe did not reach.
	probed := func(accountID string) bool {
		return accountID == acctProd || accountID == acctStaging
	}

	var rds, tables int
	for i := range snap.Resources {
		r := &snap.Resources[i]
		size, ok := r.Measure(model.MeasureSizeBytes)
		if !ok || !probed(r.AccountID) {
			continue
		}
		switch r.Type {
		case model.TypeRDSInstance:
			// No MatchKey: this stands in for a service whose RESOURCE_ID came
			// back as the ARN, so there was nothing to reconcile.
			r.AddCost(figure(cents(4_200+gib(size)*112), ""))
			rds++
		case model.TypeDynamoDBTable:
			// MatchKey set, and set to something that is visibly not an ARN.
			// Cost Explorer's RESOURCE_ID format is service-dependent — an
			// instance ID here, a bucket name there — which is the whole reason
			// the join is recorded rather than assumed.
			r.AddCost(figure(cents(90+gib(size)*7), r.Name))
			tables++
		}
	}

	estimated := true
	snap.ResourceCost = &model.ResourceCostReport{
		Window:   window,
		Metric:   snap.Cost.Metric,
		Accounts: []string{acctProd, acctStaging},
		Probes: []model.ServiceProbe{
			{Service: "Amazon Relational Database Service", Outcome: model.ProbeRows, Rows: rds, Matched: rds},
			{Service: "Amazon DynamoDB", Outcome: model.ProbeRows, Rows: tables, Matched: tables},
			// Rows above Matched: real spend on things no scanner in this run
			// reaches. "EC2 - Other" is where AWS bills EBS volumes, NAT gateway
			// data processing, and idle public IPv4 — the figures are correct and
			// there is nothing in the census to hang them on.
			{Service: "EC2 - Other", Outcome: model.ProbeRows, Rows: 3, Matched: 0},
			{Service: "Amazon ElastiCache", Outcome: model.ProbeEmpty},
			{Service: "Amazon DocumentDB (with MongoDB compatibility)", Outcome: model.ProbeEmpty},
			{Service: "Amazon Redshift", Outcome: model.ProbeUnsupported,
				Detail: "resource-level data is not available for this service"},
			{Service: "Amazon Neptune", Outcome: model.ProbeFailed,
				Detail: "ThrottlingException: Rate exceeded"},
			// Not probed, and named anyway: the spend is in the rollup, and the
			// gap is the census's rather than AWS's.
			{Service: "AWS Key Management Service", Outcome: model.ProbeUncensused},
		},
		Estimated: &estimated,
		Meter: model.CostMeter{
			Requests:           0,
			EstimatedChargeUSD: cost.ChargeUSD(0),
		},
	}
	snap.ResourceCost.Sort()
	snap.Finalize()
}

// windowBounds converts the report window's YYYY-MM-DD strings to the instants
// the per-figure window is carried as. Start is inclusive and End is exclusive,
// matching model.CostWindow, so the pair reads the same way every ResourceCost
// in the census does.
func windowBounds(w model.CostWindow) (from, to time.Time, ok bool) {
	from, err := time.Parse("2006-01-02", w.Start)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	to, err = time.Parse("2006-01-02", w.End)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

// unpricedReason mirrors what the Cost Optimization Hub stage says about a
// resource it looked up and found no figure for. It is duplicated here rather
// than imported so the demo package keeps depending on nothing but the model —
// the fixture is data, not a second caller of the enricher.
const unpricedReason = "no Cost Optimization Hub recommendation for this resource"

// gib renders a byte count as whole gibibytes, floored. The fixture prices
// from it rather than reporting it, so losing the remainder costs nothing.
func gib(bytes int64) int64 { return bytes >> 30 }

// dollars renders a whole-dollar figure as the decimal string every amount in
// the census is carried as.
func dollars(n int64) string { return fmt.Sprintf("%d.00", n) }

// cents renders a cent count the same way. Billed amounts get this rather than
// dollars because a real invoice line is not a round number of dollars, and a
// fixture full of round numbers is one where a renderer that mangles the
// fractional part still looks right.
func cents(n int64) string { return fmt.Sprintf("%d.%02d", n/100, n%100) }
