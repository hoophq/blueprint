package demo

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

// AddRecommendations attaches Cost Optimization Hub-style savings suggestions
// to the fixture, the way `--costs` would after a real scan.
//
// Nothing here is a price. Every figure is a modelled monthly saving — what AWS
// thinks the account would stop paying if the change were made — and none of it
// is reconciled against CostReport, which is billed spend. Adding the two, or
// subtracting one from the other, is the mistake this whole fixture is shaped to
// keep a renderer from being able to make: there is no cost figure here to add
// it to.
//
// Coverage is partial on purpose, in both directions. Only the resource types
// Cost Optimization Hub actually models get advice — EC2 instances, RDS
// instances and their storage, EBS volumes, Lambda functions — and within those,
// rows the hub has nothing useful to say about come away with nothing. The
// fixture's DynamoDB tables, ElastiCache clusters, load balancers and buckets
// are untouched. A fixture where every row carried a tip would let a renderer be
// built that has no silent row to handle, and silence is the common case.
//
// The awkward cases are fixtured deliberately, because they are the ones a
// consumer gets wrong:
//
//   - Two tips on one ARN. The hub models a database's compute and its storage
//     separately, so an RDS instance can carry both. They are complements, not
//     rival answers, and a consumer that keeps one and drops the other loses
//     advice AWS gave.
//   - A tip that saves exactly zero. A real answer — a change worth making that
//     pays for nothing — and the recurring bug is filtering it out as though AWS
//     had said nothing.
//   - A tip whose currency AWS did not report. It gets its own bucket and is
//     never summed with the dollars.
//   - RestartNeeded and RollbackPossible present as true, present as false, and
//     absent. "AWS did not say" is not "no".
//
// The enricher's one caveat, for a resource younger than the window its saving
// was modelled over, has no fixture: every resource here was created years
// before any plausible lookback, and moving one of their creation dates to
// manufacture the case would make the census itself change shape depending on
// whether --costs was passed. internal/enrich covers it against a fake.
func AddRecommendations(snap *model.Snapshot) {
	if snap == nil {
		return
	}
	// The window ends at the scan and runs back over a lookback, matching what
	// the enricher derives from a recommendation's LastRefreshTimestamp and
	// RecommendationLookbackPeriodInDays.
	const lookback = 14 * 24 * time.Hour
	to := snap.GeneratedAt.UTC()
	from := to.Add(-lookback)

	// Stamps the fields every tip shares and hands out an id. The ids run in
	// census order, which Finalize has already fixed, so the same fixture
	// numbers the same way on every run.
	n := 0
	mk := func(rec model.Recommendation) model.Recommendation {
		n++
		rec.ID = fmt.Sprintf("coh-%05d", n)
		rec.ObservedFrom, rec.ObservedTo = &from, &to
		return rec
	}

	var ec2, lambda int
	for i := range snap.Resources {
		r := &snap.Resources[i]
		switch r.Type {
		case model.TypeEC2Instance:
			ec2++
			class := r.Attr(model.AttrInstanceClass)
			smaller := downsize(class)
			// Every fourth box is idle rather than oversized, and one whose
			// class has no smaller rung has nothing to be rightsized to. Both
			// land on the same advice for different reasons, which is what a
			// real hub does.
			if smaller == "" || ec2%4 == 0 {
				r.AddRecommendation(mk(model.Recommendation{
					ActionType: "Stop",
					// No recommended type or summary: stopping an instance
					// does not replace it with anything, and inventing a
					// target to fill the column would describe a change AWS
					// never suggested.
					CurrentResourceType:        "Ec2Instance",
					CurrentResourceSummary:     class,
					EstimatedMonthlySavings:    cents(9_940 + int64(ec2)*877),
					Currency:                   "USD",
					EstimatedSavingsPercentage: "100",
					ImplementationEffort:       "VeryLow",
					RestartNeeded:              ptr(true),
					RollbackPossible:           ptr(true),
				}))
				continue
			}
			r.AddRecommendation(mk(model.Recommendation{
				ActionType:                 "Rightsize",
				CurrentResourceType:        "Ec2Instance",
				RecommendedResourceType:    "Ec2Instance",
				CurrentResourceSummary:     class,
				RecommendedResourceSummary: smaller,
				EstimatedMonthlySavings:    cents(4_180 + int64(ec2)*613),
				Currency:                   "USD",
				EstimatedSavingsPercentage: "50",
				ImplementationEffort:       "Medium",
				RestartNeeded:              ptr(true),
				RollbackPossible:           ptr(true),
			}))

		case model.TypeRDSInstance:
			size, sized := r.Measure(model.MeasureSizeBytes)
			class := r.Attr(model.AttrInstanceClass)
			// Compute first, and only when the class has a smaller rung. An
			// instance that is already at the bottom of its family gets the
			// storage tip alone, which is why the fixture holds rows with one
			// tip and rows with two.
			if smaller := downsize(class); smaller != "" {
				r.AddRecommendation(mk(model.Recommendation{
					ActionType:                 "Rightsize",
					CurrentResourceType:        "RdsDbInstance",
					RecommendedResourceType:    "RdsDbInstance",
					CurrentResourceSummary:     class,
					RecommendedResourceSummary: smaller,
					EstimatedMonthlySavings:    cents(3_100 + gib(size)*45),
					Currency:                   "USD",
					EstimatedSavingsPercentage: "50",
					ImplementationEffort:       "Medium",
					// A class change restarts the instance, and it can be put
					// back. Both stated, because "AWS did not say" is a third
					// answer and the fixture has to be able to tell them apart.
					RestartNeeded:    ptr(true),
					RollbackPossible: ptr(true),
				}))
			}
			if !sized {
				continue
			}
			r.AddRecommendation(mk(model.Recommendation{
				ActionType:          "Rightsize",
				CurrentResourceType: "RdsDbInstanceStorage",
				// The hub names the storage type here, not the instance class:
				// this tip is about the volume under the database, and the
				// resource type says so without a caveat having to.
				RecommendedResourceType:    "RdsDbInstanceStorage",
				CurrentResourceSummary:     "gp2",
				RecommendedResourceSummary: "gp3",
				EstimatedMonthlySavings:    cents(gib(size) * 18),
				Currency:                   "USD",
				EstimatedSavingsPercentage: "20",
				ImplementationEffort:       "Low",
				RestartNeeded:              ptr(false),
				RollbackPossible:           ptr(true),
			}))

		case model.TypeEBSVolume:
			size, sized := r.Measure(model.MeasureSizeBytes)
			if !sized {
				continue
			}
			switch {
			case r.Attr(model.AttrAttachedInstanceIDs) == "":
				r.AddRecommendation(mk(model.Recommendation{
					ActionType:                 "Delete",
					CurrentResourceType:        "EbsVolume",
					CurrentResourceSummary:     r.Attr(model.AttrVolumeType),
					EstimatedMonthlySavings:    cents(gib(size) * 8),
					Currency:                   "USD",
					EstimatedSavingsPercentage: "100",
					ImplementationEffort:       "VeryLow",
					RestartNeeded:              ptr(false),
					// Deleting a volume cannot be undone, and a consumer that
					// reads a missing rollback flag as "reversible" would be
					// wrong in the direction that loses data.
					RollbackPossible: ptr(false),
				}))
			case r.Attr(model.AttrVolumeType) == "gp2":
				r.AddRecommendation(mk(model.Recommendation{
					ActionType:                 "Upgrade",
					CurrentResourceType:        "EbsVolume",
					RecommendedResourceType:    "EbsVolume",
					CurrentResourceSummary:     "gp2",
					RecommendedResourceSummary: "gp3",
					EstimatedMonthlySavings:    cents(gib(size) * 2),
					Currency:                   "USD",
					EstimatedSavingsPercentage: "20",
					ImplementationEffort:       "VeryLow",
					RestartNeeded:              ptr(false),
					RollbackPossible:           ptr(true),
				}))
			case r.Attr(model.AttrVolumeType) == "st1":
				// Zero, stored and kept. AWS modelled the change as costing
				// the same either way, which is a different statement from AWS
				// having said nothing about it, and the tip is still worth
				// showing: gp3 is faster for what this volume is doing. A
				// consumer that filters on savings > 0 drops it and reports
				// silence AWS did not give.
				r.AddRecommendation(mk(model.Recommendation{
					ActionType:                 "Upgrade",
					CurrentResourceType:        "EbsVolume",
					RecommendedResourceType:    "EbsVolume",
					CurrentResourceSummary:     "st1",
					RecommendedResourceSummary: "gp3",
					EstimatedMonthlySavings:    "0.00",
					Currency:                   "USD",
					EstimatedSavingsPercentage: "0",
					ImplementationEffort:       "Low",
					RestartNeeded:              ptr(false),
					RollbackPossible:           ptr(true),
				}))
			}

		case model.TypeLambdaFunction:
			lambda++
			switch {
			case lambda == 1:
				// No currency, and no effort either. Both fields are optional
				// in the API and both are absent here, so a renderer cannot be
				// built that assumes either is always there. An amount with no
				// currency belongs in its own bucket — summing it with the
				// dollars would invent an exchange rate.
				r.AddRecommendation(mk(model.Recommendation{
					ActionType:                 "Rightsize",
					CurrentResourceType:        "LambdaFunction",
					RecommendedResourceType:    "LambdaFunction",
					CurrentResourceSummary:     memory(r),
					EstimatedMonthlySavings:    "41.20",
					EstimatedSavingsPercentage: "12.5",
				}))
			case r.Attr(model.AttrArchitecture) == "x86_64":
				r.AddRecommendation(mk(model.Recommendation{
					ActionType:                 "MigrateToGraviton",
					CurrentResourceType:        "LambdaFunction",
					RecommendedResourceType:    "LambdaFunction",
					CurrentResourceSummary:     "x86_64",
					RecommendedResourceSummary: "arm64",
					EstimatedMonthlySavings:    cents(410 + int64(lambda)*37),
					Currency:                   "USD",
					EstimatedSavingsPercentage: "17.5",
					ImplementationEffort:       "High",
					// Redeploying a function is not a restart, and AWS reports
					// neither flag for one. Left nil rather than guessed.
				}))
			}
		}
	}

	// Re-finalize: the fixture is meant to be indistinguishable from a real
	// snapshot, and a real one has every derivation run after enrichment.
	snap.Finalize()
}

