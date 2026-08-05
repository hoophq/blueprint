package diff

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

// Observation windows used to build comparable and incomparable figures. A
// 30-day window and a 7-day one are both legitimate answers from a cost source
// and describe different amounts of usage.
var (
	jun1 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	jun8 = time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	jul1 = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
)

// modelledMethod is a per-resource cost source this build does not have.
//
// It stands in for one, because everything in this file is method-generic: the
// figures are matched on (ARN, method), netted per method and never pooled, and
// a modelled amount is never reconciled with a billed one. Pinning that against
// the single method the census produces today would pin nothing — one bucket
// cannot be shown not to leak into another. Cost Optimization Hub used to be
// the second source and is no longer a price source at all, so the name here is
// deliberately not one AWS or this tool uses: what the tests need is a method
// the code has never heard of, which is also the case that reaches a user
// first, when a census written by a later build is diffed by this one.
const modelledMethod = "modelled"

// usd is the ordinary case in this file: a modelled monthly rate, from the
// hypothetical source above.
func usd(amount string) *model.ResourceCost {
	return &model.ResourceCost{
		Amount: amount, Currency: "USD",
		Method: modelledMethod, Estimated: true,
		ObservedFrom: &jun1, ObservedTo: &jul1,
	}
}

// costRes builds a resource whose ARN is derived from its name, so tests can
// name the resources they care about and still get stable match keys.
//
// The figure stays a pointer in the signature even though the resource carries
// a list: nil is how these tests spell "nobody priced this", and it is a case
// most of them need.
func costRes(name string, cs ...*model.ResourceCost) model.Resource {
	r := model.Resource{
		ARN:       "arn:aws:rds:us-east-1:111111111111:db:" + name,
		Name:      name,
		Service:   model.ServiceRDS,
		Type:      model.TypeRDSInstance,
		Region:    "us-east-1",
		AccountID: "111111111111",
	}
	for _, c := range cs {
		if c != nil {
			r.AddCost(*c)
		}
	}
	return r
}

// ce is the figure the census actually produces: what Cost Explorer billed over
// a closed window, as opposed to what a model expects a month to cost. Tests
// use it to put two figures on one resource, which is the case every consumer
// of this package has to get right.
func ce(amount string) *model.ResourceCost {
	return &model.ResourceCost{
		Amount: amount, Currency: "USD",
		Method: model.CostMethodCE, Estimated: false,
		ObservedFrom: &jun1, ObservedTo: &jul1,
	}
}

func costCensus(rs ...model.Resource) *model.Snapshot {
	s := &model.Snapshot{
		Schema:    model.SchemaVersion,
		Accounts:  []string{"111111111111"},
		Regions:   []string{"us-east-1"},
		Resources: rs,
	}
	s.Finalize()
	return s
}

// What AWS says about whether a billing window has stopped being restated.
// Nil — nobody said — is a third state, and it is not "settled".
var (
	settled     = new(false)
	provisional = new(true)
)

// drift compares two one-resource censuses of the same resource.
func drift(name string, old, current *model.ResourceCost) CostDrift {
	return costDrift(costCensus(costRes(name, old)), costCensus(costRes(name, current)))
}

// This is why the file exists. Cost is excluded from field drift, so a resource
// whose only movement is spend lands in none of Added, Removed, or Changed —
// and the headline case, a cleanup that lowered the bill on everything left
// standing, would be invisible if the cost pass read the Changed list instead
// of walking every matched resource.
func TestCostDriftSeesResourcesTheFieldDiffDoesNot(t *testing.T) {
	old := costCensus(costRes("orders", usd("412.50")))
	current := costCensus(costRes("orders", usd("120.00")))

	got := Compare(old, current)
	if !got.Empty() {
		t.Fatalf("spend moved but the estate did not; the resource diff should be empty: %+v", got)
	}
	if got.Cost.Empty() {
		t.Fatal("the resource is in no diff bucket, so if the cost pass misses it the drop is invisible")
	}
	if len(got.Cost.Moved) != 1 {
		t.Fatalf("Moved = %+v, want the one resource whose price dropped", got.Cost.Moved)
	}
	if d := got.Cost.Moved[0].Delta; d != "-292.50" {
		t.Errorf("Delta = %q, want %q", d, "-292.50")
	}
}

// Both thresholds are required. Each of these is a real move; only the ones
// that are material in both senses are worth a reader's attention.
func TestCostDriftListsOnlyMaterialMoves(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to string
		want     bool
	}{
		{"clears both", "100.00", "141.00", true},
		{"large bill, small move", "10000.00", "10000.50", false},
		{"small bill, large percentage", "0.50", "0.60", false},
		{"exactly at both thresholds", "20.00", "21.00", true},
		{"unchanged", "100.00", "100.00", false},
		// Width is not movement: the same money written at two precisions is
		// the same money.
		{"same money, wider", "100.0", "100.000", false},
		{"switched off", "20.00", "0.00", true},
		{"switched on", "0.00", "20.00", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := drift("orders", usd(tc.from), usd(tc.to))
			if got := len(d.Moved) == 1; got != tc.want {
				t.Errorf("listed = %v, want %v (Moved = %+v)", got, tc.want, d.Moved)
			}
		})
	}
}

