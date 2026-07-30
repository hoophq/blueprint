package enrich

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/hoophq/blueprint/internal/model"
)

// S3 is the first service whose reading is a sum rather than a single series:
// BucketSizeBytes exists once per storage class and has no all-classes rollup,
// so a bucket's size is only correct once every class it publishes has been
// found, asked for, and answered. These tests cover that path — discovery-time
// expansion, the rollups that must stay out of the sum, and the batch boundary
// a large estate will cross.

const (
	bucketDimension  = "BucketName"
	storageDimension = "StorageType"

	sizeMetric    = "BucketSizeBytes"
	objectsMetric = "NumberOfObjects"

	// The dimension value S3 publishes the object count against. Unlike the
	// size, that one metric does have an all-classes rollup.
	allClasses = "AllStorageTypes"
)

// s3Classes are the storage classes the fixtures below advertise, in the order
// CloudWatch happens to list them — which is deliberately not sorted, so a test
// that passes only because the fixture was pre-ordered will not.
var s3Classes = []string{"StandardIAStorage", "GlacierStorage", "StandardStorage"}

// s3ClassBytes is what each class reports. The values differ per class so a sum
// that silently drops or double-counts one cannot land on the right total.
var s3ClassBytes = map[string]int64{
	"StandardStorage":   4_000_000_000,
	"StandardIAStorage": 1_500_000_000,
	"GlacierStorage":    500_000_000,
}

// s3TotalBytes is what a whole bucket should come to.
var s3TotalBytes = func() int64 {
	var n int64
	for _, c := range s3Classes {
		n += s3ClassBytes[c]
	}
	return n
}()

func bucket(account, region, name string) model.Resource {
	return model.Resource{
		// No account and no region in the ARN, exactly as S3 writes it.
		ARN:       "arn:aws:s3:::" + name,
		Service:   model.ServiceS3,
		Type:      model.TypeS3Bucket,
		Name:      name,
		Region:    region,
		AccountID: account,
	}
}

func s3Series(name string, dims map[string]string) cwtypes.Metric {
	return cwtypes.Metric{
		Namespace:  aws.String("AWS/S3"),
		MetricName: aws.String(name),
		Dimensions: dimensionList(dims),
	}
}

// knowsBuckets builds a ListMetrics response for S3: one BucketSizeBytes series
// per storage class per bucket, one NumberOfObjects series against the
// all-classes rollup, and two poisoned series that must never join a sum.
//
// The poison is the point of the fixture. CloudWatch really does publish
// BucketSizeBytes against wider and narrower dimension sets than the per-class
// one — an Intelligent-Tiering filter adds a dimension, and there are shapes
// that name no bucket at all. Either would inflate a bucket's size while still
// looking like an answer, so expansion has to match the planned dimensions
// exactly plus the enumerated one, and nothing else.
//
// The response honours the requested metric name, because discovery files what
// comes back under the name it asked for.
func knowsBuckets(classes []string, names ...string) func(*cloudwatch.ListMetricsInput) (*cloudwatch.ListMetricsOutput, error) {
	return func(in *cloudwatch.ListMetricsInput) (*cloudwatch.ListMetricsOutput, error) {
		out := &cloudwatch.ListMetricsOutput{}
		switch aws.ToString(in.MetricName) {
		case sizeMetric:
			// A class-only series: no bucket named, so it is some broader
			// aggregate and belongs to no bucket in particular.
			out.Metrics = append(out.Metrics, s3Series(sizeMetric, map[string]string{
				storageDimension: poisonClass,
			}))
			for _, b := range names {
				for _, c := range classes {
					out.Metrics = append(out.Metrics, s3Series(sizeMetric, map[string]string{
						bucketDimension: b, storageDimension: c,
					}))
				}
				// A real bucket, a real class, and one dimension more.
				out.Metrics = append(out.Metrics, s3Series(sizeMetric, map[string]string{
					bucketDimension: b, storageDimension: poisonClass, "FilterId": "tiering",
				}))
			}
		case objectsMetric:
			for _, b := range names {
				out.Metrics = append(out.Metrics, s3Series(objectsMetric, map[string]string{
					bucketDimension: b, storageDimension: allClasses,
				}))
			}
		}
		return out, nil
	}
}

