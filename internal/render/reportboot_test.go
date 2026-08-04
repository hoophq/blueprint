package render

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoophq/blueprint/internal/demo"
	"github.com/hoophq/blueprint/internal/model"
)

// Every other JS test in this package lifts one pure function out of the
// template and hands it to node. That seam is deliberate and it has a hole in
// it exactly the shape of the page: nothing ever runs the template's own
// top-level code. A report whose script dies with a ReferenceError on line one
// — no table, no filters, no cost overlay, just a header and a blank page —
// passes all 100-odd of them, because each one evaluates its function in a
// scratch scope the dead statement never reaches.
//
// That is not hypothetical. Merging the cost overlay onto the virtualized
// report renamed the payload variable, moved the census behind an async
// decode, and renamed the repaint entry point; three of those breaks were
// module-scope and the suite stayed green through all of them.
//
// So this test boots the artifact. It renders a real report, pulls the script
// and the data blocks back out of the HTML, gives node enough of a DOM to
// satisfy the forty element lookups the page makes, and runs it — then clicks
// the controls that only exist after boot. It asserts very little about
// appearance on purpose: the question it answers is "does the page run", which
// is the one question the rest of the suite cannot ask.

// bootProbe is what the harness reports back after the page has booted and
// been driven. Anything nil-able is a pointer so an element the page never
// touched is distinguishable from one it set to the zero value.
type bootProbe struct {
	// MissingIDs lists ids the page looked up that the rendered HTML does not
	// contain. A browser answers those with null, so they are not necessarily
	// fatal — but every one of them is markup and script disagreeing.
	MissingIDs []string `json:"missingIds"`
	// Painted is the number of rows in the table body after boot.
	Painted int `json:"painted"`
	// Headers is the header row's cell text, cost column included.
	Headers []string `json:"headers"`
	// CostHidden and friends are the hidden flags of the overlay sections.
	CostHidden    bool `json:"costHidden"`
	SpendHidden   bool `json:"spendHidden"`
	EnginesHidden bool `json:"enginesHidden"`
	DecodeFailed  bool `json:"decodeFailed"`
	// HeroText is the cost hero tile's text, empty when the overlay is off.
	HeroText string `json:"heroText"`
	// The reconciliation panel and the coverage banner. None of what these hold
	// comes from the census — the account rollup, its breakdowns, the collection
	// windows and the request meter are all in the metadata block — so they are
	// read separately from the table's own state, and a failed decode must not
	// empty them.
	AuditHidden  bool   `json:"auditHidden"`
	ReconBody    string `json:"reconBody"`
	BannerHidden bool   `json:"bannerHidden"`
	BannerTitle  string `json:"bannerTitle"`
	BannerList   string `json:"bannerList"`
	// The coverage block — one track per source — and the two halves the cost
	// prose was split into: NoteText is what stays on the page, MoreText what
	// moved behind the disclosure. Read separately so a test can tell "this
	// sentence is gone" from "this sentence is one click away".
	CoverageHidden bool   `json:"coverageHidden"`
	CoverageText   string `json:"coverageText"`
	NoteText       string `json:"noteText"`
	MoreHidden     bool   `json:"moreHidden"`
	MoreText       string `json:"moreText"`
	CountText      string `json:"countText"`
	// AttrLegend is the attribution bar's legend, which ends in the sentence
	// naming what the bar is weighted by. LegendAfterMethod is the same legend
	// read again after the drive phase switches cost source: the bar is weighted
	// by money, so a new source has to reweight it, and the two must differ.
	AttrLegend        string `json:"attrLegend"`
	LegendAfterMethod string `json:"legendAfterMethod"`
	// SortedBy is the header text of the column the table opened sorted on, and
	// SortAria that column's aria-sort. Empty when the table opened unsorted.
	SortedBy string `json:"sortedBy"`
	SortAria string `json:"sortAria"`
	// GroupHeaders is the text of every group header in the body as the page
	// opened. A census past the collapse threshold opens grouped, so this is
	// the only view in which flatten, buildGroupRow and appendGroupCost run at
	// all — and the only one that can show what a header says about a group no
	// source priced.
	GroupHeaders []string `json:"groupHeaders"`
	// SpendBars, MethodButtons and SortableColumns count the controls the page
	// wired up — the click targets the drive phase then exercises.
	SpendBars       int `json:"spendBars"`
	MethodButtons   int `json:"methodButtons"`
	SortableColumns int `json:"sortableColumns"`
	// Drove records one entry per interaction the harness performed, in order.
	Drove []droveStep `json:"drove"`
}

type droveStep struct {
	What    string `json:"what"`
	Painted int    `json:"painted"`
	Error   string `json:"error"`
}