// The thresholds keep the list readable; they do not change the arithmetic. A
// thousand sub-dollar moves are still real money, and a net that dropped them
// would understate the bill by exactly the amount nobody was watching.
func TestCostDriftNetsMovesTooSmallToList(t *testing.T) {
	var before, after []model.Resource
	for _, n := range []string{"a", "b", "c", "d"} {
		before = append(before, costRes(n, usd("100.00")))
		after = append(after, costRes(n, usd("100.50")))
	}
	d := costDrift(costCensus(before...), costCensus(after...))

	if len(d.Moved) != 0 {
		t.Errorf("Moved = %+v, want nothing listed: every move is below both thresholds", d.Moved)
	}
	if len(d.Net) != 1 {
		t.Fatalf("Net = %+v, want one currency", d.Net)
	}
	if got := d.Net[0].Changed; got != "2.00" {
		t.Errorf("Changed = %q, want %q — four unlisted 0.50 moves are still two dollars", got, "2.00")
	}
	if got := d.Net[0].Net; got != "2.00" {
		t.Errorf("Net = %q, want %q", got, "2.00")
	}
	if got := d.Priced; got != 4 {
		t.Errorf("Priced = %d, want 4", got)
	}
}

// Gaining sight of a price is not gaining spend. Counting it in the net would
// report the tool's own improvement — a new scanner, a widened IAM policy — as
// the estate getting more expensive.
func TestCostDriftCoverageIsNotSpend(t *testing.T) {
	t.Run("gained", func(t *testing.T) {
		d := drift("orders", nil, usd("412.50"))
		if len(d.Net) != 0 {
			t.Errorf("Net = %+v, want nothing: a figure appearing is not spending", d.Net)
		}
		if len(d.Moved) != 0 {
			t.Errorf("Moved = %+v, want nothing", d.Moved)
		}
		if len(d.Coverage) != 1 {
			t.Fatalf("Coverage = %+v, want one entry", d.Coverage)
		}
		c := d.Coverage[0]
		if !c.Gained || c.Amount != "412.50" || c.Currency != "USD" {
			t.Errorf("Coverage = %+v, want a gain of 412.50 USD", c)
		}
	})

	t.Run("lost, with the source's own reason", func(t *testing.T) {
		const reason = "the cost pass looked and AWS returned nothing for this resource"
		gone := costRes("orders", nil)
		gone.CostUnavailable = reason
		d := costDrift(costCensus(costRes("orders", usd("412.50"))), costCensus(gone))

		if len(d.Net) != 0 {
			t.Errorf("Net = %+v, want nothing: losing sight of a price is not a saving", d.Net)
		}
		if len(d.Coverage) != 1 {
			t.Fatalf("Coverage = %+v, want one entry", d.Coverage)
		}
		c := d.Coverage[0]
		if c.Gained {
			t.Error("a lost figure was reported as gained")
		}
		if c.Amount != "412.50" || c.Reason != reason {
			t.Errorf("Coverage = %+v, want the last known figure and the source's reason", c)
		}
	})

	// Nothing looked is a different statement from looking and finding
	// nothing, and the renderer only qualifies the line when a source spoke.
	t.Run("lost, with nobody having said why", func(t *testing.T) {
		d := drift("orders", usd("412.50"), nil)
		if len(d.Coverage) != 1 {
			t.Fatalf("Coverage = %+v, want one entry", d.Coverage)
		}
		if got := d.Coverage[0].Reason; got != "" {
			t.Errorf("Reason = %q, want empty: no source explained the absence", got)
		}
	})
}

// Resources that appeared or went away carry their whole figure into the net,
// which is the one place spend genuinely started or stopped.
func TestCostDriftAddedAndRemoved(t *testing.T) {
	old := costCensus(costRes("retired", usd("300.00")))
	current := costCensus(costRes("fresh", usd("50.00")))
	d := costDrift(old, current)

	if len(d.Net) != 1 {
		t.Fatalf("Net = %+v, want one currency", d.Net)
	}
	n := d.Net[0]
	if n.Added != "50.00" || n.Removed != "300.00" || n.Changed != "0.00" {
		t.Errorf("Net = %+v, want 50.00 added and 300.00 removed", n)
	}
	if n.Net != "-250.00" {
		t.Errorf("Net = %q, want %q", n.Net, "-250.00")
	}
	if n.Method != modelledMethod {
		t.Errorf("Method = %q, want the attribution method disclosed", n.Method)
	}
	if d.Priced != 2 {
		t.Errorf("Priced = %d, want 2", d.Priced)
	}
}

