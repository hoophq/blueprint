package diff

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoophq/blueprint/internal/cost"
	"github.com/hoophq/blueprint/internal/model"
)

// Spend movement is diffed apart from field drift, and the separation is not
// cosmetic.
//
// A cost compared like any other field would mark almost every resource
// changed on almost every scan — AWS restates bills for weeks and a modelled
// rate is remodelled continuously — which would make --fail-on-change an
// unconditional non-zero and train a reader to ignore the diff. So cost is
// excluded from fieldChanges, and the consequence is the thing this file has
// to get right: **a resource whose only movement is spend appears in none of
// Added, Removed, or Changed.** The cost pass therefore walks every resource
// present in both snapshots, not the Changed list, or the headline case — the
// cleanup last month lowered the bill on everything left standing — would be
// invisible in the one section that exists to show it.

// The two thresholds a spend movement must clear before it is listed. Both are
// required: a $10,000 bill moving fifty cents is noise, and so is a fifty-cent
// bill moving 20%.
//
// The absolute one is one unit of whatever currency the figure is in, not one
// US dollar converted. This tool has no exchange rate and will not invent one,
// so a threshold that claimed to be a dollar in every currency would be
// fabricating the conversion.
const (
	materialPercent  = 5
	materialAbsolute = "1.00"
)

// CostDrift is spend movement between two censuses: what the resources that
// carry a cost figure now cost against what they cost before.
//
// It deliberately does not feed Result.Empty, and therefore does not feed
// --fail-on-change. Cost moves on its own — see the note at the top of this
// file — so gating an exit code on it would gate it on AWS's billing pipeline
// rather than on the estate.
//
// Every comparison in here is made *within* one attribution method. Two figures
// from different methods are not two readings of one quantity — they are
// answers to different questions, taken over different windows by different
// means — so subtracting one from the other, or summing them into one net,
// produces a number that answers neither. Only one method reports spend today,
// which makes the rule invisible rather than unnecessary: figures are matched on
// (ARN, method) rather than ARN, and every total the section prints belongs to
// exactly one method.
//
// Cost Optimization Hub suggestions are not diffed at all. They are not spend,
// so they cannot drift; a suggestion appearing or disappearing between two scans
// says AWS's model changed its mind, which is a fact about AWS's model and not
// about this estate. Putting it in a section headed by dollar movements would
// invite exactly the subtraction the paragraph above refuses.
type CostDrift struct {
	// Billed compares the account-level rollups. Nil when neither census
	// collected one, so there is nothing to say either way.
	Billed *BilledChange
	// Moved are the resources whose figures are comparable and moved past both
	// thresholds, sorted by ARN.
	Moved []CostChange
	// Coverage are resources that gained or lost a cost figure. Gaining
	// visibility of spend is not spending more, so these are reported apart
	// from movement and contribute nothing to the net.
	Coverage []CostCoverage
	// Basis are resources holding two figures that cannot be subtracted —
	// different currency, method, basis, or observation window. Reported as
	// the basis change they are, never as a delta.
	Basis []CostBasisChange
	// Net is the estate-level number, one entry per method and currency that saw
	// any movement at all.
	Net []CostNet
	// Priced and Unpriced count the resources this pass considered: how many
	// carried a figure on at least one side under at least one method, and how
	// many carried none at all. Without the second number a net reads as if it
	// covered the whole estate when it may cover a tenth of it.
	//
	// Both count *resources*, while everything above counts figures. A resource
	// priced by two sources is one resource, and counting it twice here would
	// inflate the coverage denominator the notes are built from — reporting
	// better coverage than the scan actually achieved. They partition the diff:
	// every resource on either side lands in exactly one of the two, so
	// Priced+Unpriced is the resource total and nothing else needs adding to it.
	//
	// Priced is coverage, not participation in the arithmetic. A resource whose
	// two figures were refused as incomparable is still a priced resource; that
	// the pair could not be subtracted is what Basis says.
	Priced   int
	Unpriced int
	// Notes are disclosures that qualify the numbers above.
	Notes []string
}