// poisonClass is the storage class the two rollup series above are published
// against. It is not in s3Classes, so if either ever joined a bucket's sum the
// extra bytes would be traceable to exactly one fixture entry.
const poisonClass = "IntelligentTieringAAStorage"

const poisonBytes = 900_000_000_000

func s3Key(metric, bucketName, class string) string {
	return metric + "/" + bucketName + "/" + class
}

// answersS3 replies with the datapoints registered for each series. S3's
// dimensions are two deep, so the RDS helper's single-dimension key cannot
// address them; a series absent from the map gets no result at all, the way
// CloudWatch answers for a series with nothing in the window.
func answersS3(data map[string][]point) func(*cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
	return func(in *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
		out := &cloudwatch.GetMetricDataOutput{}
		for _, q := range in.MetricDataQueries {
			dims := map[string]string{}
			for _, d := range q.MetricStat.Metric.Dimensions {
				dims[aws.ToString(d.Name)] = aws.ToString(d.Value)
			}
			key := s3Key(aws.ToString(q.MetricStat.Metric.MetricName), dims[bucketDimension], dims[storageDimension])
			pts, ok := data[key]
			if !ok {
				continue
			}
			res := cwtypes.MetricDataResult{Id: q.Id, StatusCode: cwtypes.StatusCodeComplete}
			for _, p := range pts {
				res.Timestamps = append(res.Timestamps, p.at)
				res.Values = append(res.Values, p.v)
			}
			out.MetricDataResults = append(out.MetricDataResults, res)
		}
		return out, nil
	}
}

// wholeBucket registers a full set of readings for one bucket: every storage
// class in s3Classes, the object count, and — always — the two poisoned series,
// so every test that uses it is also a test that they stayed out.
func wholeBucket(data map[string][]point, name string, objects float64, at time.Time) {
	for _, c := range s3Classes {
		data[s3Key(sizeMetric, name, c)] = []point{{at: at, v: float64(s3ClassBytes[c])}}
	}
	data[s3Key(objectsMetric, name, allClasses)] = []point{{at: at, v: objects}}
	data[s3Key(sizeMetric, name, poisonClass)] = []point{{at: at, v: poisonBytes}}
	data[s3Key(sizeMetric, "", poisonClass)] = []point{{at: at, v: poisonBytes}}
}

// dimsOf reads a sent query's dimensions back into a map.
func dimsOf(q cwtypes.MetricDataQuery) map[string]string {
	out := map[string]string{}
	for _, d := range q.MetricStat.Metric.Dimensions {
		out[aws.ToString(d.Name)] = aws.ToString(d.Value)
	}
	return out
}

// sentQueries flattens every query the fake was asked, across all batches.
func sentQueries(api *fakeCW) []cwtypes.MetricDataQuery {
	api.mu.Lock()
	defer api.mu.Unlock()
	var out []cwtypes.MetricDataQuery
	for _, in := range api.gets {
		out = append(out, in.MetricDataQueries...)
	}
	return out
}

func TestEnumeratedQueryExpandsToEveryDiscoveredStorageClass(t *testing.T) {
	at := scanTime.Add(-26 * time.Hour)
	data := map[string][]point{}
	wholeBucket(data, "assets", 18_442_017, at)

	api := &fakeCW{listFn: knowsBuckets(s3Classes, "assets"), getFn: answersS3(data)}
	resources := []model.Resource{bucket("111111111111", "us-east-1", "assets")}

	_, failures := run(t, api, resources)

	if len(failures) != 0 {
		t.Fatalf("unexpected ledger entries: %+v", failures)
	}
	if v, ok := resources[0].Measure(model.MeasureSizeBytes); !ok || v != s3TotalBytes {
		t.Errorf("size_bytes = (%d, %v), want (%d, true)", v, ok, s3TotalBytes)
	}
	if v, ok := resources[0].Measure(model.MeasureObjectCount); !ok || v != 18_442_017 {
		t.Errorf("object_count = (%d, %v), want the reported count", v, ok)
	}
	// One query per class plus the object count — the size template resolved
	// into three requests, not one.
	if got, want := len(sentQueries(api)), len(s3Classes)+1; got != want {
		t.Errorf("sent %d queries, want %d", got, want)
	}
}

