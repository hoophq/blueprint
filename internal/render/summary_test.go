package render

import (
	"testing"

	"github.com/hoophq/blueprint/internal/model"
)

// grouped builds one resource in a service, optionally priced.
//
// An empty amount means nothing priced it, which is the case the group header
// has to survive: Cost Explorer reports resource-level spend for some services
// and only for resources with usage in the window, so an unpriced row is not a
// zero and not an error, it is most of a real census.
func grouped(service, name, amount string) model.Resource {
	r := model.Resource{
		ARN: "arn:aws:" + service + ":us-east-1:1:x:" + name, Name: name,
		Service: service, Region: "us-east-1", Type: "AWS::X::Y",
	}
	if amount != "" {
		r.AddCost(model.ResourceCost{
			Amount: amount, Currency: "USD", Method: model.CostMethodCE,
		})
	}
	return r
}

// pricedBy adds one more figure to a resource, from a named source. It exists
// because a real census is priced by more than one: Cost Explorer bills a
// window, Cost Optimization Hub models a month, and the two coexist on the same
// resource without ever being summed.
func pricedBy(r model.Resource, method, amount string) model.Resource {
	r.AddCost(model.ResourceCost{Amount: amount, Currency: "USD", Method: method})
	return r
}

func bucket(t *testing.T, g summaryGroup, method string) groupCost {
	t.Helper()
	for _, c := range g.Costs {
		if c.Method == method {
			return c
		}
	}
	t.Fatalf("group %q has no %s total in %+v", g.Value, method, g.Costs)
	return groupCost{}
}

func groupNamed(t *testing.T, groups []summaryGroup, value string) summaryGroup {
	t.Helper()
	for _, g := range groups {
		if g.Value == value {
			return g
		}
	}
	t.Fatalf("no group %q in %+v", value, groups)
	return summaryGroup{}
}

// A group's spend can only be summed over the resources something priced, but
// the header prints it beside the group's full membership. So the group has to
// carry the fraction, or the two numbers together say the whole group costs
// what its priced part costs.
func TestBuildGroupsCountsEveryMemberNotOnlyThePricedOnes(t *testing.T) {
	resources := []model.Resource{
		grouped("s3", "priced-a", "10.50"),
		grouped("s3", "priced-b", "0.00"), // a real zero: reported, and it counts
		grouped("s3", "never-priced", ""),
		grouped("ec2", "only-one", "30.00"),
	}

	groups := buildGroups(resources)["service"]

	s3 := groupNamed(t, groups, "s3")
	if s3.Total != 3 {
		t.Errorf("s3 Total = %d, want 3: the group has three members whatever "+
			"Cost Explorer knew about them", s3.Total)
	}
	if s3.Priced != 2 {
		t.Errorf("s3 Priced = %d, want 2", s3.Priced)
	}
	if len(s3.Costs) != 1 || s3.Costs[0].Amount != "10.50" {
		t.Errorf("s3 Costs = %+v, want one total of 10.50", s3.Costs)
	}

	// A fully covered group must be able to say so, or "priced < total" would
	// be the only honest state and the disclosure would never switch off.
	ec2 := groupNamed(t, groups, "ec2")
	if ec2.Priced != 1 || ec2.Total != 1 {
		t.Errorf("ec2 Priced/Total = %d/%d, want 1/1", ec2.Priced, ec2.Total)
	}
}

