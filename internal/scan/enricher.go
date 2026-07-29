package scan

import (
	"context"

	"github.com/hoophq/blueprint/internal/model"
)

// Enricher fills in resource facts that no single scanner can supply, running
// once over the whole census after every scanner has finished.
//
// The split exists because cost, size, and utilization are cross-cutting
// post-passes rather than per-service concerns, and their APIs are shaped
// accordingly. CloudWatch GetMetricData carries 500 queries per call and Cost
// Optimization Hub is a flat org-wide list; asking for them inside Scan()
// would issue one narrow call per (account, region, service) unit and forfeit
// the batching entirely — and for the billed APIs it would multiply the charge
// by the region count. An Enricher sees every resource at once, so it decides
// its own fan-out and its own batch boundaries.
//
// Implementations are read-only, like scanners, and must not assume they are
// the only enricher: several run in sequence over the same slice, each writing
// its own keys.
type Enricher interface {
	// Name identifies the enricher in the failure ledger and in progress
	// output. It is a ledger Service name, not a scanner one, so it never
	// reaches Snapshot.Services and cannot re-bucket a user's diff history.
	Name() string

	// Enrich augments req.Resources in place and returns what it could not
	// see. Errors belong in the returned ledger entries rather than aborting:
	// a census that could not read one metric is still a census.
	Enrich(ctx context.Context, req Enrichment) []model.Failure
}

// Enrichment is the input to one enricher. It is a struct rather than a
// parameter list so a later enricher can be given more context without
// breaking the ones already written.
type Enrichment struct {
	// Targets carries the credentials the scan actually used, keyed by
	// account. An enricher needing per-account access in org mode reads the
	// matching Target's Cfg; one talking to a single org-wide endpoint (Cost
	// Optimization Hub from the management account) uses the caller's own.
	Targets []Target

	// Resources is the full census, sharing the snapshot's backing array:
	// writing to Resources[i] writes to the snapshot. It is deliberately not
	// a pointer slice — enrichment runs before Finalize sorts, so indexes
	// taken here would not survive that reordering anyway, and a slice makes
	// the "valid only within this call" lifetime obvious.
	Resources []model.Resource

	// Concurrency is the runner's worker budget, passed along so an enricher
	// fanning out over accounts or regions respects the same ceiling the user
	// set with --concurrency instead of inventing its own.
	Concurrency int
}
