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
	probeSection(w, snap.ResourceCost)
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
	figures, unavailable, priced := splitPriced(resources)
	if len(figures) == 0 && len(unavailable) == 0 {
		return
	}
	fmt.Fprintf(w, "\n  ── per-resource cost ──\n")

	for _, g := range groupSpenders(figures) {
		heading := "top spend"
		if g.qualified {
			// Not decoration. Every figure in this group is a floor its source
			// declined to call a total, so "top spend" would be a claim the
			// source did not make — and the reader has no way to tell from the
			// number alone that it is one.
			heading = "top spend, each figure a lower bound"
		}
		fmt.Fprintf(w, "    %s (%s)\n", heading, g.label)
		shown := min(len(g.figures), maxSpendersListed)
		// Right-align the amounts across the rows actually printed, so the
		// decimal points line up and the ranking can be read down the column.
		// The width comes from the data, not a constant: padding is layout, and
		// the figures themselves are still printed exactly as reported.
		width := 0
		for _, f := range g.figures[:shown] {
			width = max(width, len(f.cost.Amount))
		}
		for _, f := range g.figures[:shown] {
			// Pad the amount, not the whole rendered figure, so the currency
			// codes stay flush left of the names instead of ragged.
			amount := fmt.Sprintf("%*s", width, f.cost.Amount)
			fmt.Fprintf(w, "      %s  %s\n", FormatMoney(amount, f.cost.Currency), spenderName(f.res))
		}
		if len(g.figures) > shown {
			fmt.Fprintf(w, "      … and %d more (full list in the JSON and CSV output)\n",
				len(g.figures)-shown)
		}
		// The window goes above the per-resource caveats because it qualifies
		// every figure in the group, and the caveats qualify individual rows.
		if g.window != "" {
			fmt.Fprintf(w, "      ⓘ %s\n", g.window)
		}
		writeCaveats(w, g.caveats)
	}

	// The coverage caveat is the point of the section: a ranked list of priced
	// resources, read without it, looks like the whole estate. The denominator
	// counts resources, not figures — a resource priced by two sources is one
	// resource, and dividing by figures would quietly inflate coverage.
	total := priced + len(unavailable)
	for _, g := range groupReasons(unavailable) {
		fmt.Fprintf(w, "    ⚠ cost unavailable for %d of %d resources — %s\n", g.count, total, g.reason)
	}
}

// probeSection reports what each service answered when asked for resource-level
// cost, and is the whole point of the resource-level pass.
//
// The ranked figures above show what was found; this shows what was *asked*, so
// a service missing from the ranking has a stated reason rather than reading as
// a service that costs nothing. That distinction is the pass's actual finding:
// AWS's own documentation disagrees with itself about which services report
// resource-level cost, and the only way to settle it is to ask and write down
// the answer.
func probeSection(w io.Writer, rep *model.ResourceCostReport) {
	if rep == nil || len(rep.Probes) == 0 {
		return
	}
	head := []string{rep.Window.Label, rep.Metric}
	if rep.Estimated != nil && *rep.Estimated {
		head = append(head, "AWS still marks this data estimated")
	}
	fmt.Fprintf(w, "\n  ── per-resource cost probes ── %s\n", strings.Join(head, "  ·  "))
	fmt.Fprintf(w, "    asked Cost Explorer for resource-level cost, one request per service\n")

	for _, p := range rep.Probes {
		fmt.Fprintf(w, "    - %s: %s\n", p.Service, probeLine(p))
	}
	if rep.Meter.Requests > 0 {
		fmt.Fprintf(w, "    ⓘ %d Cost Explorer resource request(s) — AWS charged $%s\n",
			rep.Meter.Requests, rep.Meter.EstimatedChargeUSD)
	}
	if rep.Meter.Capped {
		fmt.Fprintf(w, "    ⚠ request budget reached — services above marked %q were never asked\n",
			model.ProbeSkipped)
	}
}

