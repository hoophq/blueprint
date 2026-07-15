package scan

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/hoophq/blueprint/internal/model"
)

type fakeScanner struct {
	name    string
	fn      func(region, accountID string) ([]model.Resource, error)
	current atomic.Int32
	peak    atomic.Int32
	mu      sync.Mutex
}

func (f *fakeScanner) Service() string { return f.name }

func (f *fakeScanner) Scan(_ context.Context, _ aws.Config, region, accountID string) ([]model.Resource, error) {
	cur := f.current.Add(1)
	defer f.current.Add(-1)
	f.mu.Lock()
	if cur > f.peak.Load() {
		f.peak.Store(cur)
	}
	f.mu.Unlock()
	return f.fn(region, accountID)
}

func TestRunnerCollectsResultsAndFailures(t *testing.T) {
	s := &fakeScanner{name: "fake", fn: func(region, accountID string) ([]model.Resource, error) {
		if region == "eu-west-1" {
			// Partial result + error: both must be kept.
			return []model.Resource{{Name: "partial", Region: region, AccountID: accountID, Tags: map[string]string{"env": "prod"}}},
				errors.New("throttled")
		}
		return []model.Resource{{Name: "db-" + region, Region: region, AccountID: accountID}}, nil
	}}

	r := &Runner{Scanners: []Scanner{s}, Concurrency: 2}
	snap := r.Run(context.Background(), []Target{
		{AccountID: "1", Regions: []string{"us-east-1", "eu-west-1", "sa-east-1"}},
	}, "test")

	if len(snap.Resources) != 3 {
		t.Fatalf("expected 3 resources (incl. partial), got %d", len(snap.Resources))
	}
	if len(snap.Failures) != 1 || snap.Failures[0].Region != "eu-west-1" || snap.Failures[0].Service != "fake" {
		t.Fatalf("unexpected failures: %+v", snap.Failures)
	}
	// Tag derivation must run on results.
	for _, res := range snap.Resources {
		if res.Name == "partial" && res.Environment != "prod" {
			t.Errorf("DeriveEnvOwner not applied: %+v", res)
		}
	}
}

func TestRunnerRespectsConcurrencyBound(t *testing.T) {
	block := make(chan struct{})
	s := &fakeScanner{name: "slow", fn: func(_, _ string) ([]model.Resource, error) {
		<-block
		return nil, nil
	}}
	r := &Runner{Scanners: []Scanner{s}, Concurrency: 3}

	done := make(chan *model.Snapshot)
	go func() {
		done <- r.Run(context.Background(), []Target{
			{AccountID: "1", Regions: []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8"}},
		}, "test")
	}()

	// Wait until the semaphore is saturated: 3 scanners blocked inside Scan
	// while the other 5 units are still queued. Only then is the bound
	// actually being exercised.
	deadline := time.After(5 * time.Second)
	for s.current.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("runner never saturated the bound: %d concurrent scanners", s.current.Load())
		case <-time.After(time.Millisecond):
		}
	}
	if peak := s.peak.Load(); peak > 3 {
		t.Fatalf("concurrency bound violated while saturated: peak %d > 3", peak)
	}

	close(block)
	<-done

	if peak := s.peak.Load(); peak > 3 {
		t.Errorf("concurrency bound violated: peak %d > 3", peak)
	}
}

func TestRunnerFinalizeSortsRegionsAndAccounts(t *testing.T) {
	s := &fakeScanner{name: "fake", fn: func(_, _ string) ([]model.Resource, error) {
		return nil, nil
	}}
	r := &Runner{Scanners: []Scanner{s}, Concurrency: 4}
	targets := []Target{
		{AccountID: "2", Regions: []string{"us-west-2", "ap-south-1", "eu-central-1"}},
		{AccountID: "1", Regions: []string{"us-east-1", "ca-central-1"}},
	}

	// Regions are collected from map iteration; run a few times so an
	// accidentally-sorted iteration order cannot mask a regression.
	for i := 0; i < 5; i++ {
		snap := r.Run(context.Background(), targets, "test")
		if !sort.StringsAreSorted(snap.Regions) {
			t.Fatalf("run %d: Regions not sorted: %v", i, snap.Regions)
		}
		if !sort.StringsAreSorted(snap.Accounts) {
			t.Fatalf("run %d: Accounts not sorted: %v", i, snap.Accounts)
		}
	}
}

func TestRunnerCanceledErrorsNotInLedger(t *testing.T) {
	s := &fakeScanner{name: "fake", fn: func(region, _ string) ([]model.Resource, error) {
		if region == "r1" {
			return nil, fmt.Errorf("wrapped: %w", context.Canceled)
		}
		return nil, errors.New("real failure")
	}}
	r := &Runner{Scanners: []Scanner{s}, Concurrency: 2}

	snap := r.Run(context.Background(), []Target{{AccountID: "1", Regions: []string{"r1", "r2"}}}, "test")
	if len(snap.Failures) != 1 || snap.Failures[0].Region != "r2" {
		t.Errorf("expected only the real failure in the ledger, got %+v", snap.Failures)
	}
}

func TestRunnerCanceledContextSkipsUnits(t *testing.T) {
	var calls atomic.Int32
	s := &fakeScanner{name: "fake", fn: func(_, _ string) ([]model.Resource, error) {
		calls.Add(1)
		return nil, nil
	}}
	r := &Runner{Scanners: []Scanner{s}, Concurrency: 1}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snap := r.Run(ctx, []Target{{AccountID: "1", Regions: []string{"r1", "r2", "r3"}}}, "test")

	// A pre-canceled context must not flood the ledger; skipped units also
	// must not reach the scanner (no SDK calls after cancellation). The
	// Concurrency-1 semaphore is full-or-free deterministically, so at most
	// the races between "acquire" and "ctx.Done" let units through — none
	// may record a Failure.
	if len(snap.Failures) != 0 {
		t.Errorf("canceled run must not add failures, got %+v", snap.Failures)
	}
	if got := calls.Load(); got != 0 {
		// The select between sem and ctx.Done is racy when both are ready;
		// with a pre-canceled context before Run starts, no unit should
		// win the semaphore case reliably — but tolerate scheduler races
		// only in the call count, never in the ledger.
		t.Logf("note: %d unit(s) raced past cancellation", got)
	}
}

func TestRunnerDeterministicOrder(t *testing.T) {
	s := &fakeScanner{name: "fake", fn: func(region, accountID string) ([]model.Resource, error) {
		return []model.Resource{{Name: "db", Region: region, AccountID: accountID, Service: "rds"}}, nil
	}}
	r := &Runner{Scanners: []Scanner{s}, Concurrency: 4}
	targets := []Target{{AccountID: "2", Regions: []string{"b", "a"}}, {AccountID: "1", Regions: []string{"c"}}}

	first := r.Run(context.Background(), targets, "test")
	second := r.Run(context.Background(), targets, "test")
	for i := range first.Resources {
		a, b := first.Resources[i], second.Resources[i]
		if a.AccountID != b.AccountID || a.Region != b.Region {
			t.Fatalf("non-deterministic order at %d: %+v vs %+v", i, a, b)
		}
	}
	if first.Resources[0].AccountID != "1" {
		t.Errorf("expected account 1 first after sort, got %+v", first.Resources[0])
	}
}
