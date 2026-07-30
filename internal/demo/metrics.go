package demo

import (
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

// AddMetrics attaches CloudWatch-style readings to the fixture, the way
// `--metrics` would after a real scan.
//
// Only RDS instances and S3 buckets get readings, because only they have specs
// in internal/enrich — a fixture that invented readings for clusters and
// tables would let a renderer look correct against data the scan can never
// produce.
//
// The three RDS shapes below exist to keep the honesty rules covered end to
// end: a healthy volume, a volume reported as exactly zero bytes free, and a
// reading old enough that a report has to disclose its age. Every fixture RDS
// instance gets one of them, cycling in census order.
func AddMetrics(snap *model.Snapshot) {
	if snap == nil {
		return
	}
	addRDSMetrics(snap)
	addS3Metrics(snap)

	// Re-finalize: the fixture is meant to be indistinguishable from a real
	// snapshot, and a real one has every derivation run after enrichment.
	snap.Finalize()
}

func addRDSMetrics(snap *model.Snapshot) {
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
}

// addS3Metrics attaches the two readings a bucket's size and object count come
// from. Both are CloudWatch daily storage metrics, so a bucket has no size at
// all until --metrics runs — which is the whole reason the base fixture leaves
// them absent.
//
// Keyed by bucket name rather than cycled, because for S3 the specific numbers
// are the point:
//
//   - acme-prod-assets is four terabytes, the estate's largest single thing
//     and invisible to a control-plane-only census.
//   - acme-etl-scratch is a stored zero, twice over. An empty bucket still
//     publishes its metrics, so 0 bytes across 0 objects is a reading, not a
//     gap, and it has to survive every formatter between here and the report.
//   - acme-staging-uploads is 917 bytes across 3 objects — small enough that a
//     formatter rounding to the nearest kilobyte would erase it.
//   - acme-nfe-documentos is deliberately absent. Its metrics simply were not
//     published in the window, which is a normal thing for CloudWatch storage
//     metrics on a young or idle bucket, and it puts "no reading" next to
//     "a reading of zero" where the difference is visible.
func addS3Metrics(snap *model.Snapshot) {
	// Ages sit inside the enricher's 72-hour lookback, and are staggered so the
	// report has more than one observation time to render. S3 storage metrics
	// land once a day, so even a fresh one is a day behind.
	readings := map[string]struct {
		bytes   int64
		objects int64
		age     time.Duration
	}{
		"acme-prod-assets":        {4_617_089_388_544, 18_442_017, 26 * time.Hour},
		"acme-public-downloads":   {2_255_388_672, 1_240, 26 * time.Hour},
		"acme-cloudtrail-archive": {812_301_615_104, 9_884_552, 50 * time.Hour},
		"acme-etl-scratch":        {0, 0, 26 * time.Hour},
		"acme-backup-vault":       {966_367_641_600, 4_418_009, 50 * time.Hour},
		"acme-staging-uploads":    {917, 3, 68 * time.Hour},
	}

	for i := range snap.Resources {
		r := &snap.Resources[i]
		if r.Type != model.TypeS3Bucket {
			continue
		}
		v, ok := readings[r.Name]
		if !ok {
			continue
		}
		at := snap.GeneratedAt.Add(-v.age)
		r.SetObservedMeasure(model.MeasureSizeBytes, v.bytes, at)
		r.SetObservedMeasure(model.MeasureObjectCount, v.objects, at)
	}
}
