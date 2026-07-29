package render

import (
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/hoophq/blueprint/internal/cost"
	"github.com/hoophq/blueprint/internal/model"
)

// Money rendering for every non-HTML artifact.
//
// One helper, used by the terminal summary and (via the CSV's own columns) by
// anything that reads the artifacts, so a figure reads the same wherever it
// appears. Two rules hold everywhere:
//
//   - The amount is printed exactly as the source reported it. Amounts are
//     validated at ingest precisely so no renderer has to reformat, round, or
//     re-align them; a figure in the terminal can be pasted into the AWS
//     console and matched character for character.
//   - No locale. No thousands separators, no currency symbols, no negative
//     parentheses — a decimal string and an ISO code, "412.73 USD". Symbols and
//     separators vary by locale, and a bill that reads differently on two
//     machines is a bill that cannot be diffed or quoted.

// FormatMoney renders one amount with its currency.
//
// An empty currency means the source reported an amount without naming one, so
// the figure prints unlabelled rather than wearing a currency nobody said it
// was in.
func FormatMoney(amount, currency string) string {
	if currency == "" {
		return amount
	}
	return amount + " " + currency
}

// maxSpendersListed caps the ranked per-resource spend list. The full set is in
// the JSON and CSV; the terminal shows the head of it.
const maxSpendersListed = 5

// maxCaveatsListed caps the qualifier texts printed under one spend group. Some
// caveats name a resource's own dates, so a large group can carry a distinct
// sentence per resource and drown the ranking it is annotating. Whatever the cap
// withholds is counted out loud — a truncation the reader cannot see is the same
// failure as the missing caveat itself.
const maxCaveatsListed = 3

// costSection prints the cost census: the account-level rollup from the billing
// system, then the per-resource figures, kept visibly apart.
//
// They are never combined into one total. A Cost Explorer figure is what AWS
// billed over a closed window; a Cost Optimization Hub figure is a
// forward-looking monthly rate modelled from recent usage. Adding them, or
// ranking one against the other, produces a number that answers no question.
func costSection(w io.Writer, snap *model.Snapshot) {
	reportSection(w, snap.Cost)
	resourceCostSection(w, snap.Resources)
}

// reportSection prints the account-level rollup, one block per currency.
func reportSection(w io.Writer, rep *model.CostReport) {
	if rep == nil || len(rep.Currencies) == 0 {
		return
	}
	head := []string{rep.Window.Label, rep.Metric}
	// Estimated is a pointer: nil means no rollup described the data, false
	// means AWS did not mark it estimated — which is not the same as a promise
	// that the invoice has settled, so nothing is claimed in that case.
	if rep.Estimated != nil && *rep.Estimated {
		head = append(head, "AWS still marks this data estimated")
	}
	fmt.Fprintf(w, "\n  ── cost ── %s\n", strings.Join(head, "  ·  "))

	partial := partialSuffix(rep.Window)
	for _, c := range rep.Currencies {
		fmt.Fprintf(w, "    reported %s%s\n", FormatMoney(c.Total, c.Currency), partial)
		fmt.Fprintf(w, "      %s attributed  ·  %s unattributed\n",
			FormatMoney(c.Attributed, c.Currency), FormatMoney(c.Unattributed, c.Currency))
		fmt.Fprintf(w, "      %s\n", reconciliation(c))
		if len(c.Services) > 0 {
			fmt.Fprintf(w, "      by service: %s\n", formatAmounts(c.Services, c.Currency))
		}
		if len(c.UnattributedRecords) > 0 {
			fmt.Fprintf(w, "      unattributed: %s\n", formatAmounts(c.UnattributedRecords, c.Currency))
		}
	}
	if rep.Meter.Requests > 0 {
		fmt.Fprintf(w, "    ⓘ %d Cost Explorer request(s) — AWS charged $%s\n",
			rep.Meter.Requests, rep.Meter.EstimatedChargeUSD)
	}
}

// reconciliation renders the identity the partition guarantees, and checks it.
//
// The type's contract says attributed + unattributed == total by construction,
// which is exactly why it is worth printing: a reader should not have to take
// the split on trust, and an arrangement of numbers that does not add up is the
// first thing they would want to know. The check is exact — big.Rat, no float —
// so "1.0" and "1.00" agree, as the same money should.
func reconciliation(c model.CostByCurrency) string {
	line := fmt.Sprintf("= %s + %s", c.Attributed, c.Unattributed)
	sum, ok := addDecimals(c.Attributed, c.Unattributed)
	switch {
	case !ok:
		// An amount that will not parse should never reach a renderer; if one
		// does, say so rather than print a tick over arithmetic never done.
		return line + "  ⚠ could not be checked against " + c.Total
	case !equalDecimals(sum, c.Total):
		return line + fmt.Sprintf("  ⚠ does not reconcile: reported %s", c.Total)
	}
	return line + "  ✓"
}

