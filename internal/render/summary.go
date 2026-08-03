package render

import (
	"math/big"
	"sort"
	"strings"

	"github.com/hoophq/blueprint/internal/model"
)

// The report's headline figures, computed here rather than in the browser.
//
// Every number above the inventory table — the KPI strip, the attribution bar,
// the platform chart, the per-group spend — is a fold over the whole census.
// The browser used to compute all of them by walking the resource array on
// load, which is fine at a hundred rows and a visible stall at fifty thousand.
// None of them respond to the filter (they describe the census, not the
// current view), so none of them need to be recomputed on a keystroke, so
// there is no reason for the browser to compute them at all.
//
// Folding them in Go costs about five kilobytes in the payload and removes the
// only remaining reason for the page to touch every row before first paint. It
// also puts the money in the one place equipped to add it up: group totals are
// summed as exact rationals from the decimal strings the sources reported,
// which JavaScript cannot do without a library this report will not ship.

// reportSummary is the pre-aggregated block the template paints from.
type reportSummary struct {
	Total    int `json:"total"`
	Types    int `json:"types"`
	Accounts int `json:"accounts"`
	Services int `json:"services"`

	// Attribution tiers, in the order the stacked bar draws them.
	Full     int `json:"full"`
	Partial  int `json:"partial"`
	Untagged int `json:"untagged"`

	NoOwner  int `json:"no_owner"`
	EOL      int `json:"eol"`
	Exposed  int `json:"exposed"`
	Failures int `json:"failures"`

	Platforms []summaryPlatform `json:"platforms,omitempty"`

	// Groups holds, per grouping dimension, the spend attributed to each group
	// value. Counts are deliberately absent: the table computes those from the
	// filtered rows, and a second copy here could only ever disagree with them.
	Groups map[string][]summaryGroup `json:"groups,omitempty"`
	// CostSortable says whether ordering groups by spend is a claim the data
	// supports. See costSortable.
	CostSortable bool `json:"cost_sortable"`
	// Costed says whether any resource carries a cost record at all — any
	// method, any currency, readable or not. It is the one cost fact the page
	// needs before the census has decoded, because it decides whether the
	// inventory table has a Cost column and the header row is built from this
	// block: a column cannot grow a header after the reader has read the
	// header. Everything else about cost waits for the rows.
	//
	// It is deliberately not derived from Groups, which drops a group whose
	// every figure was unreadable, and would therefore hide the column on
	// exactly the run whose cost data most needs looking at.
	Costed bool `json:"costed"`
}

// summaryPlatform is one row of the platform chart: the software a set of
// resources runs, which RDS calls an engine and Lambda calls a runtime.
type summaryPlatform struct {
	Name     string `json:"name"`
	Service  string `json:"service"`
	Count    int    `json:"count"`
	Versions int    `json:"versions"`
	Regions  int    `json:"regions"`
}

// summaryGroup is one value of one grouping dimension and what it costs.
//
// Priced and Total are what make the total readable. Cost Explorer prices some
// services and not others, and only resources with usage in the window, so a
// group's spend is routinely summed over a subset of its members. Printing that
// sum beside the group's full membership — which is what the header does — says
// the estate costs what its priced fraction costs. Carrying both counts lets
// the header say which fraction it is, and it carries them rather than leaving
// the page to divide its own row count by something: the totals are census-wide
// and the row count is not, so they are only the same number by a coincidence
// the header should not depend on.
// Priced is how many members carry a figure from any source. It is the
// group's coverage as a whole; what the header discloses is groupCost.Priced,
// the coverage of the one bucket it is printing, because a reader looking at a
// Cost Explorer total is owed the fraction Cost Explorer reached and not the
// fraction some other source did.
type summaryGroup struct {
	Value  string      `json:"value"`
	Costs  []groupCost `json:"costs,omitempty"`
	Priced int         `json:"priced"`
	Total  int         `json:"total"`
}

// groupCost is one group's spend as one source reported it.
//
// Method and currency are part of the identity, not decoration. A Cost
// Explorer figure is what AWS billed over a closed window and a Cost
// Optimization Hub figure is a forward-looking monthly rate modelled from
// recent usage; adding them produces a number that answers no question, and
// adding across currencies needs an exchange rate this tool does not have and
// will not invent. So a group carries one total per (method, currency) pair,
// each with the count of members it covers, and the page prints one at a time:
// the pair the reader has selected as the report's source.
type groupCost struct {
	Method   string `json:"method"`
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
	// Priced is how many of the group's members this bucket priced. The
	// header prints it against the group's full membership, so it has to be
	// this bucket's reach rather than the group's: a Cost Explorer total over
	// two of twelve resources is not made whole by ten of them having a Cost
	// Optimization Hub estimate instead.
	Priced int `json:"priced"`
	// Qualified marks a total that includes at least one figure whose source
	// attached a caveat — "storage only", say. Such a figure prices a component
	// rather than a resource, so the sum is a lower bound whose distance from
	// the real total is unknown, and the header says so instead of printing it
	// as if it were the whole bill.
	Qualified bool `json:"qualified,omitempty"`
}