// Every one of these is two figures answering different questions. Subtracting
// them would produce a number that looks like drift and means nothing, so each
// is reported as the basis change it is and contributes nothing to the net.
func TestCostDriftRefusesIncomparableFigures(t *testing.T) {
	eur := usd("100.00")
	eur.Currency = "EUR"
	actual := usd("100.00")
	actual.Estimated = false
	shortWindow := usd("100.00")
	shortWindow.ObservedTo = &jun8
	noWindow := usd("100.00")
	noWindow.ObservedFrom, noWindow.ObservedTo = nil, nil
	unreadable := usd("n/a")

	for _, tc := range []struct {
		name       string
		to         *model.ResourceCost
		wantReason string
		wantNew    string
	}{
		{"currency", eur, "currency changed (USD → EUR)", "100.00 EUR"},
		{"basis", actual, "basis changed (modelled → billed)", "100.00 USD"},
		{"window length", shortWindow, "observation window changed (30d → 7d)", "100.00 USD"},
		{"window vanished", noWindow, "observation window changed (30d → not reported)", "100.00 USD"},
		{"amount is not a number", unreadable, "amount is not a decimal number", "n/a USD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := drift("orders", usd("100.00"), tc.to)
			if len(d.Basis) != 1 {
				t.Fatalf("Basis = %+v, want the pair reported as not comparable", d.Basis)
			}
			b := d.Basis[0]
			if b.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", b.Reason, tc.wantReason)
			}
			if b.Old != "100.00 USD" || b.New != tc.wantNew {
				t.Errorf("Old/New = %q/%q, want %q/%q", b.Old, b.New, "100.00 USD", tc.wantNew)
			}
			if len(d.Moved) != 0 {
				t.Errorf("Moved = %+v, want nothing: these two cannot be subtracted", d.Moved)
			}
			if len(d.Net) != 0 {
				t.Errorf("Net = %+v, want nothing: an incomparable pair must not reach the arithmetic", d.Net)
			}
			// Priced counts coverage, not participation in the arithmetic. The
			// resource carries a figure on both sides; that the two cannot be
			// subtracted is what Basis says, and saying it twice by also calling
			// the resource unpriced would understate coverage.
			if d.Priced != 1 || d.Unpriced != 0 {
				t.Errorf("Priced/Unpriced = %d/%d, want 1/0: the resource is priced, the pair is not comparable", d.Priced, d.Unpriced)
			}
		})
	}
}

// A resource that changed attribution method is not a resource whose price
// moved. Figures are matched on (ARN, method), so the old method loses coverage
// and the new one gains it — two honest statements about two different
// questions, rather than one subtraction spanning both.
func TestCostDriftReportsAMethodChangeAsCoverage(t *testing.T) {
	d := drift("orders", usd("100.00"), ce("100.00"))

	if len(d.Basis) != 0 {
		t.Errorf("Basis = %+v, want nothing: the two figures were never a pair to refuse", d.Basis)
	}
	if len(d.Moved) != 0 {
		t.Errorf("Moved = %+v, want nothing: no method's figure moved", d.Moved)
	}
	if len(d.Coverage) != 2 {
		t.Fatalf("Coverage = %+v, want the modelled figure lost and the billed one gained", d.Coverage)
	}
	// Sorted by ARN then method, and both share an ARN, so "ce" comes first.
	if got := d.Coverage[0]; got.Method != model.CostMethodCE || !got.Gained {
		t.Errorf("Coverage[0] = %+v, want ce gained", got)
	}
	if got := d.Coverage[1]; got.Method != modelledMethod || got.Gained {
		t.Errorf("Coverage[1] = %+v, want the modelled figure lost", got)
	}
	if len(d.Net) != 0 {
		t.Errorf("Net = %+v, want nothing: coverage moving between methods is not spend moving", d.Net)
	}
}

// The case the multi-figure model exists for: one resource priced by both
// sources, each moving independently. Neither total may absorb the other.
func TestCostDriftNetsEachMethodSeparately(t *testing.T) {
	old := costCensus(costRes("orders", usd("100.00"), ce("40.00")))
	current := costCensus(costRes("orders", usd("150.00"), ce("30.00")))
	d := costDrift(old, current)

	if len(d.Net) != 2 {
		t.Fatalf("Net = %+v, want one entry per method", d.Net)
	}
	// nets sorts by method, then currency.
	if n := d.Net[0]; n.Method != model.CostMethodCE || n.Net != "-10.00" {
		t.Errorf("Net[0] = %+v, want ce at -10.00", n)
	}
	if n := d.Net[1]; n.Method != modelledMethod || n.Net != "50.00" {
		t.Errorf("Net[1] = %+v, want the modelled source at 50.00", n)
	}
	// One resource, two figures. The net counts figures; this counts resources.
	if d.Priced != 1 {
		t.Errorf("Priced = %d, want 1: two figures on one resource is one resource", d.Priced)
	}
}

// Equally unknown on both sides is not a change. Refusing here would silence
// the entire section for any cost source that does not report a window.
func TestCostDriftComparesWhenNeitherSideReportsAWindow(t *testing.T) {
	from, to := usd("100.00"), usd("141.00")
	from.ObservedFrom, from.ObservedTo = nil, nil
	to.ObservedFrom, to.ObservedTo = nil, nil

	d := drift("orders", from, to)
	if len(d.Basis) != 0 {
		t.Fatalf("Basis = %+v, want nothing refused", d.Basis)
	}
	if len(d.Moved) != 1 || d.Moved[0].Delta != "41.00" {
		t.Errorf("Moved = %+v, want the 41.00 move", d.Moved)
	}
}

