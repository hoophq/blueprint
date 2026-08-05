package render

import (
	"testing"
	"time"

	"github.com/hoophq/blueprint/internal/demo"
	"github.com/hoophq/blueprint/internal/model"
)

// The clocks this package's fixtures are judged against.
//
// demo.Snapshot finalizes against the wall clock, which is right for
// `scan --demo` and wrong for a test: Finalize flags a resource end-of-life
// only once its date has passed, so an unpinned fixture gains eol and eol_date
// fields as the calendar rolls forward and the census a test sees becomes a
// function of the day CI happened to run. The storyboard has a row waiting to
// do it — auth-db runs postgres 14.11, which the lifecycle table dates
// 2026-11-12 — so every fixture below is re-finalized at a fixed instant.
//
// There are two clocks because they answer different questions.
var (
	// renderClock is what the storyboard is judged against. The date is the
	// one internal/model's own EOL tests pin to, and it has to stay inside the
	// window where the storyboard's lifecycle contrast holds: after
	// 2023-10-31, so legacy-crm's mysql 5.7 reads as expired, and before
	// 2026-11-12, so auth-db's postgres 14 reads as still supported. Those two
	// rows sitting side by side are how the fixture covers both verdicts, and
	// a clock outside the window collapses them into one.
	renderClock = time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	// budgetClock is what the size measurement is judged against, and it is
	// past every date in the lifecycle table on purpose. A row carrying
	// eol and eol_date is some thirty-five bytes heavier than the same row
	// without them, so the honest thing for a ceiling to measure is the estate
	// in which every row that can ever flag has flagged. Pinning to a
	// realistic date instead would measure a census that quietly stops being
	// the worst case every time the table gains a row.
	budgetClock = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ptrTo takes the address of a literal, for the tri-state fields a fixture has
// to be able to set to false rather than leave unsaid. The distinction is the
// point of the pointer, and a test that could only express "true" and "absent"
// could not pin it.
func ptrTo[T any](v T) *T { return &v }

// demoSnapshot returns the storyboard fixture, judged at renderClock.
//
// Neither constructor takes a testing.TB: there is nothing here that can fail,
// and not taking one is what lets the scale benchmark build its census through
// the same call the scale test does, rather than a parallel one that could
// drift from it.
func demoSnapshot(version string) *model.Snapshot {
	snap := demo.Snapshot(version)
	snap.FinalizeAt(renderClock)
	return snap
}

// demoSnapshotN returns the fixture grown to n resources, judged at
// budgetClock. Scale is what the size budget measures, and the budget wants
// the worst case rather than today's.
func demoSnapshotN(version string, n int) *model.Snapshot {
	snap := demo.SnapshotN(version, n)
	snap.FinalizeAt(budgetClock)
	return snap
}

// demoCostSnapshot returns the storyboard with the money overlay attached —
// the rollup, the billed per-resource figures, and the hub's advice — judged at
// renderClock like its uncosted sibling.
//
// The plain fixture carries no cost data at all, which is the right default for
// the rest of the suite and useless for testing the overlay. The order here is
// the order the scan performs and is not interchangeable: AddResourceCostOverlay
// reads snap.Cost.Metric, so the rollup has to exist before it runs, and the
// advice lands first so that the rows carrying both a billed figure and a
// suggestion are built the way a real run builds them.
func demoCostSnapshot(version string) *model.Snapshot {
	snap := demo.Snapshot(version)
	snap.Cost = demo.CostReport()
	demo.AddRecommendations(snap)
	demo.AddResourceCostOverlay(snap)
	snap.FinalizeAt(renderClock)
	return snap
}

// demoCostSnapshotN is the scaled fixture priced, which is the combination the
// size budget cares about: the cost overlay puts a decimal string and a method
// on every row, and per-row weight is the only thing that scales.
func demoCostSnapshotN(version string, n int) *model.Snapshot {
	snap := demo.SnapshotN(version, n)
	snap.Cost = demo.CostReport()
	demo.AddRecommendations(snap)
	demo.AddResourceCostOverlay(snap)
	snap.FinalizeAt(budgetClock)
	return snap
}

// The pin is only worth having if something notices when it stops working, and
// until this test nothing in this package did. Breaking the seam in
// model.FinalizeAt — having it read the wall clock and ignore the instant it is
// handed — left every other test here green: budgetClock's contribution is
// thirty-five bytes in eleven megabytes, well inside the size budget's
// rounding, and the HTML and CSV assertions target legacy-crm's 2023-10-31,
// which is past under any clock this decade.
//
// So this asserts two things, and it needs both.
//
// The verdicts at renderClock are the census content the window exists to
// preserve: legacy-crm expired, auth-db still supported, one row each side.
// That catches renderClock drifting out of (2023-10-31, 2026-11-12), where
// both rows would read the same and the storyboard would stop covering both
// branches, and it catches either row being dropped.
//
// It does not, on its own, catch the seam: today's wall clock is inside that
// same window, so a FinalizeAt reading time.Now() would produce these exact
// verdicts. Hence the second assertion — auth-db judged at budgetClock, where
// its date has passed. The one row whose verdict is still ahead of us is the
// only lever this fixture has for proving the clock is honoured at all, and it
// is the same row that made the fixture drift in the first place.
func TestPinnedStoryboardKeepsBothEOLVerdicts(t *testing.T) {
	// Keyed by name, valued by the eol_date the row must carry at renderClock —
	// empty meaning the engine is still supported then.
	want := map[string]string{
		"legacy-crm": "2023-10-31", // mysql 5.7.44, expired well before the pin
		"auth-db":    "",           // postgres 14.11, supported until 2026-11-12
	}

	seen := map[string]bool{}
	for _, r := range demoSnapshot("test").Resources {
		date, ok := want[r.Name]
		if !ok {
			continue
		}
		seen[r.Name] = true
		if r.EOLDate != date || r.EOL != (date != "") {
			t.Errorf("%s at %s: EOL = (%v, %q), want (%v, %q)",
				r.Name, renderClock.Format(time.DateOnly), r.EOL, r.EOLDate, date != "", date)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("storyboard has no %s; it is one of the two rows that give the "+
				"fixture both lifecycle verdicts, so removing it needs a replacement, "+
				"not just a deletion here", name)
		}
	}

	// Same fixture, later clock: the verdict has to move. If it does not, the
	// snapshots this package builds are not being judged at the instant they
	// are handed, and every pin above is decorative.
	late := demo.Snapshot("test")
	late.FinalizeAt(budgetClock)
	for _, r := range late.Resources {
		if r.Name != "auth-db" {
			continue
		}
		if !r.EOL || r.EOLDate != "2026-11-12" {
			t.Errorf("auth-db at %s: EOL = (%v, %q), want (true, 2026-11-12) — "+
				"the fixture is not being judged at the clock it is given",
				budgetClock.Format(time.DateOnly), r.EOL, r.EOLDate)
		}
	}
}
