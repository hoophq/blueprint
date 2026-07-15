// Package demo provides a built-in fixture snapshot so anyone can render the
// report without AWS credentials (blueprint scan --demo).
package demo

import (
	"fmt"
	"time"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scanners"
)

// Fixture account IDs.
const (
	acctProd    = "111111111111"
	acctStaging = "222222222222"
)

// Snapshot returns fixture data with deterministic resources (GeneratedAt is
// the wall clock) resembling a mid-size multi-account, multi-region estate:
// 46 resources across all supported services, with realistic tag hygiene
// gaps (~30% missing owner, ~20% missing environment) and two scan failures
// for the honesty ledger.
func Snapshot(version string) *model.Snapshot {
	snap := &model.Snapshot{
		Version:     version,
		GeneratedAt: time.Now().UTC(),
		Accounts:    []string{acctProd, acctStaging},
		Regions:     []string{"us-east-1", "us-west-2", "sa-east-1", "eu-west-1"},
		Resources:   resources(),
		Failures: []model.Failure{
			{AccountID: acctProd, Region: "sa-east-1", Service: model.ServiceElastiCache,
				Error: "AccessDenied: User is not authorized to perform elasticache:DescribeCacheClusters"},
			{AccountID: acctStaging, Region: "eu-west-1", Service: model.ServiceDynamoDB,
				Error: "ThrottlingException: Rate exceeded (retries exhausted)"},
		},
	}
	snap.Finalize()
	return snap
}

