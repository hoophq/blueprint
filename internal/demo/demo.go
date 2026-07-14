// Package demo provides a built-in fixture snapshot so anyone can render the
// report without AWS credentials (dbcensus scan --demo).
package demo

import (
	"time"

	"github.com/hoophq/dbcensus/internal/model"
)

// Snapshot returns fixture data resembling a mid-size multi-region estate.
func Snapshot(version string) *model.Snapshot {
	snap := &model.Snapshot{
		Version:     version,
		GeneratedAt: time.Now().UTC(),
		Accounts:    []string{"111111111111"},
		Regions:     []string{"us-east-1", "sa-east-1"},
		Resources: []model.Resource{{
			ARN:       "arn:aws:rds:us-east-1:111111111111:db:orders-prod",
			Service:   model.ServiceRDS,
			Kind:      "instance",
			Name:      "orders-prod",
			Engine:    "postgres",
			Region:    "us-east-1",
			AccountID: "111111111111",
			Tags:      map[string]string{"environment": "production", "owner": "payments"},
		}},
	}
	for i := range snap.Resources {
		snap.Resources[i].DeriveEnvOwner()
	}
	snap.Sort()
	return snap
}
