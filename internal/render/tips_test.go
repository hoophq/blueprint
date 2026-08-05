package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hoophq/blueprint/internal/model"
)

// tipped builds a resource carrying one suggestion. Only the fields a given
// test is about are set; everything else stays absent, which is the state Cost
// Optimization Hub actually returns most of the time.
func tipped(name string, rec model.Recommendation) model.Resource {
	r := model.Resource{
		ARN: "arn:aws:rds:us-east-1:1:db:" + name, Name: name,
		Service: "rds", Region: "us-east-1", Type: model.TypeRDSInstance,
	}
	r.AddRecommendation(rec)
	return r
}

// saving is the common shape: an amount, a currency, and an action.
func saving(amount, currency, action string) model.Recommendation {
	return model.Recommendation{
		ID: amount + "/" + currency + "/" + action, ActionType: action,
		EstimatedMonthlySavings: amount, Currency: currency,
	}
}

// The section's whole purpose is to be a different answer from the spend
// above it, so it says three things in its own furniture: that the figures are
// modelled, that nobody was charged them, and that they are not netted against
// anything. A saving printed without those reads as a discount already applied.
func TestTipsSectionNeverPresentsSavingsAsMoneySpent(t *testing.T) {
	var buf bytes.Buffer
	tipsSection(&buf, []model.Resource{tipped("orders", saving("340.00", "USD", "Rightsize"))})
	out := buf.String()

	for _, want := range []string{
		"── ways to cut this bill ── AWS Cost Optimization Hub  ·  modelled monthly, never billed",
		"ranked by modelled monthly saving (USD)",
		"340.00 USD  Rightsize  ·  orders (rds, us-east-1)",
		"Σ 340.00 USD across 1 suggestion(s), if every one were acted on",
		"modelled by AWS from recent usage, not amounts you were charged",
		"nothing here is added to or subtracted from the spend above",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The phrasings that would turn advice into a forecast. None of them is
	// something AWS said, and each one implies the arithmetic this section
	// exists to refuse.
	for _, never := range []string{"net spend", "after savings", "new total", "will cost"} {
		if strings.Contains(out, never) {
			t.Errorf("section promises a lower bill with %q:\n%s", never, out)
		}
	}
}

// Savings in different currencies are not comparable and not addable, so they
// get separate rankings and separate totals — the rule spend already follows.
// A suggestion whose currency AWS never named is a third bucket rather than an
// assumed dollar.
func TestGroupTipsNeverMixesCurrencies(t *testing.T) {
	groups, unquantified := groupTips([]model.Resource{
		tipped("usd", saving("10.00", "USD", "Rightsize")),
		tipped("eur", saving("9999.00", "EUR", "Delete")),
		tipped("bare", saving("50.00", "", "Stop")),
	})
	if len(unquantified) != 0 {
		t.Fatalf("got %d unquantified, want none: every fixture has an amount", len(unquantified))
	}
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want one per currency: %+v", len(groups), groups)
	}
	// Sorted by currency, and the unnamed one sorts first as the empty string.
	for i, want := range []struct {
		currency, total string
	}{{"", "50.00"}, {"EUR", "9999.00"}, {"USD", "10.00"}} {
		if groups[i].currency != want.currency || groups[i].total != want.total {
			t.Errorf("group %d = %q/%s, want %q/%s",
				i, groups[i].currency, groups[i].total, want.currency, want.total)
		}
		if len(groups[i].tips) != 1 {
			t.Errorf("group %d holds %d tips, want 1: currencies pooled", i, len(groups[i].tips))
		}
	}

	// And the heading has to name the missing currency rather than print an
	// empty parenthesis, which reads as a rendering fault instead of a fact
	// about AWS's answer.
	var buf bytes.Buffer
	tipsSection(&buf, []model.Resource{tipped("bare", saving("50.00", "", "Stop"))})
	if !strings.Contains(buf.String(), "(currency not reported by AWS)") {
		t.Errorf("unnamed currency not disclosed:\n%s", buf.String())
	}
}

