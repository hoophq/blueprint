package enrich

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// scanTime is the instant every test pretends the scan happened at, so query
// windows and ledger timestamps are assertable.
var scanTime = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

const dbDimension = "DBInstanceIdentifier"

type fakeCW struct {
	mu     sync.Mutex
	listFn func(*cloudwatch.ListMetricsInput) (*cloudwatch.ListMetricsOutput, error)
	getFn  func(*cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error)
	lists  []*cloudwatch.ListMetricsInput
	gets   []*cloudwatch.GetMetricDataInput
}

func (f *fakeCW) ListMetrics(_ context.Context, in *cloudwatch.ListMetricsInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error) {
	f.mu.Lock()
	f.lists = append(f.lists, in)
	fn := f.listFn
	f.mu.Unlock()
	if fn == nil {
		return &cloudwatch.ListMetricsOutput{}, nil
	}
	return fn(in)
}

func (f *fakeCW) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	f.mu.Lock()
	f.gets = append(f.gets, in)
	fn := f.getFn
	f.mu.Unlock()
	if fn == nil {
		return &cloudwatch.GetMetricDataOutput{}, nil
	}
	return fn(in)
}

func (f *fakeCW) calls() (lists, gets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lists), len(f.gets)
}

// knows builds a ListMetrics response advertising FreeStorageSpace for each
// named instance, plus one engine-level rollup that shares the metric name but
// not the dimension set — the thing an inexact signature would wrongly match.
func knows(names ...string) func(*cloudwatch.ListMetricsInput) (*cloudwatch.ListMetricsOutput, error) {
	return func(*cloudwatch.ListMetricsInput) (*cloudwatch.ListMetricsOutput, error) {
		out := &cloudwatch.ListMetricsOutput{Metrics: []cwtypes.Metric{{
			Namespace:  aws.String("AWS/RDS"),
			MetricName: aws.String("FreeStorageSpace"),
			Dimensions: []cwtypes.Dimension{{Name: aws.String("EngineName"), Value: aws.String("mysql")}},
		}}}
		for _, n := range names {
			out.Metrics = append(out.Metrics, cwtypes.Metric{
				Namespace:  aws.String("AWS/RDS"),
				MetricName: aws.String("FreeStorageSpace"),
				Dimensions: []cwtypes.Dimension{{Name: aws.String(dbDimension), Value: aws.String(n)}},
			})
		}
		return out, nil
	}
}

type point struct {
	at time.Time
	v  float64
}