// probeLine renders one probe outcome as a sentence.
//
// Each outcome gets its own words rather than a shared template, because the
// difference between them is the finding. "empty" in particular is not
// "unsupported": a service that accepted the query and returned nothing may
// have had no usage in the window, or resource-level data switched on too
// recently to cover it — the AWS preference is not retroactive. Only an outright
// rejection is evidence the service does not report per-resource cost, and only
// that outcome says so.
func probeLine(p model.ServiceProbe) string {
	var line string
	switch p.Outcome {
	case model.ProbeRows:
		line = fmt.Sprintf("%s — %d row(s), %d matched to census resources", p.Outcome, p.Rows, p.Matched)
		if p.Truncated {
			line += fmt.Sprintf("; ⚠ AWS caps this query at %d groups and returned exactly that, "+
				"so spend for this service is under-counted by an unknown amount", p.Rows)
		}
	case model.ProbeEmpty:
		line = p.Outcome + " — accepted the query and returned no rows, which is not the same as " +
			"reporting no resource-level cost"
	case model.ProbeUnsupported:
		line = p.Outcome + " — AWS rejected the query for this service"
	case model.ProbeDenied:
		line = p.Outcome + " — permission or account opt-in missing"
	case model.ProbeSkipped:
		line = p.Outcome + " — never asked, request budget exhausted"
	case model.ProbeUncensused:
		line = p.Outcome + " — never asked, no scanner in this run covers it"
	default:
		line = p.Outcome
	}
	if p.Detail != "" {
		line += ": " + p.Detail
	}
	return line
}

// pricedFigure is one reported figure together with the resource it describes.
//
// The ranking walks figures rather than resources because a resource can carry
// more than one: a billed Cost Explorer window and a modelled Cost Optimization
// Hub rate are two answers to two different questions, and each belongs in its
// own ranking. Walking resources and reading "the" cost would silently drop
// whichever figure happened to sort second.
type pricedFigure struct {
	res  model.Resource
	cost model.ResourceCost
}

// splitPriced separates the figures sources reported from the resources a source
// looked up and could not price, and counts how many distinct resources carry a
// figure. Resources nothing looked at are in neither: "not asked" is not a
// coverage gap to report, it is the absence of a question.
//
// priced is returned alongside the figures because the two counts answer
// different questions and are not interchangeable — rankings are over figures,
// coverage is over resources.
func splitPriced(resources []model.Resource) (figures []pricedFigure, unavailable []model.Resource, priced int) {
	for _, r := range resources {
		switch {
		case r.Priced():
			priced++
			for _, c := range r.Costs {
				figures = append(figures, pricedFigure{res: r, cost: c})
			}
		case r.CostUnavailable != "":
			unavailable = append(unavailable, r)
		}
	}
	return figures, unavailable, priced
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
	// window is the usage period shared by every figure in the group, or empty
	// when they do not share one. It is deliberately not part of the group key:
	// Cost Optimization Hub stamps each recommendation with its own refresh
	// time, so keying on the window would split that method into one group per
	// resource and destroy the ranking.
	window string
	// caveats holds the distinct qualifier texts across the whole group, in
	// the order they are first met walking the ranked figures — which is
	// deterministic because that ranking is.
	caveats []string
	figures []pricedFigure
}

func groupSpenders(figures []pricedFigure) []spenderGroup {
	type key struct {
		method, currency string
		qualified        bool
	}
	keyOf := func(f pricedFigure) key {
		return key{f.cost.Method, f.cost.Currency, len(f.cost.Caveats) > 0}
	}
	byKey := map[key][]pricedFigure{}
	for _, f := range figures {
		byKey[keyOf(f)] = append(byKey[keyOf(f)], f)
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
		fs := byKey[k]
		sort.SliceStable(fs, func(i, j int) bool {
			a, aok := parseDecimal(fs[i].cost.Amount)
			b, bok := parseDecimal(fs[j].cost.Amount)
			if !aok || !bok {
				// Unparseable amounts sink rather than reorder the rest; the
				// name tie-break below keeps the order fixed either way.
				return aok
			}
			if cmp := b.Cmp(a); cmp != 0 {
				return cmp < 0
			}
			return fs[i].res.ARN < fs[j].res.ARN
		})
		parts := []string{k.method}
		if estimatedGroup(fs) {
			parts = append(parts, "estimated")
		}
		if k.currency != "" {
			parts = append(parts, k.currency)
		}
		from, to, shared := sharedWindow(fs)
		if span, ok := wholeDays(from, to, shared); ok {
			parts = append(parts, span)
		}
		out = append(out, spenderGroup{
			label:     strings.Join(parts, ", "),
			qualified: k.qualified,
			window:    windowSentence(from, to, shared),
			caveats:   distinctCaveats(fs),
			figures:   fs,
		})
	}
	return out
}

