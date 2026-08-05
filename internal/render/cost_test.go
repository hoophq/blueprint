package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hoophq/blueprint/internal/model"
)

func TestFormatMoney(t *testing.T) {
	cases := []struct {
		name             string
		amount, currency string
		want             string
	}{
		{"amount and currency", "412.73", "USD", "412.73 USD"},
		// No locale anywhere: the separator-free decimal is the whole point.
		{"large amount keeps no separators", "1234567.89", "USD", "1234567.89 USD"},
		// A source that named no currency gets an unlabelled figure rather
		// than a currency this tool picked for it.
		{"unknown currency", "412.73", "", "412.73"},
		// Negatives are money too — credits and refunds — and print as they
		// were reported, not in accounting parentheses.
		{"credit", "-450.00", "USD", "-450.00 USD"},
		// The amount is passed through exactly: a zero is a reported figure,
		// and trailing-zero width is the source's choice, not the renderer's.
		{"reported zero", "0.00", "USD", "0.00 USD"},
		{"single decimal place", "1.5", "USD", "1.5 USD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatMoney(tc.amount, tc.currency); got != tc.want {
				t.Errorf("FormatMoney(%q, %q) = %q, want %q", tc.amount, tc.currency, got, tc.want)
			}
		})
	}
}

func TestReconciliation(t *testing.T) {
	cases := []struct {
		name      string
		c         model.CostByCurrency
		wantSub   string
		wantNoSub string
	}{
		{
			name:      "adds up",
			c:         model.CostByCurrency{Total: "11390.96", Attributed: "10582.69", Unattributed: "808.27"},
			wantSub:   "= 10582.69 + 808.27  ✓",
			wantNoSub: "⚠",
		},
		{
			// Same money written at two widths is the same money: the check is
			// on value via big.Rat, not on the string.
			name:      "differing decimal widths still reconcile",
			c:         model.CostByCurrency{Total: "1.00", Attributed: "1.0", Unattributed: "0"},
			wantSub:   "✓",
			wantNoSub: "⚠",
		},
		{
			// A credit makes the parts sum below either one of them; the
			// identity still has to hold.
			name:      "credit",
			c:         model.CostByCurrency{Total: "550.00", Attributed: "1000.00", Unattributed: "-450.00"},
			wantSub:   "✓",
			wantNoSub: "⚠",
		},
		{
			name:    "does not reconcile",
			c:       model.CostByCurrency{Total: "100.00", Attributed: "60.00", Unattributed: "30.00"},
			wantSub: "⚠ does not reconcile: reported 100.00",
		},
		{
			// An amount that will not parse should never reach a renderer. If
			// one does, the line says the check did not happen rather than
			// printing a tick over arithmetic nobody did.
			name:    "unparseable part",
			c:       model.CostByCurrency{Total: "100.00", Attributed: "sixty", Unattributed: "40.00"},
			wantSub: "⚠ could not be checked against 100.00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconciliation(tc.c)
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("reconciliation() = %q, want it to contain %q", got, tc.wantSub)
			}
			if tc.wantNoSub != "" && strings.Contains(got, tc.wantNoSub) {
				t.Errorf("reconciliation() = %q, must not contain %q", got, tc.wantNoSub)
			}
		})
	}
}

func TestPartialSuffix(t *testing.T) {
	cases := []struct {
		name  string
		start string
		end   string
		want  string
	}{
		// End is exclusive, so a whole June is 06-01 → 07-01 and carries no
		// annotation.
		{"whole month", "2026-06-01", "2026-07-01", ""},
		{"whole 31-day month", "2026-07-01", "2026-08-01", ""},
		{"leap February", "2024-02-01", "2024-03-01", ""},
		// A month-to-date window says so on the figure. It is never scaled up
		// to what a full month "would have" cost.
		{"month to date", "2026-07-01", "2026-07-13", " (12 of 31 days)"},
		{"short February", "2026-02-01", "2026-02-15", " (14 of 28 days)"},
		// Nothing to say about a window that cannot be read, and nothing to
		// say about one that is empty or inverted either.
		{"unparseable start", "June 2026", "2026-07-01", ""},
		{"unparseable end", "2026-06-01", "", ""},
		{"empty window", "2026-06-01", "2026-06-01", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := partialSuffix(model.CostWindow{Start: tc.start, End: tc.end})
			if got != tc.want {
				t.Errorf("partialSuffix(%s→%s) = %q, want %q", tc.start, tc.end, got, tc.want)
			}
		})
	}
}