// sizeLadder is the instance size progression AWS names its classes after,
// smallest first. Only the rungs the fixture's own classes use, plus enough
// above them that stepping down from a large box lands somewhere real.
var sizeLadder = []string{"micro", "small", "medium", "large", "xlarge", "2xlarge", "4xlarge", "8xlarge"}

// downsize steps an instance class down one rung — "db.r5.xlarge" to
// "db.r5.large" — and returns "" when there is no rung below it.
//
// The families that stop at large stop here too. AWS sells no m5.medium and no
// db.r5.small, and a fixture that named one would be teaching a renderer to
// display a class that cannot exist; the burstable families are the exception
// and are the only ones allowed below large. Returning "" is not a failure — it
// is how the fixture reaches its second shape of advice.
func downsize(class string) string {
	cut := strings.LastIndex(class, ".")
	if cut < 0 {
		return ""
	}
	family, size := class[:cut], class[cut+1:]
	i := slices.Index(sizeLadder, size)
	if i <= 0 {
		return ""
	}
	if i-1 < slices.Index(sizeLadder, "large") && !burstable(family) {
		return ""
	}
	return family + "." + sizeLadder[i-1]
}

// burstable reports whether an instance family is one of the T series, which
// are the classes that go below large. The family is the last dotted token
// before the size, so this reads "t3" out of both "t3" and "db.t3".
func burstable(family string) bool {
	return strings.HasPrefix(family[strings.LastIndex(family, ".")+1:], "t")
}

// memory renders a Lambda function's configured memory the way the hub
// summarizes it, or "" when the fixture row does not report one — in which case
// the tip carries no current summary, which is a case worth having.
func memory(r *model.Resource) string {
	mb, ok := r.Measure(model.MeasureMemoryMB)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d MB", mb)
}
