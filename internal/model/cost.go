package model

import (
	"sort"
	"time"
)

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
	//
	// It is empty when a query failed or was truncated: a partial rollup is
	// indistinguishable from a complete one once it is written down, so it is
	// discarded rather than published, and the failure ledger says why. Meter
	// still reports what the attempt cost.
	Currencies []CostByCurrency `json:"currencies"`
	// Estimated is AWS's own flag on the data behind Currencies. A closed
	// month stays estimated for a while after it ends, and estimated figures
	// move — they do not reconcile to an invoice and should not be treated as
	// a settled bill.
	//
	// nil means no rollup was published, so there is nothing for the flag to
	// describe. Cost Explorer models it as a plain bool, so false means AWS
	// did not mark the data estimated rather than a positive guarantee that
	// the bill has settled.
	Estimated *bool `json:"estimated,omitempty"`
	// Meter records what asking cost this question cost the user.
	Meter CostMeter `json:"meter"`
}

// CostByCurrency is the whole report as expressed in one currency.
//
// Within an entry the partition is exact by construction:
//
//	Attributed + Unattributed == Total
//
// and UnattributedRecords sums to Unattributed. Those all come from a single
// query, so no arithmetic can leave a remainder.
//
// Services sums to Attributed too, but that one is a checked guarantee rather
// than a structural one: the breakdown comes from a second Cost Explorer
// query, and the collector verifies the two agree before publishing anything.
// A report that reached this type therefore satisfies it — a run where the
// queries disagreed produces no currencies at all and a ledger entry saying
// so.
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

// Cost attribution methods, used in ResourceCost.Method.
//
// The method travels with every per-resource amount because figures from
// different sources answer different questions. A Cost Explorer figure is what
// AWS billed over a closed month; a Cost Optimization Hub figure is a
// forward-looking monthly rate modelled from recent usage. Adding one to the
// other, or ranking resources priced by different sources against each other,
// produces a number that means nothing — so the source is never dropped, and a
// reader can always tell which question a figure answers.
const (
	// CostMethodCOH is Cost Optimization Hub's estimatedMonthlyCost for a
	// resource's current configuration.
	CostMethodCOH = "coh"
)

// ResourceCost is what one resource costs, according to one source.
//
// It hangs off Resource as a pointer and is set only when a source actually
// reported a figure for that resource. Nil means nobody reported a cost for
// it — never that the resource is free — the same rule the attribute bag's
// absent keys carry. Nothing here is ever derived by dividing a group total
// across the resources in it: a per-resource number that was never reported
// per resource is exactly the fabricated value the honesty guardrails forbid.
// That is why CostReport (account-level rollups) and this type stay separate
// and are never reconciled against each other.
type ResourceCost struct {
	// Amount is a decimal string, for the same reason every other amount in
	// this package is: binary floating point cannot hold a decimal amount
	// exactly. A stored "0.00" is a real reported figure and must survive to
	// the renderers — the absence of a cost is a nil *ResourceCost, not a zero.
	Amount string `json:"amount"`
	// Currency is the unit the source reported ("USD").
	//
	// Empty means the source reported an amount without naming a currency. It
	// is never filled in with a default: an amount labelled USD that AWS never
	// said was USD is a fabricated value, and renderers must show the figure
	// unlabelled rather than assume.
	Currency string `json:"currency,omitempty"`
	// Method names the source, one of the CostMethod* constants above.
	Method string `json:"method"`
	// Estimated says whether the figure is modelled rather than billed. It is
	// always written out, including when false, because "this is a real bill"
	// is a claim worth making explicitly rather than by omission.
	Estimated bool `json:"estimated"`
	// ObservedFrom and ObservedTo bound the usage period the figure describes,
	// in UTC. For a modelled monthly rate that is the lookback the model ran
	// over, which is not the period being charged for — an unbounded "monthly
	// cost" with no window attached cannot be judged for staleness, the same
	// problem AsOfSuffix solves for metrics.
	//
	// Either may be nil when the source did not report enough to place the
	// window.
	ObservedFrom *time.Time `json:"observed_from,omitempty"`
	ObservedTo   *time.Time `json:"observed_to,omitempty"`
	// Caveats are per-resource disclosures that qualify this figure: that it
	// covers only part of the resource, or that it extrapolates a period the
	// resource did not exist for all of. They are limited to conditions
	// derived from what the source reported about *this* resource — blanket
	// statements true of every figure from a method belong in that method's
	// documentation, not repeated on every row of the artifact.
	Caveats []string `json:"caveats,omitempty"`
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
	// Capped is true when the budget actually prevented a request the run
	// still needed. Spending the last permitted request and finishing is a
	// complete run, so this is not simply Requests == the budget — flagging
	// that case would send a reader hunting for money that is not missing.
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