// The split between ranked and unquantified is on the amount being *absent*,
// never on its value. A change AWS modelled at exactly zero is a real answer —
// worth making, saves nothing this month — and demoting it to "no figure" would
// restate what AWS said. This is the `> 0` bug in its savings form.
func TestGroupTipsRanksAReportedZeroAndSetsAsideOnlyTheUnpriced(t *testing.T) {
	groups, unquantified := groupTips([]model.Resource{
		tipped("zero", saving("0.00", "USD", "MigrateToGraviton")),
		tipped("real", saving("12.00", "USD", "Rightsize")),
		tipped("silent", model.Recommendation{ID: "n", ActionType: "Upgrade"}),
	})
	if len(unquantified) != 1 || unquantified[0].res.Name != "silent" {
		t.Fatalf("unquantified = %+v, want only the suggestion with no amount", unquantified)
	}
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: %+v", len(groups), groups)
	}
	g := groups[0]
	if len(g.tips) != 2 || g.tips[1].res.Name != "zero" {
		t.Fatalf("ranking = %+v, want the zero ranked last but present", g.tips)
	}
	if g.tips[1].rec.EstimatedMonthlySavings != "0.00" {
		t.Errorf("zero rendered as %q, want it intact",
			g.tips[1].rec.EstimatedMonthlySavings)
	}
	// The zero is a term in the total like any other, and the count covers it.
	if g.total != "12.00" {
		t.Errorf("total = %s, want 12.00", g.total)
	}

	var buf bytes.Buffer
	tipsSection(&buf, []model.Resource{
		tipped("zero", saving("0.00", "USD", "MigrateToGraviton")),
		tipped("silent", model.Recommendation{ID: "n", ActionType: "Upgrade"}),
	})
	out := buf.String()
	if !strings.Contains(out, "0.00 USD  MigrateToGraviton  ·  zero (rds, us-east-1)") {
		t.Errorf("reported zero dropped from the list:\n%s", out)
	}
	if !strings.Contains(out, "1 further suggestion(s) carry no savings figure — "+
		"AWS named the change without pricing it") {
		t.Errorf("unpriced suggestion not counted out loud:\n%s", out)
	}
}

// Ranking is numeric, and ties are broken all the way down to the
// recommendation id, because one resource can carry two suggestions worth the
// same and sort.Slice would otherwise reorder them between runs over one census.
func TestGroupTipsRanksNumericallyAndBreaksTiesDeterministically(t *testing.T) {
	tie := model.Resource{
		ARN: "arn:aws:rds:us-east-1:1:db:twin", Name: "twin",
		Service: "rds", Region: "us-east-1", Type: model.TypeRDSInstance,
	}
	tie.AddRecommendation(model.Recommendation{
		ID: "b", ActionType: "Stop", EstimatedMonthlySavings: "50.00", Currency: "USD",
	})
	tie.AddRecommendation(model.Recommendation{
		ID: "a", ActionType: "Delete", EstimatedMonthlySavings: "50.00", Currency: "USD",
	})

	groups, _ := groupTips([]model.Resource{
		tipped("small", saving("9.00", "USD", "Rightsize")),
		tipped("large", saving("100.00", "USD", "Delete")),
		tie,
	})
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: %+v", len(groups), groups)
	}
	var order []string
	for _, tp := range groups[0].tips {
		order = append(order, tp.res.Name+"/"+tp.rec.ID)
	}
	// "9.00" sorts above "100.00" as a string, which is the answer a reader
	// would notice and disbelieve.
	want := []string{"large/100.00/USD/Delete", "twin/a", "twin/b", "small/9.00/USD/Rightsize"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("ranked %v, want %v", order, want)
	}
	if groups[0].total != "209.00" {
		t.Errorf("total = %s, want 209.00", groups[0].total)
	}
}

