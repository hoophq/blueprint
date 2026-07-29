package scan

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/hoophq/blueprint/internal/model"
)

type fakeEnricher struct {
	name string
	fn   func(Enrichment) []model.Failure
	ran  bool
}

func (f *fakeEnricher) Name() string { return f.name }

func (f *fakeEnricher) Enrich(_ context.Context, req Enrichment) []model.Failure {
	f.ran = true
	if f.fn == nil {
		return nil
	}
	return f.fn(req)
}

// oneResource is a scanner returning a single resource per unit, for tests
// that care about what happens to it after the scan rather than during.
func oneResource(name string) *fakeScanner {
	return &fakeScanner{name: "fake", fn: func(region, accountID string) ([]model.Resource, error) {
		return []model.Resource{{
			ARN:     "arn:aws:rds:" + region + ":" + accountID + ":db:" + name,
			Service: model.ServiceRDS, Type: model.TypeRDSInstance,
			Name: name, Region: region, AccountID: accountID,
		}}, nil
	}}
}

func TestEnricherWritesLandInTheSnapshot(t *testing.T) {
	observed := time.Date(2026, 7, 27, 4, 0, 0, 0, time.UTC)
	e := &fakeEnricher{name: "metrics", fn: func(req Enrichment) []model.Failure {
		for i := range req.Resources {
			req.Resources[i].SetObservedMeasure(model.MeasureFreeStorageBytes, 1024, observed)
		}
		return nil
	}}
	r := &Runner{Scanners: []Scanner{oneResource("db1")}, Enrichers: []Enricher{e}}

	snap := r.Run(context.Background(), []Target{{AccountID: "111111111111", Regions: []string{"us-east-1"}}}, "test")

	if len(snap.Resources) != 1 {
		t.Fatalf("got %d resources, want 1", len(snap.Resources))
	}
	// Enrichment mutates the snapshot's own backing array, so the write must
	// be visible without the enricher returning anything.
	if v, ok := snap.Resources[0].Measure(model.MeasureFreeStorageBytes); !ok || v != 1024 {
		t.Errorf("free_storage_bytes = (%d, %v), want (1024, true)", v, ok)
	}
	if at, ok := snap.Resources[0].MeasureAsOf(model.MeasureFreeStorageBytes); !ok || !at.Equal(observed) {
		t.Errorf("observation time = (%v, %v), want (%v, true)", at, ok, observed)
	}
}

// The stage sits between the scan and Finalize, and both edges matter. This
// pins the later one: an enricher that writes a version must have it read by
// the EOL derivation, which only runs inside Finalize. Move enrichment after
// Finalize and this resource silently stops being flagged.
func TestEnrichmentRunsBeforeFinalizeDerives(t *testing.T) {
	e := &fakeEnricher{name: "metrics", fn: func(req Enrichment) []model.Failure {
		for i := range req.Resources {
			req.Resources[i].SetAttr(model.AttrEngine, "mysql")
			req.Resources[i].SetAttr(model.AttrEngineVersion, "5.6.51")
			req.Resources[i].Tags = map[string]string{"Environment": "prod", "Owner": "dba@example.com"}
		}
		return nil
	}}
	r := &Runner{Scanners: []Scanner{oneResource("db1")}, Enrichers: []Enricher{e}}

	snap := r.Run(context.Background(), []Target{{AccountID: "111111111111", Regions: []string{"us-east-1"}}}, "test")

	got := snap.Resources[0]
	if !got.EOL || got.EOLDate != "2021-02-05" {
		t.Errorf("EOL = (%v, %q), want (true, 2021-02-05) — enrichment must precede Finalize's derivation",
			got.EOL, got.EOLDate)
	}
	if got.Environment != "prod" || got.Owner != "dba@example.com" {
		t.Errorf("env/owner = (%q, %q), want (prod, dba@example.com)", got.Environment, got.Owner)
	}
}

