package demo

import (
	"fmt"
	"time"

	"github.com/hoophq/blueprint/internal/cost"
	"github.com/hoophq/blueprint/internal/model"
)

// AddResourceCostOverlay attaches Cost Explorer resource-level figures and the
// probe report, the way `--costs --cost-resources` would.
//
// It runs after AddRecommendations and deliberately overlaps with it: the RDS
// instances the hub already advised on get a billed figure here too, because a
// resource carrying both is the case every consumer gets wrong. Spend and
// savings answer different questions over different windows by different
// methods, and the one thing no reader may be shown is the two combined — so a
// fixture where they never met on one row would let a renderer be built that
// combines them and still looks right.
//
// The DynamoDB tables get a Cost Explorer figure and no advice at all, which is
// the second case worth fixturing. "Cost Optimization Hub does not model this
// resource type" stays true after Cost Explorer bills it, and a renderer that
// reads an empty tip list as an empty cost cell is answering a question nobody
// asked.
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

// gib renders a byte count as whole gibibytes, floored. The fixture derives
// figures from it rather than reporting it, so losing the remainder costs
// nothing.
func gib(bytes int64) int64 { return bytes >> 30 }

// cents renders a cent count as the decimal string every amount in the census
// is carried as. Both the billed figures and the modelled savings get it,
// because neither a real invoice line nor a real hub estimate is a round number
// of dollars, and a fixture full of round numbers is one where a renderer that
// mangles the fractional part still looks right.
func cents(n int64) string { return fmt.Sprintf("%d.%02d", n/100, n%100) }