// The header prints one bucket at a time, so coverage has to be counted one
// bucket at a time. A group-wide count answers "did anything price this
// member", which is not the question a reader looking at a Cost Explorer total
// is asking: a total over two of twelve resources is not made whole by the
// other ten carrying a Cost Optimization Hub estimate instead. Counted at the
// group, this fixture reports full coverage under both sources and the
// disclosure never fires.
func TestBuildGroupsCountsCoveragePerBucketNotPerGroup(t *testing.T) {
	resources := []model.Resource{
		grouped("s3", "billed", "10.00"),                                     // ce only
		pricedBy(grouped("s3", "modelled", ""), model.CostMethodCOH, "5.00"), // coh only
		pricedBy(grouped("s3", "both", "2.00"), model.CostMethodCOH, "7.00"),
		grouped("s3", "neither", ""),
	}

	s3 := groupNamed(t, buildGroups(resources)["service"], "s3")

	if s3.Total != 4 || s3.Priced != 3 {
		t.Errorf("s3 Priced/Total = %d/%d, want 3/4", s3.Priced, s3.Total)
	}

	ce := bucket(t, s3, model.CostMethodCE)
	if ce.Amount != "12.00" || ce.Priced != 2 {
		t.Errorf("Cost Explorer bucket = %s over %d, want 12.00 over 2: three of "+
			"the four are priced by something, but only two by this source",
			ce.Amount, ce.Priced)
	}
	coh := bucket(t, s3, model.CostMethodCOH)
	if coh.Amount != "12.00" || coh.Priced != 2 {
		t.Errorf("Cost Optimization Hub bucket = %s over %d, want 12.00 over 2",
			coh.Amount, coh.Priced)
	}
}

// A resource priced twice by one source is still one resource. Counting
// figures instead would let a service reporting per-usage-type rows inflate its
// own coverage past the group's membership — "over 9 of 4" — turning the
// disclosure into a bug report about the report.
//
// AddCost keeps one figure per method, so this state cannot be built through
// the model's own API; the slice is populated directly. That makes this a floor
// rather than a live scenario, and it is worth holding because the two
// invariants are enforced in different packages: the day a source starts
// reporting a resource per usage type, the count that reaches the page must
// already be a count of resources.
func TestBuildGroupsCountsResourcesPerBucketNotFigures(t *testing.T) {
	r := grouped("rds", "chatty", "")
	for _, amount := range []string{"1.00", "2.00", "3.00"} {
		r.Costs = append(r.Costs, model.ResourceCost{
			Amount: amount, Currency: "USD", Method: model.CostMethodCE,
		})
	}

	rds := groupNamed(t, buildGroups([]model.Resource{r, grouped("rds", "quiet", "")})["service"], "rds")

	ce := bucket(t, rds, model.CostMethodCE)
	if ce.Priced != 1 {
		t.Errorf("Cost Explorer bucket covers %d resources, want 1: three figures "+
			"came back, from one resource", ce.Priced)
	}
	if ce.Amount != "6.00" {
		t.Errorf("Cost Explorer total = %s, want 6.00: every figure still counts "+
			"towards the money even though the resource counts once", ce.Amount)
	}
	if ce.Priced > rds.Total {
		t.Errorf("bucket covers %d of a group of %d", ce.Priced, rds.Total)
	}
}

// A group nothing priced still has no total — inventing one would be worse than
// the silence — but it must not drag the whole dimension out of the payload.
func TestBuildGroupsKeepsPricedGroupsWhenAnotherGroupHasNothing(t *testing.T) {
	groups := buildGroups([]model.Resource{
		grouped("s3", "priced", "10.50"),
		grouped("ec2", "unpriced", ""),
	})["service"]

	if len(groups) != 1 || groups[0].Value != "s3" {
		t.Fatalf("groups = %+v, want only s3: a group with no figures has no "+
			"total to print, but it must not take its neighbours with it", groups)
	}
}