// The two rollups in the fixture share a metric name with the per-class series
// and would each add most of a terabyte to a four-gigabyte bucket.
func TestEnumeratedExpansionRejectsARollup(t *testing.T) {
	at := scanTime.Add(-26 * time.Hour)
	data := map[string][]point{}
	wholeBucket(data, "assets", 12, at)

	api := &fakeCW{listFn: knowsBuckets(s3Classes, "assets"), getFn: answersS3(data)}
	resources := []model.Resource{bucket("111111111111", "us-east-1", "assets")}

	run(t, api, resources)

	if v, _ := resources[0].Measure(model.MeasureSizeBytes); v != s3TotalBytes {
		t.Errorf("size_bytes = %d, want %d — a rollup joined the sum", v, s3TotalBytes)
	}
	for _, q := range sentQueries(api) {
		dims := dimsOf(q)
		if _, ok := dims["FilterId"]; ok {
			t.Errorf("asked for a filtered series: %v", dims)
		}
		if dims[bucketDimension] == "" {
			t.Errorf("asked for a series naming no bucket: %v", dims)
		}
		if dims[storageDimension] == poisonClass {
			t.Errorf("asked for a class no bucket in this fixture publishes: %v", dims)
		}
	}
}

// A class CloudWatch lists but that reported nothing in the window is an
// answered series contributing zero, not a hole: a storage class emptied last
// week keeps its listing for a while. The bucket's size is still whole.
func TestAStorageClassWithNoDatapointStillCountsAsAnswered(t *testing.T) {
	at := scanTime.Add(-26 * time.Hour)
	data := map[string][]point{}
	wholeBucket(data, "assets", 12, at)
	delete(data, s3Key(sizeMetric, "assets", "GlacierStorage"))

	api := &fakeCW{listFn: knowsBuckets(s3Classes, "assets"), getFn: answersS3(data)}
	resources := []model.Resource{bucket("111111111111", "us-east-1", "assets")}

	run(t, api, resources)

	want := s3TotalBytes - s3ClassBytes["GlacierStorage"]
	if v, ok := resources[0].Measure(model.MeasureSizeBytes); !ok || v != want {
		t.Errorf("size_bytes = (%d, %v), want (%d, true)", v, ok, want)
	}
}

// Nothing answered at all is different: there is no reading to round down to,
// so the measure stays absent rather than reading as an empty bucket.
func TestABucketThatReportedNothingHasNoSize(t *testing.T) {
	api := &fakeCW{listFn: knowsBuckets(s3Classes, "assets"), getFn: answersS3(nil)}
	resources := []model.Resource{bucket("111111111111", "us-east-1", "assets")}

	run(t, api, resources)

	if v, ok := resources[0].Measure(model.MeasureSizeBytes); ok {
		t.Errorf("size_bytes = %d, want absent", v)
	}
	if raw := resources[0].Attr(model.MeasureSizeBytes + model.AsOfSuffix); raw != "" {
		t.Errorf("orphan observation time %q with no measure", raw)
	}
}

