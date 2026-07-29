// Package demo provides a built-in fixture snapshot so anyone can render the
// report without AWS credentials (blueprint scan --demo).
package demo

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
	"github.com/hoophq/blueprint/internal/scanners"
)

// Fixture account IDs.
const (
	acctProd    = "111111111111"
	acctStaging = "222222222222"
)

// Snapshot returns fixture data with deterministic resources (GeneratedAt is
// the wall clock) resembling a mid-size multi-account, multi-region estate:
// 54 resources across all supported services, with realistic tag hygiene
// gaps (~30% missing owner, ~20% missing environment) and two scan failures
// for the honesty ledger.
func Snapshot(version string) *model.Snapshot {
	now := time.Now().UTC()
	snap := &model.Snapshot{
		Schema:      model.SchemaVersion,
		Version:     version,
		GeneratedAt: now,
		Accounts:    []string{acctProd, acctStaging},
		Regions:     []string{"us-east-1", "us-west-2", "sa-east-1", "eu-west-1"},
		// The demo simulates a scan by this binary, so its coverage scope is
		// the same scanner registry the real runner uses (importing
		// internal/scanners above registers them).
		Services:  registeredServices(),
		Resources: applyExposure(resources()),
		// Failure times are offset from the run's start, since that is what
		// they look like in a real scan: the ledger is written as units come
		// back, not when the run began.
		Failures: []model.Failure{
			{AccountID: acctProd, Region: "sa-east-1", Service: model.ServiceElastiCache,
				Error: "AccessDenied: User is not authorized to perform elasticache:DescribeCacheClusters",
				Time:  now.Add(2 * time.Second)},
			{AccountID: acctStaging, Region: "eu-west-1", Service: model.ServiceDynamoDB,
				Error: "ThrottlingException: Rate exceeded (retries exhausted)",
				Time:  now.Add(5 * time.Second)},
		},
	}
	snap.Finalize()
	return snap
}

