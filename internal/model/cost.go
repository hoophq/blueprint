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
// There is exactly one, and the constant survives a set of one on purpose. The
// method travels with every per-resource amount so that a second billed source
// added later cannot be quietly summed into this one: figures from different
// sources answer different questions, and a reader can always tell which
// question a figure answers only if the answer is written down at the point the
// figure is.
//
// Cost Optimization Hub used to be the second method here. It is not a pricing
// API and no longer reports a price — what it reports is advice, which lands on
// Resource.Recommendations instead. See Recommendation for why a modelled saving
// may not travel as a ResourceCost.
const (
	// CostMethodCE is Cost Explorer's GetCostAndUsageWithResources: what AWS
	// actually billed for one resource over a closed window in the recent past.
	CostMethodCE = "ce"
)

// CostMethods lists every attribution method, in a fixed order.
//
// It exists because the CSV header is generated from it: the column set is
// closed, and "closed" has to mean the same set of columns in the same order on
// every run, not whichever methods happened to report on this one. Ordering is
// alphabetical rather than by preference so adding a method has one obvious
// answer instead of an argument.
func CostMethods() []string {
	return []string{CostMethodCE}
}

// ResourceCost is what one resource costs, according to one source.
//
// Resource carries a slice of these, one per source that reported on it, and an
// entry exists only when a source actually reported a figure. An absent method
// means nobody reported a cost for it under that method — never that the
// resource is free — the same rule the attribute bag's absent keys carry.
// Nothing here is ever derived by dividing a group total across the resources
// in it: a per-resource number that was never reported per resource is exactly
// the fabricated value the honesty guardrails forbid. That is why CostReport
// (account-level rollups) and this type stay separate and are never reconciled
// against each other.
//
// The census has exactly one money source, and this type is where it lands.
// Anything modelled, forecast, or merely recoverable is not spend and does not
// belong here — a saving that reached Resource.Costs would be in front of every
// consumer that sums figures, and each of them would fold a hypothetical into a
// bill. That is what Recommendation exists to prevent. Consumers still group by
// Method and never add across it, so a second billed source added later cannot
// be summed into Cost Explorer's total by a renderer that has forgotten to ask.
type ResourceCost struct {
	// Amount is a decimal string, for the same reason every other amount in
	// this package is: binary floating point cannot hold a decimal amount
	// exactly. A stored "0.00" is a real reported figure and must survive to
	// the renderers — the absence of a cost is a missing entry in Resource.Costs,
	// not a zero.
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
	// MatchKey is the identifier the source used to name this resource, when it
	// is not the ARN. Empty means the source keyed on the ARN itself and there
	// was nothing to reconcile.
	//
	// It exists because Cost Explorer's RESOURCE_ID is service-dependent — an
	// instance ID for EC2, a bucket name for S3, a full ARN for others — so
	// attaching its figures to the census means matching on something other than
	// a shared key. Recording what was matched turns that join from a claim into
	// something the reader can check: if the wrong resource was priced, the
	// evidence is in the artifact rather than in this tool's source.
	MatchKey string `json:"match_key,omitempty"`
}

