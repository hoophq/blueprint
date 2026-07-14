// Package orgmode expands a scan across AWS Organizations member accounts
// via per-account assume-role.
package orgmode

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/hoophq/dbcensus/internal/scan"
)

// Targets lists Organizations member accounts and builds one scan.Target per
// account: the caller account keeps its own credentials; every other account
// gets credentials via assume-role using roleName.
func Targets(ctx context.Context, cfg aws.Config, callerAccount, roleName string, regions []string) ([]scan.Target, error) {
	_ = ctx
	_ = cfg
	_ = callerAccount
	_ = roleName
	_ = regions
	return nil, errors.New("org mode not implemented yet")
}