// bootReport renders snap, boots the resulting page under node, and returns
// what the harness saw. It fails the test on any exception the page throws,
// during boot or during the interactions.
//
// The optional flags are passed to the harness, which uses them to take a
// capability away from the environment before the page runs — see
// noDecompression.
func bootReport(t *testing.T, snap *model.Snapshot, flags ...string) bootProbe {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		// Same bargain as evalReportJS: skipping is right on a contributor's
		// machine and wrong in CI, where it would report coverage this test is
		// not providing.
		if os.Getenv("CI") != "" {
			t.Fatal("node is required to boot the report in CI")
		}
		t.Skip("node not installed; skipping report boot")
	}

	dir := t.TempDir()
	page := filepath.Join(dir, "report.html")
	if err := HTML(snap, page); err != nil {
		t.Fatalf("HTML() error: %v", err)
	}
	harness := filepath.Join(dir, "boot.js")
	if err := os.WriteFile(harness, []byte(bootHarness), 0o600); err != nil {
		t.Fatalf("writing harness: %v", err)
	}

	out, err := exec.Command(node, append([]string{harness, page}, flags...)...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		t.Fatalf("the report did not boot: %v\n%s", err, text)
	}
	const marker = "BOOT-OK "
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("harness printed no verdict:\n%s", text)
	}
	var probe bootProbe
	if err := json.Unmarshal([]byte(text[i+len(marker):]), &probe); err != nil {
		t.Fatalf("decoding harness output: %v\n%s", err, text)
	}
	if len(probe.MissingIDs) > 0 {
		t.Errorf("the page looked up ids the markup does not define: %v", probe.MissingIDs)
	}
	for _, s := range probe.Drove {
		if s.Error != "" {
			t.Errorf("%s threw after boot: %s", s.What, s.Error)
		}
	}
	return probe
}

// A costed census. The overlay has to come up, the table has to paint, and
// every control the overlay adds has to survive being clicked — the method
// switch and the spend bars are the two that call back into the table, and
// both of them were left calling a function that no longer exists.
func TestReportBootsAndDrivesWithCost(t *testing.T) {
	probe := bootReport(t, demoCostSnapshot("test"))

	if probe.Painted == 0 {
		t.Error("the page booted but painted no rows")
	}
	if probe.DecodeFailed {
		t.Error("the page reported a decode failure on its own census")
	}
	if probe.CostHidden || probe.SpendHidden {
		t.Errorf("cost data is present but the overlay stayed hidden: cost=%v spend=%v",
			probe.CostHidden, probe.SpendHidden)
	}
	// Spend by service replaces the engine breakdown rather than sitting beside
	// it. Two panels ranking the same estate by two different things is the
	// layout this feature exists to remove.
	if !probe.EnginesHidden {
		t.Error("spend by service is showing and the engines section did not give way to it")
	}
	if probe.HeroText == "" {
		t.Error("the cost hero tile is empty; initCost never ran against the decoded rows")
	}
	if probe.SpendBars == 0 {
		t.Error("no spend bars were wired up")
	}
	if probe.MethodButtons == 0 {
		t.Error("no cost method buttons were wired up")
	}

	// The Cost column exists, and its header names the source and the window.
	// A column of money with no question attached to it is the thing the header
	// subtitle exists to prevent, and it is written by syncCostHeader — which
	// runs in the decode continuation, so its absence here means the second
	// boot pass never reached it.
	var cost string
	for _, h := range probe.Headers {
		if strings.Contains(strings.ToLower(h), "cost") {
			cost = h
		}
	}
	if cost == "" {
		t.Fatalf("the census is priced but the table has no cost column: %v", probe.Headers)
	}
	if !strings.Contains(cost, "·") {
		t.Errorf("cost header %q names no source, metric or window", cost)
	}

	// Money is the organizing principle, so a priced report opens ranked by it,
	// largest first. Both halves are load-bearing: the wrong column means the
	// reader has to find the ranking, and ascending means the page opens on the
	// cheapest thing in the estate.
	if !strings.Contains(strings.ToLower(probe.SortedBy), "cost") || probe.SortAria != "descending" {
		t.Errorf("the priced table opened sorted on %q (%s), want the cost column descending",
			probe.SortedBy, probe.SortAria)
	}

	// The attribution bar is weighted by spend once there is spend to weight
	// it by; the count survives in the tooltip. Falling back to count on a
	// priced census is the bar quietly answering a different question.
	if strings.Contains(probe.AttrLegend, "weighted by resource count") {
		t.Errorf("the attribution bar stayed count-weighted on a priced census: %q", probe.AttrLegend)
	}
	if !strings.Contains(probe.AttrLegend, "weighted by ") {
		t.Errorf("the attribution bar names nothing as its weight: %q", probe.AttrLegend)
	}

	// And it re-weights when the reader switches source. The two methods price
	// different resources for different amounts, so a bar that reads the same
	// after the switch is a bar still drawn from the old source's money while
	// the legend underneath it, the table and the hero all name the new one.
	if probe.MethodButtons > 1 && probe.LegendAfterMethod == probe.AttrLegend {
		t.Errorf("switching cost source left the attribution bar untouched: %q",
			probe.AttrLegend)
	}

	// Clicking a spend bar filters the table, so the row count has to move. If
	// it does not, setSpendFilter reached rebuild and rebuild did nothing —
	// which is what a silently swallowed filter looks like.
	var filtered *droveStep
	for i, s := range probe.Drove {
		if s.What == "click spend bar" {
			filtered = &probe.Drove[i]
		}
	}
	if filtered == nil {
		t.Fatal("the harness never clicked a spend bar")
	}
	if filtered.Painted >= probe.Painted {
		t.Errorf("clicking a spend bar left %d rows painted, up from %d; the filter did nothing",
			filtered.Painted, probe.Painted)
	}
}

