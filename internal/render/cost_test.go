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

// priced builds a resource with one figure on it, for the ranking tests.
func priced(name, amount, currency, method string, estimated bool) model.Resource {
	r := model.Resource{
		ARN: "arn:aws:rds:us-east-1:1:db:" + name, Name: name,
		Service: "rds", Region: "us-east-1", Type: model.TypeRDSInstance,
	}
	r.Cost = &model.ResourceCost{
		Amount: amount, Currency: currency, Method: method, Estimated: estimated,
	}
	return r
}

// Two figures from different sources, or in different currencies, answer
// different questions and must never share a ranking. Grouping is what stops
// the terminal from putting a modelled monthly rate above a billed total and
// calling one bigger than the other.
func TestGroupSpendersSeparatesMethodAndCurrency(t *testing.T) {
	groups := groupSpenders([]model.Resource{
		priced("a", "10.00", "USD", model.CostMethodCOH, true),
		priced("b", "9999.00", "EUR", model.CostMethodCOH, true),
		priced("c", "20.00", "USD", "ce", false),
	})
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want one per (method, currency): %v", len(groups), groups)
	}
	var labels []string
	for _, g := range groups {
		labels = append(labels, g.label)
		if len(g.resources) != 1 {
			t.Errorf("group %q holds %d resources, want 1", g.label, len(g.resources))
		}
	}
	want := []string{"ce, USD", "coh, estimated, EUR", "coh, estimated, USD"}
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
	groups := groupSpenders([]model.Resource{
		priced("small", "9.00", "USD", model.CostMethodCOH, true),
		priced("large", "100.00", "USD", model.CostMethodCOH, true),
		priced("zero", "0.00", "USD", model.CostMethodCOH, true),
		priced("credit", "-5.00", "USD", model.CostMethodCOH, true),
	})
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: %v", len(groups), groups)
	}
	var order []string
	for _, r := range groups[0].resources {
		order = append(order, r.Name)
	}
	want := []string{"large", "small", "zero", "credit"}
	for i, w := range want {
		if order[i] != w {
			t.Errorf("rank %d = %q, want %q (full: %v)", i, order[i], w, order)
		}
	}
}

// Equal amounts must not swap places between two runs over one snapshot.
func TestGroupSpendersTieBreaksOnARN(t *testing.T) {
	same := func(name string) model.Resource { return priced(name, "10.00", "USD", model.CostMethodCOH, true) }
	for _, in := range [][]model.Resource{
		{same("ccc"), same("aaa"), same("bbb")},
		{same("bbb"), same("ccc"), same("aaa")},
	} {
		groups := groupSpenders(in)
		var order []string
		for _, r := range groups[0].resources {
			order = append(order, r.Name)
		}
		if strings.Join(order, ",") != "aaa,bbb,ccc" {
			t.Errorf("input order leaked into the ranking: %v", order)
		}
	}
}

// The "estimated" label is a claim about every figure under it. A group
// holding one billed figure has not earned it.
func TestEstimatedLabelIsUnanimous(t *testing.T) {
	mixed := []model.Resource{
		priced("a", "1.00", "USD", model.CostMethodCOH, true),
		priced("b", "2.00", "USD", model.CostMethodCOH, false),
	}
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
		priced("db", "10.00", "USD", model.CostMethodCOH, true),
		unpriced("t1", "no recommendation"),
		unpriced("t2", "no recommendation"),
		unpriced("t3", "unsupported resource type"),
		// Nothing looked at this one: not a coverage gap, just no question
		// asked, so it must not inflate either count.
		{ARN: "arn:untouched", Name: "untouched"},
	})
	out := buf.String()

	if !strings.Contains(out, "⚠ cost unavailable for 2 of 4 resources — no recommendation") {
		t.Errorf("missing rolled-up coverage caveat:\n%s", out)
	}
	if !strings.Contains(out, "⚠ cost unavailable for 1 of 4 resources — unsupported resource type") {
		t.Errorf("missing second reason:\n%s", out)
	}
	// Most common reason first, so one dominant gap does not read as several
	// small ones.
	if strings.Index(out, "no recommendation") > strings.Index(out, "unsupported resource type") {
		t.Errorf("reasons not ordered by count:\n%s", out)
	}
}

// A resource a source priced at zero is a finding — it must appear in the
// ranking with its figure intact, not be filtered out as if nothing reported
// it. This is the `> 0` bug in renderer form.
func TestResourceCostSectionKeepsReportedZero(t *testing.T) {
	var buf bytes.Buffer
	resourceCostSection(&buf, []model.Resource{priced("idle", "0.00", "USD", model.CostMethodCOH, true)})
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
		resources = append(resources, priced(string(rune('a'+i)), "10.00", "USD", model.CostMethodCOH, true))
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
		Resources: []model.Resource{priced("db", "1000.00", "USD", model.CostMethodCOH, true)},
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
		"top spend (coh, estimated, USD)",
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
