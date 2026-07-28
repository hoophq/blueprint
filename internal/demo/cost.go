package demo

import (
	"time"

	"github.com/hoophq/blueprint/internal/cost"
	"github.com/hoophq/blueprint/internal/model"
)

// CostReport returns the fixture cost census that --demo --costs renders.
//
// The amounts are invented, like every other number in this package. The
// meter is not: it reports zero requests and a zero charge, which is the
// literal truth for a run that makes no AWS calls. A fixture may make up an
// estate; it may not make up a bill the user was never sent.
//
// The window tracks the wall clock through the same LastFullMonth the real
// phase uses, so the demo shows the month a real run today would report.
//
// The shape is deliberately awkward in the ways real bills are: a negative
// credit, a service breakdown whose names are Cost Explorer's rather than the
// census's own service keys, and unattributed spend that is a non-trivial
// share of the total. A fixture that only exercised the tidy path would let
// the tidy path be the only one that works.
func CostReport() *model.CostReport {
	settled := false
	r := &model.CostReport{
		Window:   cost.LastFullMonth(time.Now()),
		Metric:   "AmortizedCost",
		Accounts: []string{acctProd, acctStaging},
		Currencies: []model.CostByCurrency{{
			Currency: "USD",
			// Total = Attributed + Unattributed, exactly. demo_cost_test.go
			// re-adds every figure below with exact decimal arithmetic and
			// fails if these three lines ever stop agreeing with it.
			Total:        "11390.96",
			Attributed:   "10582.69",
			Unattributed: "808.27",
			Services: []model.NamedAmount{
				{Name: "AWS Key Management Service", Amount: "28.50"},
				{Name: "Amazon DocumentDB (with MongoDB compatibility)", Amount: "903.44"},
				{Name: "Amazon DynamoDB", Amount: "642.18"},
				{Name: "Amazon ElastiCache", Amount: "1180.20"},
				{Name: "Amazon Neptune", Amount: "415.73"},
				{Name: "Amazon Redshift", Amount: "2890.00"},
				{Name: "Amazon Relational Database Service", Amount: "4210.55"},
				{Name: "EC2 - Other", Amount: "312.09"},
			},
			UnattributedRecords: []model.NamedAmount{
				// Negative on purpose: credits reduce a bill, and every layer
				// that formats money has to carry the minus sign through
				// rather than dropping or escaping it.
				{Name: "Credit", Amount: "-450.00"},
				{Name: "Support fee", Amount: "200.00"},
				{Name: "Tax", Amount: "1058.27"},
			},
			Accounts: []model.NamedAmount{
				{Name: acctProd, Amount: "8500.00"},
				{Name: acctStaging, Amount: "2890.96"},
			},
		}},
		// A published rollup always says whether it is estimated; nil is
		// reserved for having published none. The fixture stands in for a
		// settled month, so the flag is present and false rather than absent.
		Estimated: &settled,
		Meter:     model.CostMeter{Requests: 0, EstimatedChargeUSD: cost.ChargeUSD(0)},
	}
	r.Sort()
	return r
}
