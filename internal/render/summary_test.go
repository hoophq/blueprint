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