// A total missing a term it does not mention is worse than no total, so one
// unreadable amount voids the group's Σ line rather than being skipped over.
// The suggestions themselves still print — the amount is AWS's to explain, and
// hiding the advice because of it would suppress a real finding.
func TestGroupTipsVoidsTheTotalWhenOneAmountCannotBeRead(t *testing.T) {
	groups, _ := groupTips([]model.Resource{
		tipped("good", saving("10.00", "USD", "Rightsize")),
		tipped("bad", saving("not-a-number", "USD", "Delete")),
	})
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: %+v", len(groups), groups)
	}
	if groups[0].total != "" {
		t.Errorf("total = %q, want none: one term could not be read", groups[0].total)
	}
	if len(groups[0].tips) != 2 {
		t.Errorf("got %d tips, want both: the unreadable amount is still advice",
			len(groups[0].tips))
	}

	var buf bytes.Buffer
	tipsSection(&buf, []model.Resource{
		tipped("good", saving("10.00", "USD", "Rightsize")),
		tipped("bad", saving("not-a-number", "USD", "Delete")),
	})
	if out := buf.String(); strings.Contains(out, "Σ") {
		t.Errorf("printed a total over an unreadable term:\n%s", out)
	}
}

// The list is capped for readability; the tail is counted out loud, and the Σ
// covers every suggestion rather than only the five on screen — a total over
// the visible rows would silently answer a different question.
func TestTipsSectionCapsTheListWithoutShorteningTheTotal(t *testing.T) {
	var resources []model.Resource
	for i := range maxTipsListed + 3 {
		resources = append(resources, tipped(
			string(rune('a'+i)), saving("10.00", "USD", "Rightsize")))
	}
	var buf bytes.Buffer
	tipsSection(&buf, resources)
	out := buf.String()

	if got := strings.Count(out, "10.00 USD  Rightsize"); got != maxTipsListed {
		t.Errorf("listed %d suggestions, want the cap at %d:\n%s", got, maxTipsListed, out)
	}
	if !strings.Contains(out, "… and 3 more (full list in the JSON and CSV output)") {
		t.Errorf("cap applied silently:\n%s", out)
	}
	if !strings.Contains(out, "Σ 80.00 USD across 8 suggestion(s)") {
		t.Errorf("total covers only the printed rows:\n%s", out)
	}
}

