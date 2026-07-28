package model

import "sort"

// Cost census types.
//
// Money is carried as a decimal *string*, never a float. Cost Explorer
// reports amounts as decimal strings ("1234.5678901234") and binary floating
// point cannot hold them exactly; rounding a bill is the kind of quiet lie
// this tool exists to avoid. Everything that adds amounts up does it with
// math/big.Rat and formats the result back to a decimal string.
//
// Every amount is scoped by a currency. Cost Explorer reports the currency in
// each metric's Unit field, and a metric with no Unit is a currency this tool
// does not know — it is never assumed to be USD.

// CostWindow is the billing period a CostReport covers.
//
// Start is inclusive and End is exclusive, matching Cost Explorer's own
// DateInterval semantics ("the start date is inclusive, but the end date is
// exclusive"), so June 2026 is Start=2026-06-01, End=2026-07-01. Both are
// YYYY-MM-DD in UTC.
type CostWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
	// Label is the human name for the window ("2026-06"). It is derived from
	// Start, not free text.
	Label string `json:"label"`
}

// CostReport is the cost census for one scan: what the accounts in this
// snapshot cost over one closed billing window, sliced by service.
//
// It hangs off Snapshot rather than off Resource on purpose. These are
// account-level rollups from the billing system, not per-resource figures —
// nothing here may be spread across resources or folded into the attribute
// bag, because a resource-level number that was never reported per resource
// is exactly the fabricated value the honesty guardrails forbid. Per-resource
// attribution is a separate, differently-sourced problem.
//
// A nil *CostReport means cost was not collected (--costs off, or the phase
// produced nothing). That is distinct from a report whose totals are zero.
// Consumers must handle nil: cost is deliberately NOT part of the history
// scope key, so one history bucket legitimately holds a mix of snapshots with
// and without cost, and a missing baseline report means "not collected", never
// "spend went to zero".
type CostReport struct {
	// Window is the billing period covered. Reports for different windows are
	// not comparable and must not be summed or diffed against each other.
	Window CostWindow `json:"window"`
	// Metric is the Cost Explorer metric name these amounts came from, e.g.
	// "AmortizedCost". Different metrics answer different questions and are
	// likewise not comparable.
	Metric string `json:"metric"`
	// Accounts are the account IDs the report covers. This is the census's own
	// account list, not the payer's linked-account list: Cost Explorer called
	// with payer credentials reports the whole organization, so amounts are
	// restricted to the accounts actually scanned. Otherwise a one-account
	// census would silently carry an entire organization's bill.
	Accounts []string `json:"accounts"`
	// Currencies holds one entry per currency Cost Explorer reported in.
	// Amounts in different currencies are never added together.
	Currencies []CostByCurrency `json:"currencies"`
	// Meter records what asking cost this question cost the user.
	Meter CostMeter `json:"meter"`
}

// CostByCurrency is the whole report as expressed in one currency.
//
// Within an entry the partition is exact by construction:
//
//	Attributed + Unattributed == Total
//
// Services sums to Attributed, and UnattributedRecords sums to Unattributed.
type CostByCurrency struct {
	// Currency is the unit Cost Explorer reported ("USD"). Empty means Cost
	// Explorer reported no unit for these amounts — an unknown currency, not
	// an assumed one.
	Currency string `json:"currency"`
	// Total is everything Cost Explorer reported for the window, before any
	// split. It is derived by summing the record-type breakdown rather than
	// read from the API's own total field, which is not populated when results
	// are grouped.
	Total string `json:"total"`
	// Attributed is spend that names a service, so a later pass can try to
	// trace it to resources.
	Attributed string `json:"attributed"`
	// Unattributed is everything else: tax, credits, refunds, support, and
	// commitment fees that belong to the account rather than to any one
	// resource. It is never dropped and never folded into a service.
	Unattributed string `json:"unattributed"`
	// Services is the per-service breakdown of Attributed, sorted by name.
	// Names are Cost Explorer's own service names ("Amazon Relational
	// Database Service"), which do not match the census's service keys.
	Services []NamedAmount `json:"services"`
	// UnattributedRecords is the breakdown of Unattributed by Cost Explorer
	// record type ("Tax", "Credit", "Support fee"), sorted by name. Every
	// record type that is not recognized as service usage lands here under its
	// own name, so a record type AWS adds later is reported rather than lost.
	UnattributedRecords []NamedAmount `json:"unattributed_records"`
	// Accounts is the per-account breakdown of Total, sorted by account ID.
	Accounts []NamedAmount `json:"accounts,omitempty"`
}

// NamedAmount is one labelled decimal amount within a currency.
type NamedAmount struct {
	Name   string `json:"name"`
	Amount string `json:"amount"`
}

// CostMeter records what the cost lookup itself cost.
//
// The Cost Explorer API is billed: AWS charges $0.01 for each paginated
// request. A read-only tool that can spend the user's money has to say how
// much it spent, in the artifact, every time.
type CostMeter struct {
	// Requests is the number of Cost Explorer requests this run issued. The
	// client is configured with retries disabled precisely so this count is
	// the number of billed requests and not a floor on it.
	Requests int `json:"requests"`
	// EstimatedChargeUSD is Requests × $0.01 as a decimal string. It is an
	// estimate only in that AWS's published price is the input; the request
	// count itself is exact.
	EstimatedChargeUSD string `json:"estimated_charge_usd"`
	// Capped is true when the run stopped early because it reached its request
	// budget. The report is then incomplete and says so in the failure ledger.
	Capped bool `json:"capped,omitempty"`
}

// Sort orders every slice in the report so the JSON artifact is byte-for-byte
// stable for a given set of amounts.
//
// The cost phase runs after Snapshot.Finalize (which happens inside the
// scanner run), so the report sorts itself rather than relying on Finalize.
func (c *CostReport) Sort() {
	if c == nil {
		return
	}
	sort.Strings(c.Accounts)
	for i := range c.Currencies {
		cur := &c.Currencies[i]
		sortNamedAmounts(cur.Services)
		sortNamedAmounts(cur.UnattributedRecords)
		sortNamedAmounts(cur.Accounts)
	}
	sort.Slice(c.Currencies, func(i, j int) bool {
		return c.Currencies[i].Currency < c.Currencies[j].Currency
	})
}

// sortNamedAmounts orders by name, then by amount so two entries sharing a
// name (which the builders do not produce, but a hand-written fixture could)
// still land in a fixed order instead of sort.Slice's arbitrary one.
func sortNamedAmounts(n []NamedAmount) {
	sort.Slice(n, func(i, j int) bool {
		if n[i].Name != n[j].Name {
			return n[i].Name < n[j].Name
		}
		return n[i].Amount < n[j].Amount
	})
}