// Both sources at once, which is the whole of what a reader wants from a page
// that has two: what AWS billed for this resource, and what the model says it
// costs a month as configured. That used to be one column behind a toggle, so
// the comparison took two clicks and a memory of the first number.
//
// The three things this pins are the three that make it a comparison rather
// than two views. Each source has its own column, so both figures are on the
// same row. Each column carries its own source's name, so neither is read as
// the other. And the two never merge — no combined column, no combined total —
// because a billed fortnight and a modelled month have no window in common and
// adding them publishes a figure neither AWS service reported.
func TestReportGivesEachCostSourceItsOwnColumn(t *testing.T) {
	probe := bootReport(t, demoCostSnapshot("test"))

	var billed, modelled string
	for _, h := range probe.Headers {
		switch {
		case strings.HasPrefix(h, "Billed"):
			billed = h
		case strings.HasPrefix(h, "Modelled"):
			modelled = h
		}
	}
	if billed == "" || modelled == "" {
		t.Fatalf("a census priced by both sources drew %d cost columns, want one each:\n%v",
			len(billed)+len(modelled), probe.Headers)
	}
	if !strings.Contains(billed, "Cost Explorer") {
		t.Errorf("the billed column does not name Cost Explorer: %q", billed)
	}
	if !strings.Contains(modelled, "Cost Optimization Hub") {
		t.Errorf("the modelled column does not name Cost Optimization Hub: %q", modelled)
	}
	// Each column names one source and only its own. A modelled column that
	// also said "Cost Explorer" would be the old single column wearing a new
	// label, and the reader would have no way to tell which figure is which.
	if strings.Contains(modelled, "Cost Explorer ") {
		t.Errorf("the modelled column also claims Cost Explorer: %q", modelled)
	}

	// Sorting is per column, never across the two. A shared sort key would rank
	// a fortnight of billing against a month of modelling.
	if strings.Contains(reportTemplate, `sortKey === "_costSort"`) ||
		strings.Contains(reportTemplate, `key === "_costSort"`) {
		t.Error("a cost sort key with no source in it survived; the columns share an ordering")
	}

	// The coverage block leads with how much of the estate each source reached,
	// which is the question the columns cannot answer and the one that decides
	// how much either total is worth.
	if probe.CoverageHidden || probe.CoverageText == "" {
		t.Errorf("the coverage block did not render (hidden=%v, text=%q)",
			probe.CoverageHidden, probe.CoverageText)
	}
	for _, want := range []string{"Cost Explorer", "Cost Optimization Hub", "priced by neither source"} {
		if !strings.Contains(probe.CoverageText, want) {
			t.Errorf("the coverage block never says %q:\n%s", want, probe.CoverageText)
		}
	}

	// The standing explanation moved behind a disclosure rather than off the
	// page. Both halves are asserted: a note that swallowed the disclosures
	// would pass a test that only checked the note got shorter.
	if probe.MoreHidden || probe.MoreText == "" {
		t.Errorf("the cost disclosures are not reachable (hidden=%v, text=%q)",
			probe.MoreHidden, probe.MoreText)
	}
	for _, want := range []string{
		"does not estimate what deleting a resource returns",
		"modelled monthly rates",
	} {
		if !strings.Contains(probe.MoreText, want) {
			t.Errorf("a disclosure was dropped rather than moved — %q is nowhere:\n%s", want, probe.MoreText)
		}
	}
	if strings.Contains(probe.NoteText, "does not estimate what deleting a resource returns") {
		t.Error("the standing disclosures are still printed inline as well as behind the disclosure")
	}
}

