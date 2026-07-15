// Package awsx wraps AWS credential/config loading. blueprint never stores
// credentials of its own: everything rides the standard AWS credential chain
// (env vars, ~/.aws profiles, SSO, credential_process, instance roles).
package awsx

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// fallbackRegion is only used to bootstrap global calls (STS, DescribeRegions)
// when the profile has no region configured.
//
// Note this fallback is partition-blind: GovCloud or China-partition
// credentials with no configured region would send the bootstrap STS call to
// the commercial-partition endpoint and fail. Users in those partitions must
// set a region explicitly (AWS_REGION or the profile's region setting).
const fallbackRegion = "us-east-1"

// Load resolves the default credential chain, optionally pinning a profile.
// Retries are tuned up-front for high-volume scans (throttling-heavy).
func Load(ctx context.Context, profile string) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRetryer(func() aws.Retryer {
			return retry.AddWithMaxAttempts(retry.NewAdaptiveMode(), 8)
		}),
	}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("loading AWS credentials: %w", err)
	}
	if cfg.Region == "" {
		cfg.Region = fallbackRegion
	}
	return cfg, nil
}

// CallerAccount returns the account ID of the current credentials.
func CallerAccount(ctx context.Context, cfg aws.Config) (string, error) {
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("sts:GetCallerIdentity: %w", err)
	}
	return aws.ToString(out.Account), nil
}

// EnabledRegions lists all regions enabled for the account (opt-in aware).
func EnabledRegions(ctx context.Context, cfg aws.Config) ([]string, error) {
	out, err := ec2.NewFromConfig(cfg).DescribeRegions(ctx, &ec2.DescribeRegionsInput{
		Filters: []ec2types.Filter{{
			Name:   aws.String("opt-in-status"),
			Values: []string{"opt-in-not-required", "opted-in"},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("ec2:DescribeRegions: %w", err)
	}
	regions := make([]string, 0, len(out.Regions))
	for _, r := range out.Regions {
		regions = append(regions, aws.ToString(r.RegionName))
	}
	return regions, nil
}

// AssumeRole returns a config whose credentials come from assuming roleARN.
func AssumeRole(cfg aws.Config, roleARN string) aws.Config {
	out := cfg.Copy()
	out.Credentials = aws.NewCredentialsCache(
		stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), roleARN),
	)
	return out
}