func resources() []model.Resource {
	return []model.Resource{
		// ── prod account · us-east-1 ────────────────────────────────────
		res(acctProd, "us-east-1", model.ServiceRDS, "instance", "orders-prod",
			"postgres", "15.4", "db.r6g.xlarge", 500, true, "available",
			"orders-prod.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2019, 3, 14),
			tags("environment", "production", "owner", "payments", "app", "orders")),
		res(acctProd, "us-east-1", model.ServiceRDS, "instance", "orders-prod-replica",
			"postgres", "15.4", "db.r6g.xlarge", 500, false, "available",
			"orders-prod-replica.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2021, 6, 2),
			tags("environment", "production", "owner", "payments", "app", "orders")),
		res(acctProd, "us-east-1", model.ServiceRDS, "instance", "billing-db",
			"mysql", "8.0.35", "db.m6g.large", 200, true, "available",
			"billing-db.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2020, 1, 21),
			tags("environment", "production", "owner", "billing")),
		res(acctProd, "us-east-1", model.ServiceRDS, "instance", "legacy-crm",
			"mysql", "5.7.44", "db.m5.large", 100, false, "available",
			"legacy-crm.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2017, 8, 2),
			tags("app", "crm")), // no owner, no environment
		res(acctProd, "us-east-1", model.ServiceRDS, "instance", "auth-db",
			"postgres", "14.11", "db.m6g.large", 100, true, "available",
			"auth-db.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2020, 9, 10),
			tags("environment", "production", "team", "identity")),
		res(acctProd, "us-east-1", model.ServiceRDS, "instance", "reporting-mart",
			"sqlserver-se", "15.00.4345.5.v1", "db.m5.xlarge", 400, false, "available",
			"reporting-mart.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2018, 11, 27),
			tags("environment", "production", "cost-center", "finance")), // no owner
		// Two db.r6g.2xlarge writers/readers back this cluster; the census
		// counts the cluster only, so it carries the MultiAZ flag.
		res(acctProd, "us-east-1", model.ServiceAurora, "cluster", "users-aurora",
			"aurora-postgresql", "15.4", "", 0, true, "available",
			"users-aurora.cluster-c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2021, 2, 17),
			tags("environment", "production", "owner", "identity")),
		res(acctProd, "us-east-1", model.ServiceAurora, "cluster", "checkout-aurora",
			"aurora-mysql", "8.0.mysql_aurora.3.05.2", "", 0, false, "available",
			"checkout-aurora.cluster-c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2023, 4, 11),
			tags("environment", "production", "owner", "checkout")),
		res(acctProd, "us-east-1", model.ServiceDynamoDB, "table", "sessions",
			"dynamodb", "", "", 18, false, "active", "", d(2020, 5, 7),
			tags("environment", "production", "owner", "platform")),
		res(acctProd, "us-east-1", model.ServiceDynamoDB, "table", "carts",
			"dynamodb", "", "", 6, false, "active", "", d(2020, 5, 7),
			tags("environment", "production", "owner", "checkout")),
		res(acctProd, "us-east-1", model.ServiceDynamoDB, "table", "feature-flags",
			"dynamodb", "", "", 1, false, "active", "", d(2022, 10, 19),
			tags("created-by", "terraform")), // no owner, no environment
		res(acctProd, "us-east-1", model.ServiceDynamoDB, "table", "ratelimits",
			"dynamodb", "", "", 2, false, "active", "", d(2023, 1, 9),
			tags("env", "production", "team", "platform")),
		res(acctProd, "us-east-1", model.ServiceElastiCache, "cluster", "checkout-cache",
			"redis", "7.1.0", "cache.r6g.large", 0, false, "available",
			"checkout-cache.kx3q9f.ng.0001.use1.cache.amazonaws.com", d(2021, 7, 29),
			tags("environment", "production", "owner", "checkout")),
		res(acctProd, "us-east-1", model.ServiceElastiCache, "cluster", "sessions-cache",
			"redis", "7.0.7", "cache.m6g.large", 0, false, "available",
			"sessions-cache.kx3q9f.ng.0001.use1.cache.amazonaws.com", d(2020, 5, 8),
			tags("environment", "production", "squad", "platform")),
		res(acctProd, "us-east-1", model.ServiceElastiCache, "instance", "legacy-memcache",
			"memcached", "1.6.17", "cache.t3.medium", 0, false, "available",
			"legacy-memcache.kx3q9f.cfg.use1.cache.amazonaws.com", d(2018, 3, 30),
			nil), // no tags at all
		res(acctProd, "us-east-1", model.ServiceRedshift, "cluster", "analytics-wh",
			"redshift", "1.0.63269", "ra3.4xlarge", 3200, false, "available",
			"analytics-wh.cbq7xz1t9e2m.us-east-1.redshift.amazonaws.com", d(2019, 10, 15),
			tags("environment", "production", "owner", "data")),
		res(acctProd, "us-east-1", model.ServiceRedshift, "serverless", "analytics-serverless",
			"redshift-serverless", "", "", 0, false, "available",
			"analytics-serverless.111111111111.us-east-1.redshift-serverless.amazonaws.com", d(2024, 2, 6),
			tags("owner", "data")), // no environment
		res(acctProd, "us-east-1", model.ServiceDocumentDB, "cluster", "catalog-docs",
			"docdb", "5.0.0", "db.r6g.large", 350, false, "available",
			"catalog-docs.cluster-c9k2hxu3qapb.us-east-1.docdb.amazonaws.com", d(2021, 11, 23),
			tags("environment", "production", "owner", "catalog")),
		res(acctProd, "us-east-1", model.ServiceNeptune, "cluster", "fraud-graph",
			"neptune", "1.3.2.0", "db.r5.xlarge", 120, false, "available",
			"fraud-graph.cluster-c9k2hxu3qapb.us-east-1.neptune.amazonaws.com", d(2022, 3, 8),
			tags("environment", "production", "owner", "risk")),

		// ── prod account · us-west-2 ────────────────────────────────────
		res(acctProd, "us-west-2", model.ServiceRDS, "instance", "orders-dr",
			"postgres", "15.4", "db.r6g.xlarge", 500, true, "available",
			"orders-dr.c8m1kwv2rbqc.us-west-2.rds.amazonaws.com", d(2021, 6, 2),
			tags("environment", "production", "owner", "payments", "app", "orders")),
		res(acctProd, "us-west-2", model.ServiceRDS, "instance", "telemetry-tsdb",
			"postgres", "16.2", "db.m6g.2xlarge", 1000, false, "available",
			"telemetry-tsdb.c8m1kwv2rbqc.us-west-2.rds.amazonaws.com", d(2023, 9, 18),
			tags("environment", "production", "owner", "observability")),
		res(acctProd, "us-west-2", model.ServiceAurora, "cluster", "search-metadata",
			"aurora-postgresql", "14.9", "", 0, false, "available",
			"search-metadata.cluster-c8m1kwv2rbqc.us-west-2.rds.amazonaws.com", d(2022, 1, 26),
			tags("environment", "production")), // no owner
		res(acctProd, "us-west-2", model.ServiceDynamoDB, "table", "events-firehose",
			"dynamodb", "", "", 240, false, "active", "", d(2021, 4, 14),
			tags("environment", "production", "owner", "data")),
		res(acctProd, "us-west-2", model.ServiceDynamoDB, "table", "device-registry",
			"dynamodb", "", "", 9, false, "active", "", d(2019, 12, 3),
			tags("created-by", "console")), // no owner, no environment
		res(acctProd, "us-west-2", model.ServiceElastiCache, "cluster", "queue-cache",
			"redis", "6.2.14", "cache.m5.large", 0, false, "available",
			"queue-cache.kx3q9f.ng.0001.usw2.cache.amazonaws.com", d(2019, 7, 22),
			tags("environment", "production")), // no owner
		res(acctProd, "us-west-2", model.ServiceRedshift, "cluster", "analytics-dr",
			"redshift", "1.0.63269", "ra3.4xlarge", 3200, false, "paused",
			"analytics-dr.cbq7xz1t9e2m.us-west-2.redshift.amazonaws.com", d(2020, 10, 15),
			tags("environment", "production", "owner", "data")),

		// ── prod account · sa-east-1 ────────────────────────────────────
		res(acctProd, "sa-east-1", model.ServiceRDS, "instance", "orders-latam",
			"postgres", "15.4", "db.m6g.large", 200, true, "available",
			"orders-latam.c7n4jyt5sdrc.sa-east-1.rds.amazonaws.com", d(2022, 5, 31),
			tags("environment", "production", "owner", "payments-latam")),
		res(acctProd, "sa-east-1", model.ServiceRDS, "instance", "invoices-br",
			"mysql", "8.0.36", "db.t3.medium", 50, false, "storage-full",
			"invoices-br.c7n4jyt5sdrc.sa-east-1.rds.amazonaws.com", d(2023, 2, 14),
			tags("environment", "production")), // no owner
		res(acctProd, "sa-east-1", model.ServiceDynamoDB, "table", "carts-latam",
			"dynamodb", "", "", 3, false, "active", "", d(2022, 6, 9),
			tags("environment", "production", "owner", "checkout")),
		res(acctProd, "sa-east-1", model.ServiceDynamoDB, "table", "nfe-receipts",
			"dynamodb", "", "", 27, false, "active", "", d(2022, 11, 1),
			nil), // no tags at all
		res(acctProd, "sa-east-1", model.ServiceDocumentDB, "cluster", "catalog-docs-latam",
			"docdb", "4.0.0", "db.r5.large", 180, false, "available",
			"catalog-docs-latam.cluster-c7n4jyt5sdrc.sa-east-1.docdb.amazonaws.com", d(2021, 8, 17),
			tags("environment", "production")), // no owner

		// ── staging account · us-east-1 ─────────────────────────────────
		res(acctStaging, "us-east-1", model.ServiceRDS, "instance", "orders-staging",
			"postgres", "15.4", "db.t4g.medium", 100, false, "available",
			"orders-staging.c5p8gxr9tfsd.us-east-1.rds.amazonaws.com", d(2021, 6, 15),
			tags("environment", "staging", "owner", "payments")),
		res(acctStaging, "us-east-1", model.ServiceRDS, "instance", "billing-staging",
			"mysql", "8.0.35", "db.t3.medium", 50, false, "stopped",
			"billing-staging.c5p8gxr9tfsd.us-east-1.rds.amazonaws.com", d(2021, 3, 4),
			tags("environment", "staging", "owner", "billing")),
		res(acctStaging, "us-east-1", model.ServiceRDS, "instance", "qa-sandbox",
			"postgres", "13.13", "db.t3.micro", 20, false, "available",
			"qa-sandbox.c5p8gxr9tfsd.us-east-1.rds.amazonaws.com", d(2020, 7, 8),
			nil), // no tags at all
		res(acctStaging, "us-east-1", model.ServiceRDS, "instance", "load-test-db",
			"postgres", "15.4", "db.r6g.large", 200, false, "available",
			"load-test-db.c5p8gxr9tfsd.us-east-1.rds.amazonaws.com", d(2024, 1, 30),
			tags("stage", "staging")), // env via "stage" key, no owner
		res(acctStaging, "us-east-1", model.ServiceAurora, "cluster", "users-aurora-staging",
			"aurora-postgresql", "15.4", "", 0, false, "available",
			"users-aurora-staging.cluster-c5p8gxr9tfsd.us-east-1.rds.amazonaws.com", d(2021, 9, 2),
			tags("environment", "staging", "owner", "identity")),
		res(acctStaging, "us-east-1", model.ServiceDynamoDB, "table", "sessions-staging",
			"dynamodb", "", "", 2, false, "active", "", d(2020, 5, 20),
			tags("environment", "staging", "owner", "platform")),
		res(acctStaging, "us-east-1", model.ServiceDynamoDB, "table", "feature-flags-staging",
			"dynamodb", "", "", 1, false, "active", "", d(2022, 10, 19),
			nil), // no tags at all
		res(acctStaging, "us-east-1", model.ServiceDynamoDB, "table", "integration-test-artifacts",
			"dynamodb", "", "", 55, false, "active", "", d(2023, 6, 27),
			tags("environment", "staging", "owner", "qa")),
		res(acctStaging, "us-east-1", model.ServiceElastiCache, "cluster", "checkout-cache-staging",
			"redis", "7.1.0", "cache.t4g.small", 0, false, "available",
			"checkout-cache-staging.kx3q9f.ng.0001.use1.cache.amazonaws.com", d(2021, 8, 5),
			tags("environment", "staging", "owner", "checkout")),
		res(acctStaging, "us-east-1", model.ServiceRedshift, "cluster", "analytics-wh-staging",
			"redshift", "1.0.61395", "dc2.large", 640, false, "available",
			"analytics-wh-staging.cbq7xz1t9e2m.us-east-1.redshift.amazonaws.com", d(2020, 12, 1),
			tags("environment", "staging", "owner", "data")),
		res(acctStaging, "us-east-1", model.ServiceDocumentDB, "cluster", "catalog-docs-staging",
			"docdb", "5.0.0", "db.t3.medium", 40, false, "available",
			"catalog-docs-staging.cluster-c5p8gxr9tfsd.us-east-1.docdb.amazonaws.com", d(2022, 2, 22),
			tags("owner", "catalog")), // no environment
		res(acctStaging, "us-east-1", model.ServiceNeptune, "cluster", "fraud-graph-staging",
			"neptune", "1.3.2.0", "db.t3.medium", 15, false, "available",
			"fraud-graph-staging.cluster-c5p8gxr9tfsd.us-east-1.neptune.amazonaws.com", d(2022, 4, 12),
			tags("environment", "staging", "owner", "risk")),

		// ── staging account · eu-west-1 ─────────────────────────────────
		res(acctStaging, "eu-west-1", model.ServiceRDS, "instance", "gdpr-test-db",
			"postgres", "15.4", "db.t3.medium", 50, false, "available",
			"gdpr-test-db.c2r6fwq8vhte.eu-west-1.rds.amazonaws.com", d(2023, 5, 16),
			tags("environment", "staging", "owner", "compliance")),
		res(acctStaging, "eu-west-1", model.ServiceRDS, "instance", "data-residency-poc",
			"mariadb", "10.11.6", "db.t3.small", 20, false, "available",
			"data-residency-poc.c2r6fwq8vhte.eu-west-1.rds.amazonaws.com", d(2024, 3, 7),
			nil), // no tags at all
		res(acctStaging, "eu-west-1", model.ServiceElastiCache, "cluster", "gdpr-cache",
			"redis", "7.0.7", "cache.t4g.micro", 0, false, "available",
			"gdpr-cache.kx3q9f.ng.0001.euw1.cache.amazonaws.com", d(2023, 5, 16),
			tags("environment", "staging", "owner", "compliance")),
	}
}