// An amount with no currency is not dollars. Pooling it with USD would produce
// a total in a currency nobody reported.
func TestCostDriftKeepsCurrenciesApart(t *testing.T) {
	unlabelled := usd("10.00")
	unlabelled.Currency = ""
	unlabelledUp := usd("30.00")
	unlabelledUp.Currency = ""

	d := costDrift(
		costCensus(costRes("a", usd("100.00")), costRes("b", unlabelled)),
		costCensus(costRes("a", usd("141.00")), costRes("b", unlabelledUp)),
	)
	if len(d.Net) != 2 {
		t.Fatalf("Net = %+v, want one entry per currency", d.Net)
	}
	// The unnamed currency sorts first, which also pins the ordering.
	if d.Net[0].Currency != "" || d.Net[0].Net != "20.00" {
		t.Errorf("Net[0] = %+v, want the unnamed currency at 20.00", d.Net[0])
	}
	if d.Net[1].Currency != "USD" || d.Net[1].Net != "41.00" {
		t.Errorf("Net[1] = %+v, want USD at 41.00", d.Net[1])
	}
}

// Every list is sorted by ARN and every net by currency, because a census that
// reorders itself between runs cannot be diffed by eye or by a pipeline.
func TestCostDriftIsDeterministic(t *testing.T) {
	eur := usd("100.00")
	eur.Currency = "EUR"

	old := costCensus(
		costRes("zulu", usd("100.00")),
		costRes("yankee", usd("100.00")),
		costRes("xray", usd("100.00")),
		costRes("whiskey", usd("100.00")),
	)
	current := costCensus(
		costRes("zulu", usd("400.00")),
		costRes("yankee", nil),
		costRes("xray", eur),
		costRes("whiskey", usd("900.00")),
	)
	d := costDrift(old, current)

	var listed []string
	for _, c := range d.Moved {
		listed = append(listed, c.Resource.ARN)
	}
	assertSorted(t, "Moved", listed)

	listed = nil
	for _, c := range d.Coverage {
		listed = append(listed, c.Resource.ARN)
	}
	assertSorted(t, "Coverage", listed)

	listed = nil
	for _, c := range d.Basis {
		listed = append(listed, c.Resource.ARN)
	}
	assertSorted(t, "Basis", listed)

	if len(d.Moved) != 2 || d.Moved[0].Resource.Name != "whiskey" {
		t.Errorf("Moved = %+v, want whiskey before zulu", d.Moved)
	}
	if len(d.Net) != 1 || d.Net[0].Currency != "USD" {
		t.Errorf("Net = %+v, want only the currency that actually moved", d.Net)
	}
}

func assertSorted(t *testing.T, name string, arns []string) {
	t.Helper()
	for i := 1; i < len(arns); i++ {
		if arns[i-1] > arns[i] {
			t.Errorf("%s is not ARN-sorted: %v", name, arns)
			return
		}
	}
}

// A net that covers a tenth of the estate reads exactly like a net that covers
// all of it unless the uncounted resources are counted out loud.
func TestCostDriftCountsWhatItCouldNotPrice(t *testing.T) {
	d := costDrift(
		costCensus(costRes("priced", usd("100.00")), costRes("dark", nil), costRes("gone", nil)),
		costCensus(costRes("priced", usd("141.00")), costRes("dark", nil), costRes("new", nil)),
	)
	if d.Priced != 1 {
		t.Errorf("Priced = %d, want 1", d.Priced)
	}
	// Three resources carry no figure on either side: the one present in both,
	// the one that went away, and the one that appeared.
	if d.Unpriced != 3 {
		t.Errorf("Unpriced = %d, want 3", d.Unpriced)
	}
	if !hasNote(d, "carry no cost figure at all") {
		t.Errorf("Notes = %v, want the coverage of the net disclosed", d.Notes)
	}
}

