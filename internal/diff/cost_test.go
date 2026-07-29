package diff

import (
	"testing"

	"github.com/hoophq/blueprint/internal/model"
)

// --costs is opt-in, so the same estate legitimately produces snapshots with
// and without a cost report — including in the same history bucket, since
// cost is deliberately not part of the scope key. Cost must therefore be
// invisible to the resource diff: reading it here would report the whole
// estate as drifted the first time a user adds --costs, and again the first
// time they leave it off. Cost drift is its own comparison (ATR-175), not
// resource drift.
func TestCompareIgnoresCost(t *testing.T) {
	resource := model.Resource{
		ARN:       "arn:aws:rds:us-east-1:111111111111:db:orders",
		Name:      "orders",
		Service:   model.ServiceRDS,
		Type:      model.TypeRDSInstance,
		Region:    "us-east-1",
		AccountID: "111111111111",
	}
	snap := func(cost *model.CostReport) *model.Snapshot {
		s := &model.Snapshot{
			Schema:    model.SchemaVersion,
			Accounts:  []string{"111111111111"},
			Regions:   []string{"us-east-1"},
			Resources: []model.Resource{resource},
			Cost:      cost,
		}
		s.Finalize()
		return s
	}
	report := &model.CostReport{
		Window:   model.CostWindow{Start: "2026-06-01", End: "2026-07-01", Label: "2026-06"},
		Metric:   "AmortizedCost",
		Accounts: []string{"111111111111"},
		Currencies: []model.CostByCurrency{{
			Currency: "USD", Total: "100.00", Attributed: "100.00", Unattributed: "0.00",
		}},
		Meter: model.CostMeter{Requests: 2, EstimatedChargeUSD: "0.02"},
	}
	other := *report
	other.Currencies = []model.CostByCurrency{{
		Currency: "USD", Total: "999.00", Attributed: "999.00", Unattributed: "0.00",
	}}

	for _, tc := range []struct {
		name      string
		old, curr *model.CostReport
	}{
		{"cost added", nil, report},
		{"cost removed", report, nil},
		{"cost changed", report, &other},
		{"no cost either side", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Compare(snap(tc.old), snap(tc.curr)); !got.Empty() {
				t.Errorf("cost leaked into the resource diff: %+v", got)
			}
		})
	}
}

// The per-resource estimate is invisible to the resource diff for the same
// reason the account rollup is, plus one of its own: a Cost Optimization Hub
// figure is a modelled monthly rate that moves on its own as usage shifts, so
// a resource nobody touched would report drift on every scan. Whether a price
// changed is a cost question (ATR-175), not a "did this database change" one.
func TestCompareIgnoresPerResourceCost(t *testing.T) {
	snap := func(cost *model.ResourceCost) *model.Snapshot {
		s := &model.Snapshot{
			Schema:   model.SchemaVersion,
			Accounts: []string{"111111111111"},
			Regions:  []string{"us-east-1"},
			Resources: []model.Resource{{
				ARN:       "arn:aws:rds:us-east-1:111111111111:db:orders",
				Name:      "orders",
				Service:   model.ServiceRDS,
				Type:      model.TypeRDSInstance,
				Region:    "us-east-1",
				AccountID: "111111111111",
				Cost:      cost,
			}},
		}
		s.Finalize()
		return s
	}
	priced := &model.ResourceCost{Amount: "412.50", Currency: "USD", Method: model.CostMethodCOH, Estimated: true}
	dearer := &model.ResourceCost{Amount: "980.00", Currency: "USD", Method: model.CostMethodCOH, Estimated: true}
	// A resource priced at exactly zero is a real reading, not an absent one,
	// and must be as invisible to the diff as any other figure.
	free := &model.ResourceCost{Amount: "0.00", Currency: "USD", Method: model.CostMethodCOH, Estimated: true}

	for _, tc := range []struct {
		name      string
		old, curr *model.ResourceCost
	}{
		{"cost added", nil, priced},
		{"cost removed", priced, nil},
		{"cost changed", priced, dearer},
		{"cost fell to zero", priced, free},
		{"no cost either side", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Compare(snap(tc.old), snap(tc.curr)); !got.Empty() {
				t.Errorf("per-resource cost leaked into the resource diff: %+v", got)
			}
		})
	}
}

// The named absence is invisible to the resource diff too. It says what a cost
// source found, not what the resource is, so it appears and disappears purely
// with whether --costs was passed — the same toggle the cost report itself is
// kept out of the diff for.
func TestCompareIgnoresCostUnavailableReason(t *testing.T) {
	snap := func(reason string) *model.Snapshot {
		r := model.Resource{
			ARN:       "arn:aws:dynamodb:us-east-1:111111111111:table/orders",
			Name:      "orders",
			Service:   model.ServiceDynamoDB,
			Type:      model.TypeDynamoDBTable,
			Region:    "us-east-1",
			AccountID: "111111111111",
		}
		r.CostUnavailable = reason
		s := &model.Snapshot{
			Schema:    model.SchemaVersion,
			Accounts:  []string{"111111111111"},
			Regions:   []string{"us-east-1"},
			Resources: []model.Resource{r},
		}
		s.Finalize()
		return s
	}
	const reason = "no Cost Optimization Hub recommendation for this resource"
	for _, tc := range []struct {
		name      string
		old, curr string
	}{
		{"reason added", "", reason},
		{"reason removed", reason, ""},
		{"reason reworded", reason, "resource type not modelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Compare(snap(tc.old), snap(tc.curr)); !got.Empty() {
				t.Errorf("cost-unavailable reason leaked into the resource diff: %+v", got)
			}
		})
	}
}