// modelledMethod is a per-resource cost source this build does not have.
//
// It stands in for one, because the spend renderer is method-generic: figures
// are grouped by (method, currency, qualified), ranked only within a group, and
// never pooled across one. Pinning that against the single method the census
// produces today would pin nothing — one bucket cannot be shown not to leak into
// another when there is only one bucket. Cost Optimization Hub used to be the
// second source here and is no longer a price source at all, so the name is
// deliberately not one AWS or this tool uses: what these tests need is a method
// the code has never heard of, which is also the case that reaches a user first,
// when a census written by a later build is rendered by this one.
const modelledMethod = "modelled"

// priced builds a resource with one figure on it, for the ranking tests.
func priced(name, amount, currency, method string, estimated bool) model.Resource {
	r := model.Resource{
		ARN: "arn:aws:rds:us-east-1:1:db:" + name, Name: name,
		Service: "rds", Region: "us-east-1", Type: model.TypeRDSInstance,
	}
	r.AddCost(model.ResourceCost{
		Amount: amount, Currency: currency, Method: method, Estimated: estimated,
	})
	return r
}

// figures runs the renderer's own split, so the ranking tests hand groupSpenders
// exactly what costSection hands it rather than a hand-built slice that could
// drift from it.
func figures(rs ...model.Resource) []pricedFigure {
	f, _, _ := splitPriced(rs)
	return f
}

// names lists a group's resources in ranked order.
func names(g spenderGroup) []string {
	out := make([]string, 0, len(g.figures))
	for _, f := range g.figures {
		out = append(out, f.res.Name)
	}
	return out
}

// Two figures from different sources, or in different currencies, answer
// different questions and must never share a ranking. Grouping is what stops
// the terminal from putting a modelled monthly rate above a billed total and
// calling one bigger than the other.
func TestGroupSpendersSeparatesMethodAndCurrency(t *testing.T) {
	groups := groupSpenders(figures(
		priced("a", "10.00", "USD", modelledMethod, true),
		priced("b", "9999.00", "EUR", modelledMethod, true),
		priced("c", "20.00", "USD", model.CostMethodCE, false),
	))
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want one per (method, currency): %v", len(groups), groups)
	}
	var labels []string
	for _, g := range groups {
		labels = append(labels, g.label)
		if len(g.figures) != 1 {
			t.Errorf("group %q holds %d figures, want 1", g.label, len(g.figures))
		}
	}
	want := []string{"ce, USD", "modelled, estimated, EUR", "modelled, estimated, USD"}
	for i, w := range want {
		if labels[i] != w {
			t.Errorf("group %d label = %q, want %q (full: %v)", i, labels[i], w, labels)
		}
	}
}

// Ranking is numeric, not lexical: "9.00" is more than "100.00" as a string
// and less than it as money, and the string answer is the one a reader would
// notice and disbelieve.
func TestGroupSpendersRanksNumerically(t *testing.T) {
	groups := groupSpenders(figures(
		priced("small", "9.00", "USD", modelledMethod, true),
		priced("large", "100.00", "USD", modelledMethod, true),
		priced("zero", "0.00", "USD", modelledMethod, true),
		priced("credit", "-5.00", "USD", modelledMethod, true),
	))
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: %v", len(groups), groups)
	}
	order := names(groups[0])
	want := []string{"large", "small", "zero", "credit"}
	for i, w := range want {
		if order[i] != w {
			t.Errorf("rank %d = %q, want %q (full: %v)", i, order[i], w, order)
		}
	}
}

// Equal amounts must not swap places between two runs over one snapshot.
func TestGroupSpendersTieBreaksOnARN(t *testing.T) {
	same := func(name string) model.Resource { return priced(name, "10.00", "USD", modelledMethod, true) }
	for _, in := range [][]model.Resource{
		{same("ccc"), same("aaa"), same("bbb")},
		{same("bbb"), same("ccc"), same("aaa")},
	} {
		groups := groupSpenders(figures(in...))
		order := names(groups[0])
		if strings.Join(order, ",") != "aaa,bbb,ccc" {
			t.Errorf("input order leaked into the ranking: %v", order)
		}
	}
}