func hasNote(d CostDrift, substr string) bool {
	for _, n := range d.Notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// The reader has to know which kind of number they are looking at, and a
// modelled monthly rate is not an amount AWS has billed.
func TestCostDriftDisclosesModelledFigures(t *testing.T) {
	t.Run("all modelled", func(t *testing.T) {
		d := drift("orders", usd("100.00"), usd("141.00"))
		if !hasNote(d, "modelled monthly rates for the current configuration") {
			t.Errorf("Notes = %v, want the modelled disclosure", d.Notes)
		}
	})

	t.Run("modelled and billed together", func(t *testing.T) {
		actual := usd("50.00")
		actual.Estimated = false
		d := costDrift(costCensus(), costCensus(costRes("a", usd("100.00")), costRes("b", actual)))
		if !hasNote(d, "answer different questions") {
			t.Errorf("Notes = %v, want the mixed-basis disclosure", d.Notes)
		}
	})

	// A coverage line prints an amount, so it needs the same disclosure as any
	// other printed amount — this is the path that had none.
	t.Run("coverage alone still discloses", func(t *testing.T) {
		d := drift("orders", nil, usd("412.50"))
		if !hasNote(d, "modelled monthly rates for the current configuration") {
			t.Errorf("Notes = %v, want a printed modelled amount to say so", d.Notes)
		}
	})

	t.Run("all billed", func(t *testing.T) {
		from, to := usd("100.00"), usd("141.00")
		from.Estimated, to.Estimated = false, false
		d := drift("orders", from, to)
		if hasNote(d, "modelled") {
			t.Errorf("Notes = %v, want no modelled disclosure on billed figures", d.Notes)
		}
	})
}

// A commitment discount floats across an account, so churn is a real
// alternative explanation for a move on a resource nobody touched. It is
// disclosed rather than used to suppress the move.
func TestCostDriftDisclosesCommitmentFloat(t *testing.T) {
	const note = "float across an account"

	t.Run("churn and a move", func(t *testing.T) {
		d := costDrift(
			costCensus(costRes("stays", usd("100.00"))),
			costCensus(costRes("stays", usd("141.00")), costRes("arrives", usd("60.00"))),
		)
		if !hasNote(d, note) {
			t.Errorf("Notes = %v, want the commitment-float disclosure", d.Notes)
		}
	})

	// With nothing added or removed there is no float to explain the move,
	// and a note that always fires is a note nobody reads.
	t.Run("a move with no churn", func(t *testing.T) {
		d := drift("orders", usd("100.00"), usd("141.00"))
		if hasNote(d, note) {
			t.Errorf("Notes = %v, want no commitment-float disclosure without churn", d.Notes)
		}
	})

	t.Run("churn with nothing moved", func(t *testing.T) {
		d := costDrift(costCensus(), costCensus(costRes("arrives", usd("60.00"))))
		if hasNote(d, note) {
			t.Errorf("Notes = %v, want no commitment-float disclosure when nothing moved", d.Notes)
		}
	})
}

// A steady estate prints nothing rather than a page of zeroes.
func TestCostDriftEmptyWhenNothingMoved(t *testing.T) {
	d := drift("orders", usd("100.00"), usd("100.00"))
	if !d.Empty() {
		t.Errorf("CostDrift = %+v, want empty when the same figure is reported twice", d)
	}
	var buf bytes.Buffer
	d.WriteCost(&buf, "yesterday")
	if buf.Len() != 0 {
		t.Errorf("WriteCost wrote %q, want silence", buf.String())
	}
}

func report(label, metric string, estimated *bool, totals ...model.CostByCurrency) *model.CostReport {
	return &model.CostReport{
		Window:     model.CostWindow{Start: "2026-06-01", End: "2026-07-01", Label: label},
		Metric:     metric,
		Accounts:   []string{"111111111111"},
		Currencies: totals,
		Estimated:  estimated,
	}
}

func total(currency, amount string) model.CostByCurrency {
	return model.CostByCurrency{Currency: currency, Total: amount, Attributed: amount, Unattributed: "0.00"}
}

// The rollup is a bill for a closed window, and AWS restates it for weeks.
// Reporting a restatement as spend movement would be the clearest possible
// version of the lie this package exists to avoid, so the comparison refuses
// far more often than it subtracts — and says why every time.
func TestBilledChangeRefusesRatherThanGuess(t *testing.T) {
	final := report("2026-06", "AmortizedCost", settled, total("USD", "1000.00"))

	for _, tc := range []struct {
		name          string
		old, current  *model.CostReport
		wantRefusal   string
		wantNoSection bool
	}{
		{name: "neither census collected cost", wantNoSection: true},
		{
			name: "no baseline", current: final,
			wantRefusal: "the baseline census collected no cost data",
		},
		{
			name: "no current", old: final,
			wantRefusal: "this census collected no cost data",
		},
		{
			name: "different window",
			old:  final, current: report("2026-07", "AmortizedCost", settled, total("USD", "1000.00")),
			wantRefusal: "billing window changed (2026-06 → 2026-07)",
		},
		{
			name: "different metric",
			old:  final, current: report("2026-06", "UnblendedCost", settled, total("USD", "1000.00")),
			wantRefusal: "cost metric changed (AmortizedCost → UnblendedCost)",
		},
		{
			name: "AWS is still restating this bill",
			old:  final, current: report("2026-06", "AmortizedCost", provisional, total("USD", "2000.00")),
			wantRefusal: "AWS has not finalized the 2026-06 bill",
		},
		{
			name: "the baseline was an estimate",
			old:  report("2026-06", "AmortizedCost", provisional, total("USD", "900.00")), current: final,
			wantRefusal: "AWS has not finalized the 2026-06 bill",
		},
		{
			// A missing flag is not a claim that the bill has settled.
			name: "nobody said whether it settled",
			old:  final, current: report("2026-06", "AmortizedCost", nil, total("USD", "2000.00")),
			wantRefusal: "AWS has not finalized the 2026-06 bill",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := billedChange(tc.old, tc.current)
			if tc.wantNoSection {
				if got != nil {
					t.Fatalf("billedChange = %+v, want nil: there is nothing to say either way", got)
				}
				return
			}
			if got == nil {
				t.Fatal("billedChange = nil, want a stated refusal")
			}
			if !strings.Contains(got.Reason, tc.wantRefusal) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tc.wantRefusal)
			}
			if len(got.Totals) != 0 {
				t.Errorf("Totals = %+v, want none: the refusal is the finding", got.Totals)
			}
		})
	}
}

