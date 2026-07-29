package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoophq/blueprint/internal/enrich"
	"github.com/hoophq/blueprint/internal/model"
)

// CloudWatch is the second billed API blueprint touches, so it gets the same
// treatment as the first: off unless asked for, priced in the help text.
func TestMetricsFlagDefaultsAndHelp(t *testing.T) {
	f := scanCmd().Flags().Lookup("metrics")
	if f == nil {
		t.Fatal("--metrics flag is missing")
	}
	if f.DefValue != "false" {
		t.Errorf("--metrics defaults to %q, want false", f.DefValue)
	}
	usage := strings.ToLower(f.Usage)
	// The rate comes from ChargeUSD so the help, the pre-flight notice, and
	// the post-run figure can never quote three different prices.
	if !strings.Contains(usage, "$"+enrich.ChargeUSD(1000)) {
		t.Errorf("--metrics help does not state the price per 1,000 metrics: %q", f.Usage)
	}
	if !strings.Contains(usage, "1,000") {
		t.Errorf("--metrics help does not say what the price is per: %q", f.Usage)
	}
}

// --demo makes no AWS calls, so the flag has to reach the fixture instead of
// the enricher — otherwise the metric-bearing report is only reachable by
// spending money.
func TestDemoWithMetricsWritesReadings(t *testing.T) {
	dir := t.TempDir()
	cmd := scanCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--demo", "--metrics", "--no-open", "--no-history", "--formats", "json", "-o", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("scan --demo --metrics: %v", err)
	}

	snap := readOnlySnapshot(t, dir)
	observed, zero := 0, 0
	for _, r := range snap.Resources {
		v, ok := r.Measure(model.MeasureFreeStorageBytes)
		if !ok {
			continue
		}
		observed++
		if v == 0 {
			zero++
		}
		if r.Type != model.TypeRDSInstance {
			t.Errorf("%s (%s) got a free-storage reading it cannot have", r.Name, r.Type)
		}
		// A reading with no observation time is one whose staleness cannot be
		// judged, which is the whole reason the fixture carries one.
		if at, ok := r.MeasureAsOf(model.MeasureFreeStorageBytes); !ok || at.IsZero() {
			t.Errorf("%s has a reading with no observation time", r.Name)
		}
	}
	if observed == 0 {
		t.Fatal("--demo --metrics produced no readings")
	}
	// A volume with exactly zero bytes free is a real finding, and the fixture
	// carries one so every renderer has to survive it.
	if zero == 0 {
		t.Error("no fixture resource reports zero free bytes; the stored-zero path is uncovered")
	}
}

// Without the flag the fixture stays as it was, so a report rendered from it
// cannot show a number no scan asked for.
func TestDemoWithoutMetricsHasNoReadings(t *testing.T) {
	dir := t.TempDir()
	cmd := scanCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--demo", "--no-open", "--no-history", "--formats", "json", "-o", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("scan --demo: %v", err)
	}

	for _, r := range readOnlySnapshot(t, dir).Resources {
		if _, ok := r.Measure(model.MeasureFreeStorageBytes); ok {
			t.Fatalf("%s has a free-storage reading without --metrics", r.Name)
		}
	}
}

func readOnlySnapshot(t *testing.T, dir string) *model.Snapshot {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("glob %s: %d matches, %v", dir, len(matches), err)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("reading %s: %v", matches[0], err)
	}
	var snap model.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("decoding %s: %v", matches[0], err)
	}
	return &snap
}