// sharedWindow returns the usage period every figure in the group agrees on.
//
// Agreement has to be unanimous and both ends have to be placed, because the
// window is printed as a statement about the whole group. Stating one figure's
// window over a group that does not share it would put dates on figures that
// were never observed between them.
func sharedWindow(fs []pricedFigure) (from, to time.Time, ok bool) {
	for i, f := range fs {
		if f.cost.ObservedFrom == nil || f.cost.ObservedTo == nil {
			return time.Time{}, time.Time{}, false
		}
		a, b := f.cost.ObservedFrom.UTC(), f.cost.ObservedTo.UTC()
		if i == 0 {
			from, to = a, b
			continue
		}
		if !a.Equal(from) || !b.Equal(to) {
			return time.Time{}, time.Time{}, false
		}
	}
	return from, to, len(fs) > 0
}

// windowSentence states the period a group's figures cover.
//
// "up to" rather than a second date joined by an arrow, because ObservedTo is
// the exclusive end of the period for both sources — Cost Explorer's window end
// and Cost Optimization Hub's refresh instant. An arrow between two dates reads
// as an inclusive range, which would claim a day of usage that is not in the
// figure.
func windowSentence(from, to time.Time, ok bool) string {
	if !ok {
		return ""
	}
	return "covers usage from " + windowStamp(from) + " up to " + windowStamp(to) + ", UTC"
}

// windowStamp prints a window endpoint, keeping the time of day only when there
// is one. Cost Explorer's window ends on a midnight boundary and printing
// "00:00" on it is noise; Cost Optimization Hub's ends at whatever instant the
// model last refreshed, and rounding that to a date would move the boundary.
func windowStamp(t time.Time) string {
	u := t.UTC()
	if u.Hour() == 0 && u.Minute() == 0 && u.Second() == 0 && u.Nanosecond() == 0 {
		return u.Format("2006-01-02")
	}
	return u.Format("2006-01-02 15:04")
}

// wholeDays names a window's length in the group label, but only when it is an
// exact number of days.
//
// The length is the disclosure that matters most in the label: a reader who
// sees a ranked list assumes a month unless told otherwise, and Cost Explorer's
// resource-level data reaches back 14 days. A window that is not a whole number
// of days is left out rather than rounded — "30-day window" over 29 days and 3
// hours is a small lie in the one place the reader is relying on precision.
func wholeDays(from, to time.Time, ok bool) (string, bool) {
	if !ok {
		return "", false
	}
	d := to.Sub(from)
	if d <= 0 || d%(24*time.Hour) != 0 {
		return "", false
	}
	return fmt.Sprintf("%d-day window", int(d/(24*time.Hour))), true
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

// distinctCaveats collects the qualifier texts across a ranked group, dropping
// exact repeats so one disclosure shared by every figure is stated once.
//
// Deduplication is by exact string equality, which is not interpretation: two
// identical sentences say one thing. Texts that differ — the partial-period
// caveat names the resource's own creation date — stay separate, because
// collapsing them would be this renderer deciding they mean the same thing.
func distinctCaveats(fs []pricedFigure) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range fs {
		for _, c := range f.cost.Caveats {
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
func estimatedGroup(fs []pricedFigure) bool {
	for _, f := range fs {
		if !f.cost.Estimated {
			return false
		}
	}
	return len(fs) > 0
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