func TestBilledChangeSubtractsSettledBills(t *testing.T) {
	old := report("2026-06", "AmortizedCost", settled, total("USD", "1000.00"))

	t.Run("a material move", func(t *testing.T) {
		got := billedChange(old, report("2026-06", "AmortizedCost", settled, total("USD", "1410.00")))
		if got == nil || got.Reason != "" {
			t.Fatalf("billedChange = %+v, want a comparison", got)
		}
		if got.Window != "2026-06" || got.Metric != "AmortizedCost" {
			t.Errorf("compared %q/%q, want the window and metric named", got.Window, got.Metric)
		}
		if len(got.Totals) != 1 {
			t.Fatalf("Totals = %+v, want one currency", got.Totals)
		}
		if tt := got.Totals[0]; tt.Delta != "410.00" || tt.Percent != "41.0" {
			t.Errorf("Totals[0] = %+v, want a 410.00 / 41.0%% move", tt)
		}
	})

	t.Run("billing jitter", func(t *testing.T) {
		if got := billedChange(old, report("2026-06", "AmortizedCost", settled, total("USD", "1000.40"))); got != nil {
			t.Errorf("billedChange = %+v, want nil: forty cents on a thousand dollars is not news", got)
		}
	})

	// What a bill is denominated in changing is always worth a line, and there
	// is no prior figure to subtract from.
	t.Run("a currency appears", func(t *testing.T) {
		got := billedChange(old, report("2026-06", "AmortizedCost", settled,
			total("USD", "1000.00"), total("EUR", "60.00")))
		if got == nil || len(got.Totals) != 1 {
			t.Fatalf("Totals = %+v, want the new currency listed", got)
		}
		tt := got.Totals[0]
		if tt.Currency != "EUR" || tt.Old != "" || tt.New != "60.00" || tt.Delta != "" {
			t.Errorf("Totals[0] = %+v, want EUR appearing with no delta", tt)
		}
	})

	t.Run("a currency disappears", func(t *testing.T) {
		got := billedChange(
			report("2026-06", "AmortizedCost", settled, total("USD", "1000.00"), total("EUR", "60.00")),
			old)
		if got == nil || len(got.Totals) != 1 {
			t.Fatalf("Totals = %+v, want the lost currency listed", got)
		}
		if tt := got.Totals[0]; tt.Currency != "EUR" || tt.Old != "60.00" || tt.New != "" {
			t.Errorf("Totals[0] = %+v, want EUR disappearing", tt)
		}
	})

	t.Run("a total that is not a number", func(t *testing.T) {
		got := billedChange(old, report("2026-06", "AmortizedCost", settled, total("USD", "unavailable")))
		if got == nil || len(got.Totals) != 1 {
			t.Fatalf("Totals = %+v, want the unreadable pair shown rather than dropped", got)
		}
		if tt := got.Totals[0]; tt.Delta != "" || tt.New != "unavailable" {
			t.Errorf("Totals[0] = %+v, want both figures and no delta", tt)
		}
	})
}

// A zero is a reading, and it has to survive all the way to the terminal: a
// resource that stopped costing anything is exactly the finding a cost diff
// exists to surface.
func TestWriteCostKeepsZeroAndOmitsImpossiblePercentages(t *testing.T) {
	var buf bytes.Buffer
	costDrift(
		costCensus(costRes("switched-off", usd("20.00")), costRes("switched-on", usd("0.00"))),
		costCensus(costRes("switched-off", usd("0.00")), costRes("switched-on", usd("20.00"))),
	).WriteCost(&buf, "last week")
	out := buf.String()

	for _, want := range []string{
		"━━ spend vs last week ━━",
		"20.00 → 0.00", // the zero survives rendering
		"0.00 → 20.00", // and so does the one it started from
		"-100.0%",      // a drop to nothing has a percentage
		"net 0.00 USD", // the two moves cancel, and a zero carries no sign
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteCost output is missing %q:\n%s", want, out)
		}
	}
	// There is no percentage of nothing, and inventing one would be worse than
	// showing the amounts.
	if strings.Contains(out, "+∞") || strings.Contains(out, ", +%") {
		t.Errorf("WriteCost invented a percentage for a move away from zero:\n%s", out)
	}
}