// An empty bucket still publishes its metrics. Zero bytes across zero objects
// is a reading, and it has to survive as one.
func TestABucketReportedAsZeroBytesKeepsTheZero(t *testing.T) {
	at := scanTime.Add(-26 * time.Hour)
	data := map[string][]point{}
	for _, c := range s3Classes {
		data[s3Key(sizeMetric, "scratch", c)] = []point{{at: at, v: 0}}
	}
	data[s3Key(objectsMetric, "scratch", allClasses)] = []point{{at: at, v: 0}}

	api := &fakeCW{listFn: knowsBuckets(s3Classes, "scratch"), getFn: answersS3(data)}
	resources := []model.Resource{bucket("111111111111", "us-east-1", "scratch")}

	run(t, api, resources)

	if v, ok := resources[0].Measure(model.MeasureSizeBytes); !ok || v != 0 {
		t.Errorf("size_bytes = (%d, %v), want (0, true) — an empty bucket is a finding", v, ok)
	}
	if v, ok := resources[0].Measure(model.MeasureObjectCount); !ok || v != 0 {
		t.Errorf("object_count = (%d, %v), want (0, true)", v, ok)
	}
	if _, ok := resources[0].MeasureAsOf(model.MeasureSizeBytes); !ok {
		t.Error("a stored zero lost its observation time")
	}
}

// A sum is only as current as its stalest part, so the oldest contributing
// datapoint sets the age — anything else would present a week-old class as if
// it had been read this morning.
func TestTheOldestContributingObservationSetsTheAge(t *testing.T) {
	oldest := scanTime.Add(-70 * time.Hour)
	data := map[string][]point{
		s3Key(sizeMetric, "assets", "StandardStorage"):   {{at: scanTime.Add(-26 * time.Hour), v: 1}},
		s3Key(sizeMetric, "assets", "StandardIAStorage"): {{at: oldest, v: 1}},
		s3Key(sizeMetric, "assets", "GlacierStorage"):    {{at: scanTime.Add(-48 * time.Hour), v: 1}},
	}

	api := &fakeCW{listFn: knowsBuckets(s3Classes, "assets"), getFn: answersS3(data)}
	resources := []model.Resource{bucket("111111111111", "us-east-1", "assets")}

	run(t, api, resources)

	at, ok := resources[0].MeasureAsOf(model.MeasureSizeBytes)
	if !ok || !at.Equal(oldest) {
		t.Errorf("observed at = (%v, %v), want %v", at, ok, oldest)
	}
}

// The object count is a plain spec, not an enumerated one, because it is the
// one S3 storage metric with an all-classes rollup. Asking for it per class and
// summing would double-count every object.
func TestObjectCountUsesTheAllStorageTypesDimension(t *testing.T) {
	at := scanTime.Add(-26 * time.Hour)
	data := map[string][]point{}
	wholeBucket(data, "assets", 1_240, at)

	api := &fakeCW{listFn: knowsBuckets(s3Classes, "assets"), getFn: answersS3(data)}
	resources := []model.Resource{bucket("111111111111", "us-east-1", "assets")}

	run(t, api, resources)

	asked := 0
	for _, q := range sentQueries(api) {
		if aws.ToString(q.MetricStat.Metric.MetricName) != objectsMetric {
			continue
		}
		asked++
		dims := dimsOf(q)
		if len(dims) != 2 || dims[bucketDimension] != "assets" || dims[storageDimension] != allClasses {
			t.Errorf("object count dimensions = %v, want the bucket plus %s", dims, allClasses)
		}
	}
	if asked != 1 {
		t.Errorf("asked for the object count %d times, want once", asked)
	}
}

// estate is a census large enough that one bucket's storage classes fall either
// side of a batch boundary.
type estate struct {
	resources []model.Resource
	names     []string
	classes   []string
	data      map[string][]point
	// straddler indexes the bucket whose series cross the boundary.
	straddler int
	// total is what every bucket in the estate should sum to.
	total int64
}