// One source, which is every report until Cost Optimization Hub is enrolled in.
// It gets one column, and the control for picking a basis stays away: a toggle
// between one thing is furniture.
//
// AddResourceCostOverlay is the Cost Explorer half of the fixture — the names
// read the other way round from what they price — so this is the ordinary
// --costs run with no hub enrollment behind it.
func TestReportWithOneCostSourceDrawsOneColumn(t *testing.T) {
	snap := demo.Snapshot("test")
	snap.Cost = demo.CostReport()
	demo.AddResourceCostOverlay(snap)
	snap.FinalizeAt(renderClock)

	probe := bootReport(t, snap)

	var cost []string
	for _, h := range probe.Headers {
		if strings.HasPrefix(h, "Billed") || strings.HasPrefix(h, "Modelled") {
			cost = append(cost, h)
		}
	}
	if len(cost) != 1 {
		t.Fatalf("a census priced by one source drew %d cost columns: %v", len(cost), cost)
	}
	if !strings.HasPrefix(cost[0], "Billed") {
		t.Errorf("the one column is not the billed one: %q", cost[0])
	}
	if probe.MethodButtons != 0 {
		t.Errorf("a basis selector appeared for %d source(s)", probe.MethodButtons)
	}
	// One track is still worth drawing: "Cost Explorer priced 28 of 98" is the
	// same finding whether or not a second source exists to compare it against.
	if probe.CoverageHidden {
		t.Error("the coverage block hid itself because there was only one source to chart")
	}
}

// The same page with nothing priced. The overlay has to stay out of the way,
// the engines section has to keep its place, and — the part a lifted-function
// test cannot see — the table still has to come up. costColumn is decided at
// module scope from the metadata block, before a single row has decoded, so an
// uncosted run is the case where that decision is load-bearing.
func TestReportBootsWithoutCost(t *testing.T) {
	probe := bootReport(t, demoSnapshot("test"))

	if probe.Painted == 0 {
		t.Error("the page booted but painted no rows")
	}
	if !probe.CostHidden || !probe.SpendHidden {
		t.Errorf("nothing is priced but the overlay is showing: cost=%v spend=%v",
			probe.CostHidden, probe.SpendHidden)
	}
	if probe.EnginesHidden {
		t.Error("no spend panel replaced it, so the engines section should still be showing")
	}
	for _, h := range probe.Headers {
		if strings.Contains(strings.ToLower(h), "cost") {
			t.Errorf("the census carries no cost and the table grew a cost column anyway: %v",
				probe.Headers)
		}
	}
	// With nothing to weight it by, the bar says so rather than implying the
	// widths mean money.
	if !strings.Contains(probe.AttrLegend, "weighted by resource count") {
		t.Errorf("nothing is priced, so the attribution bar should say what it is "+
			"actually weighted by: %q", probe.AttrLegend)
	}
}

// Above the compression threshold the census arrives as gzip in base64 and is
// decoded through DecompressionStream, which is a different code path from the
// plain-JSON one the two tests above take — and it is asynchronous in a way
// the cost overlay now depends on, since initCost runs in its continuation. A
// census that decodes but never reaches the continuation paints an unpriced
// table off a priced snapshot, silently.
func TestReportBootsFromACompressedCensus(t *testing.T) {
	probe := bootReport(t, compressedCostSnapshot(t))
	if probe.DecodeFailed {
		t.Fatal("the page could not decode its own compressed census")
	}
	if probe.Painted == 0 {
		t.Error("the compressed census decoded but painted no rows")
	}
	// Virtualization is the reason this size is worth booting at all: the body
	// holds a window, not the estate.
	if probe.Painted >= compressAbove {
		t.Errorf("painted %d rows of a %d-row census; the body is not virtualized",
			probe.Painted, compressAbove+1)
	}
	if probe.HeroText == "" {
		t.Error("the compressed census is priced but the cost hero is empty; " +
			"the decode continuation never ran initCost")
	}

	// The cost panels paint twice — once from the metadata at boot, once when
	// the rows land — and this is the second pass. It is asserted here because
	// the first pass is what the unreadable-census test below pins: a first pass
	// that satisfied that test and then failed to be superseded would leave a
	// priced report wearing an empty one's coverage.
	if probe.SpendBars == 0 {
		t.Error("the census decoded and nothing ranked it by spend; the second " +
			"pass never reached renderSpend")
	}
	if probe.AuditHidden || probe.ReconBody == "" {
		t.Error("the reconciliation panel is empty on a census that decoded")
	}
	if strings.Contains(probe.BannerList, "could not be read") {
		t.Errorf("the banner reports an unreadable inventory on a census it read: %q",
			probe.BannerList)
	}
}

// compressedCostSnapshot is a priced census large enough to be written as gzip
// in base64, which is the only encoding whose decode a browser can lack the
// means to perform.
func compressedCostSnapshot(t *testing.T) *model.Snapshot {
	t.Helper()
	snap := demo.SnapshotN("test", compressAbove+1)
	snap.Cost = demo.CostReport()
	demo.AddResourceCosts(snap)
	demo.AddResourceCostOverlay(snap)
	snap.FinalizeAt(renderClock)
	return snap
}

