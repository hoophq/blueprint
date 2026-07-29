package scan

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/hoophq/blueprint/internal/model"
)

// Target is one account to scan: a base config plus the regions to cover.
type Target struct {
	AccountID string
	Cfg       aws.Config // credentials resolved for this account, region unset
	Regions   []string
}

// Runner fans scanners out over targets × regions with bounded concurrency,
// then hands the assembled census to each enricher in turn.
type Runner struct {
	Scanners []Scanner
	// Enrichers run after every scanner, in order. The caller decides which
	// to include, which is what makes each one independently skippable —
	// several reach paid APIs and are off unless the user asks.
	Enrichers   []Enricher
	Concurrency int
	// OnUnit, if set, is called after each (account, region, service) unit
	// completes — used for progress output.
	OnUnit func(accountID, region, service string, found int, err error)
	// OnEnrich, if set, is called after each enricher finishes.
	OnEnrich func(name string, failed int)
}

// Run executes the scan and returns a sorted snapshot with a failure ledger.
// Individual unit errors never abort the run; they land in Failures.
func (r *Runner) Run(ctx context.Context, targets []Target, version string) *model.Snapshot {
	if r.Concurrency <= 0 {
		r.Concurrency = 8
	}
	snap := &model.Snapshot{Schema: model.SchemaVersion, Version: version, GeneratedAt: time.Now().UTC()}
	seenServices := map[string]bool{}
	for _, s := range r.Scanners {
		if svc := s.Service(); !seenServices[svc] {
			seenServices[svc] = true
			snap.Services = append(snap.Services, svc)
		}
	}

	type unit struct {
		target  Target
		region  string
		scanner Scanner
	}
	var units []unit
	regionSet := map[string]bool{}
	for _, t := range targets {
		snap.Accounts = append(snap.Accounts, t.AccountID)
		for _, region := range t.Regions {
			regionSet[region] = true
			for _, s := range r.Scanners {
				units = append(units, unit{t, region, s})
			}
		}
	}
	for region := range regionSet {
		snap.Regions = append(snap.Regions, region)
	}

	var mu sync.Mutex
	sem := make(chan struct{}, r.Concurrency)
	var wg sync.WaitGroup
	for _, u := range units {
		wg.Add(1)
		go func(u unit) {
			defer wg.Done()
			// The acquire is ctx-aware so a canceled run stops issuing new
			// SDK calls instead of still draining the whole unit queue.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			cfg := u.target.Cfg.Copy()
			cfg.Region = u.region
			resources, err := u.scanner.Scan(ctx, cfg, u.region, u.target.AccountID)

			// Partial results are kept even on error: the failure ledger
			// records what could not be seen, without discarding what was.
			// User-initiated cancellation is not a coverage gap, though —
			// recording it would flood the ledger with one entry per
			// in-flight unit.
			mu.Lock()
			snap.Resources = append(snap.Resources, resources...)
			if err != nil && !errors.Is(err, context.Canceled) {
				snap.Failures = append(snap.Failures, model.Failure{
					AccountID: u.target.AccountID,
					Region:    u.region,
					Service:   u.scanner.Service(),
					Error:     err.Error(),
					Time:      time.Now().UTC(),
				})
			}
			mu.Unlock()

			if r.OnUnit != nil {
				r.OnUnit(u.target.AccountID, u.region, u.scanner.Service(), len(resources), err)
			}
		}(u)
	}
	wg.Wait()

	r.enrich(ctx, snap, targets)
	snap.Finalize()
	return snap
}

// enrich runs each enricher over the assembled census.
//
// The placement is load-bearing at both ends. It is after the scan because an
// enricher's whole advantage is seeing every resource at once. It is before
// Finalize because Finalize sorts Resources, and an enricher works by index
// into that slice — running afterwards would either invalidate those indexes
// or force a second sort to keep the artifact deterministic. Enrichers may
// also write the tags and versions that DeriveEnvOwner and DeriveEOL read, so
// they have to land before those run, not after.
//
// Enrichers run in sequence rather than concurrently: each already fans out
// internally over accounts and regions, so overlapping them would multiply
// concurrency past the ceiling the user set, for two or three stages.
func (r *Runner) enrich(ctx context.Context, snap *model.Snapshot, targets []Target) {
	for _, e := range r.Enrichers {
		// A canceled run stops here rather than starting a stage that would
		// immediately fail every call and, for the paid enrichers, spend on
		// the way out.
		if ctx.Err() != nil {
			return
		}
		failures := e.Enrich(ctx, Enrichment{
			Targets:     targets,
			Resources:   snap.Resources,
			Concurrency: r.Concurrency,
		})
		snap.Failures = append(snap.Failures, failures...)
		if r.OnEnrich != nil {
			r.OnEnrich(e.Name(), len(failures))
		}
	}
}