// The "estimated" label is a claim about every figure under it. A group
// holding one billed figure has not earned it.
func TestEstimatedLabelIsUnanimous(t *testing.T) {
	mixed := figures(
		priced("a", "1.00", "USD", modelledMethod, true),
		priced("b", "2.00", "USD", modelledMethod, false),
	)
	if estimatedGroup(mixed) {
		t.Error("a group with one billed figure is labelled estimated")
	}
	if got := groupSpenders(mixed)[0].label; strings.Contains(got, "estimated") {
		t.Errorf("label %q claims estimated for a mixed group", got)
	}
	if !estimatedGroup(mixed[:1]) {
		t.Error("an all-modelled group is not labelled estimated")
	}
	// An empty group has nothing to be unanimous about and must not claim it.
	if estimatedGroup(nil) {
		t.Error("an empty group is labelled estimated")
	}
}

// A ranked list of priced resources, read without the coverage caveat, looks
// like the whole estate. The caveat is what stops that reading, so it names
// the count against the full population and rolls identical reasons together.
func TestResourceCostSectionNamesCoverageGap(t *testing.T) {
	unpriced := func(name, reason string) model.Resource {
		r := model.Resource{ARN: "arn:" + name, Name: name, Service: "dynamodb"}
		r.CostUnavailable = reason
		return r
	}
	var buf bytes.Buffer
	resourceCostSection(&buf, []model.Resource{
		priced("db", "10.00", "USD", modelledMethod, true),
		unpriced("t1", "the cost pass reported nothing for this resource"),
		unpriced("t2", "the cost pass reported nothing for this resource"),
		unpriced("t3", "unsupported resource type"),
		// Nothing looked at this one: not a coverage gap, just no question
		// asked, so it must not inflate either count.
		{ARN: "arn:untouched", Name: "untouched"},
	})
	out := buf.String()

	if !strings.Contains(out, "⚠ cost unavailable for 2 of 4 resources — the cost pass reported nothing") {
		t.Errorf("missing rolled-up coverage caveat:\n%s", out)
	}
	if !strings.Contains(out, "⚠ cost unavailable for 1 of 4 resources — unsupported resource type") {
		t.Errorf("missing second reason:\n%s", out)
	}
	// Most common reason first, so one dominant gap does not read as several
	// small ones.
	if strings.Index(out, "the cost pass reported nothing") > strings.Index(out, "unsupported resource type") {
		t.Errorf("reasons not ordered by count:\n%s", out)
	}
}

// A resource a source priced at zero is a finding — it must appear in the
// ranking with its figure intact, not be filtered out as if nothing reported
// it. This is the `> 0` bug in renderer form.
func TestResourceCostSectionKeepsReportedZero(t *testing.T) {
	var buf bytes.Buffer
	resourceCostSection(&buf, []model.Resource{priced("idle", "0.00", "USD", modelledMethod, true)})
	out := buf.String()
	if !strings.Contains(out, "0.00 USD  idle (rds, us-east-1)") {
		t.Errorf("reported zero dropped or reformatted:\n%s", out)
	}
	if strings.Contains(out, "cost unavailable") {
		t.Errorf("a priced resource counted as a coverage gap:\n%s", out)
	}
}

// The head of the list is capped, and the line that says so points at the
// artifacts that hold the rest — otherwise the cap silently truncates.
func TestResourceCostSectionCapsTheList(t *testing.T) {
	var resources []model.Resource
	for i := range maxSpendersListed + 3 {
		resources = append(resources, priced(string(rune('a'+i)), "10.00", "USD", modelledMethod, true))
	}
	var buf bytes.Buffer
	resourceCostSection(&buf, resources)
	out := buf.String()
	if got := strings.Count(out, " USD  "); got != maxSpendersListed {
		t.Errorf("listed %d spenders, want cap at %d:\n%s", got, maxSpendersListed, out)
	}
	if !strings.Contains(out, "… and 3 more (full list in the JSON and CSV output)") {
		t.Errorf("missing overflow line:\n%s", out)
	}
}

// Cost is opt-in. A scan without --costs must not grow a cost section, empty
// headers included: a heading with nothing under it reads as "we looked and
// there was no spend".
func TestTerminalSaysNothingAboutCostWhenNoneCollected(t *testing.T) {
	var buf bytes.Buffer
	Terminal(&buf, &model.Snapshot{Resources: []model.Resource{
		{ARN: "arn:aws:rds:us-east-1:1:db:x", Name: "x", AccountID: "1", Service: "rds"},
	}}, nil)
	if out := buf.String(); strings.Contains(out, "cost") {
		t.Errorf("cost mentioned without a cost census:\n%s", out)
	}
}