// CostChange is one resource whose cost moved by enough to be worth printing.
type CostChange struct {
	Resource model.Resource // the current state
	// Method is the attribution method both figures came from. Movement is only
	// ever computed within one method, so there is one name here, not two.
	Method   string
	Currency string
	Old      string
	New      string
	// Delta is New − Old, computed exactly.
	Delta string
	// Percent is the move as a percentage of Old, empty when Old was zero —
	// there is no percentage of nothing.
	Percent string
}

// CostCoverage is a resource that gained or lost a cost figure under one
// method. A resource can appear twice — one source starting to price it while
// another stops is two coverage changes, not one.
type CostCoverage struct {
	Resource model.Resource
	Method   string
	Gained   bool
	// Amount and Currency are the figure that appeared, or the one that went
	// away.
	Amount   string
	Currency string
	// Reason is the source's own explanation for the absence, when it gave
	// one. Empty means nothing looked, which is a different statement from
	// looking and finding nothing.
	Reason string
}

// CostBasisChange is a resource whose two figures under one method answer
// different questions.
type CostBasisChange struct {
	Resource model.Resource
	Method   string
	// Reason names what changed, e.g. "currency changed (USD → EUR)".
	Reason   string
	Old, New string
}

// CostNet is the net spend change for one method and currency.
type CostNet struct {
	// Method is the attribution method these totals came from. It is part of the
	// grouping, not a label on it: a billed 14-day window and a modelled monthly
	// rate summed into one figure would be arithmetic across two questions.
	Method string
	// Currency is empty when the source reported amounts without naming one.
	// Those are never pooled with a named currency.
	Currency string
	// Added is spend on resources that appeared, Removed is spend on resources
	// that went away, and Changed is the net of every comparable move on the
	// resources present in both.
	//
	// Changed sums *all* comparable moves, including the ones too small to be
	// listed. The thresholds exist to keep the list readable, not to change the
	// arithmetic — a thousand sub-dollar moves are still real money.
	Added   string
	Removed string
	Changed string
	// Net is Added − Removed + Changed.
	Net string
}

// BilledChange compares the two account-level cost rollups.
type BilledChange struct {
	// Reason, when set, says why the rollups were not compared. Totals is
	// empty in that case: the refusal is the finding.
	Reason string
	// Window and Metric name what was compared, when it was.
	Window string
	Metric string
	// Totals holds the currencies whose billed total moved past the
	// thresholds, or that one rollup reported and the other did not.
	Totals []BilledTotal
}

// BilledTotal is one currency's movement in the account-level rollup.
type BilledTotal struct {
	Currency string
	// Old and New are empty when that rollup did not report this currency.
	Old, New string
	// Delta and Percent are empty when there was nothing to subtract.
	Delta, Percent string
}

// Empty reports whether the cost pass found nothing worth printing.
func (d CostDrift) Empty() bool {
	return d.Billed == nil && len(d.Moved) == 0 && len(d.Coverage) == 0 &&
		len(d.Basis) == 0 && len(d.Net) == 0
}

// netKey groups the running totals. Method is part of the key rather than a
// label recorded alongside it, so no arithmetic can cross a method boundary
// even by accident.
type netKey struct {
	method   string
	currency string
}

// netAcc accumulates one method and currency's four running totals. Net is kept
// as its own sum rather than derived at the end so it is never out of step with
// the three parts that feed it.
type netAcc struct {
	added, removed, changed, net cost.Sum
	// moved records whether any single figure that fed these totals was
	// non-zero. It is what decides whether the currency is printed, because
	// the totals cannot: a currency where one resource rose $500 and another
	// fell $500 nets to zero, and suppressing it on that basis would print two
	// movement lines and then refuse to say they cancelled — which is the one
	// thing the reader wants to know.
	moved bool
}