// Ordering groups by spend asserts that one group costs more than another. A
// total summed over two of forty members supports no such claim, and the
// arithmetic here is the reason: ranked on partial sums, ec2 outranks an rds
// estate sixteen times its size.
func TestCostSortableWithholdsRankingWhenSomeResourcesWereNeverPriced(t *testing.T) {
	var resources []model.Resource
	for i := range 40 {
		amount := "" // thirty-eight rds instances Cost Explorer never reported
		if i < 2 {
			amount = "500.00"
		}
		resources = append(resources, grouped("rds", "db"+string(rune('a'+i%26))+string(rune('0'+i/26)), amount))
	}
	for i := range 40 {
		resources = append(resources, grouped("ec2", "i"+string(rune('a'+i%26))+string(rune('0'+i/26)), "30.00"))
	}

	if costSortable(resources) {
		t.Error("costSortable = true with 38 of 80 resources unpriced: ranking " +
			"rds's 1000.00 (2 of 40 priced) against ec2's 1200.00 (40 of 40) " +
			"puts the smaller estate on top")
	}

	// The same census, once everything is priced, is rankable — the coverage
	// test must gate the claim, not retire it.
	for i := range resources {
		if len(resources[i].Costs) == 0 {
			resources[i].AddCost(model.ResourceCost{
				Amount: "500.00", Currency: "USD", Method: model.CostMethodCE,
			})
		}
	}
	if !costSortable(resources) {
		t.Error("costSortable = false with every resource priced by one method " +
			"in one currency with no caveats")
	}
}

// A census nobody costed at all is not sortable either, and must not be
// mistaken for one that is uniformly covered.
func TestCostSortableIsFalseWhenNothingWasPriced(t *testing.T) {
	if costSortable([]model.Resource{grouped("s3", "a", ""), grouped("s3", "b", "")}) {
		t.Error("costSortable = true for a census with no cost data at all")
	}
}

// Costed decides whether the table is built with a Cost column, and it is
// decided in the metadata block because the header row is written before the
// census has finished decoding — a column cannot appear after the reader has
// already read the header. So it answers one narrow question, "did anything
// come back with a cost record", and it has to keep answering it in the states
// where every other cost fact in the summary has gone quiet.
func TestSummaryCostedSurvivesFiguresNothingCanRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resources []model.Resource
		want      bool
	}{
		{
			// The case the field exists for. An amount that does not parse
			// voids its bucket, the bucket was the group's only figure, and
			// buildGroups drops a group with no figures — so Groups is empty
			// on precisely the run whose cost data most needs looking at.
			// Deriving the column from Groups would hide it there.
			name:      "figures that do not parse",
			resources: []model.Resource{grouped("s3", "unreadable", "not-a-number")},
			want:      true,
		},
		{
			// A stored zero is a reported figure, not an absence.
			name:      "a reported zero",
			resources: []model.Resource{grouped("s3", "free", "0.00")},
			want:      true,
		},
		{
			// One priced row among unpriced ones is still a priced census: the
			// column has somewhere to put a number, and the rest render as "—".
			name: "one priced row among many",
			resources: []model.Resource{
				grouped("s3", "a", ""), grouped("s3", "b", ""), grouped("ec2", "c", "1.00"),
			},
			want: true,
		},
		{
			// Nothing asked, nothing answered: no column, and no empty one
			// inviting the reader to wonder what happened to their money.
			name:      "nothing priced at all",
			resources: []model.Resource{grouped("s3", "a", ""), grouped("ec2", "b", "")},
			want:      false,
		},
		{
			name:      "no resources at all",
			resources: nil,
			want:      false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &model.Snapshot{Resources: tc.resources}
			if got := buildSummary(snap).Costed; got != tc.want {
				t.Errorf("Costed = %v, want %v", got, tc.want)
			}
		})
	}
}

// And it is not a restatement of the other two: on an unreadable census the
// column has to appear while the ranking and the group totals both withhold.
// A Costed wired to either of them would pass every case above and still drop
// the column exactly where it is needed.
func TestSummaryCostedIsNotDerivedFromSortabilityOrGroups(t *testing.T) {
	snap := &model.Snapshot{Resources: []model.Resource{
		grouped("s3", "unreadable", "not-a-number"),
		grouped("ec2", "never-priced", ""),
	}}

	sum := buildSummary(snap)
	if !sum.Costed {
		t.Fatal("Costed = false with a cost record present")
	}
	if sum.CostSortable {
		t.Error("CostSortable = true on a census whose only figure does not parse")
	}
	if len(sum.Groups) != 0 {
		t.Errorf("Groups = %+v, want none: the only figure was unreadable", sum.Groups)
	}
}
