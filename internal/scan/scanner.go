// Package scan defines the Scanner contract and the concurrent runner that
// executes scanners across accounts × regions with a bounded worker pool.
package scan

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/hoophq/dbcensus/internal/model"
)

// Scanner enumerates one AWS service in one (account, region) pair.
// Implementations must be strictly read-only (Describe*/List* only) and
// must paginate fully.
type Scanner interface {
	// Service is the identifier used in the failure ledger and logs.
	Service() string
	// Scan returns all resources visible in the given region. cfg is already
	// scoped to the target account and region.
	// On error, return the resources gathered so far along with the error;
	// the runner keeps partial results and records the error in the failure
	// ledger.
	Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error)
}

var (
	registryMu sync.Mutex
	registry   []Scanner
)

// Register adds a scanner to the global registry. Scanner implementations
// self-register from init() in their own file so no shared file needs edits.
func Register(s Scanner) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = append(registry, s)
}

// All returns the registered scanners.
func All() []Scanner {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]Scanner, len(registry))
	copy(out, registry)
	return out
}