// add records spend that started, remove records spend that stopped, and
// change records a movement on a resource present in both. Each updates net
// alongside its own total, so the two can never fall out of step, and each
// records whether the figure it took was money. A figure the sum rejected is
// not movement: it never reached the arithmetic.
func (a *netAcc) add(amount string) {
	if a.added.Add(amount) && !zeroAmount(amount) {
		a.moved = true
	}
	a.net.Add(amount)
}

func (a *netAcc) remove(amount string) {
	if a.removed.Add(amount) && !zeroAmount(amount) {
		a.moved = true
	}
	a.net.Sub(amount)
}

func (a *netAcc) change(mv cost.Move) {
	a.changed.AddMove(mv)
	a.net.AddMove(mv)
	if !mv.Zero() {
		a.moved = true
	}
}

// costDrift compares spend across two censuses, matching resources by ARN —
// the same key the resource diff matches on — and figures by (ARN, method).
func costDrift(old, current *model.Snapshot) CostDrift {
	var d CostDrift
	prev := make(map[string][]model.ResourceCost, len(old.Resources))
	prevSeen := make(map[string]bool, len(old.Resources))
	for i := range old.Resources {
		// Presence is recorded separately from the figures: a resource that was
		// in the baseline carrying no price at all is still a resource the
		// current census can be compared against, and reading presence off the
		// figures map would turn it into one that was never there.
		prevSeen[old.Resources[i].ARN] = true
		prev[old.Resources[i].ARN] = old.Resources[i].Costs
	}
	accs := map[netKey]*netAcc{}
	acc := func(method, currency string) *netAcc {
		k := netKey{method: method, currency: currency}
		a := accs[k]
		if a == nil {
			a = &netAcc{}
			accs[k] = a
		}
		return a
	}
	estimated, billed, churn := false, false, 0

	seen := make(map[string]bool, len(current.Resources))
	for _, r := range current.Resources {
		seen[r.ARN] = true
		if !prevSeen[r.ARN] {
			churn++
			if !r.Priced() {
				d.Unpriced++
				continue
			}
			d.Priced++
			for _, c := range r.Costs {
				a := acc(c.Method, c.Currency)
				a.add(c.Amount)
				flag(c, &estimated, &billed)
			}
			continue
		}
		olds := prev[r.ARN]
		methods := unionMethods(olds, r.Costs)
		if len(methods) == 0 {
			d.Unpriced++
			continue
		}
		// Counted once for the resource, whatever the per-method outcome below:
		// Priced is a coverage statistic over resources, and the same resource
		// arriving under two methods is still one resource that carries a price.
		d.Priced++
		for _, m := range methods {
			o, n := findCost(olds, m), findCost(r.Costs, m)
			switch {
			case o == nil:
				// Seeing a price for the first time is coverage, not spending.
				// Adding it to the net would report the tool's own improvement as
				// the estate getting more expensive.
				//
				// It is still flagged: a coverage line prints the amount, and an
				// amount printed without saying whether it is a modelled rate or a
				// bill is the thing the note below exists to prevent.
				flag(*n, &estimated, &billed)
				d.Coverage = append(d.Coverage, CostCoverage{
					Resource: r, Method: m, Gained: true,
					Amount: n.Amount, Currency: n.Currency,
				})
			case n == nil:
				flag(*o, &estimated, &billed)
				d.Coverage = append(d.Coverage, CostCoverage{
					Resource: r, Method: m,
					Amount: o.Amount, Currency: o.Currency,
					Reason: r.CostUnavailable,
				})
			default:
				d.compare(r, m, o, n, acc, &estimated, &billed)
			}
		}
	}
	for _, r := range old.Resources {
		if seen[r.ARN] {
			continue
		}
		churn++
		if !r.Priced() {
			d.Unpriced++
			continue
		}
		d.Priced++
		for _, c := range r.Costs {
			a := acc(c.Method, c.Currency)
			a.remove(c.Amount)
			flag(c, &estimated, &billed)
		}
	}

	d.Billed = billedChange(old.Cost, current.Cost)
	d.Net = nets(accs)
	d.Notes = notes(d, estimated, billed, churn)
	// ARN then method: with more than one figure per resource, ARN alone is no
	// longer a total order, and an unstable order across runs would show up as
	// diff churn in whatever consumes the JSON.
	sort.Slice(d.Moved, func(i, j int) bool {
		return lessByResourceMethod(d.Moved[i].Resource.ARN, d.Moved[i].Method, d.Moved[j].Resource.ARN, d.Moved[j].Method)
	})
	sort.Slice(d.Coverage, func(i, j int) bool {
		return lessByResourceMethod(d.Coverage[i].Resource.ARN, d.Coverage[i].Method, d.Coverage[j].Resource.ARN, d.Coverage[j].Method)
	})
	sort.Slice(d.Basis, func(i, j int) bool {
		return lessByResourceMethod(d.Basis[i].Resource.ARN, d.Basis[i].Method, d.Basis[j].Resource.ARN, d.Basis[j].Method)
	})
	return d
}