// The rollup and the per-resource figures are separate sections on purpose:
// one is what AWS billed over a closed window, the other a modelled forward
// rate. They are never summed, and the terminal never implies they could be.
func TestTerminalCostSectionsStaySeparate(t *testing.T) {
	estimated := true
	snap := &model.Snapshot{
		Resources: []model.Resource{priced("db", "1000.00", "USD", modelledMethod, true)},
		Cost: &model.CostReport{
			Window: model.CostWindow{Start: "2026-06-01", End: "2026-07-01", Label: "2026-06"},
			Metric: "AmortizedCost", Estimated: &estimated,
			Currencies: []model.CostByCurrency{{
				Currency: "USD", Total: "110.00", Attributed: "100.00", Unattributed: "10.00",
				Services:            []model.NamedAmount{{Name: "Amazon Relational Database Service", Amount: "100.00"}},
				UnattributedRecords: []model.NamedAmount{{Name: "Tax", Amount: "10.00"}},
			}},
			Meter: model.CostMeter{Requests: 2, EstimatedChargeUSD: "0.02"},
		},
	}
	var buf bytes.Buffer
	Terminal(&buf, snap, nil)
	out := buf.String()

	for _, want := range []string{
		"── cost ── 2026-06  ·  AmortizedCost  ·  AWS still marks this data estimated",
		"reported 110.00 USD",
		"100.00 USD attributed  ·  10.00 USD unattributed",
		"= 100.00 + 10.00  ✓",
		"by service: Amazon Relational Database Service 100.00 USD",
		"unattributed: Tax 10.00 USD",
		"ⓘ 2 Cost Explorer request(s) — AWS charged $0.02",
		"── per-resource cost ──",
		"top spend (modelled, estimated, USD)",
		"1000.00 USD  db (rds, us-east-1)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The per-resource figure is larger than the whole billed month, which is
	// exactly the kind of comparison the split prevents: they must not appear
	// under one heading.
	if strings.Index(out, "── per-resource cost ──") < strings.Index(out, "reported 110.00 USD") {
		t.Errorf("per-resource block precedes the rollup:\n%s", out)
	}
}

// A rollup with no currencies is a query that failed or was truncated; the
// failure ledger reports it. Printing an empty cost heading over it would say
// the census looked and found no spend.
func TestReportSectionSilentWithoutCurrencies(t *testing.T) {
	var buf bytes.Buffer
	reportSection(&buf, &model.CostReport{
		Window: model.CostWindow{Label: "2026-06"}, Metric: "AmortizedCost",
		Meter: model.CostMeter{Requests: 1, EstimatedChargeUSD: "0.01"},
	})
	if out := buf.String(); out != "" {
		t.Errorf("printed a cost block with no currencies:\n%s", out)
	}
}

// qualified returns a priced resource whose figure carries the caveats its
// source attached — the shape a source produces when it prices a component
// (storage, say) rather than a whole resource.
func qualified(name, amount string, caveats ...string) model.Resource {
	r := priced(name, amount, "USD", modelledMethod, true)
	r.CostBy(modelledMethod).Caveats = caveats
	return r
}

// A figure its source qualified is a floor, not a total, so it cannot be ranked
// against one that is not qualified: the comparison asserts an ordering the data
// does not support. This is the same reason method and currency split a
// ranking, on a third axis, and it must split it the same way.
func TestGroupSpendersSeparatesQualifiedFigures(t *testing.T) {
	groups := groupSpenders(figures(
		qualified("storage-only", "2000.00", "covers storage only"),
		priced("whole", "1620.00", "USD", modelledMethod, true),
	))
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want the qualified figure separated: %v", len(groups), groups)
	}
	if groups[0].qualified {
		t.Error("unqualified figures must lead; the qualified list reads as a correction to them")
	}
	if !groups[1].qualified {
		t.Fatalf("second group not marked qualified: %v", groups[1])
	}
	if n := groups[0].figures[0].res.Name; n != "whole" {
		t.Errorf("unqualified group holds %q, want the whole-resource figure", n)
	}
	if n := groups[1].figures[0].res.Name; n != "storage-only" {
		t.Errorf("qualified group holds %q, want the storage-only figure", n)
	}
	// The bug this replaces: 2000.00 storage-only printed above 1620.00
	// whole-resource, as if the first resource cost more than the second.
	if len(groups[0].figures) != 1 || len(groups[1].figures) != 1 {
		t.Errorf("figures ranked together: %v", groups)
	}
}