// groupDimensions are the table's grouping choices, keyed exactly as the
// template's group-by control names them.
var groupDimensions = map[string]func(*model.Resource) string{
	"service":     func(r *model.Resource) string { return r.Service },
	"type":        func(r *model.Resource) string { return r.Type },
	"engine":      platformOf,
	"environment": func(r *model.Resource) string { return r.Environment },
	"region":      func(r *model.Resource) string { return r.Region },
	"account":     func(r *model.Resource) string { return r.AccountID },
}

// platformKeys names the software a resource runs, which each service calls
// something different. The template reads the same two keys in the same order.
var platformKeys = []string{model.AttrEngine, model.AttrRuntime}

func platformOf(r *model.Resource) string {
	for _, k := range platformKeys {
		if v := r.Attr(k); v != "" {
			return v
		}
	}
	return ""
}

// tierOf classifies a resource's ownership hygiene from its imported tags.
// Both tags present is full attribution, one is partial, neither is untagged —
// and nothing here infers either value from a name or an ARN.
func tierOf(r *model.Resource) string {
	switch {
	case r.Owner != "" && r.Environment != "":
		return "full"
	case r.Owner != "" || r.Environment != "":
		return "partial"
	default:
		return "untagged"
	}
}

func buildSummary(snap *model.Snapshot) reportSummary {
	sum := reportSummary{
		Total:    len(snap.Resources),
		Failures: len(snap.Failures),
	}
	types := map[string]struct{}{}
	accounts := map[string]struct{}{}
	services := map[string]struct{}{}

	for i := range snap.Resources {
		r := &snap.Resources[i]
		if r.Type != "" {
			types[r.Type] = struct{}{}
		}
		if r.AccountID != "" {
			accounts[r.AccountID] = struct{}{}
		}
		if r.Service != "" {
			services[r.Service] = struct{}{}
		}
		switch tierOf(r) {
		case "full":
			sum.Full++
		case "partial":
			sum.Partial++
		default:
			sum.Untagged++
		}
		if r.Owner == "" {
			sum.NoOwner++
		}
		if r.EOL {
			sum.EOL++
		}
		if r.Exposed() {
			sum.Exposed++
		}
		if len(r.Costs) > 0 {
			sum.Costed = true
		}
	}
	sum.Types = len(types)
	sum.Accounts = len(accounts)
	sum.Services = len(services)
	sum.Platforms = buildPlatforms(snap.Resources)
	sum.Groups = buildGroups(snap.Resources)
	sum.CostSortable = costSortable(snap.Resources)
	return sum
}