func TestWriteCostRendersEverySection(t *testing.T) {
	eur := usd("100.00")
	eur.Currency = "EUR"
	dark := costRes("unpriced", nil)
	dark.CostUnavailable = "the cost pass looked and AWS returned nothing"

	var buf bytes.Buffer
	costDrift(
		costCensus(costRes("moves", usd("100.00")), costRes("unpriced", usd("70.00")), costRes("rebased", usd("100.00"))),
		costCensus(costRes("moves", usd("141.00")), dark, costRes("rebased", eur)),
	).WriteCost(&buf, "yesterday")
	out := buf.String()

	for _, want := range []string{
		"~ moves (rds, us-east-1): 100.00 → 141.00  (+41.00, +41.0%)",
		"· unpriced (rds, us-east-1): no longer priced, was 70.00 USD (the cost pass looked and AWS returned nothing)",
		"! rebased (rds, us-east-1): not compared — currency changed (USD → EUR) (100.00 USD → 100.00 EUR)",
		"[modelled]",
		"note: these are modelled monthly rates",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteCost output is missing:\n  %s\ngot:\n%s", want, out)
		}
	}
}

// Spend must never reach the exit code. --fail-on-change is gated on Empty,
// and AWS restating a bill is not the estate changing.
func TestSpendMovementNeverFailsOnChange(t *testing.T) {
	got := Compare(
		costCensus(costRes("orders", usd("100.00"))),
		costCensus(costRes("orders", usd("100000.00"))),
	)
	if !got.Empty() {
		t.Error("a spend move made the estate diff non-empty; --fail-on-change would fire on AWS's billing pipeline")
	}
	if len(got.Cost.Moved) != 1 {
		t.Errorf("Cost.Moved = %+v, want the move still reported in its own section", got.Cost.Moved)
	}
}

// A total of zero is two different situations, and the reader needs them told
// apart: nothing moved, or a lot moved and it cancelled. The second is the
// interesting estate — a resize paid for by a deletion — and dropping the
// currency because its net came to zero would print the two movement lines and
// then refuse to say they offset.
func TestCostDriftNetsMovesThatCancel(t *testing.T) {
	d := costDrift(
		costCensus(
			costRes("orders", usd("100.00")),
			costRes("ledger", usd("600.00")),
		),
		costCensus(
			costRes("orders", usd("600.00")),
			costRes("ledger", usd("100.00")),
		),
	)
	if len(d.Moved) != 2 {
		t.Fatalf("Moved = %+v, want both material moves listed", d.Moved)
	}
	if len(d.Net) != 1 {
		t.Fatalf("Net = %+v, want the currency reported even though it nets to nothing", d.Net)
	}
	if n := d.Net[0]; n.Changed != "0.00" || n.Net != "0.00" {
		t.Errorf("Net[0] changed=%q net=%q, want both 0.00 — the moves offset exactly", n.Changed, n.Net)
	}
	if d.Empty() {
		t.Error("CostDrift reports empty; two 500-dollar moves happened")
	}
	var buf bytes.Buffer
	d.WriteCost(&buf, "yesterday")
	if out := buf.String(); !strings.Contains(out, "net 0.00 USD") {
		t.Errorf("WriteCost never says the moves cancelled:\n%s", out)
	}
}

// The other half of that distinction: an estate whose figures did not move
// prints no net at all, rather than a row of zeroes per currency. Width is not
// movement, so a source that widened its formatting stays silent too.
func TestCostDriftNetsNothingWhenNoFigureMoved(t *testing.T) {
	oldEUR, newEUR := usd("600.00"), usd("600.0")
	oldEUR.Currency, newEUR.Currency = "EUR", "EUR"
	d := costDrift(
		costCensus(
			costRes("orders", usd("100.00")),
			costRes("ledger", oldEUR),
		),
		costCensus(
			costRes("orders", usd("100.000")),
			costRes("ledger", newEUR),
		),
	)
	if len(d.Net) != 0 {
		t.Errorf("Net = %+v, want nothing: the same money written at another width has not moved", d.Net)
	}
	if !d.Empty() {
		t.Errorf("CostDrift = %+v, want empty", d)
	}
}

// The account-level rollup runs the same dual threshold as a per-resource move,
// and for the same reason: AWS reprices a closed month for weeks, and either
// test on its own prints that jitter — the percentage alone on a small account,
// the absolute alone on a large one.
func TestBilledChangeNeedsBothThresholds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		before, then string
		want         bool
	}{
		{"clears both", "1000.00", "1200.00", true},
		// Ten cents on a one-dollar bill is 10%, and it is ten cents.
		{"large percentage, small money", "1.00", "1.10", false},
		// Two dollars on a ten-thousand-dollar bill is money and clears the
		// absolute bound; at 0.02% the relative test is the one holding it back.
		{"large money, small percentage", "10000.00", "10002.00", false},
		// Fifty cents on the same bill clears neither.
		{"small on both counts", "10000.00", "10000.50", false},
		// Exactly one unit and exactly five percent both qualify.
		{"exactly on both bounds", "20.00", "21.00", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := billedChange(
				report("2026-06", "AmortizedCost", settled, total("USD", tc.before)),
				report("2026-06", "AmortizedCost", settled, total("USD", tc.then)),
			)
			if !tc.want {
				if got != nil {
					t.Fatalf("billedChange = %+v, want nothing: the move clears only one threshold", got)
				}
				return
			}
			if got == nil || len(got.Totals) != 1 {
				t.Fatalf("billedChange = %+v, want the move reported", got)
			}
			if tt := got.Totals[0]; tt.Old != tc.before || tt.New != tc.then {
				t.Errorf("Totals[0] = %+v, want %s → %s", tt, tc.before, tc.then)
			}
		})
	}
}

