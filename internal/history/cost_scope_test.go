package history

import (
	"testing"

	"github.com/hoophq/blueprint/internal/model"
)

// Turning --costs on must not re-bucket a user's history.
//
// The scope key is what decides which past scan a new one is compared
// against. It covers accounts, regions, and scanned services because those
// change what a census can see. Cost changes none of that: it is the same
// estate, described from the same scan units, with a billing rollup attached.
// If cost fed the key, the first --costs run would land in an empty bucket,
// find no baseline, and report an unchanged estate as brand new — and the
// next run without the flag would do it again in the other direction.
func TestScopeKeyIgnoresCost(t *testing.T) {
	base := func() *model.Snapshot {
		return &model.Snapshot{
			Accounts: []string{"111111111111"},
			Regions:  []string{"us-east-1", "eu-west-1"},
			Services: []string{model.ServiceRDS, model.ServiceDynamoDB},
		}
	}
	without := base()
	with := base()
	with.Cost = &model.CostReport{
		Window:   model.CostWindow{Start: "2026-06-01", End: "2026-07-01", Label: "2026-06"},
		Metric:   "AmortizedCost",
		Accounts: []string{"111111111111"},
		Currencies: []model.CostByCurrency{{
			Currency: "USD", Total: "100.00", Attributed: "100.00", Unattributed: "0.00",
		}},
		Meter: model.CostMeter{Requests: 2, EstimatedChargeUSD: "0.02"},
	}

	if a, b := ScopeKey(without), ScopeKey(with); a != b {
		t.Errorf("ScopeKey changed when cost was attached: %s != %s", a, b)
	}
}