// s3Estate builds that census. Six classes and the object count make seven
// series per bucket, chosen because seven does not divide the batch size —
// which is what puts a bucket astride the boundary rather than neatly after it.
//
// Its own storage classes, not the shared ones, so the fixture cannot be
// perturbed by whatever another test decided a class was worth.
func s3Estate(t *testing.T) estate {
	t.Helper()
	e := estate{data: map[string][]point{}}
	bytes := map[string]int64{}
	for i := 0; i < 6; i++ {
		c := fmt.Sprintf("Class%dStorage", i)
		e.classes = append(e.classes, c)
		bytes[c] = int64(1_000_000 * (i + 1))
		e.total += bytes[c]
	}

	perBucket := len(e.classes) + 1
	if maxQueriesPerCall%perBucket == 0 {
		t.Fatalf("fixture cannot straddle: %d series per bucket divides the %d batch size", perBucket, maxQueriesPerCall)
	}
	e.straddler = maxQueriesPerCall / perBucket

	at := scanTime.Add(-26 * time.Hour)
	for i := 0; i <= e.straddler; i++ {
		name := fmt.Sprintf("bucket-%03d", i)
		e.names = append(e.names, name)
		e.resources = append(e.resources, bucket("111111111111", "us-east-1", name))
		for _, c := range e.classes {
			e.data[s3Key(sizeMetric, name, c)] = []point{{at: at, v: float64(bytes[c])}}
		}
		e.data[s3Key(objectsMetric, name, allClasses)] = []point{{at: at, v: float64(i)}}
	}
	return e
}

// The bucket whose classes fall either side of a 500-query call must come out
// whole. Writing each batch as it returned would store the first three classes
// as the bucket's size — a smaller number wearing the same clothes as a real
// one.
func TestASizeSpanningTwoBatchesIsSummedOnce(t *testing.T) {
	e := s3Estate(t)
	api := &fakeCW{listFn: knowsBuckets(e.classes, e.names...), getFn: answersS3(e.data)}

	_, failures := run(t, api, e.resources)

	if len(failures) != 0 {
		t.Fatalf("unexpected ledger entries: %+v", failures)
	}
	if _, gets := api.calls(); gets < 2 {
		t.Fatalf("GetMetricData calls = %d, want the fixture to have crossed a batch boundary", gets)
	}
	for _, r := range e.resources {
		if v, ok := r.Measure(model.MeasureSizeBytes); !ok || v != e.total {
			t.Fatalf("%s: size_bytes = (%d, %v), want (%d, true)", r.Name, v, ok, e.total)
		}
	}
	if v, ok := e.resources[e.straddler].Measure(model.MeasureObjectCount); !ok || v != int64(e.straddler) {
		t.Errorf("straddling bucket object_count = (%d, %v), want (%d, true)", v, ok, e.straddler)
	}
}

// And when the second call fails, the straddling bucket must report no size at
// all rather than the half that did arrive.
func TestAFailedBatchBlocksThePartialSum(t *testing.T) {
	e := s3Estate(t)
	answer := answersS3(e.data)
	calls := 0
	api := &fakeCW{
		listFn: knowsBuckets(e.classes, e.names...),
		getFn: func(in *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			calls++
			if calls > 1 {
				return nil, errors.New("throttled")
			}
			return answer(in)
		},
	}

	_, failures := run(t, api, e.resources)

	if v, ok := e.resources[e.straddler].Measure(model.MeasureSizeBytes); ok {
		t.Errorf("straddling bucket size_bytes = %d, want absent — this is a partial sum", v)
	}
	if v, ok := e.resources[e.straddler].Measure(model.MeasureObjectCount); ok {
		t.Errorf("straddling bucket object_count = %d, want absent", v)
	}
	// A bucket entirely inside the batch that did arrive keeps its reading: one
	// failed call is not a reason to throw away the answers that came back.
	if v, ok := e.resources[0].Measure(model.MeasureSizeBytes); !ok || v != e.total {
		t.Errorf("first bucket size_bytes = (%d, %v), want (%d, true)", v, ok, e.total)
	}
	// And the gap is disclosed rather than left as a bucket that looks sizeless.
	if len(failures) == 0 {
		t.Fatal("a failed batch left no ledger entry")
	}
	if !strings.Contains(failures[0].Error, "GetMetricData") {
		t.Errorf("ledger entry = %q, want it to name the failed call", failures[0].Error)
	}
}