// A census past the collapse threshold opens grouped by service, which is the
// only view where the group headers are what the reader sees — and the census
// is priced, so every one of those headers is a statement about money whether
// it carries a figure or not.
//
// Two groups have no figure for two different reasons, and the header used to
// treat them differently: a group the selected source did not price said "no
// Cost Explorer figure", while a group nothing priced at all appended no cost
// slot and rendered blank. On the scaled fixture that is most of the headers
// blank beside two that print thousands of dollars, and one that says out loud
// what all of them mean. A blank slot among slots that carry money does not
// read as unpriced; it reads as free.
//
// This is the whole chain — flatten deciding the dimension has a cost slot,
// buildGroupRow honouring it, appendGroupCost filling it — against a real
// rendered artifact, which is the only place the three meet.
func TestReportGroupedHeadersAllAnswerTheCostQuestion(t *testing.T) {
	probe := bootReport(t, compressedCostSnapshot(t))

	if len(probe.GroupHeaders) < 2 {
		t.Fatalf("a %d-resource census painted %d group headers; it did not open "+
			"grouped, so this test saw nothing", compressAbove+1, len(probe.GroupHeaders))
	}

	var priced, labelled int
	for _, h := range probe.GroupHeaders {
		switch {
		case strings.Contains(h, "USD"):
			priced++
		case strings.Contains(h, "figure"):
			labelled++
		default:
			t.Errorf("group header %q carries no cost slot at all while its "+
				"neighbours print totals: silence beside a figure reads as free, "+
				"and this group is unpriced, not free", h)
		}
	}

	// Both kinds have to be on screen, or the assertion above is vacuous: a run
	// where every group happened to be priced would pass it without exercising
	// the branch that exists for the ones that are not.
	if priced == 0 {
		t.Error("no group header printed a total, on a census the report says is priced")
	}
	if labelled == 0 {
		t.Error("every group was priced by the selected source, so nothing here " +
			"exercised the header's answer for a group that was not")
	}
}

// The same size of census with no costs at all, which is the other half of that
// rule and the reason the slot is decided per dimension rather than per group.
//
// "No Cost Explorer figure" on every one of fourteen headers is the coverage
// banner restated fourteen times, in a report that has no cost column, no cost
// source selected and nothing for the reader to switch to. There is no figure
// missing here because there was never a figure to miss.
func TestReportGroupedHeadersSayNothingAboutCostOnAnUnpricedCensus(t *testing.T) {
	probe := bootReport(t, demoSnapshotN("test", compressAbove+1))

	if len(probe.GroupHeaders) < 2 {
		t.Fatalf("a %d-resource census painted %d group headers; it did not open "+
			"grouped, so this test saw nothing", compressAbove+1, len(probe.GroupHeaders))
	}
	for _, h := range probe.GroupHeaders {
		if strings.Contains(h, "figure") || strings.Contains(h, "USD") {
			t.Errorf("group header %q talks about cost on a census with none: the "+
				"header answers a question this report never asked", h)
		}
	}
}

// What survives when the inventory does not.
//
// The census decodes asynchronously; almost nothing in the cost overlay came
// out of it. The account rollup, its per-service and per-account breakdowns,
// both collection windows and what the questions cost are all in the plain JSON
// metadata block, which parses synchronously and is still sitting there, intact
// and readable, when the compressed inventory beside it fails to inflate.
//
// Drawing the overlay only inside the decode continuation made every one of
// those a hostage of the census. A browser without DecompressionStream — Safari
// before 16.4, Firefox before 113 — got a report with no account total, no
// reconciliation trail and no request meter, on the run that most needs them:
// the reader was billed per Cost Explorer request for exactly these answers.
//
// So the panels paint from the metadata at boot, and again when the rows land.
func TestReportKeepsTheCostOverlayWhenTheCensusCannotBeRead(t *testing.T) {
	probe := bootReport(t, compressedCostSnapshot(t), "no-decompression")

	// The premise. If this ever stops failing, the test below is asserting
	// nothing and has to be re-aimed rather than deleted.
	if !probe.DecodeFailed {
		t.Fatal("the census decoded without DecompressionStream; this test no " +
			"longer exercises a failed decode")
	}
	if probe.Painted != 0 {
		t.Errorf("painted %d rows off a census that could not be read", probe.Painted)
	}

	if probe.CostHidden {
		t.Error("the cost section is hidden: the account rollup came from the " +
			"metadata block, which read fine")
	}
	if probe.HeroText == "" {
		t.Error("the cost hero is empty; the account total is in the metadata block")
	}
	if probe.AuditHidden || probe.ReconBody == "" {
		t.Errorf("the reconciliation panel is gone (hidden=%v, body=%q): it is the "+
			"audit trail for figures that are still in this file",
			probe.AuditHidden, probe.ReconBody)
	}

	// And it says why the per-resource half is missing, in the banner that
	// exists to say so — naming the fix, because there is one.
	if probe.BannerHidden {
		t.Fatal("the census could not be read and the coverage banner says nothing")
	}
	if !strings.Contains(probe.BannerList, "could not be read") {
		t.Errorf("the coverage banner does not say the inventory is unreadable: %q",
			probe.BannerList)
	}
	if !strings.Contains(probe.BannerList, "rerun the scan") {
		t.Errorf("the coverage banner names no way out: %q", probe.BannerList)
	}

	// What it must not say. "No resource-level figure reached any resource" is a
	// finding about rows, and nobody read the rows — it is the same sentence a
	// genuinely unpriced census earns, printed over a census whose figures are
	// unknown rather than absent.
	for _, claim := range []string{
		"No resource-level cost data",
		"reached any resource",
		"carry no",
	} {
		if strings.Contains(probe.BannerTitle+" "+probe.BannerList, claim) {
			t.Errorf("the banner claims %q about rows that were never read:\ntitle %q\nlist %q",
				claim, probe.BannerTitle, probe.BannerList)
		}
	}

	// The spend bars and the cost column rank resources, so they are the two
	// things that genuinely cannot survive. They must be absent rather than
	// empty.
	if !probe.SpendHidden {
		t.Error("spend by service is showing with no resources to rank")
	}
	if probe.SpendBars != 0 {
		t.Errorf("%d spend bars were drawn from a census nobody read", probe.SpendBars)
	}
}