func registeredServices() []string {
	var out []string
	for _, s := range scan.All() {
		out = append(out, s.Service())
	}
	return out
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
		// Serverless is sized by a number of RPUs instead of a named instance
		// class, so it exercises the one shape whose capacity lives in the
		// measure bag rather than the attribute bag.
		withMeasure(res(acctProd, "us-east-1", model.ServiceRedshift, "serverless", "analytics-serverless",
			"redshift-serverless", "", "", 0, false, "available",
			"analytics-serverless.111111111111.us-east-1.redshift-serverless.amazonaws.com", d(2024, 2, 6),
			tags("owner", "data")), // no environment
			model.MeasureBaseCapacityRPU, 8),
		res(acctProd, "us-east-1", model.ServiceDocumentDB, "cluster", "catalog-docs",
			"docdb", "5.0.0", "db.r6g.large", 350, false, "available",
			"catalog-docs.cluster-c9k2hxu3qapb.us-east-1.docdb.amazonaws.com", d(2021, 11, 23),
			tags("environment", "production", "owner", "catalog")),
		res(acctProd, "us-east-1", model.ServiceNeptune, "cluster", "fraud-graph",
			"neptune", "1.3.2.0", "db.r5.xlarge", 120, false, "available",
			"fraud-graph.cluster-c9k2hxu3qapb.us-east-1.neptune.amazonaws.com", d(2022, 3, 8),
			tags("environment", "production", "owner", "risk")),
		ec2Instance(acctProd, "us-east-1", "us-east-1a", "i-0a1b2c3d4e5f60011",
			"m5.xlarge", "Linux/UNIX", "running", "ip-10-0-1-11.ec2.internal",
			false, []string{"vol-0a1b2c3d4e5f60011"}, d(2021, 4, 2),
			tags("Name", "api-gateway-1", "environment", "production", "owner", "platform")),
		// Two volumes, to exercise the attachment list joining more than one.
		ec2Instance(acctProd, "us-east-1", "us-east-1b", "i-0a1b2c3d4e5f60012",
			"m5.xlarge", "Linux/UNIX", "running", "ip-10-0-2-12.ec2.internal",
			false, []string{"vol-0a1b2c3d4e5f60013", "vol-0a1b2c3d4e5f60012"}, d(2021, 4, 2),
			tags("Name", "api-gateway-2", "environment", "production", "owner", "platform")),
		// The one box with a public IPv4 — an exposure row that is not a database.
		ec2Instance(acctProd, "us-east-1", "us-east-1a", "i-0a1b2c3d4e5f60013",
			"t3.small", "Linux/UNIX", "running", "ip-10-0-1-20.ec2.internal",
			true, []string{"vol-0a1b2c3d4e5f60020"}, d(2018, 6, 11),
			tags("Name", "bastion", "environment", "production")), // no owner
		// No tags at all, so the name falls back to the instance ID the way an
		// untagged instance reads in the console.
		ec2Instance(acctProd, "us-east-1", "us-east-1b", "i-0a1b2c3d4e5f60014",
			"r5.2xlarge", "Red Hat Enterprise Linux", "running", "ip-10-0-2-33.ec2.internal",
			false, []string{"vol-0a1b2c3d4e5f60014"}, d(2019, 9, 5),
			nil),

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
		ec2Instance(acctProd, "us-west-2", "us-west-2a", "i-0b2c3d4e5f6a70021",
			"c5.large", "Linux/UNIX", "running", "ip-10-1-1-21.us-west-2.compute.internal",
			false, []string{"vol-0b2c3d4e5f6a70021"}, d(2022, 3, 15),
			tags("Name", "batch-worker-1", "environment", "production", "owner", "data")),

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
		// Stopped, and therefore not publicly accessible: an instance without an
		// Elastic IP releases its public address when it stops.
		ec2Instance(acctProd, "sa-east-1", "sa-east-1a", "i-0c3d4e5f6a7b80031",
			"t3.medium", "Windows", "stopped", "ip-10-2-1-31.sa-east-1.compute.internal",
			false, []string{"vol-0c3d4e5f6a7b80031"}, d(2020, 11, 19),
			tags("Name", "nfe-gateway", "environment", "production", "owner", "payments-latam")),

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
		ec2Instance(acctStaging, "us-east-1", "us-east-1a", "i-0d4e5f6a7b8c90041",
			"t3.medium", "Linux/UNIX", "running", "ip-10-3-1-41.ec2.internal",
			false, []string{"vol-0d4e5f6a7b8c90041"}, d(2023, 1, 12),
			tags("Name", "ci-runner", "environment", "staging", "owner", "platform")),
		ec2Instance(acctStaging, "us-east-1", "us-east-1b", "i-0d4e5f6a7b8c90042",
			"t3.micro", "Linux/UNIX", "stopped", "ip-10-3-2-42.ec2.internal",
			false, []string{"vol-0d4e5f6a7b8c90042"}, d(2022, 8, 3),
			tags("Name", "scratch-box")), // no owner, no environment

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

// applyExposure fills exposure fields the way the real scanners report them:
// the RDS family and Redshift always carry all three, Aurora clusters have no
// public-accessibility flag, Redshift serverless only reports public access,
// ElastiCache Redis reports encryption and snapshot retention, and memcached
// and DynamoDB report nothing. A few fixtures are deliberately risky so the
// report has exposure rows to show.
func applyExposure(rs []model.Resource) []model.Resource {
	public := map[string]bool{"legacy-crm": true, "data-residency-poc": true}
	unencrypted := map[string]bool{"legacy-crm": true, "qa-sandbox": true}
	noBackups := map[string]bool{"legacy-crm": true, "qa-sandbox": true, "load-test-db": true}
	for i := range rs {
		r := &rs[i]
		switch r.Service {
		case model.ServiceRDS, model.ServiceDocumentDB, model.ServiceNeptune:
			r.PubliclyAccessible = ptr(public[r.Name])
			fallthrough
		case model.ServiceAurora:
			r.Encrypted = ptr(!unencrypted[r.Name])
			days := int64(7)
			if noBackups[r.Name] {
				days = 0
			}
			r.SetMeasure(model.MeasureBackupRetentionDays, days)
		case model.ServiceRedshift:
			r.PubliclyAccessible = ptr(false)
			if r.Type != model.TypeRedshiftServerlessWorkgroup {
				r.Encrypted = ptr(true)
				r.SetMeasure(model.MeasureBackupRetentionDays, 1)
			}
		case model.ServiceElastiCache:
			if r.Attr(model.AttrEngine) == "redis" {
				r.Encrypted = ptr(true)
				r.SetMeasure(model.MeasureBackupRetentionDays, 1)
			}
		}
	}
	return rs
}

func ptr[T any](v T) *T { return &v }

// withMeasure attaches a measure res() has no parameter for. res() covers the
// dimensions most of the fixture shares; anything one service alone is sized
// by belongs at its own callsite rather than in the shared signature.
func withMeasure(r model.Resource, key string, v int64) model.Resource {
	r.SetMeasure(key, v)
	return r
}

// res builds one fixture resource with a service-appropriate ARN. shape is a
// fixture-local discriminator (instance | cluster | table | serverless) used
// to pick the CloudFormation type name and ARN namespace — real scanners get
// both from the API response instead.
func res(account, region, svc, shape, name, engine, version, class string,
	storageGB int32, multiAZ bool, status, endpoint string,
	created time.Time, t map[string]string) model.Resource {
	r := model.Resource{
		ARN:       arnFor(svc, shape, region, account, name),
		Service:   svc,
		Type:      typeFor(svc, shape),
		Name:      name,
		Status:    status,
		Region:    region,
		AccountID: account,
		CreatedAt: &created,
		Tags:      t,
	}
	r.SetAttr(model.AttrEngine, engine)
	r.SetAttr(model.AttrEngineVersion, version)
	r.SetAttr(model.AttrInstanceClass, class)
	r.SetAttr(model.AttrEndpoint, endpoint)
	// Multi-AZ mirrors which control planes actually report it: the RDS family
	// and provisioned Redshift/ElastiCache clusters do; DynamoDB and the
	// serverless/standalone shapes never do, so the key stays absent — the bag
	// equivalent of the nil that used to mean "not reported".
	if reportsMultiAZ(svc, shape) {
		r.SetBoolAttr(model.AttrMultiAZ, &multiAZ)
	}
	if storageGB > 0 {
		r.SetMeasure(model.MeasureSizeBytes, int64(storageGB)<<30)
	}
	return r
}

func reportsMultiAZ(svc, shape string) bool {
	switch svc {
	case model.ServiceDynamoDB:
		return false
	case model.ServiceElastiCache:
		return shape == "cluster"
	case model.ServiceRedshift:
		return shape != "serverless"
	default:
		return true
	}
}

func typeFor(svc, shape string) string {
	switch svc {
	case model.ServiceDynamoDB:
		return model.TypeDynamoDBTable
	case model.ServiceElastiCache:
		switch shape {
		case "cluster":
			return model.TypeElastiCacheReplicationGroup
		case "serverless":
			return model.TypeElastiCacheServerlessCache
		default:
			return model.TypeElastiCacheCacheCluster
		}
	case model.ServiceRedshift:
		if shape == "serverless" {
			return model.TypeRedshiftServerlessWorkgroup
		}
		return model.TypeRedshiftCluster
	case model.ServiceDocumentDB:
		if shape == "instance" {
			return model.TypeDocDBInstance
		}
		return model.TypeDocDBCluster
	case model.ServiceNeptune:
		if shape == "instance" {
			return model.TypeNeptuneInstance
		}
		return model.TypeNeptuneCluster
	default: // rds and aurora both live in the RDS CloudFormation namespace
		if shape == "instance" {
			return model.TypeRDSInstance
		}
		return model.TypeRDSCluster
	}
}

func arnFor(svc, shape, region, account, name string) string {
	switch svc {
	case model.ServiceDynamoDB:
		return fmt.Sprintf("arn:aws:dynamodb:%s:%s:table/%s", region, account, name)
	case model.ServiceElastiCache:
		return fmt.Sprintf("arn:aws:elasticache:%s:%s:cluster:%s", region, account, name)
	case model.ServiceRedshift:
		if shape == "serverless" {
			return fmt.Sprintf("arn:aws:redshift-serverless:%s:%s:workgroup/%s", region, account, name)
		}
		// Reuse the scanner's builder so fixture ARNs match real scan output.
		return scanners.RedshiftClusterARN("aws", region, account, name)
	default: // rds, aurora, documentdb, neptune share the RDS ARN namespace
		if shape == "instance" {
			return fmt.Sprintf("arn:aws:rds:%s:%s:db:%s", region, account, name)
		}
		return fmt.Sprintf("arn:aws:rds:%s:%s:cluster:%s", region, account, name)
	}
}

// ec2Instance builds one fixture EC2 instance. res() is shaped for managed
// databases — engine, version, storage, Multi-AZ — and compute shares almost
// none of those dimensions, so it gets its own constructor rather than four
// more parameters most callers would pass empty (the reason withMeasure
// exists, applied to a whole service).
//
// The name is resolved the way the scanner resolves it: the Name tag when
// there is one, the instance ID otherwise, so an untagged fixture box reads
// exactly like an untagged real one.
func ec2Instance(account, region, az, id, instanceType, platform, status, privateDNS string,
	publicIP bool, volumes []string, launched time.Time, t map[string]string) model.Resource {
	name := t["Name"]
	if name == "" {
		name = id
	}
	r := model.Resource{
		// Reuse the scanner's builder so fixture ARNs match real scan output.
		ARN:       scanners.EC2InstanceARN("aws", region, account, id),
		Service:   model.ServiceEC2,
		Type:      model.TypeEC2Instance,
		Name:      name,
		Status:    status,
		Region:    region,
		AccountID: account,
		// LaunchTime, which is what EC2 reports — the last start, not the
		// original creation.
		CreatedAt: &launched,
		Tags:      t,
		// Set here rather than in applyExposure: unlike the database fixtures,
		// whose exposure is overlaid by name, this follows from a field the
		// callsite already states. Encrypted stays nil on purpose — on EC2
		// that is a property of each attached volume, not of the instance.
		PubliclyAccessible: ptr(publicIP),
	}
	r.SetAttr(model.AttrInstanceClass, instanceType)
	r.SetAttr(model.AttrPlatform, platform)
	// Private DNS only; the public name is an exposure signal, not an endpoint.
	r.SetAttr(model.AttrEndpoint, privateDNS)
	r.SetAttr(model.AttrAvailabilityZone, az)
	r.SetAttr(model.AttrVPCID, demoVPCs[account+"/"+region])
	r.SetAttr(model.AttrSubnetID, demoSubnets[account+"/"+az])
	// Sorted, like the scanner sorts them, so the fixture cannot drift into a
	// value shape a real scan would never produce. The attachment only: the
	// volumes are their own rows once ATR-182 lands, and summing their sizes
	// here would count the same storage twice estate-wide.
	ids := append([]string(nil), volumes...)
	sort.Strings(ids)
	r.SetAttr(model.AttrEBSVolumeIDs, strings.Join(ids, ","))
	return r
}

// The fixture's network layout: one VPC per account per region, one subnet per
// AZ. Two accounts never share a VPC, which is why these are keyed by account
// and not by region alone.
var (
	demoVPCs = map[string]string{
		acctProd + "/us-east-1":    "vpc-0a1b2c3d4e5f6a7b8",
		acctProd + "/us-west-2":    "vpc-0b2c3d4e5f6a7b8c9",
		acctProd + "/sa-east-1":    "vpc-0c3d4e5f6a7b8c9d0",
		acctStaging + "/us-east-1": "vpc-0d4e5f6a7b8c9d0e1",
	}
	demoSubnets = map[string]string{
		acctProd + "/us-east-1a":    "subnet-0a1b2c3d4e5f6a001",
		acctProd + "/us-east-1b":    "subnet-0a1b2c3d4e5f6a002",
		acctProd + "/us-west-2a":    "subnet-0b2c3d4e5f6a7b001",
		acctProd + "/sa-east-1a":    "subnet-0c3d4e5f6a7b8c001",
		acctStaging + "/us-east-1a": "subnet-0d4e5f6a7b8c9d001",
		acctStaging + "/us-east-1b": "subnet-0d4e5f6a7b8c9d002",
	}
)

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