func lessByResourceMethod(arnA, methodA, arnB, methodB string) bool {
	if arnA != arnB {
		return arnA < arnB
	}
	return methodA < methodB
}

// findCost returns the figure one method reported, or nil.
func findCost(costs []model.ResourceCost, method string) *model.ResourceCost {
	for i := range costs {
		if costs[i].Method == method {
			return &costs[i]
		}
	}
	return nil
}

// unionMethods lists every method that priced a resource on either side, sorted.
//
// It reads the methods off the figures rather than off model.CostMethods()
// because a baseline written by an older build can carry a method this one no
// longer emits. Walking the known list would drop that figure silently, which
// would read as spend that never existed rather than as the coverage change it
// is.
func unionMethods(old, current []model.ResourceCost) []string {
	seen := make(map[string]bool, len(old)+len(current))
	var out []string
	for _, cs := range [][]model.ResourceCost{old, current} {
		for _, c := range cs {
			if seen[c.Method] {
				continue
			}
			seen[c.Method] = true
			out = append(out, c.Method)
		}
	}
	sort.Strings(out)
	return out
}

// compare handles one method's figure present in both censuses.
func (d *CostDrift) compare(r model.Resource, method string, o, n *model.ResourceCost, acc func(string, string) *netAcc, estimated, billed *bool) {
	if reason := incomparable(o, n); reason != "" {
		d.Basis = append(d.Basis, CostBasisChange{
			Resource: r, Method: method, Reason: reason,
			Old: money(o.Amount, o.Currency), New: money(n.Amount, n.Currency),
		})
		return
	}
	mv, ok := cost.Movement(o.Amount, n.Amount)
	if !ok {
		// A figure that is not a number cannot be subtracted or believed. It is
		// reported as a basis change rather than skipped, so an amount AWS sent
		// in a form this tool does not accept is visible instead of silently
		// dropping out of the net.
		d.Basis = append(d.Basis, CostBasisChange{
			Resource: r, Method: method, Reason: "amount is not a decimal number",
			Old: money(o.Amount, o.Currency), New: money(n.Amount, n.Currency),
		})
		return
	}
	acc(method, n.Currency).change(mv)
	flag(*n, estimated, billed)
	if !mv.AtLeast(materialAbsolute) || !mv.AtLeastPercent(materialPercent) {
		return
	}
	d.Moved = append(d.Moved, CostChange{
		Resource: r, Method: method, Currency: n.Currency,
		Old: o.Amount, New: n.Amount,
		Delta: mv.Delta(), Percent: mv.Percent(),
	})
}

// flag records whether a figure was modelled or billed, so the section can say
// which kind of number the reader is looking at.
func flag(c model.ResourceCost, estimated, billed *bool) {
	if c.Estimated {
		*estimated = true
		return
	}
	*billed = true
}