// And the state in between, which is the one every large report opens in.
//
// A failed decode is repainted by failCensus, so the panels can be drawn only
// in the two continuations and still satisfy the test above. What that misses
// is the interval before either of them runs: the census is inflating, the
// controls are disabled, the table says "reading…", and the account total —
// which was never in the census, and has been sitting parsed in this page since
// the first synchronous statement — is either on screen or it is not. On a
// census of tens of thousands of rows that interval is the report's first
// impression.
func TestReportShowsTheCostOverlayWhileTheCensusIsStillDecoding(t *testing.T) {
	probe := bootReport(t, compressedCostSnapshot(t), "stall-decode")

	// The premise: still decoding, which is neither of the other two states —
	// no rows (so it did not finish) and no error (so it did not fail).
	if probe.Painted != 0 || probe.DecodeFailed {
		t.Fatalf("the decode settled (painted=%d, failed=%v); this test no longer "+
			"observes a report mid-decode", probe.Painted, probe.DecodeFailed)
	}
	if !strings.Contains(probe.CountText, "reading") {
		t.Fatalf("row count = %q, want the reading-in-progress label", probe.CountText)
	}

	if probe.CostHidden {
		t.Error("the cost section is hidden while the census decodes; the account " +
			"rollup it leads with is in the metadata block and was parsed at boot")
	}
	if probe.HeroText == "" {
		t.Error("the cost hero is empty while the census decodes")
	}
	if probe.AuditHidden || probe.ReconBody == "" {
		t.Errorf("the reconciliation panel is empty while the census decodes "+
			"(hidden=%v, body=%q)", probe.AuditHidden, probe.ReconBody)
	}

	// What it must not do is fill that interval with findings about rows nobody
	// has read yet — the coverage the census will report is unknown, not absent,
	// and it must not be printed and then quietly corrected once the rows land.
	for _, claim := range []string{"No resource-level cost data", "reached any resource"} {
		if strings.Contains(probe.BannerTitle+" "+probe.BannerList, claim) {
			t.Errorf("the banner claims %q about rows that are still decoding:\ntitle %q\nlist %q",
				claim, probe.BannerTitle, probe.BannerList)
		}
	}
	if !probe.SpendHidden || probe.SpendBars != 0 {
		t.Errorf("spend by service is ranking %d services off a census that has "+
			"not arrived (hidden=%v)", probe.SpendBars, probe.SpendHidden)
	}
}

