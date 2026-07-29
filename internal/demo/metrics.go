package demo

import (
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

// AddMetrics attaches CloudWatch-style readings to the fixture, the way
// `--metrics` would after a real scan.
//
// Only RDS instances get one, because only they have a FreeStorageSpace spec
// in internal/enrich — a fixture that invented readings for clusters and
// tables would let a renderer look correct against data the scan can never
// produce.
//
// The three shapes below exist to keep the honesty rules covered end to end:
// a healthy volume, a volume reported as exactly zero bytes free, and a
// reading old enough that a report has to disclose its age. Every fixture RDS
// instance gets one of them, cycling in census order.
func AddMetrics(snap *model.Snapshot) {
	if snap == nil {
		return
	}
	// Ages are relative to the scan, since that is what a staleness judgement
	// compares against, and are all inside the enricher's 72-hour lookback —
	// nothing older can be observed, so nothing older belongs in the fixture.
	shapes := []struct {
		// free derives the observed value from the volume's size, so the
		// fixture never claims more free space than a disk has.
		free func(sizeBytes int64) int64
		age  time.Duration
	}{
		{func(size int64) int64 { return size / 4 }, 26 * time.Hour},
		{func(int64) int64 { return 0 }, 26 * time.Hour},
		{func(size int64) int64 { return size / 50 }, 68 * time.Hour},
	}

	n := 0
	for i := range snap.Resources {
		r := &snap.Resources[i]
		if r.Type != model.TypeRDSInstance {
			continue
		}
		size, ok := r.Measure(model.MeasureSizeBytes)
		if !ok {
			continue
		}
		s := shapes[n%len(shapes)]
		n++
		r.SetObservedMeasure(model.MeasureFreeStorageBytes, s.free(size), snap.GeneratedAt.Add(-s.age))
	}

	// Re-finalize: the fixture is meant to be indistinguishable from a real
	// snapshot, and a real one has every derivation run after enrichment.
	snap.Finalize()
}