// The heading carries the whole warning for readers who skim the numbers, so
// a qualified group may never be introduced as plain "top spend".
func TestQualifiedFiguresNeverPrintAsBareSpend(t *testing.T) {
	var buf bytes.Buffer
	resourceCostSection(&buf, []model.Resource{
		qualified("storage-only", "2000.00",
			"covers storage only; other charges for this resource are not included"),
	})
	out := buf.String()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "top spend (") {
			t.Errorf("qualified figure headed as an unqualified total:\n%s", out)
		}
	}
	if !strings.Contains(out, "lower bound") {
		t.Errorf("heading does not say the figure is a floor:\n%s", out)
	}
	if !strings.Contains(out, "covers storage only; other charges for this resource are not included") {
		t.Errorf("caveat text not reproduced verbatim:\n%s", out)
	}
}

// One disclosure shared by a whole group is stated once, and disclosures that
// differ stay separate — the partial-period caveat names each resource's own
// dates, and collapsing two different sentences would be the renderer deciding
// they mean the same thing.
func TestDistinctCaveatsDedupesExactRepeatsOnly(t *testing.T) {
	got := distinctCaveats(figures(
		qualified("a", "3.00", "covers storage only", "created 2026-07-01, after the period began"),
		qualified("b", "2.00", "covers storage only", "created 2026-07-14, after the period began"),
		qualified("c", "1.00", "covers storage only"),
	))
	want := []string{
		"covers storage only",
		"created 2026-07-01, after the period began",
		"created 2026-07-14, after the period began",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d distinct caveats, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("caveat %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// Capping the qualifier list is a readability call; hiding that it happened is
// not. Whatever the cap withholds must be counted out loud, or the group reads
// as fully disclosed when it is not.
func TestCaveatOverflowIsCounted(t *testing.T) {
	var resources []model.Resource
	for i := range maxCaveatsListed + 2 {
		resources = append(resources, qualified(
			string(rune('a'+i)), "10.00", "qualifier "+string(rune('a'+i))))
	}
	var buf bytes.Buffer
	resourceCostSection(&buf, resources)
	out := buf.String()
	if got := strings.Count(out, "ⓘ qualifier "); got != maxCaveatsListed {
		t.Errorf("printed %d qualifiers, want cap at %d:\n%s", got, maxCaveatsListed, out)
	}
	if !strings.Contains(out, "… and 2 further qualifier(s)") {
		t.Errorf("cap applied silently:\n%s", out)
	}
}

// An unqualified group has nothing to footnote, and an empty marker line would
// read as a warning with the text missing.
func TestUnqualifiedFiguresPrintNoCaveatLine(t *testing.T) {
	var buf bytes.Buffer
	resourceCostSection(&buf, []model.Resource{
		priced("plain", "10.00", "USD", modelledMethod, true),
	})
	if out := buf.String(); strings.Contains(out, "ⓘ") {
		t.Errorf("caveat marker printed for a figure with no caveats:\n%s", out)
	}
}

// probes wraps a probe list in the smallest report that renders.
func probes(ps ...model.ServiceProbe) *model.ResourceCostReport {
	return &model.ResourceCostReport{
		Window: model.CostWindow{Start: "2026-07-17", End: "2026-07-31", Label: "last 14 days"},
		Metric: "AmortizedCost", Probes: ps,
	}
}

// Every outcome says something different, because the difference between them
// is the whole finding of this pass. Two outcomes sharing a sentence would let a
// reader conclude a service does not report per-resource cost when it merely had
// no usage, or when the tool never asked.
//
// ProbeDenied and ProbeSkipped are here rather than in the demo fixture: both
// are run-wide conditions — a missing permission or opt-in denies every service,
// an exhausted budget skips every service after the one that spent the last
// request — so a fixture pairing either with a successful probe would contradict
// itself. They are still lines a user will see, so they are pinned here.
func TestProbeLinesAreDistinguishable(t *testing.T) {
	all := []model.ServiceProbe{
		{Service: "Amazon Relational Database Service", Outcome: model.ProbeRows, Rows: 4, Matched: 3},
		{Service: "Amazon ElastiCache", Outcome: model.ProbeEmpty},
		{Service: "Amazon Redshift", Outcome: model.ProbeUnsupported,
			Detail: "resource-level data is not available for this service"},
		{Service: "Amazon DynamoDB", Outcome: model.ProbeDenied,
			Detail: "AccessDeniedException: not authorized"},
		{Service: "Amazon Neptune", Outcome: model.ProbeSkipped},
		{Service: "AWS Key Management Service", Outcome: model.ProbeUncensused},
	}
	var buf bytes.Buffer
	probeSection(&buf, probes(all...))
	out := buf.String()

	for _, want := range []string{
		"── per-resource cost probes ── last 14 days  ·  AmortizedCost",
		"Amazon Relational Database Service: rows — 4 row(s), 3 matched to census resources",
		// "empty" is the outcome most likely to be misread as proof a service
		// reports nothing, so its line spends a clause saying it is not that.
		"Amazon ElastiCache: empty — accepted the query and returned no rows, " +
			"which is not the same as reporting no resource-level cost",
		"Amazon Redshift: unsupported — AWS rejected the query for this service: " +
			"resource-level data is not available for this service",
		"Amazon DynamoDB: denied — permission or account opt-in missing: " +
			"AccessDeniedException: not authorized",
		"Amazon Neptune: skipped — never asked, request budget exhausted",
		"AWS Key Management Service: uncensused — never asked, no scanner in this run covers it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// A probe nobody paid for must not print a charge. Zero requests is the
	// demo run and the all-skipped run alike, and "$0.00 charged" invites the
	// reading that AWS was asked and billed nothing.
	if strings.Contains(out, "AWS charged") {
		t.Errorf("charge line printed for an unmetered report:\n%s", out)
	}
}

// Silent truncation is an under-count with no error attached: AWS caps the query
// at a fixed number of groups and returns exactly that many, so the figures are
// each correct while the total is short by an unknown amount. Nothing else in
// the output would give it away.
func TestProbeSectionWarnsOnTruncation(t *testing.T) {
	var buf bytes.Buffer
	probeSection(&buf, probes(model.ServiceProbe{
		Service: "Amazon Relational Database Service",
		Outcome: model.ProbeRows, Rows: 5000, Matched: 4100, Truncated: true,
	}))
	out := buf.String()
	if !strings.Contains(out, "under-counted by an unknown amount") {
		t.Errorf("truncation not disclosed:\n%s", out)
	}
	// The count is still reported. A truncated answer is a partial one, not a
	// void one, and dropping the rows would discard real spend.
	if !strings.Contains(out, "5000 row(s), 4100 matched") {
		t.Errorf("truncated probe dropped its counts:\n%s", out)
	}
}

// The budget is a run-wide ceiling, so "skipped" lines and the cap notice are
// two halves of one statement: which services went unasked, and why.
func TestProbeSectionReportsTheBudgetCap(t *testing.T) {
	rep := probes(
		model.ServiceProbe{Service: "Amazon Relational Database Service", Outcome: model.ProbeRows, Rows: 2, Matched: 2},
		model.ServiceProbe{Service: "Amazon DynamoDB", Outcome: model.ProbeSkipped},
	)
	rep.Meter = model.CostMeter{Requests: 1, EstimatedChargeUSD: "0.01", Capped: true}
	var buf bytes.Buffer
	probeSection(&buf, rep)
	out := buf.String()
	if !strings.Contains(out, "ⓘ 1 Cost Explorer resource request(s) — AWS charged $0.01") {
		t.Errorf("spend not reported:\n%s", out)
	}
	if !strings.Contains(out, "⚠ request budget reached") {
		t.Errorf("budget cap not disclosed:\n%s", out)
	}
}

// A report with no probes is a pass that never ran. Printing its heading over
// nothing would say Cost Explorer was asked and had nothing to say.
func TestProbeSectionSilentWithoutProbes(t *testing.T) {
	for _, rep := range []*model.ResourceCostReport{nil, probes()} {
		var buf bytes.Buffer
		probeSection(&buf, rep)
		if out := buf.String(); out != "" {
			t.Errorf("printed a probe block with no probes:\n%s", out)
		}
	}
}
