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
	if r.Service != model.ServiceElastiCache || r.Type != model.TypeElastiCacheReplicationGroup {
		t.Errorf("unexpected service/type: %+v", r)
	}
	if got := r.Attr(model.AttrEngine); got != "valkey" {
		t.Errorf("engine = %q, want valkey", got)
	}
	if got := r.Attr(model.AttrInstanceClass); got != "cache.r7g.large" {
		t.Errorf("instance_class = %q, want cache.r7g.large", got)
	}
	if got := r.Attr(model.AttrMultiAZ); got != "true" {
		t.Errorf("multi_az = %q, want \"true\" for MultiAZStatusEnabled", got)
	}
	if got := r.Attr(model.AttrEndpoint); got != "primary.rg-1.cache.amazonaws.com" {
		t.Errorf("expected primary endpoint fallback, got %q", got)
	}

	// An empty MultiAZ enum means the API did not report it, which must leave
	// the key absent rather than reading as "false".
	g.MultiAZ = ""
	r = replicationGroupResource(g, "us-east-1", "1")
	if _, ok := r.Attributes[model.AttrMultiAZ]; ok {
		t.Errorf("expected no multi_az attribute, got %q", r.Attr(model.AttrMultiAZ))
	}

	// ConfigurationEndpoint wins when present (cluster-mode enabled).
	g.ConfigurationEndpoint = &ectypes.Endpoint{Address: aws.String("config.rg-1.cache.amazonaws.com")}
	r = replicationGroupResource(g, "us-east-1", "1")
	if got := r.Attr(model.AttrEndpoint); got != "config.rg-1.cache.amazonaws.com" {
		t.Errorf("expected configuration endpoint, got %q", got)
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
	if r.Type != model.TypeElastiCacheCacheCluster || r.Service != model.ServiceElastiCache {
		t.Errorf("unexpected type/service: %+v", r)
	}
	if got := r.Attr(model.AttrEngine); got != "memcached" {
		t.Errorf("engine = %q, want memcached", got)
	}
	if got := r.Attr(model.AttrEngineVersion); got != "1.6.22" {
		t.Errorf("engine_version = %q, want 1.6.22", got)
	}
	if got := r.Attr(model.AttrEndpoint); got != "mc-1.cfg.cache.amazonaws.com" {
		t.Errorf("unexpected endpoint: %q", got)
	}

	// Standalone node: falls back to the first cache node endpoint.
	c.ConfigurationEndpoint = nil
	c.CacheNodes = []ectypes.CacheNode{
		{Endpoint: &ectypes.Endpoint{Address: aws.String("node-0001.cache.amazonaws.com")}},
	}
	r = cacheClusterResource(c, "us-east-1", "1")
	if got := r.Attr(model.AttrEndpoint); got != "node-0001.cache.amazonaws.com" {
		t.Errorf("expected node endpoint fallback, got %q", got)
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
	if r.Type != model.TypeElastiCacheServerlessCache || r.Service != model.ServiceElastiCache {
		t.Errorf("unexpected type/service: %+v", r)
	}
	if r.Name != "sc-1" {
		t.Errorf("unexpected name: %q", r.Name)
	}
	if got := r.Attr(model.AttrEngine); got != "redis" {
		t.Errorf("engine = %q, want redis", got)
	}
	if got := r.Attr(model.AttrEngineVersion); got != "7.1" {
		t.Errorf("engine_version = %q, want 7.1", got)
	}
	if got := r.Attr(model.AttrEndpoint); got != "sc-1.serverless.cache.amazonaws.com" {
		t.Errorf("unexpected endpoint: %q", got)
	}
}