// resourceCostSection ranks the resources a source priced and names how many it
// did not.
func resourceCostSection(w io.Writer, resources []model.Resource) {
	priced, unavailable := splitPriced(resources)
	if len(priced) == 0 && len(unavailable) == 0 {
		return
	}
	fmt.Fprintf(w, "\n  ── per-resource cost ──\n")

	for _, g := range groupSpenders(priced) {
		heading := "top spend"
		if g.qualified {
			// Not decoration. Every figure in this group is a floor its source
			// declined to call a total, so "top spend" would be a claim the
			// source did not make — and the reader has no way to tell from the
			// number alone that it is one.
			heading = "top spend, each figure a lower bound"
		}
		fmt.Fprintf(w, "    %s (%s)\n", heading, g.label)
		shown := min(len(g.resources), maxSpendersListed)
		// Right-align the amounts across the rows actually printed, so the
		// decimal points line up and the ranking can be read down the column.
		// The width comes from the data, not a constant: padding is layout, and
		// the figures themselves are still printed exactly as reported.
		width := 0
		for _, r := range g.resources[:shown] {
			width = max(width, len(r.Cost.Amount))
		}
		for _, r := range g.resources[:shown] {
			// Pad the amount, not the whole rendered figure, so the currency
			// codes stay flush left of the names instead of ragged.
			amount := fmt.Sprintf("%*s", width, r.Cost.Amount)
			fmt.Fprintf(w, "      %s  %s\n", FormatMoney(amount, r.Cost.Currency), spenderName(r))
		}
		if len(g.resources) > shown {
			fmt.Fprintf(w, "      … and %d more (full list in the JSON and CSV output)\n",
				len(g.resources)-shown)
		}
		writeCaveats(w, g.caveats)
	}

	// The coverage caveat is the point of the section: a ranked list of priced
	// resources, read without it, looks like the whole estate.
	total := len(priced) + len(unavailable)
	for _, g := range groupReasons(unavailable) {
		fmt.Fprintf(w, "    ⚠ cost unavailable for %d of %d resources — %s\n", g.count, total, g.reason)
	}
}

// writeCaveats prints the qualifiers attached to a spend group, verbatim.
//
// The text is the source's own sentence, reproduced without editing: it says
// what the figure covers and what it leaves out, and only the source knows
// that. Paraphrasing to fit the column would be this renderer restating a
// disclosure it does not understand.
func writeCaveats(w io.Writer, caveats []string) {
	if len(caveats) == 0 {
		return
	}
	shown := min(len(caveats), maxCaveatsListed)
	for _, c := range caveats[:shown] {
		fmt.Fprintf(w, "      ⓘ %s\n", c)
	}
	if len(caveats) > shown {
		fmt.Fprintf(w, "      ⓘ … and %d further qualifier(s) on these figures "+
			"(full text in the JSON and CSV output)\n", len(caveats)-shown)
	}
}

// splitPriced separates resources a source priced from resources a source
// looked up and could not price. Resources nothing looked at are in neither:
// "not asked" is not a coverage gap to report, it is the absence of a question.
func splitPriced(resources []model.Resource) (priced, unavailable []model.Resource) {
	for _, r := range resources {
		switch {
		case r.Cost != nil:
			priced = append(priced, r)
		case r.CostUnavailable != "":
			unavailable = append(unavailable, r)
		}
	}
	return priced, unavailable
}

// spenderGroup is one ranked list. Figures are grouped by method and currency
// because a ranking across either is meaningless: different methods answer
// different questions, and different currencies are not comparable at all
// without an exchange rate this tool does not have and will not invent.
//
// Whether a figure carries caveats is the third such axis. A source that says
// "this covers storage only" has priced a component, not a resource, so the
// number is a lower bound on what the resource costs and its distance from the
// real total is unknown. Ranking that against a figure covering a whole
// resource asserts an ordering the data does not support — the same error as
// ranking a billed figure against a modelled one, on a different axis. So the
// two are separated and the qualifiers are printed with the group.
//
// Grouping reads only whether Caveats is empty, never what the caveats say.
// Classifying a figure from the wording of its disclosure would be this
// renderer deciding something the source is responsible for; the text is passed
// through verbatim instead.
type spenderGroup struct {
	label     string
	qualified bool
	// caveats holds the distinct qualifier texts across the whole group, in
	// the order they are first met walking the ranked resources — which is
	// deterministic because that ranking is.
	caveats   []string
	resources []model.Resource
}