// answers replies with the datapoints registered for each instance. An
// instance mapped to an empty slice gets an empty result, the way CloudWatch
// reports a series it knows about but has no data for; one absent from the map
// gets no result at all.
func answers(data map[string][]point) func(*cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
	return func(in *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
		out := &cloudwatch.GetMetricDataOutput{}
		for _, q := range in.MetricDataQueries {
			name := ""
			for _, d := range q.MetricStat.Metric.Dimensions {
				if aws.ToString(d.Name) == dbDimension {
					name = aws.ToString(d.Value)
				}
			}
			pts, ok := data[name]
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

func instance(account, region, name string) model.Resource {
	return model.Resource{
		ARN:       "arn:aws:rds:" + region + ":" + account + ":db:" + name,
		Service:   model.ServiceRDS,
		Type:      model.TypeRDSInstance,
		Name:      name,
		Region:    region,
		AccountID: account,
	}
}

// run drives the stage over resources with one target per distinct account.
func run(t *testing.T, api API, resources []model.Resource) (*Metrics, []model.Failure) {
	t.Helper()
	seen := map[string]bool{}
	var targets []scan.Target
	for _, r := range resources {
		if !seen[r.AccountID] {
			seen[r.AccountID] = true
			targets = append(targets, scan.Target{AccountID: r.AccountID, Cfg: aws.Config{}})
		}
	}
	m := &Metrics{
		NewClient: func(aws.Config) API { return api },
		Now:       func() time.Time { return scanTime },
	}
	failures := m.Enrich(context.Background(), scan.Enrichment{
		Targets: targets, Resources: resources, Concurrency: 4,
	})
	return m, failures
}

func TestNewestDatapointBecomesAnObservedMeasure(t *testing.T) {
	// Out of order on purpose: correctness must come from comparing
	// timestamps, not from trusting the response's order.
	api := &fakeCW{listFn: knows("db1"), getFn: answers(map[string][]point{
		"db1": {
			{at: scanTime.Add(-48 * time.Hour), v: 9e9},
			{at: scanTime.Add(-24 * time.Hour), v: 4_294_967_296},
			{at: scanTime.Add(-72 * time.Hour), v: 1e9},
		},
	})}
	resources := []model.Resource{instance("111111111111", "us-east-1", "db1")}

	_, failures := run(t, api, resources)

	if len(failures) != 0 {
		t.Fatalf("unexpected ledger entries: %+v", failures)
	}
	if v, ok := resources[0].Measure(model.MeasureFreeStorageBytes); !ok || v != 4_294_967_296 {
		t.Errorf("free_storage_bytes = (%d, %v), want (4294967296, true)", v, ok)
	}
	at, ok := resources[0].MeasureAsOf(model.MeasureFreeStorageBytes)
	if !ok || !at.Equal(scanTime.Add(-24*time.Hour)) {
		t.Errorf("observed at = (%v, %v), want %v", at, ok, scanTime.Add(-24*time.Hour))
	}
	// The value is a day old and the artifact has to say so rather than let
	// GeneratedAt imply it is current.
	if at.Equal(scanTime) {
		t.Error("observation time collapsed onto the scan time")
	}
}

// A volume with nothing left is the finding this metric exists to surface. It
// must not be mistaken for a series that reported nothing.
func TestAReportedZeroSurvives(t *testing.T) {
	api := &fakeCW{listFn: knows("db1"), getFn: answers(map[string][]point{
		"db1": {{at: scanTime.Add(-24 * time.Hour), v: 0}},
	})}
	resources := []model.Resource{instance("111111111111", "us-east-1", "db1")}

	run(t, api, resources)

	v, ok := resources[0].Measure(model.MeasureFreeStorageBytes)
	if !ok || v != 0 {
		t.Errorf("free_storage_bytes = (%d, %v), want (0, true) — a full volume is a reading, not an absence", v, ok)
	}
	if _, ok := resources[0].MeasureAsOf(model.MeasureFreeStorageBytes); !ok {
		t.Error("a stored zero lost its observation time")
	}
}

func TestNoDatapointLeavesTheMeasureAbsent(t *testing.T) {
	// "db1" is known to CloudWatch but reported nothing in the window — a
	// stopped instance stops publishing.
	api := &fakeCW{listFn: knows("db1"), getFn: answers(map[string][]point{"db1": nil})}
	resources := []model.Resource{instance("111111111111", "us-east-1", "db1")}

	run(t, api, resources)

	if v, ok := resources[0].Measure(model.MeasureFreeStorageBytes); ok {
		t.Errorf("free_storage_bytes = %d, want absent", v)
	}
	if raw := resources[0].Attr(model.MeasureFreeStorageBytes + model.AsOfSuffix); raw != "" {
		t.Errorf("orphan observation time %q with no measure", raw)
	}
}

// Discovery is free and GetMetricData is not, so a series CloudWatch has never
// heard of must not be asked for. The rollup entry in knows() also has to stay
// unmatched: it shares the metric name but not the dimension set.
func TestDiscoveryFiltersOutSeriesThatDoNotExist(t *testing.T) {
	api := &fakeCW{listFn: knows("db1"), getFn: answers(map[string][]point{
		"db1": {{at: scanTime.Add(-24 * time.Hour), v: 512}},
		"db2": {{at: scanTime.Add(-24 * time.Hour), v: 999}},
	})}
	resources := []model.Resource{
		instance("111111111111", "us-east-1", "db1"),
		instance("111111111111", "us-east-1", "db2"),
	}

	run(t, api, resources)

	if _, gets := api.calls(); gets != 1 {
		t.Fatalf("GetMetricData calls = %d, want 1", gets)
	}
	if n := len(api.gets[0].MetricDataQueries); n != 1 {
		t.Errorf("queries in the batch = %d, want 1 (only the discovered series)", n)
	}
	if _, ok := resources[1].Measure(model.MeasureFreeStorageBytes); ok {
		t.Error("db2 got a measure despite not being discovered")
	}
}

// Discovery is an optimization, not a gate. If it fails, the census still
// wants the numbers — the alternative is a silent coverage gap to save a
// fraction of a cent.
func TestDiscoveryFailureFallsBackToQueryingEverything(t *testing.T) {
	api := &fakeCW{
		listFn: func(*cloudwatch.ListMetricsInput) (*cloudwatch.ListMetricsOutput, error) {
			return nil, errors.New("AccessDenied: cloudwatch:ListMetrics")
		},
		getFn: answers(map[string][]point{"db1": {{at: scanTime.Add(-24 * time.Hour), v: 777}}}),
	}
	resources := []model.Resource{instance("111111111111", "us-east-1", "db1")}

	_, failures := run(t, api, resources)

	if v, ok := resources[0].Measure(model.MeasureFreeStorageBytes); !ok || v != 777 {
		t.Errorf("free_storage_bytes = (%d, %v), want (777, true) despite failed discovery", v, ok)
	}
	if len(failures) != 1 {
		t.Fatalf("ledger entries = %d, want 1: %+v", len(failures), failures)
	}
	f := failures[0]
	if f.Service != MetricsService || f.Region != "us-east-1" || f.AccountID != "111111111111" {
		t.Errorf("ledger entry = %+v, want the metrics stage in us-east-1", f)
	}
	if f.Time.IsZero() {
		t.Error("ledger entry has no timestamp")
	}
}

func TestOnlyResourceTypesWithASpecAreQueried(t *testing.T) {
	api := &fakeCW{listFn: knows("db1")}
	aurora := instance("111111111111", "us-east-1", "cluster1")
	aurora.Service, aurora.Type = model.ServiceAurora, model.TypeRDSCluster
	table := instance("111111111111", "us-east-1", "table1")
	table.Service, table.Type = model.ServiceDynamoDB, model.TypeDynamoDBTable
	resources := []model.Resource{aurora, table, instance("111111111111", "us-east-1", "db1")}

	run(t, api, resources)

	if len(api.gets) != 1 || len(api.gets[0].MetricDataQueries) != 1 {
		t.Fatalf("GetMetricData calls = %d, want one call with one query", len(api.gets))
	}
	dims := api.gets[0].MetricDataQueries[0].MetricStat.Metric.Dimensions
	if len(dims) != 1 || aws.ToString(dims[0].Value) != "db1" {
		t.Errorf("queried dimensions = %v, want only db1 — Aurora has no FreeStorageSpace series", dims)
	}
}

// A census with nothing to enrich must not touch CloudWatch at all: an opt-in
// stage that still costs money on an estate it cannot help would be a trap.
func TestNoEligibleResourcesMakesNoCalls(t *testing.T) {
	api := &fakeCW{}
	table := instance("111111111111", "us-east-1", "table1")
	table.Service, table.Type = model.ServiceDynamoDB, model.TypeDynamoDBTable
	resources := []model.Resource{table}

	_, failures := run(t, api, resources)

	if lists, gets := api.calls(); lists != 0 || gets != 0 {
		t.Errorf("calls = (%d list, %d get), want none", lists, gets)
	}
	if len(failures) != 0 {
		t.Errorf("unexpected ledger entries: %+v", failures)
	}
}

// An instance whose identifier the scan never captured cannot be queried: a
// blank dimension value does not error, it matches a broader aggregate, so the
// query would attach somebody else's number.
func TestAnUnnamedResourceIsNotQueried(t *testing.T) {
	api := &fakeCW{listFn: knows("db1")}
	resources := []model.Resource{instance("111111111111", "us-east-1", "")}

	run(t, api, resources)

	if lists, gets := api.calls(); lists != 0 || gets != 0 {
		t.Errorf("calls = (%d list, %d get), want none for an unidentifiable series", lists, gets)
	}
}

func TestBatchesRespectTheFiveHundredQueryLimit(t *testing.T) {
	const total = 1201
	names := make([]string, total)
	resources := make([]model.Resource, total)
	for i := range resources {
		names[i] = "db" + strconv.Itoa(i)
		resources[i] = instance("111111111111", "us-east-1", names[i])
	}
	data := map[string][]point{}
	for _, n := range names {
		data[n] = []point{{at: scanTime.Add(-24 * time.Hour), v: 1024}}
	}
	api := &fakeCW{listFn: knows(names...), getFn: answers(data)}

	_, failures := run(t, api, resources)

	if len(failures) != 0 {
		t.Fatalf("unexpected ledger entries: %+v", failures)
	}
	var sizes []int
	for _, in := range api.gets {
		sizes = append(sizes, len(in.MetricDataQueries))
	}
	want := []int{maxQueriesPerCall, maxQueriesPerCall, total - 2*maxQueriesPerCall}
	if fmt.Sprint(sizes) != fmt.Sprint(want) {
		t.Errorf("batch sizes = %v, want %v", sizes, want)
	}
	// Ids only have to be unique within a call, so each batch restarts at q0.
	// If they ever collide, results silently overwrite each other.
	for _, in := range api.gets {
		seen := map[string]bool{}
		for _, q := range in.MetricDataQueries {
			id := aws.ToString(q.Id)
			if seen[id] {
				t.Fatalf("duplicate query id %q within one call", id)
			}
			seen[id] = true
		}
	}
	for i := range resources {
		if _, ok := resources[i].Measure(model.MeasureFreeStorageBytes); !ok {
			t.Fatalf("resource %d (%s) got no measure", i, resources[i].Name)
		}
	}
}

// Every batch and every scope asks about the same span, so which datapoint a
// resource gets never depends on how far into a slow scan its region was
// reached. Reading the clock per batch would let two instances in one account
// disagree about "latest" whenever a run straddled a daily bucket boundary,
// and would make the census report the scan's duration rather than the estate.
func TestTheQueryWindowIsFixedForTheWholeRun(t *testing.T) {
	const total = 1201 // three batches, so the clock has room to move between them
	names := make([]string, total)
	var resources []model.Resource
	for i := range names {
		names[i] = "db" + strconv.Itoa(i)
		resources = append(resources, instance("111111111111", "us-east-1", names[i]))
	}
	// A second account so concurrent scopes are in play too, not just batching.
	resources = append(resources, instance("222222222222", "eu-west-1", "db-other"))
	names = append(names, "db-other")

	api := &fakeCW{listFn: knows(names...), getFn: answers(map[string][]point{})}

	// A clock that advances a full day per reading — far more than any real
	// scan drifts, and enough that a per-batch EndTime could not be mistaken
	// for jitter.
	var mu sync.Mutex
	ticks := 0
	m := &Metrics{
		NewClient: func(aws.Config) API { return api },
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			ticks++
			return scanTime.Add(time.Duration(ticks) * 24 * time.Hour)
		},
	}
	m.Enrich(context.Background(), scan.Enrichment{
		Targets: []scan.Target{
			{AccountID: "111111111111", Cfg: aws.Config{}},
			{AccountID: "222222222222", Cfg: aws.Config{}},
		},
		Resources:   resources,
		Concurrency: 4,
	})

	if len(api.gets) < 4 {
		t.Fatalf("GetMetricData calls = %d, want at least 4 (three batches plus the second scope)", len(api.gets))
	}
	want := aws.ToTime(api.gets[0].EndTime)
	for i, in := range api.gets {
		if got := aws.ToTime(in.EndTime); !got.Equal(want) {
			t.Errorf("call %d EndTime = %v, want %v — the window moved mid-run", i, got, want)
		}
		// StartTime is EndTime minus the batch's own reach, so it has to track
		// the fixed end rather than a fresh clock reading.
		if got, wantStart := aws.ToTime(in.StartTime), want.Add(-defaultLookback); !got.Equal(wantStart) {
			t.Errorf("call %d StartTime = %v, want %v", i, got, wantStart)
		}
	}
}

// The query shape is the documented workaround for the Metrics Insights trap:
// a plain MetricStat over a multi-day window, never a SELECT … FROM SCHEMA
// expression, which would only see the last three hours and return empty.
func TestQueryIsAPlainMetricStatOverAMultiDayWindow(t *testing.T) {
	api := &fakeCW{listFn: knows("db1"), getFn: answers(map[string][]point{"db1": nil})}
	resources := []model.Resource{instance("111111111111", "us-east-1", "db1")}

	run(t, api, resources)

	if len(api.gets) != 1 {
		t.Fatalf("GetMetricData calls = %d, want 1", len(api.gets))
	}
	in := api.gets[0]
	if in.ScanBy != cwtypes.ScanByTimestampDescending {
		t.Errorf("ScanBy = %q, want TimestampDescending so the newest datapoint is on the first page", in.ScanBy)
	}
	if got := aws.ToTime(in.EndTime); !got.Equal(scanTime) {
		t.Errorf("EndTime = %v, want %v", got, scanTime)
	}
	if got, want := aws.ToTime(in.StartTime), scanTime.Add(-defaultLookback); !got.Equal(want) {
		t.Errorf("StartTime = %v, want %v (three daily buckets back)", got, want)
	}
	q := in.MetricDataQueries[0]
	if q.Expression != nil {
		t.Errorf("query carries expression %q; Metrics Insights only reads the last 3 hours and returns empty for daily metrics",
			aws.ToString(q.Expression))
	}
	if q.MetricStat == nil {
		t.Fatal("query has no MetricStat")
	}
	if got := aws.ToInt32(q.MetricStat.Period); got != defaultPeriod {
		t.Errorf("Period = %d, want %d", got, defaultPeriod)
	}
	if got := aws.ToString(q.MetricStat.Stat); got != "Minimum" {
		t.Errorf("Stat = %q, want Minimum — the tightest the volume got, not a smoothed average", got)
	}
	if got := aws.ToString(q.MetricStat.Metric.Namespace); got != "AWS/RDS" {
		t.Errorf("Namespace = %q, want AWS/RDS", got)
	}
	if !aws.ToBool(q.ReturnData) {
		t.Error("ReturnData is false, so the query would be billed and return nothing")
	}
}

// The mirror-image trap: RecentlyActive: "PT3H" is the only value the field
// accepts, and it would hide every metric published less often than that.
func TestDiscoveryDoesNotNarrowToRecentlyActiveMetrics(t *testing.T) {
	api := &fakeCW{listFn: knows("db1"), getFn: answers(map[string][]point{"db1": nil})}
	resources := []model.Resource{instance("111111111111", "us-east-1", "db1")}

	run(t, api, resources)

	if len(api.lists) != 1 {
		t.Fatalf("ListMetrics calls = %d, want 1", len(api.lists))
	}
	if in := api.lists[0]; in.RecentlyActive != "" {
		t.Errorf("RecentlyActive = %q, want unset so discovery keeps its two-week horizon", in.RecentlyActive)
	}
}

func TestDiscoveryFollowsPagination(t *testing.T) {
	var page int
	api := &fakeCW{
		listFn: func(in *cloudwatch.ListMetricsInput) (*cloudwatch.ListMetricsOutput, error) {
			page++
			if page == 1 {
				if in.NextToken != nil {
					t.Errorf("first page sent a token %q", aws.ToString(in.NextToken))
				}
				out, _ := knows("db1")(in)
				out.NextToken = aws.String("more")
				return out, nil
			}
			if aws.ToString(in.NextToken) != "more" {
				t.Errorf("second page token = %q, want more", aws.ToString(in.NextToken))
			}
			return knows("db2")(in)
		},
		getFn: answers(map[string][]point{
			"db1": {{at: scanTime.Add(-24 * time.Hour), v: 1}},
			"db2": {{at: scanTime.Add(-24 * time.Hour), v: 2}},
		}),
	}
	resources := []model.Resource{
		instance("111111111111", "us-east-1", "db1"),
		instance("111111111111", "us-east-1", "db2"),
	}

	run(t, api, resources)

	// db2 only exists on the second page: stopping at the first would drop it
	// as undiscovered, which looks exactly like a resource with no metrics.
	if v, ok := resources[1].Measure(model.MeasureFreeStorageBytes); !ok || v != 2 {
		t.Errorf("db2 free_storage_bytes = (%d, %v), want (2, true)", v, ok)
	}
}

func TestARegionalFailureDoesNotStopTheOtherRegions(t *testing.T) {
	api := &fakeCW{
		listFn: knows("db1", "db2"),
		getFn: func(in *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			for _, q := range in.MetricDataQueries {
				for _, d := range q.MetricStat.Metric.Dimensions {
					if aws.ToString(d.Value) == "db1" {
						return nil, errors.New("ThrottlingException: rate exceeded")
					}
				}
			}
			return answers(map[string][]point{"db2": {{at: scanTime.Add(-24 * time.Hour), v: 42}}})(in)
		},
	}
	resources := []model.Resource{
		instance("111111111111", "us-east-1", "db1"),
		instance("111111111111", "eu-west-1", "db2"),
	}

	_, failures := run(t, api, resources)

	if v, ok := resources[1].Measure(model.MeasureFreeStorageBytes); !ok || v != 42 {
		t.Errorf("eu-west-1 free_storage_bytes = (%d, %v), want (42, true)", v, ok)
	}
	if _, ok := resources[0].Measure(model.MeasureFreeStorageBytes); ok {
		t.Error("us-east-1 reported a measure despite the call failing")
	}
	if len(failures) != 1 || failures[0].Region != "us-east-1" {
		t.Fatalf("ledger = %+v, want one entry naming us-east-1", failures)
	}
}

func TestAccountWithoutCredentialsIsLedgered(t *testing.T) {
	api := &fakeCW{listFn: knows("db1")}
	resources := []model.Resource{instance("999999999999", "us-east-1", "db1")}

	m := &Metrics{
		NewClient: func(aws.Config) API { return api },
		Now:       func() time.Time { return scanTime },
	}
	failures := m.Enrich(context.Background(), scan.Enrichment{
		Targets:   []scan.Target{{AccountID: "111111111111", Cfg: aws.Config{}}},
		Resources: resources,
	})

	if lists, gets := api.calls(); lists != 0 || gets != 0 {
		t.Errorf("calls = (%d list, %d get), want none for an account with no credentials", lists, gets)
	}
	if len(failures) != 1 || failures[0].AccountID != "999999999999" {
		t.Fatalf("ledger = %+v, want one entry naming the uncredentialed account", failures)
	}
}

// A saturated int64 would render as a real reading, so an unrepresentable
// value is dropped and counted instead of clamped.
func TestNonFiniteValueIsDroppedAndLedgered(t *testing.T) {
	api := &fakeCW{listFn: knows("db1"), getFn: answers(map[string][]point{
		"db1": {{at: scanTime.Add(-24 * time.Hour), v: math.NaN()}},
	})}
	resources := []model.Resource{instance("111111111111", "us-east-1", "db1")}

	_, failures := run(t, api, resources)

	if v, ok := resources[0].Measure(model.MeasureFreeStorageBytes); ok {
		t.Errorf("free_storage_bytes = %d, want absent for a NaN reading", v)
	}
	if len(failures) != 1 {
		t.Fatalf("ledger = %+v, want one entry", failures)
	}
}

func TestUnexpectedContinuationIsLedgered(t *testing.T) {
	api := &fakeCW{listFn: knows("db1"), getFn: func(in *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
		out, _ := answers(map[string][]point{"db1": {{at: scanTime.Add(-24 * time.Hour), v: 5}}})(in)
		out.NextToken = aws.String("more")
		return out, nil
	}}
	resources := []model.Resource{instance("111111111111", "us-east-1", "db1")}

	_, failures := run(t, api, resources)

	// The reading on the first page is still real and still kept — the ledger
	// entry says the batch may be short, not that it is worthless.
	if v, ok := resources[0].Measure(model.MeasureFreeStorageBytes); !ok || v != 5 {
		t.Errorf("free_storage_bytes = (%d, %v), want (5, true)", v, ok)
	}
	if len(failures) != 1 {
		t.Fatalf("ledger = %+v, want one entry about the continuation", failures)
	}
}

func TestDegradedResultsAreLedgeredOnce(t *testing.T) {
	api := &fakeCW{listFn: knows("db1", "db2"), getFn: func(in *cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
		out := &cloudwatch.GetMetricDataOutput{}
		for _, q := range in.MetricDataQueries {
			out.MetricDataResults = append(out.MetricDataResults, cwtypes.MetricDataResult{
				Id: q.Id, StatusCode: cwtypes.StatusCodeInternalError,
			})
		}
		return out, nil
	}}
	resources := []model.Resource{
		instance("111111111111", "us-east-1", "db1"),
		instance("111111111111", "us-east-1", "db2"),
	}

	_, failures := run(t, api, resources)

	// One entry for the batch, not one per series: a broad CloudWatch outage
	// must not bury the rest of the ledger.
	if len(failures) != 1 {
		t.Fatalf("ledger = %+v, want a single aggregated entry", failures)
	}
}

func TestCanceledContextStopsBeforeSpending(t *testing.T) {
	api := &fakeCW{listFn: knows("db1")}
	resources := []model.Resource{instance("111111111111", "us-east-1", "db1")}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &Metrics{NewClient: func(aws.Config) API { return api }, Now: func() time.Time { return scanTime }}
	m.Enrich(ctx, scan.Enrichment{
		Targets:   []scan.Target{{AccountID: "111111111111", Cfg: aws.Config{}}},
		Resources: resources, Concurrency: 4,
	})

	if lists, gets := api.calls(); lists != 0 || gets != 0 {
		t.Errorf("calls = (%d list, %d get), want none under a canceled context", lists, gets)
	}
}

// Signatures are how a per-resource series is told apart from a rollup that
// shares its name, so an inexact match is a wrong number, not a missing one.
func TestSignatureIsExactOnDimensions(t *testing.T) {
	base := signature("AWS/RDS", "FreeStorageSpace", map[string]string{dbDimension: "db1"})
	cases := map[string]map[string]string{
		"extra dimension":  {dbDimension: "db1", "EngineName": "mysql"},
		"different value":  {dbDimension: "db2"},
		"different name":   {"EngineName": "db1"},
		"no dimensions":    {},
		"reversed mapping": {"db1": dbDimension},
	}
	for name, dims := range cases {
		if got := signature("AWS/RDS", "FreeStorageSpace", dims); got == base {
			t.Errorf("%s: signature collided with the per-instance series", name)
		}
	}
	// Order of insertion must not matter — map iteration is random, and a
	// signature that depended on it would drop metrics at random.
	multi := signature("AWS/RDS", "X", map[string]string{"a": "1", "b": "2"})
	if other := signature("AWS/RDS", "X", map[string]string{"b": "2", "a": "1"}); other != multi {
		t.Error("signature depends on map iteration order")
	}
}