// An enricher covers no single region, so its ledger entries carry an empty
// Region. They must still sort into the same ledger as scan failures.
func TestEnricherFailuresJoinTheSortedLedger(t *testing.T) {
	e := &fakeEnricher{name: "metrics", fn: func(Enrichment) []model.Failure {
		return []model.Failure{{
			AccountID: "111111111111", Service: "metrics",
			Error: "AccessDenied: cloudwatch:GetMetricData", Time: time.Now().UTC(),
		}}
	}}
	failing := &fakeScanner{name: "fake", fn: func(region, _ string) ([]model.Resource, error) {
		return nil, context.DeadlineExceeded
	}}
	r := &Runner{Scanners: []Scanner{failing}, Enrichers: []Enricher{e}}

	snap := r.Run(context.Background(), []Target{{AccountID: "111111111111", Regions: []string{"us-east-1"}}}, "test")

	if len(snap.Failures) != 2 {
		t.Fatalf("got %d ledger entries, want 2: %+v", len(snap.Failures), snap.Failures)
	}
	// "fake" (regional) sorts after "metrics" (region-less) because the empty
	// region compares first — the point being that Finalize sorted both, not
	// that the enricher's entry was appended raw.
	if snap.Failures[0].Service != "metrics" || snap.Failures[0].Region != "" {
		t.Errorf("first entry = %+v, want the region-less metrics failure", snap.Failures[0])
	}
	if snap.Failures[1].Service != "fake" || snap.Failures[1].Region != "us-east-1" {
		t.Errorf("second entry = %+v, want the regional scan failure", snap.Failures[1])
	}
}

func TestEnrichersRunInOrderWithTheRunnerBudget(t *testing.T) {
	var order []string
	var gotConcurrency int
	var gotTargets int
	first := &fakeEnricher{name: "first", fn: func(req Enrichment) []model.Failure {
		order = append(order, "first")
		gotConcurrency = req.Concurrency
		gotTargets = len(req.Targets)
		return nil
	}}
	second := &fakeEnricher{name: "second", fn: func(Enrichment) []model.Failure {
		order = append(order, "second")
		return nil
	}}
	var reported []string
	r := &Runner{
		Scanners: []Scanner{oneResource("db1")}, Enrichers: []Enricher{first, second},
		Concurrency: 3,
		OnEnrich:    func(name string, _ int) { reported = append(reported, name) },
	}

	r.Run(context.Background(), []Target{
		{AccountID: "111111111111", Cfg: aws.Config{}, Regions: []string{"us-east-1"}},
		{AccountID: "222222222222", Cfg: aws.Config{}, Regions: []string{"us-east-1"}},
	}, "test")

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("run order = %v, want [first second]", order)
	}
	if len(reported) != 2 || reported[0] != "first" || reported[1] != "second" {
		t.Errorf("OnEnrich order = %v, want [first second]", reported)
	}
	// An enricher fanning out must respect --concurrency rather than picking
	// its own ceiling, and must see every account the scan covered.
	if gotConcurrency != 3 {
		t.Errorf("Enrichment.Concurrency = %d, want the runner's 3", gotConcurrency)
	}
	if gotTargets != 2 {
		t.Errorf("Enrichment.Targets = %d, want both scanned accounts", gotTargets)
	}
}

// Cancellation stops the stage instead of letting each enricher start and fail
// per call. For the enrichers that reach billed APIs that is the difference
// between exiting and spending on the way out.
func TestCanceledContextSkipsEnrichers(t *testing.T) {
	e := &fakeEnricher{name: "metrics"}
	r := &Runner{Scanners: []Scanner{oneResource("db1")}, Enrichers: []Enricher{e}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snap := r.Run(ctx, []Target{{AccountID: "111111111111", Regions: []string{"us-east-1"}}}, "test")

	if e.ran {
		t.Error("enricher ran under a canceled context")
	}
	// The snapshot is still well-formed and still sorted — a canceled run
	// produces a short census, not a broken one.
	if snap.Schema != model.SchemaVersion {
		t.Errorf("schema = %d, want %d", snap.Schema, model.SchemaVersion)
	}
}

// A runner with no enrichers is the default path and must not change.
func TestRunnerWithoutEnrichersIsUnchanged(t *testing.T) {
	r := &Runner{Scanners: []Scanner{oneResource("db1")}}

	snap := r.Run(context.Background(), []Target{{AccountID: "111111111111", Regions: []string{"us-east-1"}}}, "test")

	if len(snap.Resources) != 1 || len(snap.Failures) != 0 {
		t.Errorf("got %d resources and %d failures, want 1 and 0", len(snap.Resources), len(snap.Failures))
	}
}