// Recommendation is one thing AWS says could be changed about a resource to
// spend less on it, as reported by Cost Optimization Hub's ListRecommendations.
//
// It is deliberately not a ResourceCost, and that distinction is the whole
// reason the type exists. A ResourceCost is money that moved against this
// resource as it is configured today. A Recommendation is money AWS models as
// recoverable if the configuration changes — nobody has been invoiced it, and
// nobody will be unless someone acts. Putting a saving in Resource.Costs would
// put it in front of every consumer that adds figures up: the report's group
// totals, the diff's net drift, the CSV's summable columns. Each of them would
// fold a hypothetical into a bill, and the error would run in the direction
// that gets things deleted.
//
// So: a saving is never added to, subtracted from, or reconciled against
// anything in Resource.Costs or Snapshot.Cost. "Your bill would be $X" is not a
// sentence this tool can support, because the two figures come from different
// methods over different windows and neither was measured against the other.
//
// # What one recommendation covers
//
// The hub reasons about resources more finely than the census does. A database
// is one census row, but its compute and its storage are separate
// recommendations with separate savings — so one ARN can legitimately carry
// several of these, and they are complements rather than contradictions.
// CurrentResourceType is what tells them apart: a saving on
// "RdsDbInstanceStorage" is a saving on storage, and reading it as the
// database's saving overstates it.
//
// blueprint asks ListRecommendations to de-duplicate by resource, so what
// arrives is AWS's own best suggestion per resource rather than every option it
// considered. That matters for what may be totalled. Alternatives — rightsize
// this instance *or* migrate it to Graviton — are mutually exclusive, and
// adding them would count the same dollars twice; AWS ships a separately
// de-duplicated total on another operation precisely because the naive sum is
// wrong. The de-duplicated set this tool reads does not have that overlap, so a
// total over it is defensible as long as it is labelled for what it is:
// modelled, monthly, per currency, and not money anyone will be refunded.
//
// # Verbatim
//
// Every string here is what AWS sent. ActionType, ImplementationEffort and the
// two resource types arrive as *string on ListRecommendations — they are typed
// enums only on the request filter — so a value AWS adds after this SDK pin is
// still real advice and must reach the report unchanged rather than being
// dropped by an allow-list this tool maintains.
type Recommendation struct {
	// ID is AWS's own recommendation identifier. Empty means it reported none.
	//
	// It is kept because it is the only field that can tell two otherwise
	// identical rows apart, which makes it the tie-break that holds the JSON
	// artifact stable. It is emphatically not an identity across runs: AWS
	// documents it as valid for about 24 hours because recommendations are
	// regenerated daily, so matching on it between two censuses would fabricate
	// drift on every scan.
	ID string `json:"id,omitempty"`
	// ActionType is what AWS says to do — "Rightsize", "Stop", "Delete",
	// "MigrateToGraviton" and so on. Stored as the raw string and never mapped
	// onto a local enum: an action this build does not recognize is still advice
	// AWS gave, and a closed list would silently drop it.
	ActionType string `json:"action_type,omitempty"`
	// EstimatedMonthlySavings is a decimal string, for the same reason every
	// other amount in this package is one. Empty means AWS reported no savings
	// figure; "0.00" means it reported zero — a change worth making that saves
	// nothing this month — which is a real answer and must survive to the
	// renderers. The two are never collapsed into each other.
	//
	// Unlike ResourceCost.Amount this is omitempty, because a recommendation can
	// exist with an action and no figure, whereas a ResourceCost exists only
	// when there is an amount.
	EstimatedMonthlySavings string `json:"estimated_monthly_savings,omitempty"`
	// Currency is the unit AWS named for the savings ("USD"). Empty means it
	// named none, and it is never filled in with a default. Savings in different
	// currencies are never summed, and one whose currency AWS did not report
	// gets its own bucket — the rule CostByCurrency follows for spend.
	Currency string `json:"currency,omitempty"`
	// EstimatedSavingsPercentage is a decimal string too, but it is not money
	// and is not padded to cents. AWS states it relative to the total cost over
	// the lookback period, so it is carried as a number AWS stated and never
	// multiplied against a spend figure to back out a dollar amount. Empty means
	// unreported; "0" means AWS said zero.
	EstimatedSavingsPercentage string `json:"estimated_savings_percentage,omitempty"`
	// CurrentResourceType and RecommendedResourceType are Cost Optimization
	// Hub's own type names ("RdsDbInstance", "Ec2Instance"), not CloudFormation
	// types, and they are how a reader tells a whole-resource recommendation
	// from one that covers a component.
	CurrentResourceType     string `json:"current_resource_type,omitempty"`
	RecommendedResourceType string `json:"recommended_resource_type,omitempty"`
	// CurrentResourceSummary and RecommendedResourceSummary are AWS's own
	// free-text descriptions of the before and after shape, passed through
	// unedited. Paraphrasing them would be this tool restating a recommendation
	// it did not make.
	CurrentResourceSummary     string `json:"current_resource_summary,omitempty"`
	RecommendedResourceSummary string `json:"recommended_resource_summary,omitempty"`
	// ImplementationEffort is AWS's own grading — "VeryLow" through "VeryHigh".
	// Stored raw, for the same reason as ActionType. Any ordering this tool
	// imposes on it is a local table rather than the SDK's enum order, which the
	// generated code does not promise to keep stable.
	ImplementationEffort string `json:"implementation_effort,omitempty"`
	// RestartNeeded and RollbackPossible are tri-state, and the third state is
	// the useful one. nil means AWS did not say; a pointer to false is a
	// positive statement — "this needs no restart" is exactly what makes a
	// change safe to schedule — and collapsing the two into a bare bool would
	// tell a reader no restart is required when AWS never said so.
	RestartNeeded    *bool `json:"restart_needed,omitempty"`
	RollbackPossible *bool `json:"rollback_possible,omitempty"`
	// ObservedFrom and ObservedTo bound the usage AWS modelled this
	// recommendation from, in UTC: the refresh timestamp and the lookback it ran
	// over. A monthly saving with no window attached cannot be judged for
	// staleness — the problem AsOfSuffix solves for metrics — and a
	// recommendation is a perishable thing. Either may be nil when the
	// recommendation did not report enough to place the window.
	ObservedFrom *time.Time `json:"observed_from,omitempty"`
	ObservedTo   *time.Time `json:"observed_to,omitempty"`
	// Caveats are disclosures derived from what the source said about this
	// specific recommendation — that it was modelled over a period the resource
	// did not exist for all of, for instance. Blanket truths about every Cost
	// Optimization Hub recommendation belong in this type's documentation, not
	// repeated on every row of the artifact.
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

// Outcomes of one resource-level Cost Explorer probe, used in
// ServiceProbe.Outcome.
//
// The set is finer-grained than success/failure because the interesting answers
// are in between. AWS's own documentation contradicts itself about which
// services report resource-level cost at all — the API reference restricts it
// to EC2-Compute, the console guide offers a per-service picker — so which
// services answer is not something this tool can know in advance. It asks, and
// records what came back. The distinctions below are the ones that change what
// a reader should conclude.
const (
	// ProbeRows means the service answered with per-resource rows.
	ProbeRows = "rows"
	// ProbeEmpty means the service accepted the query and returned nothing.
	//
	// It is deliberately not called "unsupported". An empty answer is
	// indistinguishable from a service that reports nothing because there was no
	// usage in the window, or because resource-level data was switched on too
	// recently to cover it — that preference is not retroactive. Naming this
	// "unsupported" would publish a claim about AWS that the evidence does not
	// support.
	ProbeEmpty = "empty"
	// ProbeUnsupported means AWS rejected the query for this service
	// specifically, which is the only positive evidence of non-support there is.
	ProbeUnsupported = "unsupported"
	// ProbeDenied means the caller lacks the permission or the account-level
	// opt-in. Nothing is known about the service either way.
	ProbeDenied = "denied"
	// ProbeFailed means the request errored for some other reason.
	ProbeFailed = "failed"
	// ProbeSkipped means the probe was never issued — the request budget ran out
	// first. A skipped service is not a service without spend.
	ProbeSkipped = "skipped"
	// ProbeUncensused means the probe was not issued because no scanner covers
	// this service, so its per-resource figures would have no resource to attach
	// to. The spend is real and is in the rollup; it is the census that does not
	// reach it. Recording the service by name is the point — it is a coverage
	// gap the reader can act on.
	ProbeUncensused = "uncensused"
)

// ResourceCostReport records what the resource-level Cost Explorer pass asked
// and what came back, per service.
//
// It exists as its own artifact section rather than only as a set of
// ResourceCost entries because the negatives carry the information. A service
// that returned no rows, a service AWS rejected, and a service never asked
// about because the budget ran out all leave the same trace on the census —
// resources with no Cost Explorer figure — and they mean entirely different
// things. Without this the reader cannot tell "this resource cost nothing" from
// "this tool never found out", which is the distinction the honesty guardrails
// exist to preserve.
//
// A nil *ResourceCostReport means the pass did not run.
type ResourceCostReport struct {
	// Window is the period the figures cover. Cost Explorer's resource-level
	// data reaches back roughly 14 days, so this is never the same window as the
	// rollup's closed month and the two must not be compared.
	Window CostWindow `json:"window"`
	// Metric is the Cost Explorer metric name, matching the rollup's.
	Metric string `json:"metric"`
	// Accounts are the account IDs the pass covered, as in CostReport.
	Accounts []string `json:"accounts"`
	// Probes is one entry per service considered, sorted by service name —
	// including the ones that produced nothing, which is most of the point.
	Probes []ServiceProbe `json:"probes"`
	// Estimated is AWS's own flag on the returned data. A window this recent is
	// normally still estimated; the figures move as AWS restates them.
	//
	// nil means no service returned rows, so there is nothing for the flag to
	// describe. It lives here rather than on each ResourceCost because
	// ResourceCost.Estimated answers a different question — whether the figure
	// is modelled rather than billed — and a billed-but-not-yet-final amount is
	// not a modelled one.
	Estimated *bool `json:"estimated,omitempty"`
	// Meter records what this pass cost the user, separately from the rollup's.
	Meter CostMeter `json:"meter"`
}

// ServiceProbe is the result of asking Cost Explorer for one service's
// resource-level costs.
type ServiceProbe struct {
	// Service is Cost Explorer's own SERVICE dimension value ("Amazon Elastic
	// Compute Cloud - Compute"), stored verbatim as the filter that was sent.
	// These names do not match the census's service keys, and the value here is
	// the one AWS reported rather than one this tool composed.
	Service string `json:"service"`
	// Outcome is one of the Probe* constants.
	Outcome string `json:"outcome"`
	// Detail carries the API's own message for the rejected and failed outcomes.
	Detail string `json:"detail,omitempty"`
	// Rows is how many resource groups the service reported.
	Rows int `json:"rows"`
	// Matched is how many of those rows were joined to a census resource. Rows
	// above Matched is spend on things this census does not cover — a scanner
	// gap, not an error.
	Matched int `json:"matched"`
	// Truncated is true when the row count reached Cost Explorer's per-request
	// group ceiling, so the service's real total may be higher than Rows.
	//
	// The API truncates at that ceiling without erroring, which would otherwise
	// make an under-count look like a complete answer.
	Truncated bool `json:"truncated,omitempty"`
}

// Sort orders the report so the JSON artifact is byte-for-byte stable.
func (c *ResourceCostReport) Sort() {
	if c == nil {
		return
	}
	sort.Strings(c.Accounts)
	sort.Slice(c.Probes, func(i, j int) bool {
		return c.Probes[i].Service < c.Probes[j].Service
	})
}