// Without discovery an enumerated spec names no series at all: it is a template
// over a dimension whose values only CloudWatch knows. Widening it to the
// dimensions it does have would match a rollup, so it is dropped — and said out
// loud, because a bucket silently missing its size looks like an empty one.
func TestEnumeratedQueriesAreDroppedWhenDiscoveryFails(t *testing.T) {
	at := scanTime.Add(-26 * time.Hour)
	data := map[string][]point{}
	wholeBucket(data, "assets", 1_240, at)

	api := &fakeCW{
		listFn: func(*cloudwatch.ListMetricsInput) (*cloudwatch.ListMetricsOutput, error) {
			return nil, errors.New("AccessDenied")
		},
		getFn: answersS3(data),
	}
	resources := []model.Resource{bucket("111111111111", "us-east-1", "assets")}

	_, failures := run(t, api, resources)

	if v, ok := resources[0].Measure(model.MeasureSizeBytes); ok {
		t.Errorf("size_bytes = %d, want absent — an unexpanded template cannot be asked for", v)
	}
	// The plain spec still goes out. Discovery only filters those, and failing
	// open costs a query that may come back empty.
	if v, ok := resources[0].Measure(model.MeasureObjectCount); !ok || v != 1_240 {
		t.Errorf("object_count = (%d, %v), want the reported count", v, ok)
	}

	var announced bool
	for _, f := range failures {
		if strings.Contains(f.Error, "need discovery to enumerate") {
			announced = true
		}
	}
	if !announced {
		t.Errorf("dropped series left no ledger entry: %+v", failures)
	}
}

func TestIndexByBase(t *testing.T) {
	sets := []map[string]string{
		{bucketDimension: "a", storageDimension: "StandardStorage"},
		{bucketDimension: "a", storageDimension: "GlacierStorage"},
		{bucketDimension: "b", storageDimension: "StandardStorage"},
		// Wider: one dimension more than the planned set plus the enumerated
		// one, so it belongs to no bucket's group.
		{bucketDimension: "a", storageDimension: "StandardStorage", "FilterId": "tiering"},
		// Narrower: names no bucket.
		{storageDimension: "StandardStorage"},
		// Does not carry the enumerated dimension at all.
		{bucketDimension: "a"},
	}

	idx := indexByBase(sets, storageDimension)

	got := len(idx[baseSignature(map[string]string{bucketDimension: "a"})])
	if got != 2 {
		t.Errorf("bucket a matched %d series, want its 2 storage classes", got)
	}
	if got := len(idx[baseSignature(map[string]string{bucketDimension: "b"})]); got != 1 {
		t.Errorf("bucket b matched %d series, want 1", got)
	}
	// A spec that enumerates a dimension it has already pinned has nothing to
	// enumerate over, and must expand to nothing rather than to everything.
	pinned := map[string]string{bucketDimension: "a", storageDimension: "StandardStorage"}
	if got := len(idx[baseSignature(pinned)]); got != 0 {
		t.Errorf("a pinned enumerated dimension matched %d series, want 0", got)
	}
}

// The registry is what decides which of the two S3 metrics is summed and which
// is read straight, so it is worth pinning directly: swapping them would be
// invisible in a single-class fixture and wrong on every real bucket.
func TestS3SpecsEnumerateOnlyTheSize(t *testing.T) {
	specs := metricSpecs[model.TypeS3Bucket]
	if len(specs) != 2 {
		t.Fatalf("S3 has %d specs, want 2", len(specs))
	}
	byName := map[string]Spec{}
	for _, s := range specs {
		byName[s.MetricName] = s
	}
	if got := byName[sizeMetric].Enumerate; got != storageDimension {
		t.Errorf("%s enumerates %q, want %q — it has no all-classes rollup", sizeMetric, got, storageDimension)
	}
	if got := byName[objectsMetric].Enumerate; got != "" {
		t.Errorf("%s enumerates %q, want it read straight from the rollup", objectsMetric, got)
	}
	if byName[sizeMetric].Measure != model.MeasureSizeBytes || byName[objectsMetric].Measure != model.MeasureObjectCount {
		t.Error("the two S3 specs write each other's measures")
	}
}
