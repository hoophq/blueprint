package scanners

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	ectypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// elastiCacheScanner covers the three ElastiCache shapes without double
// counting: replication groups are the census unit for grouped Redis/Valkey
// (their member cache clusters are skipped), standalone cache clusters and
// memcached remain individual instances, and serverless caches are their own
// kind. Tags need a separate ListTagsForResource call per ARN; a tag failure
// keeps the resource (with nil tags) and tag failures are aggregated into a
// single returned error so the runner ledgers the gap once.
type elastiCacheScanner struct{}

func init() { scan.Register(elastiCacheScanner{}) }

func (elastiCacheScanner) Service() string { return "elasticache" }

func (elastiCacheScanner) Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error) {
	client := elasticache.NewFromConfig(cfg)
	var out []model.Resource

	// Tag failures never drop a resource, but silently ignoring them would
	// inflate the missing-owner/env metrics with no ledger entry. Aggregate
	// them into one error joined with whatever else the scan returns.
	agg := tagFailures{}
	getTags := func(arn string) map[string]string {
		tags, err := elastiCacheTags(ctx, client, arn)
		agg.record(err)
		return tags
	}

	groups := elasticache.NewDescribeReplicationGroupsPaginator(client, &elasticache.DescribeReplicationGroupsInput{})
	for groups.HasMorePages() {
		page, err := groups.NextPage(ctx)
		if err != nil {
			return out, errors.Join(err, agg.err())
		}
		for _, g := range page.ReplicationGroups {
			r := replicationGroupResource(g, region, accountID)
			r.Tags = getTags(r.ARN)
			out = append(out, r)
		}
	}

	// ShowCacheNodeInfo populates per-node endpoints for standalone clusters.
	clusters := elasticache.NewDescribeCacheClustersPaginator(client, &elasticache.DescribeCacheClustersInput{
		ShowCacheNodeInfo: aws.Bool(true),
	})
	for clusters.HasMorePages() {
		page, err := clusters.NextPage(ctx)
		if err != nil {
			return out, errors.Join(err, agg.err())
		}
		for _, c := range page.CacheClusters {
			if inReplicationGroup(c) {
				continue // already counted as part of its replication group
			}
			r := cacheClusterResource(c, region, accountID)
			r.Tags = getTags(r.ARN)
			out = append(out, r)
		}
	}

	serverless := elasticache.NewDescribeServerlessCachesPaginator(client, &elasticache.DescribeServerlessCachesInput{})
	for serverless.HasMorePages() {
		page, err := serverless.NextPage(ctx)
		if err != nil {
			return out, errors.Join(err, agg.err())
		}
		for _, s := range page.ServerlessCaches {
			r := serverlessCacheResource(s, region, accountID)
			r.Tags = getTags(r.ARN)
			out = append(out, r)
		}
	}
	return out, agg.err()
}

// inReplicationGroup reports whether a cache cluster is a member of a
// replication group and therefore already represented by it.
func inReplicationGroup(c ectypes.CacheCluster) bool {
	return aws.ToString(c.ReplicationGroupId) != ""
}

func replicationGroupResource(g ectypes.ReplicationGroup, region, accountID string) model.Resource {
	endpoint := ""
	if g.ConfigurationEndpoint != nil {
		endpoint = aws.ToString(g.ConfigurationEndpoint.Address)
	} else if len(g.NodeGroups) > 0 && g.NodeGroups[0].PrimaryEndpoint != nil {
		endpoint = aws.ToString(g.NodeGroups[0].PrimaryEndpoint.Address)
	}
	return model.Resource{
		ARN:           aws.ToString(g.ARN),
		Service:       model.ServiceElastiCache,
		Kind:          "cluster",
		Name:          aws.ToString(g.ReplicationGroupId),
		Engine:        aws.ToString(g.Engine),
		InstanceClass: aws.ToString(g.CacheNodeType),
		MultiAZ:       g.MultiAZ == ectypes.MultiAZStatusEnabled,
		Status:        aws.ToString(g.Status),
		Endpoint:      endpoint,
		Region:        region,
		AccountID:     accountID,
		CreatedAt:     g.ReplicationGroupCreateTime,
		// ElastiCache is VPC-only (no public-accessibility flag to report).
		Encrypted:           g.AtRestEncryptionEnabled,
		BackupRetentionDays: g.SnapshotRetentionLimit,
	}
}

func cacheClusterResource(c ectypes.CacheCluster, region, accountID string) model.Resource {
	endpoint := ""
	if c.ConfigurationEndpoint != nil {
		endpoint = aws.ToString(c.ConfigurationEndpoint.Address)
	} else if len(c.CacheNodes) > 0 && c.CacheNodes[0].Endpoint != nil {
		endpoint = aws.ToString(c.CacheNodes[0].Endpoint.Address)
	}
	return model.Resource{
		ARN:           aws.ToString(c.ARN),
		Service:       model.ServiceElastiCache,
		Kind:          "instance",
		Name:          aws.ToString(c.CacheClusterId),
		Engine:        aws.ToString(c.Engine),
		EngineVersion: aws.ToString(c.EngineVersion),
		InstanceClass: aws.ToString(c.CacheNodeType),
		Status:        aws.ToString(c.CacheClusterStatus),
		Endpoint:      endpoint,
		Region:        region,
		AccountID:     accountID,
		CreatedAt:     c.CacheClusterCreateTime,

		Encrypted:           c.AtRestEncryptionEnabled,
		BackupRetentionDays: c.SnapshotRetentionLimit,
	}
}

func serverlessCacheResource(s ectypes.ServerlessCache, region, accountID string) model.Resource {
	endpoint := ""
	if s.Endpoint != nil {
		endpoint = aws.ToString(s.Endpoint.Address)
	}
	return model.Resource{
		ARN:           aws.ToString(s.ARN),
		Service:       model.ServiceElastiCache,
		Kind:          "serverless",
		Name:          aws.ToString(s.ServerlessCacheName),
		Engine:        aws.ToString(s.Engine),
		EngineVersion: aws.ToString(s.FullEngineVersion),
		Status:        aws.ToString(s.Status),
		Endpoint:      endpoint,
		Region:        region,
		AccountID:     accountID,
		CreatedAt:     s.CreateTime,
	}
}

// elastiCacheTags fetches tags for one ARN. A failure (common on caches in
// transitional states) yields nil tags plus the error; the caller keeps the
// resource and aggregates the failure.
func elastiCacheTags(ctx context.Context, client *elasticache.Client, arn string) (map[string]string, error) {
	if arn == "" {
		return nil, nil
	}
	resp, err := client.ListTagsForResource(ctx, &elasticache.ListTagsForResourceInput{ResourceName: aws.String(arn)})
	if err != nil {
		return nil, err
	}
	return toTagMap(resp.TagList, func(t ectypes.Tag) (*string, *string) { return t.Key, t.Value }), nil
}
