package scanners

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ectypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"

	"github.com/hoophq/blueprint/internal/model"
)

func TestInReplicationGroup(t *testing.T) {
	member := ectypes.CacheCluster{ReplicationGroupId: aws.String("rg-1")}
	if !inReplicationGroup(member) {
		t.Error("cluster with ReplicationGroupId should be skipped")
	}
	standalone := ectypes.CacheCluster{}
	if inReplicationGroup(standalone) {
		t.Error("standalone cluster should not be skipped")
	}
	empty := ectypes.CacheCluster{ReplicationGroupId: aws.String("")}
	if inReplicationGroup(empty) {
		t.Error("empty ReplicationGroupId should not be skipped")
	}
}

func TestReplicationGroupResource(t *testing.T) {
	g := ectypes.ReplicationGroup{
		ARN:                aws.String("arn:aws:elasticache:us-east-1:1:replicationgroup:rg-1"),
		ReplicationGroupId: aws.String("rg-1"),
		Engine:             aws.String("valkey"),
		CacheNodeType:      aws.String("cache.r7g.large"),
		MultiAZ:            ectypes.MultiAZStatusEnabled,
		Status:             aws.String("available"),
		NodeGroups: []ectypes.NodeGroup{
			{PrimaryEndpoint: &ectypes.Endpoint{Address: aws.String("primary.rg-1.cache.amazonaws.com")}},
		},
	}
	r := replicationGroupResource(g, "us-east-1", "1")
	if r.Service != model.ServiceElastiCache || r.Kind != "cluster" {
		t.Errorf("unexpected service/kind: %+v", r)
	}
	if r.Engine != "valkey" || r.InstanceClass != "cache.r7g.large" || !r.MultiAZ {
		t.Errorf("unexpected engine/class/multiAZ: %+v", r)
	}
	if r.Endpoint != "primary.rg-1.cache.amazonaws.com" {
		t.Errorf("expected primary endpoint fallback, got %q", r.Endpoint)
	}

	// ConfigurationEndpoint wins when present (cluster-mode enabled).
	g.ConfigurationEndpoint = &ectypes.Endpoint{Address: aws.String("config.rg-1.cache.amazonaws.com")}
	r = replicationGroupResource(g, "us-east-1", "1")
	if r.Endpoint != "config.rg-1.cache.amazonaws.com" {
		t.Errorf("expected configuration endpoint, got %q", r.Endpoint)
	}
}

func TestCacheClusterResource(t *testing.T) {
	c := ectypes.CacheCluster{
		ARN:                aws.String("arn:aws:elasticache:us-east-1:1:cluster:mc-1"),
		CacheClusterId:     aws.String("mc-1"),
		Engine:             aws.String("memcached"),
		EngineVersion:      aws.String("1.6.22"),
		CacheNodeType:      aws.String("cache.t4g.micro"),
		CacheClusterStatus: aws.String("available"),
		ConfigurationEndpoint: &ectypes.Endpoint{
			Address: aws.String("mc-1.cfg.cache.amazonaws.com"),
		},
	}
	r := cacheClusterResource(c, "us-east-1", "1")
	if r.Kind != "instance" || r.Service != model.ServiceElastiCache {
		t.Errorf("unexpected kind/service: %+v", r)
	}
	if r.Engine != "memcached" || r.EngineVersion != "1.6.22" {
		t.Errorf("unexpected engine: %+v", r)
	}
	if r.Endpoint != "mc-1.cfg.cache.amazonaws.com" {
		t.Errorf("unexpected endpoint: %q", r.Endpoint)
	}

	// Standalone node: falls back to the first cache node endpoint.
	c.ConfigurationEndpoint = nil
	c.CacheNodes = []ectypes.CacheNode{
		{Endpoint: &ectypes.Endpoint{Address: aws.String("node-0001.cache.amazonaws.com")}},
	}
	r = cacheClusterResource(c, "us-east-1", "1")
	if r.Endpoint != "node-0001.cache.amazonaws.com" {
		t.Errorf("expected node endpoint fallback, got %q", r.Endpoint)
	}
}

func TestServerlessCacheResource(t *testing.T) {
	s := ectypes.ServerlessCache{
		ARN:                 aws.String("arn:aws:elasticache:us-east-1:1:serverlesscache:sc-1"),
		ServerlessCacheName: aws.String("sc-1"),
		Engine:              aws.String("redis"),
		FullEngineVersion:   aws.String("7.1"),
		Status:              aws.String("available"),
		Endpoint:            &ectypes.Endpoint{Address: aws.String("sc-1.serverless.cache.amazonaws.com")},
	}
	r := serverlessCacheResource(s, "us-east-1", "1")
	if r.Kind != "serverless" || r.Service != model.ServiceElastiCache {
		t.Errorf("unexpected kind/service: %+v", r)
	}
	if r.Name != "sc-1" || r.Engine != "redis" || r.EngineVersion != "7.1" {
		t.Errorf("unexpected identity fields: %+v", r)
	}
	if r.Endpoint != "sc-1.serverless.cache.amazonaws.com" {
		t.Errorf("unexpected endpoint: %q", r.Endpoint)
	}
}
