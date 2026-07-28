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
