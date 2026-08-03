package render

import (
	"os"
	"path/filepath"
	"testing"
)

// budgetResources is the estate the size budget is measured against. Large
// enough that the template's fixed weight — some sixty kilobytes of markup,
// style and script — has amortised away and what is left is the marginal cost
// of a row, which is the only part that scales.
const budgetResources = 20_000

// budgetBytesPerResource caps that marginal cost.
//
// The report embeds the census as parallel columns with the repetitive ones
// dictionary-encoded, gzipped and base64'd into the page. That measures 58
// bytes per resource, and the ceiling is set above it: loose enough that a
// scanner adding an attribute does not fail CI, tight enough that losing the
// encoding shows up as a failure rather than as a report that quietly takes a
// second longer to open.
//
// The band this number sits in is narrow, and knowing where its edges are is
// what makes a failure readable. Measured on this fixture — the same page each
// time, with only the data block's encoding varied, so the rows compare
// against each other and not against some other build's page:
//
//	 58 B/resource  what ships — columnar, dictionary-encoded, gzipped
//	 78 B/resource  transposition lost, gzip kept: rows back to objects
//	397 B/resource  gzip lost, transposition kept
//	588 B/resource  both lost — the pre-ATR-186 encoding, one JSON array
//	                of objects with every row repeating every key it has
//
// So a breach in the low hundreds means compression stopped happening, and one
// just past the ceiling means the columns did. The second is the tighter call,
// which is why the ceiling sits below 78 rather than at a rounder, safer
// number: the shape assertions in payload_test.go would also catch a census
// that stopped being columnar, but they would not catch one that stayed
// columnar and got heavy.
//
// The measurement is a worst case, not a reading of the estate as it stands
// today: the census below is finalized at budgetClock, past every date in the
// lifecycle table, so every row that can ever carry eol and eol_date already
// does. What the ceiling has to survive is the heaviest census this fixture
// can produce, and the calendar must not be what decides how close to it a
// given CI run lands.
const budgetBytesPerResource = 70

// budgetTotalBytes is a second, absolute ceiling. It cannot fire at the
// current row count — twenty thousand rows at the per-resource ceiling is
// under a megabyte and a half — so the ratio above is the live assertion
// today. It is here for the day someone raises budgetResources: a single-file
// report is something a reader opens in a browser and mails to a colleague,
// and past this size it stops being either.
const budgetTotalBytes = 16 << 20

// The one scale test in CI. Everything else in this package renders the
// curated storyboard, so `go test ./...` stays fast; the fifty-thousand-row
// case is BenchmarkHTMLAtScale below, which CI does not run.
func TestHTMLStaysWithinItsSizeBudgetAtScale(t *testing.T) {
	snap := demoSnapshotN("budget-test", budgetResources)
	if got := len(snap.Resources); got != budgetResources {
		t.Fatalf("fixture produced %d resources, want %d; the budget below is calibrated to that count",
			got, budgetResources)
	}

	path := filepath.Join(t.TempDir(), "report.html")
	if err := HTML(snap, path); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	size := info.Size()
	perResource := float64(size) / float64(len(snap.Resources))
	t.Logf("%d resources rendered to %d bytes (%.0f per resource)", len(snap.Resources), size, perResource)

	if perResource > budgetBytesPerResource {
		t.Errorf("report is %.0f bytes per resource, over the %d-byte budget: "+
			"%d resources rendered to %d bytes", perResource, budgetBytesPerResource, len(snap.Resources), size)
	}
	if size > budgetTotalBytes {
		t.Errorf("report is %d bytes, over the %d-byte ceiling for a single file "+
			"a reader is expected to open in a browser", size, budgetTotalBytes)
	}
}

// BenchmarkHTMLAtScale renders fifty thousand resources. It is a benchmark and
// not a test on purpose: `go test ./...` does not run it, so the cost lands on
// whoever is working on the encoder and asks for it, and nowhere else. Run it
// with `go test ./internal/render -bench HTMLAtScale -benchtime 1x`.
//
// The reported bytes/resource is the number to watch; the wall clock is a
// secondary reading, since a report is written once and read many times.
func BenchmarkHTMLAtScale(b *testing.B) {
	snap := demoSnapshotN("benchmark", 50_000)
	dir := b.TempDir()
	var size int64

	for b.Loop() {
		path := filepath.Join(dir, "report.html")
		if err := HTML(snap, path); err != nil {
			b.Fatalf("HTML: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			b.Fatalf("stat: %v", err)
		}
		size = info.Size()
	}

	b.ReportMetric(float64(size)/float64(len(snap.Resources)), "bytes/resource")
	b.ReportMetric(float64(size), "bytes")
}