// res builds one fixture resource with a service-appropriate ARN.
func res(account, region, svc, kind, name, engine, version, class string,
	storageGB int32, multiAZ bool, status, endpoint string,
	created time.Time, t map[string]string) model.Resource {
	return model.Resource{
		ARN:           arnFor(svc, kind, region, account, name),
		Service:       svc,
		Kind:          kind,
		Name:          name,
		Engine:        engine,
		EngineVersion: version,
		InstanceClass: class,
		StorageGB:     storageGB,
		MultiAZ:       multiAZ,
		Status:        status,
		Endpoint:      endpoint,
		Region:        region,
		AccountID:     account,
		CreatedAt:     &created,
		Tags:          t,
	}
}

func arnFor(svc, kind, region, account, name string) string {
	switch svc {
	case model.ServiceDynamoDB:
		return fmt.Sprintf("arn:aws:dynamodb:%s:%s:table/%s", region, account, name)
	case model.ServiceElastiCache:
		return fmt.Sprintf("arn:aws:elasticache:%s:%s:cluster:%s", region, account, name)
	case model.ServiceRedshift:
		if kind == "serverless" {
			return fmt.Sprintf("arn:aws:redshift-serverless:%s:%s:workgroup/%s", region, account, name)
		}
		// Reuse the scanner's builder so fixture ARNs match real scan output.
		return scanners.RedshiftClusterARN("aws", region, account, name)
	default: // rds, aurora, documentdb, neptune share the RDS ARN namespace
		if kind == "instance" {
			return fmt.Sprintf("arn:aws:rds:%s:%s:db:%s", region, account, name)
		}
		return fmt.Sprintf("arn:aws:rds:%s:%s:cluster:%s", region, account, name)
	}
}

// d returns a fixed timestamp so resource timestamps are deterministic.
func d(year, month, day int) time.Time {
	return time.Date(year, time.Month(month), day, 10, 30, 0, 0, time.UTC)
}

// tags builds a tag map from alternating key/value pairs.
func tags(kv ...string) map[string]string {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}