func groupSpenders(priced []model.Resource) []spenderGroup {
	type key struct {
		method, currency string
		qualified        bool
	}
	keyOf := func(r model.Resource) key {
		return key{r.Cost.Method, r.Cost.Currency, len(r.Cost.Caveats) > 0}
	}
	byKey := map[key][]model.Resource{}
	for _, r := range priced {
		byKey[keyOf(r)] = append(byKey[keyOf(r)], r)
	}
	keys := make([]key, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		if keys[i].currency != keys[j].currency {
			return keys[i].currency < keys[j].currency
		}
		// Unqualified figures first: they are the ones that mean what they
		// look like, so they lead and the qualified list follows as a
		// correction to it rather than the other way round.
		return !keys[i].qualified
	})

	out := make([]spenderGroup, 0, len(keys))
	for _, k := range keys {
		rs := byKey[k]
		sort.SliceStable(rs, func(i, j int) bool {
			a, aok := parseDecimal(rs[i].Cost.Amount)
			b, bok := parseDecimal(rs[j].Cost.Amount)
			if !aok || !bok {
				// Unparseable amounts sink rather than reorder the rest; the
				// name tie-break below keeps the order fixed either way.
				return aok
			}
			if cmp := b.Cmp(a); cmp != 0 {
				return cmp < 0
			}
			return rs[i].ARN < rs[j].ARN
		})
		parts := []string{k.method}
		if estimatedGroup(rs) {
			parts = append(parts, "estimated")
		}
		if k.currency != "" {
			parts = append(parts, k.currency)
		}
		out = append(out, spenderGroup{
			label:     strings.Join(parts, ", "),
			qualified: k.qualified,
			caveats:   distinctCaveats(rs),
			resources: rs,
		})
	}
	return out
}

// distinctCaveats collects the qualifier texts across a ranked group, dropping
// exact repeats so one disclosure shared by every figure is stated once.
//
// Deduplication is by exact string equality, which is not interpretation: two
// identical sentences say one thing. Texts that differ — the partial-period
// caveat names the resource's own creation date — stay separate, because
// collapsing them would be this renderer deciding they mean the same thing.
func distinctCaveats(rs []model.Resource) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range rs {
		for _, c := range r.Cost.Caveats {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// estimatedGroup reports whether every figure in the group is modelled. The
// label is only earned unanimously — calling a mixed group estimated would
// understate the billed figures in it, and calling it billed would overstate
// the modelled ones.
func estimatedGroup(rs []model.Resource) bool {
	for _, r := range rs {
		if !r.Cost.Estimated {
			return false
		}
	}
	return len(rs) > 0
}

func spenderName(r model.Resource) string {
	name := r.Name
	if name == "" {
		name = r.ARN
	}
	scope := []string{}
	if r.Service != "" {
		scope = append(scope, r.Service)
	}
	if r.Region != "" {
		scope = append(scope, r.Region)
	}
	if len(scope) == 0 {
		return name
	}
	return name + " (" + strings.Join(scope, ", ") + ")"
}

type reasonGroup struct {
	reason string
	count  int
}

// groupReasons counts unpriced resources by the reason given, most common
// first, so one dominant gap does not read as several small ones.
func groupReasons(unavailable []model.Resource) []reasonGroup {
	counts := map[string]int{}
	for _, r := range unavailable {
		counts[r.CostUnavailable]++
	}
	out := make([]reasonGroup, 0, len(counts))
	for reason, n := range counts {
		out = append(out, reasonGroup{reason: reason, count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].reason < out[j].reason
	})
	return out
}

func formatAmounts(amounts []model.NamedAmount, currency string) string {
	parts := make([]string, 0, len(amounts))
	for _, a := range amounts {
		parts = append(parts, a.Name+" "+FormatMoney(a.Amount, currency))
	}
	return strings.Join(parts, " · ")
}

// partialSuffix annotates a window that does not cover the whole month it is
// labelled with.
//
// A partial period is stated on the figure and never corrected for: a 12-day
// total is reported as a 12-day total, not scaled up to what a month "would
// have" cost. Projecting it would replace a number AWS reported with one this
// tool invented, and the invented one is the one people would quote.
func partialSuffix(w model.CostWindow) string {
	start, err := time.Parse(time.DateOnly, w.Start)
	if err != nil {
		return ""
	}
	end, err := time.Parse(time.DateOnly, w.End)
	if err != nil {
		return ""
	}
	// End is exclusive, matching Cost Explorer's DateInterval, so the covered
	// span is a plain difference.
	covered := int(end.Sub(start).Hours() / 24)
	if covered <= 0 {
		return ""
	}
	month := daysInMonth(start)
	if covered >= month {
		return ""
	}
	return fmt.Sprintf(" (%d of %d days)", covered, month)
}

func daysInMonth(t time.Time) int {
	first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return int(first.AddDate(0, 1, 0).Sub(first).Hours() / 24)
}

// parseDecimal parses an amount the same way the cost collector validated it on
// the way in. cost.ValidDecimal is the single definition of what counts as an
// amount in this tool; a renderer that accepted more than the collector did
// would be able to print figures the collector would have refused.
func parseDecimal(s string) (*big.Rat, bool) {
	if !cost.ValidDecimal(s) {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

func addDecimals(a, b string) (*big.Rat, bool) {
	ra, ok := parseDecimal(a)
	if !ok {
		return nil, false
	}
	rb, ok := parseDecimal(b)
	if !ok {
		return nil, false
	}
	return new(big.Rat).Add(ra, rb), true
}

// equalDecimals compares a computed rational against a reported amount by
// value, so the comparison never depends on the width either was written at.
func equalDecimals(sum *big.Rat, reported string) bool {
	r, ok := parseDecimal(reported)
	if !ok {
		return false
	}
	return sum.Cmp(r) == 0
}
