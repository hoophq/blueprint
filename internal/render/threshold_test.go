package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The census block is written one of two ways, and the only thing that decides
// which is how many resources there are. Every other test in this package sees
// one side or the other by accident of its fixture — the storyboard is small,
// the hostile and budget censuses are large — so the branch itself is never the
// thing under test. This pins both sides at the exact count where it turns
// over, which is the one place an off-by-one would hide.
//
// What is deliberately not here: the size budget that motivated the compressed
// encoding, and the fifty-thousand-row case. Both live in budget_test.go, which
// measures bytes per resource at 20,000 rows and keeps the larger census as a
// benchmark CI does not run. A second scale test here would render a second
// large report on every push to measure something already measured.
func TestHTMLEncodingFlipsAtTheThreshold(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		want string
	}{
		{"at the threshold", compressAbove, encodingJSON},
		{"one past it", compressAbove + 1, encodingGzip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := demoSnapshotN("threshold-test", tc.n)
			if got := len(snap.Resources); got != tc.n {
				t.Fatalf("fixture produced %d resources, want %d; this test is "+
					"calibrated to the exact count", got, tc.n)
			}

			path := filepath.Join(t.TempDir(), "report.html")
			if err := HTML(snap, path); err != nil {
				t.Fatalf("HTML: %v", err)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read report: %v", err)
			}
			page := string(b)

			if got := metaBlock(t, page).Encoding; got != tc.want {
				t.Errorf("encoding = %q for a %d-resource census, want %q",
					got, tc.n, tc.want)
			}

			// Below the threshold the census stays text on purpose: a report
			// small enough to open anywhere should also be greppable, diffable,
			// and readable by a browser that has never heard of
			// DecompressionStream. Checking the encoding field alone would pass
			// on a page that labelled itself json and shipped base64 anyway.
			if tc.want == encodingJSON && !strings.Contains(dataBlock(t, page), `"str"`) {
				t.Error("census block is labelled json but is not readable JSON")
			}

			// Fitting in the budget is worth nothing if the estate did not
			// survive the trip. Both encodings decode back to the same census.
			if got := len(decodeDataBlock(t, page)); got != tc.n {
				t.Errorf("decoded %d resources, want %d", got, tc.n)
			}
		})
	}
}