// incomparable names why two figures for the same resource and method cannot be
// subtracted, or returns "" when they can.
//
// Every check here is a refusal to guess. Two amounts in different currencies,
// or describing different lengths of usage, are not the same question asked
// twice, and subtracting them would produce a number that looks like drift and
// means nothing.
//
// The method is not checked, because it cannot differ: figures are matched on
// (ARN, method), so a method change shows up as one method losing coverage and
// another gaining it — two honest coverage lines rather than one subtraction
// across two questions.
func incomparable(o, n *model.ResourceCost) string {
	if o.Currency != n.Currency {
		return fmt.Sprintf("currency changed (%s → %s)", currency(o.Currency), currency(n.Currency))
	}
	if o.Estimated != n.Estimated {
		return fmt.Sprintf("basis changed (%s → %s)", basis(o.Estimated), basis(n.Estimated))
	}
	ow, ook := window(o)
	nw, nok := window(n)
	switch {
	case !ook && !nok:
		// Neither figure says what period it covers. That is equally unknown on
		// both sides rather than a change, and refusing here would silence the
		// whole section for any source that does not report a window.
		return ""
	case ook != nok || ow != nw:
		return fmt.Sprintf("observation window changed (%s → %s)", windowStr(ow, ook), windowStr(nw, nok))
	}
	return ""
}

func window(c *model.ResourceCost) (time.Duration, bool) {
	if c.ObservedFrom == nil || c.ObservedTo == nil {
		return 0, false
	}
	return c.ObservedTo.Sub(*c.ObservedFrom), true
}

func windowStr(d time.Duration, ok bool) string {
	if !ok {
		return "not reported"
	}
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	return d.String()
}

func basis(estimated bool) string {
	if estimated {
		return "modelled"
	}
	return "billed"
}

func currency(c string) string {
	if c == "" {
		return "no currency reported"
	}
	return c
}

// money renders an amount with the currency it is in, never without: an
// unlabelled figure invites the reader to assume dollars.
func money(amount, cur string) string {
	return amount + " " + currency(cur)
}

