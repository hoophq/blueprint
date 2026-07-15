package scan

import (
	"context"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/hoophq/dbcensus/internal/model"
)

// Target is one account to scan: a base config plus the regions to cover.
type Target struct {
	AccountID string
	Cfg       aws.Config // credentials resolved for this account, region unset
	Regions   []string
}

// Runner fans scanners out over targets × regions with bounded concurrency.
type Runner struct {
	Scanners    []Scanner
	Concurrency int
	// OnUnit, if set, is called after each (account, region, service) unit
	// completes — used for progress output.
	OnUnit func(accountID, region, service string, found int, err error)
}

// Run executes the scan and returns a sorted snapshot with a failure ledger.
// Individual unit errors never abort the run; they land in Failures.
func (r *Runner) Run(ctx context.Context, targets []Target, version string) *model.Snapshot {
	if r.Concurrency <= 0 {
		r.Concurrency = 8
	}
	snap := &model.Snapshot{Version: version, GeneratedAt: time.Now().UTC()}

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
			sem <- struct{}{}
			defer func() { <-sem }()

			cfg := u.target.Cfg.Copy()
			cfg.Region = u.region
			resources, err := u.scanner.Scan(ctx, cfg, u.region, u.target.AccountID)

			// Partial results are kept even on error: the failure ledger
			// records what could not be seen, without discarding what was.
			mu.Lock()
			snap.Resources = append(snap.Resources, resources...)
			if err != nil {
				snap.Failures = append(snap.Failures, model.Failure{
					AccountID: u.target.AccountID,
					Region:    u.region,
					Service:   u.scanner.Service(),
					Error:     err.Error(),
				})
			}
			mu.Unlock()

			if r.OnUnit != nil {
				r.OnUnit(u.target.AccountID, u.region, u.scanner.Service(), len(resources), err)
			}
		}(u)
	}
	wg.Wait()

	for i := range snap.Resources {
		snap.Resources[i].DeriveEnvOwner()
	}
	snap.Sort()
	return snap
}
