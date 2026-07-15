// Package orgmode expands a scan across AWS Organizations member accounts
// via per-account assume-role.
package orgmode

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"

	"github.com/hoophq/blueprint/internal/awsx"
	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// validateConcurrency bounds the per-account role-validation fan-out.
const validateConcurrency = 8

// partitionFromARN extracts the partition segment from an ARN — "aws",
// "aws-us-gov", "aws-cn", ... Falls back to "aws" when the ARN is missing or
// malformed. Organizations ListAccounts returns each account's ARN (e.g.
// arn:aws-us-gov:organizations::...), which carries the org's partition.
func partitionFromARN(arn string) string {
	parts := strings.SplitN(arn, ":", 3)
	if len(parts) == 3 && parts[0] == "arn" && parts[1] != "" {
		return parts[1]
	}
	return "aws"
}

// roleARN builds the ARN of the role blueprint assumes in a member account,
// within the given partition.
func roleARN(partition, accountID, roleName string) string {
	return "arn:" + partition + ":iam::" + accountID + ":role/" + roleName
}

// isActive reports whether an Organizations account is in the ACTIVE
// lifecycle phase. The Account.Status field is scheduled for retirement
// (September 2026) in favor of Account.State, so State wins whenever the
// API populates it; Status remains as a fallback for older responses.
func isActive(a orgtypes.Account) bool {
	if a.State != "" {
		return a.State == orgtypes.AccountStateActive
	}
	return a.Status == orgtypes.AccountStatusActive
}

// Targets lists Organizations member accounts and builds one scan.Target per
// ACTIVE account. The caller account keeps its own credentials and scans
// callerRegions (already resolved upstream). Every other account gets
// credentials via assume-role using roleName, built in the account's own
// partition (derived from its Organizations ARN), and is validated up front
// with a single ec2:DescribeRegions call — which doubles as the source of
// that account's own opt-in region list. When explicitRegions is non-empty
// (user passed --regions) it is used verbatim for every account; the one-call
// role validation still runs.
//
// Accounts whose role cannot be assumed or validated are skipped and recorded
// as one "orgmode" entry each in the returned pre-failures — one per account,
// not one per scanner×region.
func Targets(ctx context.Context, cfg aws.Config, callerAccount, roleName string, callerRegions, explicitRegions []string) ([]scan.Target, []model.Failure, error) {
	client := organizations.NewFromConfig(cfg)
	pager := organizations.NewListAccountsPaginator(client, &organizations.ListAccountsInput{})

	type entry struct {
		id, arn  string
		isCaller bool
	}
	var entries []entry
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("organizations:ListAccounts: %w\n"+
				"Org mode (--org) must run with credentials from the organization's "+
				"management account or a delegated administrator account, and those "+
				"credentials need the organizations:ListAccounts permission. "+
				"The role passed via --role-name (default OrganizationAccountAccessRole) "+
				"must exist in every member account and trust the calling account", err)
		}
		for _, acct := range page.Accounts {
			if !isActive(acct) {
				continue
			}
			id := aws.ToString(acct.Id)
			if id == "" {
				continue
			}
			entries = append(entries, entry{id: id, arn: aws.ToString(acct.Arn), isCaller: id == callerAccount})
		}
	}

	// Validate each member role and resolve its region list with bounded
	// concurrency. One slot per account keeps output order deterministic.
	targetSlots := make([]*scan.Target, len(entries))
	failureSlots := make([]*model.Failure, len(entries))
	sem := make(chan struct{}, validateConcurrency)
	var wg sync.WaitGroup
	for i, e := range entries {
		if e.isCaller {
			// The caller's own credentials already cover this account —
			// no assume-role hop and no extra validation call needed.
			targetSlots[i] = &scan.Target{AccountID: e.id, Cfg: cfg, Regions: callerRegions}
			continue
		}
		wg.Add(1)
		go func(i int, e entry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			arn := roleARN(partitionFromARN(e.arn), e.id, roleName)
			memberCfg := awsx.AssumeRole(cfg, arn)
			// AssumeRole credentials are lazy, so this single call both
			// proves the role is assumable and returns the member account's
			// own opt-in region list.
			regions, err := awsx.EnabledRegions(ctx, memberCfg)
			if err != nil {
				failureSlots[i] = &model.Failure{
					AccountID: e.id,
					Service:   "orgmode",
					Error:     fmt.Sprintf("validating %s: %v", arn, err),
				}
				return
			}
			if len(explicitRegions) > 0 {
				regions = explicitRegions
			}
			targetSlots[i] = &scan.Target{AccountID: e.id, Cfg: memberCfg, Regions: regions}
		}(i, e)
	}
	wg.Wait()

	var targets []scan.Target
	var preFailures []model.Failure
	for i := range entries {
		if targetSlots[i] != nil {
			targets = append(targets, *targetSlots[i])
		}
		if failureSlots[i] != nil {
			preFailures = append(preFailures, *failureSlots[i])
		}
	}
	return targets, preFailures, nil
}