// A row has to say enough to schedule the change, and every part of it comes
// from AWS. Both tri-state flags print in all three states, because "AWS did
// not say whether this needs a restart" is not the same as "it does not".
func TestTipLineStatesWhatItTakesInEveryStateAWSReports(t *testing.T) {
	res := model.Resource{
		ARN: "arn:aws:rds:us-east-1:1:db:orders", Name: "orders",
		Service: "rds", Region: "us-east-1", Type: model.TypeRDSInstance,
	}
	for _, tc := range []struct {
		name string
		rec  model.Recommendation
		want string
	}{
		{
			name: "everything reported",
			rec: model.Recommendation{
				ActionType: "Rightsize", ImplementationEffort: "Medium",
				CurrentResourceSummary: "db.r5.2xlarge", RecommendedResourceSummary: "db.r5.xlarge",
				RestartNeeded: ptrTo(true), RollbackPossible: ptrTo(true),
			},
			want: "Rightsize db.r5.2xlarge → db.r5.xlarge  ·  orders (rds, us-east-1)  ·  " +
				"medium effort  ·  restart needed  ·  reversible",
		},
		{
			// The pointer-to-false case, and the reason both flags are
			// pointers: "no restart" is what makes a change safe to schedule
			// in business hours, and it is a positive statement AWS made.
			name: "reported as needing nothing",
			rec: model.Recommendation{
				ActionType: "Delete", ImplementationEffort: "VeryLow",
				CurrentResourceType: "EbsVolume",
				RestartNeeded:       ptrTo(false), RollbackPossible: ptrTo(false),
			},
			want: "Delete EbsVolume  ·  orders (rds, us-east-1)  ·  very low effort  ·  " +
				"no restart  ·  not reversible",
		},
		{
			// Nothing about effort or restarts. The row shrinks to what AWS
			// said instead of filling the gaps with defaults.
			name: "action only",
			rec:  model.Recommendation{ActionType: "Stop"},
			want: "Stop  ·  orders (rds, us-east-1)",
		},
		{
			// No action either. The change itself is still nameable from the
			// types, and a row with a saving and no sentence would be unreadable.
			name: "types only",
			rec: model.Recommendation{
				CurrentResourceType: "RdsDbInstance", RecommendedResourceType: "RdsDbInstanceStorage",
			},
			want: "RdsDbInstance → RdsDbInstanceStorage  ·  orders (rds, us-east-1)",
		},
		{
			// AWS names the same type on both sides for an in-place change;
			// "Ec2Instance → Ec2Instance" would read as a move to nowhere.
			name: "same type both sides",
			rec: model.Recommendation{
				ActionType:          "MigrateToGraviton",
				CurrentResourceType: "Ec2Instance", RecommendedResourceType: "Ec2Instance",
			},
			want: "MigrateToGraviton Ec2Instance  ·  orders (rds, us-east-1)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tipLine(resourceTip{res: res, rec: tc.rec}); got != tc.want {
				t.Errorf("tipLine =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// The summaries are AWS's readable end and the types are the fallback, so a
// suggestion about a database's storage is distinguishable from one about its
// compute. Neither is ever rewritten into a shape AWS did not name.
func TestTipChangePrefersTheSummaryAndFallsBackToTheType(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  model.Recommendation
		want string
	}{
		{
			name: "summaries win over types",
			rec: model.Recommendation{
				ActionType:          "Rightsize",
				CurrentResourceType: "RdsDbInstance", RecommendedResourceType: "RdsDbInstance",
				CurrentResourceSummary: "db.m6g.2xlarge", RecommendedResourceSummary: "db.m6g.xlarge",
			},
			want: "Rightsize db.m6g.2xlarge → db.m6g.xlarge",
		},
		{
			// A recommendation to delete has no "after" shape at all.
			name: "no recommended side",
			rec: model.Recommendation{
				ActionType: "Delete", CurrentResourceSummary: "gp3",
			},
			want: "Delete gp3",
		},
		{
			// Nothing but the action. Returning "Stop " with a trailing space
			// would be a rendering artifact in the middle of a joined row.
			name: "nothing to change",
			rec:  model.Recommendation{ActionType: "Stop"},
			want: "Stop",
		},
		{
			// An action AWS invents after this build ships still renders: the
			// string is passed through rather than matched against a local enum.
			name: "an action this build does not know",
			rec: model.Recommendation{
				ActionType: "MigrateToSomethingNewer", CurrentResourceSummary: "old",
			},
			want: "MigrateToSomethingNewer old",
		},
		{
			name: "nothing at all",
			rec:  model.Recommendation{ID: "x"},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tipChange(tc.rec); got != tc.want {
				t.Errorf("tipChange = %q, want %q", got, tc.want)
			}
		})
	}
}

// AWS's effort grades are a closed enum, so spacing and lower-casing them is
// presentation. A value outside the set is passed through as AWS wrote it —
// a grade this build does not recognise is not one it may rephrase — and an
// unreported grade prints nothing rather than a guess at the middle.
func TestEffortLabelRephrasesOnlyWhatItRecognises(t *testing.T) {
	for in, want := range map[string]string{
		"":          "",
		"VeryLow":   "very low effort",
		"Low":       "low effort",
		"Medium":    "medium effort",
		"High":      "high effort",
		"VeryHigh":  "very high effort",
		"Excessive": "Excessive effort",
	} {
		if got := effortLabel(in); got != want {
			t.Errorf("effortLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// Suggestions carry their own disclosures — modelled over a window the resource
// did not exist for all of, say — and the group states each distinct one once.
// Two that name different dates are two different statements.
func TestDistinctTipCaveatsDedupesExactRepeatsOnly(t *testing.T) {
	withCaveats := func(name string, caveats ...string) resourceTip {
		return resourceTip{
			res: model.Resource{Name: name},
			rec: model.Recommendation{Caveats: caveats},
		}
	}
	got := distinctTipCaveats([]resourceTip{
		withCaveats("a", "created 2026-07-01, after this recommendation's usage period began"),
		withCaveats("b", "created 2026-07-14, after this recommendation's usage period began"),
		withCaveats("c", "created 2026-07-01, after this recommendation's usage period began"),
		withCaveats("d"),
	})
	want := []string{
		"created 2026-07-01, after this recommendation's usage period began",
		"created 2026-07-14, after this recommendation's usage period began",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("distinct caveats = %v, want %v", got, want)
	}

	// And they reach the page. A qualifier collected and not printed is worse
	// than one never derived.
	r := tipped("young", saving("10.00", "USD", "Rightsize"))
	r.Recommendations[0].Caveats = want[:1]
	var buf bytes.Buffer
	tipsSection(&buf, []model.Resource{r})
	if !strings.Contains(buf.String(), want[0]) {
		t.Errorf("caveat not reproduced verbatim:\n%s", buf.String())
	}
}

// A sum is checked by re-adding the terms, so it has to be printed at a scale
// that can hold them. Cost Optimization Hub's amounts come off a float64 and
// can run to many places; fixing the total at cents would print a figure that
// does not add up over a list that does.
func TestSumDecimalsKeepsTheWidestScaleItWasGiven(t *testing.T) {
	for _, tc := range []struct {
		name    string
		amounts []string
		want    string
		ok      bool
	}{
		{name: "cents", amounts: []string{"10.00", "0.50"}, want: "10.50", ok: true},
		{
			// Two places is the floor, not the format: whole numbers still
			// print as money.
			name: "integers pad to cents", amounts: []string{"1", "2"}, want: "3.00", ok: true,
		},
		{
			name:    "a long term widens the total",
			amounts: []string{"0.000000001", "1.00"}, want: "1.000000001", ok: true,
		},
		{
			// Exactly the case big.Rat is here for: two float64-derived
			// amounts whose binary forms do not add to the decimal answer.
			name: "no float drift", amounts: []string{"0.1", "0.2"}, want: "0.30", ok: true,
		},
		{name: "a credit", amounts: []string{"10.00", "-4.00"}, want: "6.00", ok: true},
		{name: "one unreadable term voids it", amounts: []string{"1.00", "eleven"}, ok: false},
		{
			// Not "0.00". Nothing was added, so there is no total, and a zero
			// here would be a figure this tool invented out of an empty list.
			name: "nothing to add", amounts: nil, ok: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sumDecimals(tc.amounts)
			if ok != tc.ok {
				t.Fatalf("sumDecimals(%v) ok = %v, want %v (got %q)", tc.amounts, ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("sumDecimals(%v) = %q, want %q", tc.amounts, got, tc.want)
			}
		})
	}
}

// Advice is opt-in with the rest of cost, and a census nothing suggested
// anything about must not grow the heading: "ways to cut this bill" over
// nothing reads as a verdict that there are none.
func TestTipsSectionSilentWithoutSuggestions(t *testing.T) {
	var buf bytes.Buffer
	tipsSection(&buf, []model.Resource{priced("db", "10.00", "USD", model.CostMethodCE, false)})
	if out := buf.String(); out != "" {
		t.Errorf("printed a savings section with nothing in it:\n%s", out)
	}
}

// The two sections answer different questions and the reader meets them in
// order: what you paid, then what you could stop paying. Reversed, the savings
// arrive before there is a bill to cut.
func TestTerminalPrintsSavingsAfterSpendAndNeverInsideIt(t *testing.T) {
	r := tipped("orders", saving("340.00", "USD", "Rightsize"))
	r.AddCost(model.ResourceCost{
		Amount: "602.00", Currency: "USD", Method: model.CostMethodCE, Estimated: true,
	})
	var buf bytes.Buffer
	Terminal(&buf, &model.Snapshot{Resources: []model.Resource{r}}, nil)
	out := buf.String()

	spend := strings.Index(out, "── per-resource cost ──")
	tips := strings.Index(out, "── ways to cut this bill ──")
	if spend < 0 || tips < 0 {
		t.Fatalf("one of the two sections is missing:\n%s", out)
	}
	if tips < spend {
		t.Errorf("savings printed above the spend they would reduce:\n%s", out)
	}
	// 602.00 billed and 340.00 modelled: the arithmetic a reader would do if
	// the two ever shared a heading is the arithmetic this split refuses.
	if strings.Contains(out, "262.00") {
		t.Errorf("spend and savings reconciled into a net figure:\n%s", out)
	}
}