// bootHarness is a DOM stub big enough to run the report and nothing more. It
// takes the path to a rendered report, pulls the ids, the data blocks and the
// page script back out of it, evaluates the script, waits for the census to
// decode, drives the controls, and prints one JSON verdict.
//
// getElementById answers null for an id the markup does not define, exactly as
// a browser does, and records it — the page is allowed to guard against null,
// and the test reports the disagreement rather than deciding for it.
const bootHarness = `
const fs = require("fs");
const html = fs.readFileSync(process.argv[2], "utf8");

const SCRIPT_RE = /<script([^>]*)>([\s\S]*?)<\/script>/g;

// Every element the markup names, with the two attributes the page reads back
// off one. hidden matters: the overlay's panels ship hidden and are revealed
// by the script, so a stub that defaults them to visible cannot tell "the
// script showed this" from "the script never touched it".
const ids = new Map();
for (const m of html.matchAll(/<([a-zA-Z][\w-]*)((?:"[^"]*"|[^>"])*)>/g)) {
  const id = /\bid="([^"]+)"/.exec(m[2]);
  if (id) ids.set(id[1], { tag: m[1], hidden: /\bhidden(?=[\s>=])|\bhidden$/.test(m[2]) });
}

const texts = {};
let page = null;
for (const m of html.matchAll(SCRIPT_RE)) {
  const attrs = m[1], body = m[2];
  const id = /id="([^"]+)"/.exec(attrs);
  if (id) { texts[id[1]] = body; continue; }
  if (!attrs.trim()) {
    if (page !== null) { console.log("two bare script blocks"); process.exit(1); }
    page = body;
  }
}
if (page === null) { console.log("no page script"); process.exit(1); }

const missing = new Set();

class ClassList {
  constructor() { this.s = new Set(); }
  add(...c) { c.forEach((x) => this.s.add(x)); }
  remove(...c) { c.forEach((x) => this.s.delete(x)); }
  contains(c) { return this.s.has(c); }
  toggle(c, on) {
    if (on === undefined) on = !this.s.has(c);
    if (on) this.s.add(c); else this.s.delete(c);
    return on;
  }
}

class TextNode {
  constructor(t) { this.nodeValue = String(t); this.childNodes = []; }
  get textContent() { return this.nodeValue; }
  set textContent(v) { this.nodeValue = String(v); }
}

class El {
  constructor(tag) {
    this.tagName = String(tag || "div").toUpperCase();
    this.childNodes = [];
    this.style = {};
    this.dataset = {};
    this.classList = new ClassList();
    this.attributes = {};
    this.listeners = {};
    this.hidden = false;
    this.disabled = false;
    this.title = "";
    this.className = "";
    this.value = "";
    this.type = "";
    this.scope = "";
    this.colSpan = 1;
    this.scrollTop = 0;
    // Any non-zero row height will do: the virtualizer only needs the window
    // to be smaller than the census for its arithmetic to be exercised.
    this.offsetHeight = 21;
    this.clientHeight = 640;
  }
  get children() { return this.childNodes.filter((n) => n instanceof El); }
  get firstChild() { return this.childNodes[0] || null; }
  get textContent() { return this.childNodes.map((n) => n.textContent).join(""); }
  // Assigning textContent replaces the children with one text node, which is
  // what a browser does — it does not set text *beside* the children. An
  // earlier stub kept it in a field that appendChild then cleared, so any
  // element the page filled with el(tag, cls, text) and then appended to lost
  // its text; the coverage banner builds every one of its entries that way.
  set textContent(v) {
    this.childNodes = String(v) === "" ? [] : [new TextNode(v)];
  }
  appendChild(c) {
    if (c instanceof Fragment) {
      c.childNodes.slice().forEach((n) => this.appendChild(n));
      c.childNodes = [];
      return c;
    }
    this.childNodes.push(c);
    return c;
  }
  append(...cs) {
    cs.forEach((c) => this.appendChild(typeof c === "string" ? new TextNode(c) : c));
  }
  replaceChildren(...cs) { this.childNodes = []; this.append(...cs); }
  removeChild(c) { this.childNodes = this.childNodes.filter((n) => n !== c); return c; }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  getAttribute(k) { return k in this.attributes ? this.attributes[k] : null; }
  removeAttribute(k) { delete this.attributes[k]; }
  addEventListener(ev, fn) { (this.listeners[ev] || (this.listeners[ev] = [])).push(fn); }
  removeEventListener(ev, fn) {
    this.listeners[ev] = (this.listeners[ev] || []).filter((f) => f !== fn);
  }
  click() {
    (this.listeners.click || []).forEach((f) =>
      f.call(this, { target: this, preventDefault() {}, stopPropagation() {} }));
  }
  focus() {}
  blur() {}
  getBoundingClientRect() {
    return { top: 0, left: 0, width: 900, height: 640, bottom: 640, right: 900 };
  }
}

class Fragment extends El {
  constructor() { super("#fragment"); }
}

const byId = new Map();
for (const [id, spec] of ids) {
  const el = new El(spec.tag);
  el.id = id;
  el.hidden = spec.hidden;
  if (id in texts) el.textContent = texts[id];
  byId.set(id, el);
}

const document = {
  getElementById(id) {
    if (byId.has(id)) return byId.get(id);
    missing.add(id);
    return null;
  },
  createElement(tag) { return new El(tag); },
  createTextNode(t) { return new TextNode(t); },
  createDocumentFragment() { return new Fragment(); },
  addEventListener() {},
  body: new El("body"),
  documentElement: new El("html"),
};

const window = {
  addEventListener() {},
  removeEventListener() {},
  document,
  setTimeout,
  clearTimeout,
};
// The wrapper's height cap is read off CSS. Any pixel value keeps the
// virtualizer's window finite; a missing one would make it paint nothing.
const getComputedStyle = () => ({ maxHeight: "640px", getPropertyValue: () => "" });
const requestAnimationFrame = (fn) => setTimeout(fn, 0);
Object.assign(globalThis, {
  window, document, getComputedStyle, requestAnimationFrame, self: window,
});
// navigator has to be defined rather than assigned. node 21 and later ship a
// real one, as a getter with no setter, and Object.assign throws on those
// instead of skipping them — so a runner that upgrades node takes every boot
// test down at once, which is how this was found. Defining it also keeps the
// stub identical on both: a harness whose job is to be one fixed browser
// should not pick up the host's user agent on the newer runner.
Object.defineProperty(globalThis, "navigator", {
  value: { userAgent: "node" }, configurable: true, writable: true,
});

let rejected = null;
process.on("unhandledRejection", (e) => { rejected = e; });

// A browser without DecompressionStream — Safari before 16.4, Firefox before
// 113 — which is the environment the report's own decode-failure message names.
// node has it, so the only way to reach that path is to take it away.
if (process.argv.includes("no-decompression")) delete globalThis.DecompressionStream;

// A decode that is still running when the reader looks at the page. It is the
// state a large report opens in — inflating a compressed census of tens of
// thousands of rows is not instant — and the only way to hold a page in it long
// enough to read is a decompressor whose output never arrives.
if (process.argv.includes("stall-decode")) {
  globalThis.DecompressionStream = class {
    constructor() {
      this.writable = new WritableStream({ write() {}, close() {}, abort() {} });
      this.readable = new ReadableStream({ start() {} });
    }
  };
}

try {
  new Function(page)();
} catch (e) {
  console.log("BOOT-THREW " + (e && e.stack ? e.stack : e));
  process.exit(1);
}

const el = (id) => byId.get(id) || new El("div");
const painted = () => el("rows").children.length;
const drove = [];

function drive(what, fn) {
  let error = "";
  try { fn(); } catch (e) { error = String(e && e.stack ? e.stack : e); }
  drove.push({ what, painted: painted(), error });
}

// One turn of the loop per await point in decodeCensus, then the drive phase.
setTimeout(function () {
  if (rejected) {
    console.log("BOOT-REJECTED " + (rejected.stack || rejected));
    process.exit(1);
  }

  // Everything below the drive phase is read after the clicks, so anything
  // that describes the page as it *opened* has to be captured here first.
  const before = painted();
  const groupHeaders = el("rows").children
    .filter((tr) => tr.className === "group-row")
    .map((tr) => tr.textContent);
  const attrLegend = el("attr-legend").textContent;
  const headers = el("head-row").children.map((th) => th.textContent);
  let sortedBy = "", sortAria = "";
  el("head-row").children.forEach((th) => {
    const a = th.getAttribute("aria-sort");
    if (a && a !== "none") { sortedBy = th.textContent; sortAria = a; }
  });
  const spendBars = el("spend-rows").children;
  const methodButtons = el("cost-method").children;
  const sortable = el("head-row").children
    .map((th) => th.children[0])
    .filter((b) => b && (b.listeners.click || []).length);

  // Sorting first, while the whole census is in view: the cost column is the
  // default sort on a priced report, so clicking it twice walks it through
  // both directions and back off.
  sortable.slice(0, 3).forEach((b, i) => drive("click header " + i, () => { b.click(); b.click(); }));
  // The last button is the source that is not currently selected, since
  // initCost defaults to the one that priced the most.
  let legendAfterMethod = "";
  if (methodButtons.length) {
    drive("click method button", () => {
      methodButtons[methodButtons.length - 1].click();
      legendAfterMethod = el("attr-legend").textContent;
    });
  }
  // Last, because it narrows the table and the row count afterwards is the
  // assertion that the filter reached the body.
  if (spendBars.length) drive("click spend bar", () => spendBars[0].click());

  console.log("BOOT-OK " + JSON.stringify({
    missingIds: [...missing].sort(),
    painted: before,
    headers,
    costHidden: el("cost-section").hidden,
    spendHidden: el("spend-section").hidden,
    enginesHidden: el("engines-section").hidden,
    decodeFailed: !el("decode-error").hidden,
    heroText: el("cost-hero").textContent,
    auditHidden: el("cost-audit-section").hidden,
    reconBody: el("cost-recon-body").textContent,
    bannerHidden: el("cost-banner").hidden,
    bannerTitle: el("cost-banner-title").textContent,
    bannerList: el("cost-banner-list").textContent,
    coverageHidden: el("cost-coverage").hidden,
    coverageText: el("cost-coverage").textContent,
    noteText: el("cost-note").textContent,
    moreHidden: el("cost-more").hidden,
    moreText: el("cost-more-body").textContent,
    countText: el("row-count").textContent,
    groupHeaders,
    attrLegend,
    legendAfterMethod,
    sortedBy,
    sortAria,
    spendBars: spendBars.length,
    methodButtons: methodButtons.length,
    sortableColumns: sortable.length,
    drove,
  }));
}, 250);
`