// buildPlatforms folds the platform chart: one row per distinct engine or
// runtime, ranked by how many resources run it.
//
// Services with no platform concept — object stores, networking, block storage
// — contribute no row at all rather than piling into an "(unknown)" bucket
// that would imply blueprint failed to read something.
func buildPlatforms(resources []model.Resource) []summaryPlatform {
	type acc struct {
		summaryPlatform
		versions map[string]struct{}
		regions  map[string]struct{}
	}
	byName := map[string]*acc{}
	var order []*acc
	for i := range resources {
		r := &resources[i]
		name := platformOf(r)
		if name == "" {
			continue
		}
		a := byName[name]
		if a == nil {
			a = &acc{
				summaryPlatform: summaryPlatform{Name: name, Service: r.Service},
				versions:        map[string]struct{}{},
				regions:         map[string]struct{}{},
			}
			byName[name] = a
			order = append(order, a)
		}
		a.Count++
		if v := r.Attr(model.AttrEngineVersion); v != "" {
			a.versions[v] = struct{}{}
		}
		if r.Region != "" {
			a.regions[r.Region] = struct{}{}
		}
	}
	out := make([]summaryPlatform, 0, len(order))
	for _, a := range order {
		p := a.summaryPlatform
		p.Versions = len(a.versions)
		p.Regions = len(a.regions)
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// buildGroups totals spend per group value, for every grouping dimension the
// table offers. Only dimensions that actually carry money produce entries, so
// a census run without --cost-resources adds nothing to the payload.
//
// Every member is counted, not only the priced ones. The sum can only be over
// what was priced, but the fraction that produced it is the reader's business:
// see summaryGroup.
func buildGroups(resources []model.Resource) map[string][]summaryGroup {
	out := map[string][]summaryGroup{}
	for dim, field := range groupDimensions {
		// Grouped per resource rather than flattened, because each bucket has
		// to report how many resources it reached and a flat list of figures
		// cannot say which resource any of them came from.
		byValue := map[string][][]model.ResourceCost{}
		priced := map[string]int{}
		total := map[string]int{}
		var order []string
		for i := range resources {
			r := &resources[i]
			v := field(r)
			if _, ok := total[v]; !ok {
				order = append(order, v)
			}
			total[v]++
			if len(r.Costs) == 0 {
				continue
			}
			priced[v]++
			byValue[v] = append(byValue[v], r.Costs)
		}
		var groups []summaryGroup
		for _, v := range order {
			if costs := totalCosts(byValue[v]); len(costs) > 0 {
				groups = append(groups, summaryGroup{
					Value:  v,
					Costs:  costs,
					Priced: priced[v],
					Total:  total[v],
				})
			}
		}
		if len(groups) == 0 {
			continue
		}
		sort.Slice(groups, func(i, j int) bool { return groups[i].Value < groups[j].Value })
		out[dim] = groups
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// totalCosts adds figures exactly, one total per (method, currency) pair.
//
// The sum is carried as a rational and rendered at the widest precision any
// input was written at, so it is the exact decimal sum of what the sources
// said — never a float, never rounded to a currency's usual two places. A
// figure whose amount does not parse takes its whole bucket out rather than
// being skipped: a total quietly missing one of its parts is worse than no
// total, because nothing on the page would show it was short.
//
// The input is one slice of figures per resource, not one flat list, so each
// bucket can count the resources it reached. A resource counts once towards a
// bucket however many figures it contributed to it.
func totalCosts(perResource [][]model.ResourceCost) []groupCost {
	type key struct{ method, currency string }
	type bucket struct {
		sum       *big.Rat
		digits    int
		priced    int
		qualified bool
		broken    bool
	}
	buckets := map[key]*bucket{}
	var order []key
	for _, costs := range perResource {
		counted := map[key]bool{}
		for _, c := range costs {
			k := key{c.Method, c.Currency}
			b := buckets[k]
			if b == nil {
				b = &bucket{sum: new(big.Rat)}
				buckets[k] = b
				order = append(order, k)
			}
			if !counted[k] {
				counted[k] = true
				b.priced++
			}
			if len(c.Caveats) > 0 {
				b.qualified = true
			}
			amount, ok := parseDecimal(c.Amount)
			if !ok {
				b.broken = true
				continue
			}
			b.sum.Add(b.sum, amount)
			if d := fractionDigits(c.Amount); d > b.digits {
				b.digits = d
			}
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].method != order[j].method {
			return order[i].method < order[j].method
		}
		return order[i].currency < order[j].currency
	})
	out := make([]groupCost, 0, len(order))
	for _, k := range order {
		b := buckets[k]
		if b.broken {
			continue
		}
		out = append(out, groupCost{
			Method:    k.method,
			Currency:  k.currency,
			Amount:    b.sum.FloatString(b.digits),
			Priced:    b.priced,
			Qualified: b.qualified,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// fractionDigits counts the decimal places an amount was written at. Amounts
// are validated at ingest as plain decimal strings — no exponents — so the
// digits after the point are all there are.
func fractionDigits(amount string) int {
	_, frac, ok := strings.Cut(amount, ".")
	if !ok {
		return 0
	}
	return len(frac)
}

// costSortable reports whether ordering groups by spend states something the
// figures support.
//
// It takes four things to be true at once. One method, because ranking a
// billed figure against a modelled one asserts an ordering across two
// different questions. One currency, for want of an exchange rate. No caveats
// anywhere, because a qualified figure is a lower bound of unknown distance
// from the real total — putting a group with a lower bound of 100 above a
// group with an exact 90 claims a comparison nobody can make.
//
// And every resource priced, which is the same argument one level up. A group
// summed over two of its forty members is a lower bound exactly as a caveated
// figure is, and the distance is unknown in exactly the same way: forty rds
// instances with two priced at 500 rank below forty ec2 instances all priced
// at 30, and the header that says so is off by a factor of sixteen. A group
// with nothing priced is worse, because it carries no total at all and sorts
// as though it were the cheapest thing in the census. Resource-level cost
// covers only some services and only resources with usage in the window, so
// this is the ordinary case at census scale rather than an edge of it.
//
// When any of those fails the table falls back to ranking groups by resource
// count, and the totals still print in the group headers, next to the fraction
// of the group they cover. The money is shown either way; only the claim
// implied by the ordering is withheld.
func costSortable(resources []model.Resource) bool {
	var method, currency string
	found := false
	for i := range resources {
		if len(resources[i].Costs) == 0 {
			return false
		}
		for _, c := range resources[i].Costs {
			if len(c.Caveats) > 0 {
				return false
			}
			if !found {
				method, currency, found = c.Method, c.Currency, true
				continue
			}
			if c.Method != method || c.Currency != currency {
				return false
			}
		}
	}
	return found
}