// A resource that appeared, vanished, or sat still at no cost is not spend
// movement. The zero itself is not dropped — it is in the census artifact, and
// the resource is in the estate section above — but this section reports money
// moving, and none did.
func TestCostDriftZeroFiguresAreNotMovement(t *testing.T) {
	d := costDrift(
		costCensus(
			costRes("archived", usd("0.00")),
			costRes("gone", usd("0.00")),
		),
		costCensus(
			costRes("archived", usd("0.00")),
			costRes("scratch", usd("0.00")),
		),
	)
	if len(d.Net) != 0 {
		t.Errorf("Net = %+v, want nothing: nothing that fed it was money", d.Net)
	}
	if !d.Empty() {
		t.Errorf("CostDrift = %+v, want empty", d)
	}
	if d.Priced != 3 {
		t.Errorf("Priced = %d, want 3 — a zero figure is still a figure, and still counted", d.Priced)
	}

	// The same zeroes alongside a real move do not suppress the currency, and
	// they do not disturb its arithmetic either.
	d = costDrift(
		costCensus(
			costRes("orders", usd("100.00")),
			costRes("gone", usd("0.00")),
		),
		costCensus(
			costRes("orders", usd("400.00")),
			costRes("scratch", usd("0.00")),
		),
	)
	if len(d.Net) != 1 {
		t.Fatalf("Net = %+v, want the currency reported: one resource moved 300 dollars", d.Net)
	}
	if n := d.Net[0]; n.Added != "0.00" || n.Removed != "0.00" || n.Changed != "300.00" || n.Net != "300.00" {
		t.Errorf("Net[0] added=%q removed=%q changed=%q net=%q, want 0.00/0.00/300.00/300.00",
			n.Added, n.Removed, n.Changed, n.Net)
	}
}

// The spend section is ordered by ARN, matching the estate section above it,
// and not by the order the census handed the resources over in.
//
// The two orders come apart whenever a display name and the identifier inside
// the ARN disagree, which is routine: Name comes from a mutable tag for several
// of the types the census covers, while the snapshot sorts by name. So the sort
// here is doing work rather than restating its input, and this test unties the
// two deliberately to prove it.
func TestCostDriftOrdersByARNNotByName(t *testing.T) {
	renamed := func(name string, r model.Resource) model.Resource {
		r.Name = name
		return r
	}
	eur := usd("100.00")
	eur.Currency = "EUR"
	before := costCensus(
		renamed("aaa", costRes("zzz-moved", usd("100.00"))),
		renamed("zzz", costRes("aaa-moved", usd("100.00"))),
		renamed("bbb", costRes("yyy-coverage", nil)),
		renamed("yyy", costRes("bbb-coverage", nil)),
		renamed("ccc", costRes("xxx-basis", usd("100.00"))),
		renamed("xxx", costRes("ccc-basis", usd("100.00"))),
	)
	then := costCensus(
		renamed("aaa", costRes("zzz-moved", usd("400.00"))),
		renamed("zzz", costRes("aaa-moved", usd("400.00"))),
		renamed("bbb", costRes("yyy-coverage", usd("50.00"))),
		renamed("yyy", costRes("bbb-coverage", usd("50.00"))),
		renamed("ccc", costRes("xxx-basis", eur)),
		renamed("xxx", costRes("ccc-basis", eur)),
	)
	// If the census ever starts handing these over in ARN order the test stops
	// exercising anything, so it says so rather than passing quietly.
	ordered := true
	for i := 1; i < len(then.Resources); i++ {
		if then.Resources[i-1].ARN > then.Resources[i].ARN {
			ordered = false
			break
		}
	}
	if ordered {
		t.Fatal("the census already hands these over ARN-sorted; this test would pass without the sort")
	}

	d := costDrift(before, then)
	if len(d.Moved) != 2 || len(d.Coverage) != 2 || len(d.Basis) != 2 {
		t.Fatalf("Moved=%d Coverage=%d Basis=%d, want two of each",
			len(d.Moved), len(d.Coverage), len(d.Basis))
	}
	var moved, coverage, basis []string
	for _, c := range d.Moved {
		moved = append(moved, c.Resource.ARN)
	}
	for _, c := range d.Coverage {
		coverage = append(coverage, c.Resource.ARN)
	}
	for _, c := range d.Basis {
		basis = append(basis, c.Resource.ARN)
	}
	assertSorted(t, "Moved", moved)
	assertSorted(t, "Coverage", coverage)
	assertSorted(t, "Basis", basis)
}