// nets turns the per-method, per-currency accumulators into sorted output,
// dropping any group where not one figure moved so a steady estate prints
// nothing rather than a page of zeroes.
//
// The test is per-figure rather than on the totals, because a total of zero is
// two different situations: nothing happened, or a lot happened and it
// cancelled. Only the first is silence.
func nets(accs map[netKey]*netAcc) []CostNet {
	var out []CostNet
	for k, a := range accs {
		if !a.moved {
			continue
		}
		out = append(out, CostNet{
			Method:   k.method,
			Currency: k.currency,
			Added:    a.added.String(),
			Removed:  a.removed.String(),
			Changed:  a.changed.String(),
			Net:      a.net.String(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// notes assembles the disclosures that qualify the numbers.
func notes(d CostDrift, estimated, billed bool, churn int) []string {
	var out []string
	switch {
	case estimated && billed:
		out = append(out, "some of these figures are modelled monthly rates and some are billed amounts; they answer different questions")
	case estimated:
		out = append(out, "these are modelled monthly rates for the current configuration, not amounts AWS has billed")
	}
	// A commitment discount floats across the estate, so churn is a real
	// alternative explanation for a move on a resource nobody touched. It is
	// disclosed rather than used to suppress the move: the move happened, and
	// the reader is the one who knows whether a reservation covers it.
	if churn > 0 && len(d.Moved) > 0 {
		out = append(out, "reserved-instance and savings-plan discounts float across an account: adding or removing a covered resource changes what other, untouched resources are modelled to cost")
	}
	if len(d.Moved) > 0 || len(d.Net) > 0 {
		// Priced and Unpriced partition the resources, so the denominator is
		// their sum. Coverage and Basis are counts of *figures* and adding them
		// here would inflate the total — and understate the share of the estate
		// the reader is being told is uncovered.
		if total := d.Priced + d.Unpriced; d.Unpriced > 0 {
			out = append(out, fmt.Sprintf("%d of the %d resources in this diff carry no cost figure at all, so the net covers the rest", d.Unpriced, total))
		}
	}
	return out
}

// billedChange compares the account-level rollups, refusing far more often
// than it subtracts.
//
// The rollup is a bill for a closed window, and two scans a week apart report
// the same window while AWS is still restating it. Reporting that restatement
// as spend movement would be the clearest possible version of the lie this
// package exists to avoid, so a bill is only subtracted when both censuses
// cover the same window, on the same metric, and AWS has stopped calling
// either of them an estimate.
func billedChange(old, current *model.CostReport) *BilledChange {
	switch {
	case old == nil && current == nil:
		return nil
	case old == nil:
		return &BilledChange{Reason: "the baseline census collected no cost data, so there is no bill to compare against"}
	case current == nil:
		return &BilledChange{Reason: "this census collected no cost data, so there is no bill to compare"}
	}
	if old.Window != current.Window {
		return &BilledChange{Reason: fmt.Sprintf("billing window changed (%s → %s); bills for different periods are not comparable",
			windowLabel(old.Window), windowLabel(current.Window))}
	}
	if old.Metric != current.Metric {
		return &BilledChange{Reason: fmt.Sprintf("cost metric changed (%s → %s); the two answer different questions",
			orDash(old.Metric), orDash(current.Metric))}
	}
	if estimatedReport(old) || estimatedReport(current) {
		return &BilledChange{Reason: fmt.Sprintf("AWS has not finalized the %s bill, and a restated estimate is not spend movement",
			windowLabel(current.Window))}
	}
	b := &BilledChange{Window: windowLabel(current.Window), Metric: current.Metric}
	oldTotals := totalsByCurrency(old)
	newTotals := totalsByCurrency(current)
	for _, cur := range unionKeys(oldTotals, newTotals) {
		o, hadOld := oldTotals[cur]
		n, hadNew := newTotals[cur]
		if !hadOld || !hadNew {
			// A currency appearing or disappearing is always worth a line: it
			// is a change in what the bill is denominated in, and there is no
			// prior figure to subtract from.
			b.Totals = append(b.Totals, BilledTotal{Currency: cur, Old: o, New: n})
			continue
		}
		mv, ok := cost.Movement(o, n)
		if !ok {
			b.Totals = append(b.Totals, BilledTotal{Currency: cur, Old: o, New: n})
			continue
		}
		if !mv.AtLeast(materialAbsolute) || !mv.AtLeastPercent(materialPercent) {
			continue
		}
		b.Totals = append(b.Totals, BilledTotal{
			Currency: cur, Old: o, New: n,
			Delta: mv.Delta(), Percent: mv.Percent(),
		})
	}
	if len(b.Totals) == 0 {
		return nil
	}
	return b
}

// estimatedReport reports whether a rollup must be kept out of the comparison.
// A nil flag means no rollup was published for the flag to describe, which is
// not a claim that the bill has settled.
func estimatedReport(c *model.CostReport) bool {
	return c.Estimated == nil || *c.Estimated
}

func totalsByCurrency(c *model.CostReport) map[string]string {
	out := make(map[string]string, len(c.Currencies))
	for _, cur := range c.Currencies {
		out[cur.Currency] = cur.Total
	}
	return out
}

func windowLabel(w model.CostWindow) string {
	if w.Label != "" {
		return w.Label
	}
	return orDash(w.Start + "→" + w.End)
}

// WriteCost renders the spend section. label names the baseline, matching the
// resource section above it.
func (d CostDrift) WriteCost(w io.Writer, label string) {
	if d.Empty() {
		return
	}
	fmt.Fprintf(w, "\n━━ spend vs %s ━━\n", label)
	for _, n := range d.Net {
		fmt.Fprintf(w, "  net %s %s  ·  %s new  ·  %s removed  ·  %s on existing%s\n",
			plus(n.Net), currency(n.Currency),
			plus(n.Added), "−"+n.Removed, plus(n.Changed), methodSuffix(n.Method))
	}
	for i, c := range d.Moved {
		if i == maxListed {
			fmt.Fprintf(w, "  ~ … and %d more moved\n", len(d.Moved)-maxListed)
			break
		}
		fmt.Fprintf(w, "  ~ %s (%s, %s): %s → %s  (%s%s)%s\n",
			c.Resource.Name, c.Resource.Service, c.Resource.Region,
			c.Old, c.New, plus(c.Delta), percentSuffix(c.Percent), methodSuffix(c.Method))
	}
	for i, c := range d.Coverage {
		if i == maxListed {
			fmt.Fprintf(w, "  · … and %d more coverage changes\n", len(d.Coverage)-maxListed)
			break
		}
		if c.Gained {
			fmt.Fprintf(w, "  · %s (%s, %s): now priced at %s — new visibility, not new spend%s\n",
				c.Resource.Name, c.Resource.Service, c.Resource.Region,
				money(c.Amount, c.Currency), methodSuffix(c.Method))
			continue
		}
		fmt.Fprintf(w, "  · %s (%s, %s): no longer priced, was %s%s%s\n",
			c.Resource.Name, c.Resource.Service, c.Resource.Region,
			money(c.Amount, c.Currency), reasonSuffix(c.Reason), methodSuffix(c.Method))
	}
	for i, c := range d.Basis {
		if i == maxListed {
			fmt.Fprintf(w, "  ! … and %d more not compared\n", len(d.Basis)-maxListed)
			break
		}
		fmt.Fprintf(w, "  ! %s (%s, %s): not compared — %s (%s → %s)%s\n",
			c.Resource.Name, c.Resource.Service, c.Resource.Region, c.Reason, c.Old, c.New,
			methodSuffix(c.Method))
	}
	if d.Billed != nil {
		writeBilled(w, d.Billed)
	}
	for _, n := range d.Notes {
		fmt.Fprintf(w, "  note: %s\n", n)
	}
}

func writeBilled(w io.Writer, b *BilledChange) {
	if b.Reason != "" {
		fmt.Fprintf(w, "  billed total: not compared — %s\n", b.Reason)
		return
	}
	for _, t := range b.Totals {
		switch {
		case t.Old == "":
			fmt.Fprintf(w, "  billed total (%s %s): %s first reported, %s\n", b.Window, b.Metric, currency(t.Currency), t.New)
		case t.New == "":
			fmt.Fprintf(w, "  billed total (%s %s): %s no longer reported, was %s\n", b.Window, b.Metric, currency(t.Currency), t.Old)
		case t.Delta == "":
			fmt.Fprintf(w, "  billed total (%s %s): %s → %s %s, not subtractable\n", b.Window, b.Metric, t.Old, t.New, currency(t.Currency))
		default:
			fmt.Fprintf(w, "  billed total (%s %s): %s → %s %s  (%s%s)\n",
				b.Window, b.Metric, t.Old, t.New, currency(t.Currency), plus(t.Delta), percentSuffix(t.Percent))
		}
	}
}

// methodSuffix labels a line with the method its figures came from.
//
// Every printed line now belongs to exactly one method, so this names it rather
// than confessing that several were added together — the arithmetic that
// confession existed to disclose is no longer possible.
func methodSuffix(method string) string {
	if method == "" {
		return ""
	}
	return "  [" + method + "]"
}

func percentSuffix(pct string) string {
	if pct == "" {
		return ""
	}
	return ", " + plus(pct) + "%"
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

// plus prefixes a rendered amount with "+" when it is positive, so a net reads
// as a direction rather than a bare number. FloatString emits a leading "-"
// for negatives and never for positives, and a zero of any width has no
// non-zero digit — so the sign is readable off the string without carrying it
// separately.
func plus(s string) string {
	if strings.HasPrefix(s, "-") || zeroAmount(s) {
		return s
	}
	return "+" + s
}

func zeroAmount(s string) bool {
	return !strings.ContainsAny(s, "123456789")
}
