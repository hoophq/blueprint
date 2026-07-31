// Package demo provides a built-in fixture snapshot so anyone can render the
// report without AWS credentials (blueprint scan --demo).
package demo

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
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
// 98 resources across all supported services, with realistic tag hygiene
// gaps (~30% missing owner, ~20% missing environment) and five scan failures
// for the honesty ledger.
//
// This is the storyboard: every row is here because some rendering path needs
// it, and the tests name several of them directly. SnapshotN builds on it for
// the scale cases rather than replacing it.
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
			// The other half of the withheld-verdict story: the us-west-2
			// snapshot row carries a source volume ID and no verdict on whether
			// that volume still exists, and this is the entry that says why.
			// Read together they are the guardrail; read alone, either one is
			// just a gap.
			{AccountID: acctProd, Region: "us-west-2", Service: model.ServiceEBS,
				Error: "RequestLimitExceeded: Request limit exceeded (ec2:DescribeVolumes)",
				Time:  now.Add(7 * time.Second)},
			// The same story on a second measure. The load balancers in this
			// region were listed; their target groups were not, so those rows
			// carry no target group count. A partial unit is still a ledgered
			// unit — the rows it did return are real and the gap is stated.
			{AccountID: acctProd, Region: "us-west-2", Service: model.ServiceELB,
				Error: "ThrottlingException: Rate exceeded (elasticloadbalancing:DescribeTargetGroups)",
				Time:  now.Add(9 * time.Second)},
			// A per-bucket call that failed on every bucket it was tried on.
			// The count is the point: the two us-west-2 buckets show no
			// encryption attribute, and this says the setting was unreadable
			// rather than unset. Without it the same rows read as
			// "unencrypted", which is a claim the scan never earned.
			{AccountID: acctProd, Region: "us-west-2", Service: model.ServiceS3,
				Error: "GetBucketEncryption: 2 of 2 buckets: AccessDenied: User is not authorized to perform s3:GetEncryptionConfiguration",
				Time:  now.Add(11 * time.Second)},
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
			"postgres", "15.4", "db.r6g.xlarge", storage(500), true, "available",
			"orders-prod.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2019, 3, 14),
			tags("environment", "production", "owner", "payments", "app", "orders")),
		res(acctProd, "us-east-1", model.ServiceRDS, "instance", "orders-prod-replica",
			"postgres", "15.4", "db.r6g.xlarge", storage(500), false, "available",
			"orders-prod-replica.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2021, 6, 2),
			tags("environment", "production", "owner", "payments", "app", "orders")),
		res(acctProd, "us-east-1", model.ServiceRDS, "instance", "billing-db",
			"mysql", "8.0.35", "db.m6g.large", storage(200), true, "available",
			"billing-db.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2020, 1, 21),
			tags("environment", "production", "owner", "billing")),
		res(acctProd, "us-east-1", model.ServiceRDS, "instance", "legacy-crm",
			"mysql", "5.7.44", "db.m5.large", storage(100), false, "available",
			"legacy-crm.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2017, 8, 2),
			tags("app", "crm")), // no owner, no environment
		res(acctProd, "us-east-1", model.ServiceRDS, "instance", "auth-db",
			"postgres", "14.11", "db.m6g.large", storage(100), true, "available",
			"auth-db.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2020, 9, 10),
			tags("environment", "production", "team", "identity")),
		res(acctProd, "us-east-1", model.ServiceRDS, "instance", "reporting-mart",
			"sqlserver-se", "15.00.4345.5.v1", "db.m5.xlarge", storage(400), false, "available",
			"reporting-mart.c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2018, 11, 27),
			tags("environment", "production", "cost-center", "finance")), // no owner
		// Two db.r6g.2xlarge writers/readers back this cluster; the census
		// counts the cluster only, so it carries the MultiAZ flag.
		res(acctProd, "us-east-1", model.ServiceAurora, "cluster", "users-aurora",
			"aurora-postgresql", "15.4", "", nil, true, "available",
			"users-aurora.cluster-c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2021, 2, 17),
			tags("environment", "production", "owner", "identity")),
		res(acctProd, "us-east-1", model.ServiceAurora, "cluster", "checkout-aurora",
			"aurora-mysql", "8.0.mysql_aurora.3.05.2", "", nil, false, "available",
			"checkout-aurora.cluster-c9k2hxu3qapb.us-east-1.rds.amazonaws.com", d(2023, 4, 11),
			tags("environment", "production", "owner", "checkout")),
		res(acctProd, "us-east-1", model.ServiceDynamoDB, "table", "sessions",
			"dynamodb", "", "PAY_PER_REQUEST", storage(18), false, "active", "", d(2020, 5, 7),
			tags("environment", "production", "owner", "platform")),
		res(acctProd, "us-east-1", model.ServiceDynamoDB, "table", "carts",
			"dynamodb", "", "PAY_PER_REQUEST", storage(6), false, "active", "", d(2020, 5, 7),
			tags("environment", "production", "owner", "checkout")),
		res(acctProd, "us-east-1", model.ServiceDynamoDB, "table", "feature-flags",
			"dynamodb", "", "PROVISIONED", storage(1), false, "active", "", d(2022, 10, 19),
			tags("created-by", "terraform")), // no owner, no environment
		res(acctProd, "us-east-1", model.ServiceDynamoDB, "table", "ratelimits",
			"dynamodb", "", "PAY_PER_REQUEST", storage(2), false, "active", "", d(2023, 1, 9),
			tags("env", "production", "team", "platform")),
		// Zero bytes, and the zero is stored rather than dropped. DynamoDB
		// reported it — the table is either empty or younger than the roughly
		// six-hourly refresh of that figure — and an absent key would claim the
		// service said nothing. Provisioned, so it bills for throughput it is
		// not using. This row's whole job is to prove the zero survives every
		// hop to the page, including a formatter that might round it up.
		res(acctProd, "us-east-1", model.ServiceDynamoDB, "table", "audit-trail",
			"dynamodb", "", "PROVISIONED", storage(0), false, "active", "", d(2024, 11, 4),
			tags("environment", "production", "owner", "compliance")),
		// No engine version on any of the replication groups below.
		// DescribeReplicationGroups reports the engine and not its version — the
		// version lives on the member cache clusters, which this census skips to
		// avoid counting a group twice. The report's lifecycle table therefore
		// has nothing to say about grouped Redis, and that gap is real.
		res(acctProd, "us-east-1", model.ServiceElastiCache, "cluster", "checkout-cache",
			"redis", "", "cache.r6g.large", nil, false, "available",
			"checkout-cache.kx3q9f.ng.0001.use1.cache.amazonaws.com", d(2021, 7, 29),
			tags("environment", "production", "owner", "checkout")),
		res(acctProd, "us-east-1", model.ServiceElastiCache, "cluster", "sessions-cache",
			"redis", "", "cache.m6g.large", nil, false, "available",
			"sessions-cache.kx3q9f.ng.0001.use1.cache.amazonaws.com", d(2020, 5, 8),
			tags("environment", "production", "squad", "platform")),
		// The third ElastiCache shape, and the only cache with no node type at
		// all: serverless is sized by usage, so the report's Class column is
		// legitimately empty here. DescribeServerlessCaches does report the
		// engine's full version — unlike the replication groups above — and
		// reports no encryption flag and no retention, so this row carries
		// neither. See applyExposure, which excludes it by type.
		res(acctProd, "us-east-1", model.ServiceElastiCache, "serverless", "ratelimits-cache",
			"redis", "7.1.0", "", nil, false, "available",
			"ratelimits-cache-kx3q9f.serverless.use1.cache.amazonaws.com", d(2024, 5, 20),
			tags("environment", "production", "owner", "platform")),
		res(acctProd, "us-east-1", model.ServiceElastiCache, "instance", "legacy-memcache",
			"memcached", "1.6.17", "cache.t3.medium", nil, false, "available",
			"legacy-memcache.kx3q9f.cfg.use1.cache.amazonaws.com", d(2018, 3, 30),
			nil), // no tags at all
		res(acctProd, "us-east-1", model.ServiceRedshift, "cluster", "analytics-wh",
			"redshift", "1.0.63269", "ra3.4xlarge", storage(3200), false, "available",
			"analytics-wh.cbq7xz1t9e2m.us-east-1.redshift.amazonaws.com", d(2019, 10, 15),
			tags("environment", "production", "owner", "data")),
		// Serverless is sized by a number of RPUs instead of a named instance
		// class, so it exercises the one shape whose capacity lives in the
		// measure bag rather than the attribute bag.
		withMeasure(res(acctProd, "us-east-1", model.ServiceRedshift, "serverless", "analytics-serverless",
			"redshift-serverless", "", "", nil, false, "available",
			"analytics-serverless.111111111111.us-east-1.redshift-serverless.amazonaws.com", d(2024, 2, 6),
			tags("owner", "data")), // no environment
			model.MeasureBaseCapacityRPU, 8),
		res(acctProd, "us-east-1", model.ServiceDocumentDB, "cluster", "catalog-docs",
			"docdb", "5.0.0", "db.r6g.large", storage(350), false, "available",
			"catalog-docs.cluster-c9k2hxu3qapb.us-east-1.docdb.amazonaws.com", d(2021, 11, 23),
			tags("environment", "production", "owner", "catalog")),
		res(acctProd, "us-east-1", model.ServiceNeptune, "cluster", "fraud-graph",
			"neptune", "1.3.2.0", "db.r5.xlarge", storage(120), false, "available",
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

		// The volumes those instances are attached to, as their own rows. The
		// instance rows record the attachment and none of the bytes, so the
		// same storage is never counted twice.
		ebsVolume(acctProd, "us-east-1", "us-east-1a", "vol-0a1b2c3d4e5f60011",
			"gp3", 100, ptr(int32(3000)), ptr(int32(125)), true,
			[]string{"i-0a1b2c3d4e5f60011"}, d(2021, 4, 2),
			tags("Name", "api-gateway-1-root", "environment", "production", "owner", "platform")),
		ebsVolume(acctProd, "us-east-1", "us-east-1b", "vol-0a1b2c3d4e5f60012",
			"gp3", 200, ptr(int32(3000)), ptr(int32(125)), true,
			[]string{"i-0a1b2c3d4e5f60012"}, d(2021, 4, 2),
			tags("Name", "api-gateway-2-root", "environment", "production", "owner", "platform")),
		// st1 reports neither IOPS nor throughput, so both keys are absent —
		// the fixture's proof that absent and zero stay distinguishable.
		ebsVolume(acctProd, "us-east-1", "us-east-1b", "vol-0a1b2c3d4e5f60013",
			"st1", 4096, nil, nil, true,
			[]string{"i-0a1b2c3d4e5f60012"}, d(2021, 4, 2),
			tags("Name", "api-gateway-2-logs", "environment", "production", "owner", "platform")),
		// A terabyte of gp2 at 3072 provisioned IOPS: the gp2-to-gp3 case, put
		// on the page as three numbers with no recommendation attached.
		ebsVolume(acctProd, "us-east-1", "us-east-1b", "vol-0a1b2c3d4e5f60014",
			"gp2", 1024, ptr(int32(3072)), nil, false,
			[]string{"i-0a1b2c3d4e5f60014"}, d(2019, 9, 5),
			nil),
		ebsVolume(acctProd, "us-east-1", "us-east-1a", "vol-0a1b2c3d4e5f60020",
			"gp2", 8, ptr(int32(100)), nil, false,
			[]string{"i-0a1b2c3d4e5f60013"}, d(2018, 6, 11),
			tags("Name", "bastion-root", "environment", "production")),
		// Unattached, and billing at the full per-GiB rate for six years. State
		// "available" with no attachments is the whole finding.
		ebsVolume(acctProd, "us-east-1", "us-east-1a", "vol-0a1b2c3d4e5f60099",
			"gp2", 512, ptr(int32(1536)), nil, false,
			nil, d(2019, 2, 14),
			tags("Name", "orders-migration-scratch", "environment", "production", "owner", "payments")),

		// A snapshot whose source volume is still there: the ordinary case, and
		// the control the two below are read against.
		ebsSnapshot(acctProd, "us-east-1", "snap-0a1b2c3d4e5f60031", "vol-0a1b2c3d4e5f60011",
			100, 38_654_705_664, true, nil, ptr(true), d(2024, 11, 3),
			tags("Name", "api-gateway-1-root-daily", "environment", "production", "owner", "platform")),
		// Source volume long gone and backing nothing — an orphan, stated as
		// the two facts it is made of rather than as a verdict.
		ebsSnapshot(acctProd, "us-east-1", "snap-0a1b2c3d4e5f60032", "vol-0a1b2c3d4e5f60077",
			512, 190_753_542_144, false, nil, ptr(false), d(2019, 3, 20),
			nil),
		// Reads like an orphan and is not: the source volume is gone, but an AMI
		// is built on it. This row is why the scanner spends a DescribeImages
		// call it otherwise would not need.
		ebsSnapshot(acctProd, "us-east-1", "snap-0a1b2c3d4e5f60033", "vol-0a1b2c3d4e5f60088",
			64, 21_474_836_480, true, []string{"ami-0a1b2c3d4e5f60041"}, ptr(false), d(2020, 7, 9),
			tags("Name", "golden-base-image", "environment", "production", "owner", "platform")),

		// Two NAT gateways, one per AZ, for one VPC. Neither is remarkable on
		// its own and together they are ~$64 a month before a byte of data
		// processing — the multiplier the console never adds up.
		natGateway(acctProd, "us-east-1", "us-east-1a", "nat-0a1b2c3d4e5f60051",
			"203.0.113.11", d(2021, 4, 2),
			tags("Name", "egress-1a", "environment", "production", "owner", "platform")),
		natGateway(acctProd, "us-east-1", "us-east-1b", "nat-0a1b2c3d4e5f60052",
			"203.0.113.12", d(2021, 4, 2),
			tags("Name", "egress-1b", "environment", "production")), // no owner

		// The addresses those gateways hold, as their own rows: each is a
		// separately billable public IPv4, and the gateway row records the same
		// address as the join between the two rather than as a second charge.
		elasticIP(acctProd, "us-east-1", "eipalloc-0a1b2c3d4e5f60061", "203.0.113.11",
			"nat-0a1b2c3d4e5f60051", subnetID(acctProd, "us-east-1a"),
			tags("Name", "egress-1a-eip", "environment", "production", "owner", "platform")),
		elasticIP(acctProd, "us-east-1", "eipalloc-0a1b2c3d4e5f60062", "203.0.113.12",
			"nat-0a1b2c3d4e5f60052", subnetID(acctProd, "us-east-1b"),
			tags("Name", "egress-1b-eip", "environment", "production")), // no owner
		// Allocated, attached to nothing, billing the same as the two above.
		// Since February 2024 that is true of every public IPv4, which turned
		// this from a tidiness problem into a line item.
		elasticIP(acctProd, "us-east-1", "eipalloc-0a1b2c3d4e5f60063", "198.51.100.23",
			"", "", tags("Name", "orders-migration-vip", "environment", "production",
				"owner", "payments")),
		// The bastion's launch-assigned address. No allocation stands behind it,
		// so DescribeAddresses does not return it — this row exists only because
		// the scanner also reads the network interfaces, and in most accounts
		// addresses like this one are the majority.
		autoAssignedIP(acctProd, "us-east-1", "us-east-1a", "eni-0a1b2c3d4e5f60064",
			"198.51.100.77", "i-0a1b2c3d4e5f60013",
			tags("Name", "bastion", "environment", "production")), // no owner

		// The ordinary case: an internet-facing ALB with traffic to send.
		loadBalancerV2(acctProd, "us-east-1", "application", "web", "internet-facing",
			"web-0a1b2c3d4e5f6071.us-east-1.elb.amazonaws.com", 2, ptr(3), d(2021, 4, 2),
			tags("environment", "production", "owner", "platform")),
		// Zero target groups, and the zero is complete — the listing succeeded
		// in this region and found none. A load balancer with nowhere to send
		// traffic, still billing by the hour since the checkout rewrite.
		loadBalancerV2(acctProd, "us-east-1", "application", "legacy-checkout", "internet-facing",
			"legacy-checkout-0a1b2c3d4e5f6072.us-east-1.elb.amazonaws.com", 2, ptr(0), d(2019, 8, 21),
			tags("environment", "production", "owner", "payments")),
		loadBalancerV2(acctProd, "us-east-1", "network", "internal-grpc", "internal",
			"internal-grpc-0a1b2c3d4e5f6073.elb.us-east-1.amazonaws.com", 2, ptr(2), d(2022, 6, 30),
			tags("environment", "production", "owner", "platform")),
		// The other zero, and the one the v2 rows cannot express: the classic
		// API returns registered instances inline, so this count is complete by
		// construction. Nine years old, internal, nothing behind it.
		classicLoadBalancer(acctProd, "us-east-1", "legacy-payments-elb", "internal",
			"internal-legacy-payments-elb-1234567890.us-east-1.elb.amazonaws.com",
			2, 0, d(2016, 2, 9),
			nil), // no tags at all

		// The point of scanning Lambda at all. This function costs nothing at
		// rest, so it has never appeared in a cost report, never triggered a
		// rightsizing recommendation, and nothing has ever prompted anyone to
		// look at it. It has been running on a runtime AWS stopped patching in
		// October 2024 since before the checkout rewrite — and it is wired to
		// the internet-facing ALB two rows up.
		lambdaFunction(acctProd, "us-east-1", "checkout-webhook", "python3.8", "x86_64",
			512, 30, 8_388_608, false, d(2021, 11, 4),
			tags("environment", "production", "owner", "payments")),
		lambdaFunction(acctProd, "us-east-1", "image-resize", "nodejs22.x", "arm64",
			1024, 60, 2_097_152, false, d(2025, 9, 12),
			tags("environment", "production", "owner", "platform")),
		// A container-image function: no runtime reported, so no lifecycle
		// verdict — the base image may well be years stale, but nothing in the
		// response says so and the census does not guess. Its zero code size is
		// the honest one, the image being in ECR. Also the fixture's
		// over-provisioned row: 10 GB of memory and 15 minutes of timeout, paid
		// for by the millisecond on every invocation.
		withMeasure(
			lambdaFunction(acctProd, "us-east-1", "report-generator", "", "x86_64",
				10240, 900, 0, false, d(2024, 7, 19),
				tags("environment", "production", "owner", "data")),
			model.MeasureEphemeralStorageMB, 10240),
		// The estate's largest single thing, and the row a control-plane-only
		// census cannot see: nothing in ListBuckets says four terabytes. Its
		// size arrives with --metrics, from CloudWatch, with an age attached.
		s3Bucket(acctProd, "us-east-1", "acme-prod-assets", "AES256", ptr(false),
			"Enabled", "", bpaLocked, ptr(false), d(2018, 2, 9),
			tags("environment", "production", "owner", "platform", "app", "web")),
		// The finding. Three of the four Block Public Access switches are on,
		// which reads as locked down at a glance and is not: BlockPublicPolicy
		// only refuses *new* public policies, and the one attached in 2019 is
		// still live because RestrictPublicBuckets is off. Public, provably.
		s3Bucket(acctProd, "us-east-1", "acme-public-downloads", "AES256", ptr(false),
			"", "", bpaPolicyLive, ptr(true), d(2019, 4, 30),
			tags("environment", "production", "owner", "marketing")),
		// The well-run one, for contrast: customer-managed KMS with a bucket
		// key, versioned, and every switch on.
		s3Bucket(acctProd, "us-east-1", "acme-cloudtrail-archive", "aws:kms", ptr(true),
			"Enabled", "", bpaLocked, ptr(false), d(2020, 8, 17),
			tags("environment", "production", "owner", "security", "app", "audit")),

		// ── prod account · us-west-2 ────────────────────────────────────
		// The guardrail on the page: DescribeVolumes failed in this region (see
		// the ledger), so this snapshot carries the source volume ID AWS
		// reported and no verdict about whether that volume still exists.
		// "I could not finish looking" is not "it is gone".
		ebsSnapshot(acctProd, "us-west-2", "snap-0b2c3d4e5f6a70051", "vol-0b2c3d4e5f6a70052",
			256, 88_046_829_568, true, nil, nil, d(2023, 5, 30),
			tags("Name", "search-metadata-pre-upgrade", "environment", "production", "owner", "search")),

		res(acctProd, "us-west-2", model.ServiceRDS, "instance", "orders-dr",
			"postgres", "15.4", "db.r6g.xlarge", storage(500), true, "available",
			"orders-dr.c8m1kwv2rbqc.us-west-2.rds.amazonaws.com", d(2021, 6, 2),
			tags("environment", "production", "owner", "payments", "app", "orders")),
		res(acctProd, "us-west-2", model.ServiceRDS, "instance", "telemetry-tsdb",
			"postgres", "16.2", "db.m6g.2xlarge", storage(1000), false, "available",
			"telemetry-tsdb.c8m1kwv2rbqc.us-west-2.rds.amazonaws.com", d(2023, 9, 18),
			tags("environment", "production", "owner", "observability")),
		res(acctProd, "us-west-2", model.ServiceAurora, "cluster", "search-metadata",
			"aurora-postgresql", "14.9", "", nil, false, "available",
			"search-metadata.cluster-c8m1kwv2rbqc.us-west-2.rds.amazonaws.com", d(2022, 1, 26),
			tags("environment", "production")), // no owner
		res(acctProd, "us-west-2", model.ServiceDynamoDB, "table", "events-firehose",
			"dynamodb", "", "PROVISIONED", storage(240), false, "active", "", d(2021, 4, 14),
			tags("environment", "production", "owner", "data")),
		res(acctProd, "us-west-2", model.ServiceDynamoDB, "table", "device-registry",
			"dynamodb", "", "PROVISIONED", storage(9), false, "active", "", d(2019, 12, 3),
			tags("created-by", "console")), // no owner, no environment
		res(acctProd, "us-west-2", model.ServiceElastiCache, "cluster", "queue-cache",
			"redis", "", "cache.m5.large", nil, false, "available",
			"queue-cache.kx3q9f.ng.0001.usw2.cache.amazonaws.com", d(2019, 7, 22),
			tags("environment", "production")), // no owner
		res(acctProd, "us-west-2", model.ServiceRedshift, "cluster", "analytics-dr",
			"redshift", "1.0.63269", "ra3.4xlarge", storage(3200), false, "paused",
			"analytics-dr.cbq7xz1t9e2m.us-west-2.redshift.amazonaws.com", d(2020, 10, 15),
			tags("environment", "production", "owner", "data")),
		// This instance names an attached volume that has no row of its own —
		// DescribeVolumes failed in this region, so the volumes here were never
		// enumerated. The gap is in the ledger, which is the difference between
		// a census that is incomplete and one that is wrong.
		ec2Instance(acctProd, "us-west-2", "us-west-2a", "i-0b2c3d4e5f6a70021",
			"c5.large", "Linux/UNIX", "running", "ip-10-1-1-21.us-west-2.compute.internal",
			false, []string{"vol-0b2c3d4e5f6a70021"}, d(2022, 3, 15),
			tags("Name", "batch-worker-1", "environment", "production", "owner", "data")),
		// The same guardrail as the snapshot above, on a different number.
		// DescribeTargetGroups was throttled in this region (see the ledger), so
		// this load balancer carries no target group count at all — an unknown
		// count and a complete zero read identically, and only one of them is a
		// finding.
		loadBalancerV2(acctProd, "us-west-2", "application", "batch-api", "internal",
			"internal-batch-api-0b2c3d4e5f6a7074.us-west-2.elb.amazonaws.com",
			1, nil, d(2022, 3, 15),
			tags("environment", "production", "owner", "data")),

		// Deprecated, untagged, and last touched in 2019 — the shape the
		// tag-hygiene numbers and the lifecycle table describe together. Nobody
		// listed on it, and go1.x stopped being patched in January 2024.
		lambdaFunction(acctProd, "us-west-2", "log-shipper", "go1.x", "x86_64",
			256, 120, 12_582_912, false, d(2019, 10, 8),
			nil), // no tags at all
		// Both buckets in this region carry no encryption attribute and no
		// Encrypted verdict: GetBucketEncryption was denied here, and the
		// ledger says so. Not "unencrypted" — unanswered.
		s3Bucket(acctProd, "us-west-2", "acme-etl-scratch", "", nil,
			"", "", bpaLocked, ptr(false), d(2021, 9, 2),
			tags("environment", "production", "owner", "data")),
		s3Bucket(acctProd, "us-west-2", "acme-backup-vault", "", nil,
			"Enabled", "", bpaLocked, ptr(false), d(2020, 11, 12),
			tags("environment", "production", "owner", "platform")),

		// ── prod account · sa-east-1 ────────────────────────────────────
		res(acctProd, "sa-east-1", model.ServiceRDS, "instance", "orders-latam",
			"postgres", "15.4", "db.m6g.large", storage(200), true, "available",
			"orders-latam.c7n4jyt5sdrc.sa-east-1.rds.amazonaws.com", d(2022, 5, 31),
			tags("environment", "production", "owner", "payments-latam")),
		res(acctProd, "sa-east-1", model.ServiceRDS, "instance", "invoices-br",
			"mysql", "8.0.36", "db.t3.medium", storage(50), false, "storage-full",
			"invoices-br.c7n4jyt5sdrc.sa-east-1.rds.amazonaws.com", d(2023, 2, 14),
			tags("environment", "production")), // no owner
		res(acctProd, "sa-east-1", model.ServiceDynamoDB, "table", "carts-latam",
			"dynamodb", "", "PAY_PER_REQUEST", storage(3), false, "active", "", d(2022, 6, 9),
			tags("environment", "production", "owner", "checkout")),
		res(acctProd, "sa-east-1", model.ServiceDynamoDB, "table", "nfe-receipts",
			"dynamodb", "", "PROVISIONED", storage(27), false, "active", "", d(2022, 11, 1),
			nil), // no tags at all
		res(acctProd, "sa-east-1", model.ServiceDocumentDB, "cluster", "catalog-docs-latam",
			"docdb", "4.0.0", "db.r5.large", storage(180), false, "available",
			"catalog-docs-latam.cluster-c7n4jyt5sdrc.sa-east-1.docdb.amazonaws.com", d(2021, 8, 17),
			tags("environment", "production")), // no owner
		// Stopped, and therefore not publicly accessible: an instance without an
		// Elastic IP releases its public address when it stops.
		ec2Instance(acctProd, "sa-east-1", "sa-east-1a", "i-0c3d4e5f6a7b80031",
			"t3.medium", "Windows", "stopped", "ip-10-2-1-31.sa-east-1.compute.internal",
			false, []string{"vol-0c3d4e5f6a7b80031"}, d(2020, 11, 19),
			tags("Name", "nfe-gateway", "environment", "production", "owner", "payments-latam")),
		// The stopped instance's volume, still attached and still billing.
		ebsVolume(acctProd, "sa-east-1", "sa-east-1a", "vol-0c3d4e5f6a7b80031",
			"gp2", 120, ptr(int32(360)), nil, false,
			[]string{"i-0c3d4e5f6a7b80031"}, d(2020, 11, 19),
			tags("Name", "nfe-gateway-root", "environment", "production", "owner", "payments-latam")),
		// A private NAT gateway: it routes to on-premises over Direct Connect
		// and holds no public address at all. The public_ip key is absent rather
		// than empty, which is the difference between "it has none" and "the
		// scan could not read it" — and it bills the same hourly rate either way.
		natGateway(acctProd, "sa-east-1", "sa-east-1a", "nat-0c3d4e5f6a7b80051",
			"", d(2022, 5, 31),
			tags("Name", "onprem-egress", "environment", "production", "owner", "payments-latam")),
		// The row that shows why the lifecycle table is keyed by service. Java 8
		// reached upstream end of public updates years ago, and this function
		// still gets no red pill — AWS patches java8.al2 on its own calendar and
		// has not deprecated it. Reporting the upstream date here would flag a
		// runtime somebody is actively maintaining.
		lambdaFunction(acctProd, "sa-east-1", "nfe-signer", "java8.al2", "x86_64",
			1536, 30, 41_943_040, false, d(2024, 2, 21),
			tags("environment", "production")), // no owner
		// The fixture's only bucket with MFA delete on — a compliance posture
		// almost nobody adopts, kept here because the attribute is otherwise
		// absent from every row and an absent-everywhere key is untestable.
		s3Bucket(acctProd, "sa-east-1", "acme-nfe-documentos", "aws:kms", ptr(true),
			"Enabled", "Enabled", bpaLocked, ptr(false), d(2022, 5, 31),
			tags("environment", "production", "owner", "payments-latam", "app", "nfe")),

		// ── staging account · us-east-1 ─────────────────────────────────
		res(acctStaging, "us-east-1", model.ServiceRDS, "instance", "orders-staging",
			"postgres", "15.4", "db.t4g.medium", storage(100), false, "available",
			"orders-staging.c5p8gxr9tfsd.us-east-1.rds.amazonaws.com", d(2021, 6, 15),
			tags("environment", "staging", "owner", "payments")),
		res(acctStaging, "us-east-1", model.ServiceRDS, "instance", "billing-staging",
			"mysql", "8.0.35", "db.t3.medium", storage(50), false, "stopped",
			"billing-staging.c5p8gxr9tfsd.us-east-1.rds.amazonaws.com", d(2021, 3, 4),
			tags("environment", "staging", "owner", "billing")),
		res(acctStaging, "us-east-1", model.ServiceRDS, "instance", "qa-sandbox",
			"postgres", "13.13", "db.t3.micro", storage(20), false, "available",
			"qa-sandbox.c5p8gxr9tfsd.us-east-1.rds.amazonaws.com", d(2020, 7, 8),
			nil), // no tags at all
		res(acctStaging, "us-east-1", model.ServiceRDS, "instance", "load-test-db",
			"postgres", "15.4", "db.r6g.large", storage(200), false, "available",
			"load-test-db.c5p8gxr9tfsd.us-east-1.rds.amazonaws.com", d(2024, 1, 30),
			tags("stage", "staging")), // env via "stage" key, no owner
		res(acctStaging, "us-east-1", model.ServiceAurora, "cluster", "users-aurora-staging",
			"aurora-postgresql", "15.4", "", nil, false, "available",
			"users-aurora-staging.cluster-c5p8gxr9tfsd.us-east-1.rds.amazonaws.com", d(2021, 9, 2),
			tags("environment", "staging", "owner", "identity")),
		res(acctStaging, "us-east-1", model.ServiceDynamoDB, "table", "sessions-staging",
			"dynamodb", "", "PAY_PER_REQUEST", storage(2), false, "active", "", d(2020, 5, 20),
			tags("environment", "staging", "owner", "platform")),
		res(acctStaging, "us-east-1", model.ServiceDynamoDB, "table", "feature-flags-staging",
			"dynamodb", "", "PROVISIONED", storage(1), false, "active", "", d(2022, 10, 19),
			nil), // no tags at all
		res(acctStaging, "us-east-1", model.ServiceDynamoDB, "table", "integration-test-artifacts",
			"dynamodb", "", "PROVISIONED", storage(55), false, "active", "", d(2023, 6, 27),
			tags("environment", "staging", "owner", "qa")),
		res(acctStaging, "us-east-1", model.ServiceElastiCache, "cluster", "checkout-cache-staging",
			"redis", "", "cache.t4g.small", nil, false, "available",
			"checkout-cache-staging.kx3q9f.ng.0001.use1.cache.amazonaws.com", d(2021, 8, 5),
			tags("environment", "staging", "owner", "checkout")),
		res(acctStaging, "us-east-1", model.ServiceRedshift, "cluster", "analytics-wh-staging",
			"redshift", "1.0.61395", "dc2.large", storage(640), false, "available",
			"analytics-wh-staging.cbq7xz1t9e2m.us-east-1.redshift.amazonaws.com", d(2020, 12, 1),
			tags("environment", "staging", "owner", "data")),
		res(acctStaging, "us-east-1", model.ServiceDocumentDB, "cluster", "catalog-docs-staging",
			"docdb", "5.0.0", "db.t3.medium", storage(40), false, "available",
			"catalog-docs-staging.cluster-c5p8gxr9tfsd.us-east-1.docdb.amazonaws.com", d(2022, 2, 22),
			tags("owner", "catalog")), // no environment
		res(acctStaging, "us-east-1", model.ServiceNeptune, "cluster", "fraud-graph-staging",
			"neptune", "1.3.2.0", "db.t3.medium", storage(15), false, "available",
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

		ebsVolume(acctStaging, "us-east-1", "us-east-1a", "vol-0d4e5f6a7b8c90041",
			"gp3", 50, ptr(int32(3000)), ptr(int32(125)), true,
			[]string{"i-0d4e5f6a7b8c90041"}, d(2023, 1, 12),
			tags("Name", "ci-runner-root", "environment", "staging", "owner", "platform")),
		// Attached to a stopped instance. EBS bills a stopped box's volume at
		// the same per-GiB rate as a running one, which is the point of showing
		// the volume as its own row rather than as a field on the instance.
		ebsVolume(acctStaging, "us-east-1", "us-east-1b", "vol-0d4e5f6a7b8c90042",
			"gp3", 30, ptr(int32(3000)), ptr(int32(125)), true,
			[]string{"i-0d4e5f6a7b8c90042"}, d(2022, 8, 3),
			nil),
		// Sixteen terabytes, unattached, untagged, unencrypted. Nobody is going
		// to find this by looking at the EC2 console's instance list.
		ebsVolume(acctStaging, "us-east-1", "us-east-1b", "vol-0d4e5f6a7b8c90093",
			"gp3", 16384, ptr(int32(3000)), ptr(int32(125)), false,
			nil, d(2022, 11, 8),
			nil),
		// Staging's own NAT gateway and its address: the same ~$32 a month as
		// production's, for an environment nobody uses at the weekend.
		natGateway(acctStaging, "us-east-1", "us-east-1a", "nat-0d4e5f6a7b8c90051",
			"203.0.113.44", d(2021, 6, 15),
			tags("environment", "staging", "owner", "platform")),
		elasticIP(acctStaging, "us-east-1", "eipalloc-0d4e5f6a7b8c90061", "203.0.113.44",
			"nat-0d4e5f6a7b8c90051", subnetID(acctStaging, "us-east-1a"),
			tags("environment", "staging", "owner", "platform")),
		loadBalancerV2(acctStaging, "us-east-1", "application", "staging-frontend",
			"internet-facing", "staging-frontend-0d4e5f6a7b8c9075.us-east-1.elb.amazonaws.com",
			2, ptr(1), d(2021, 6, 15),
			tags("environment", "staging")), // no owner
		// In the VPC, so it holds ENIs in staging's subnets, and on a runtime
		// deprecated in June 2024. Staging is where these accumulate: the blast
		// radius is small enough that nobody upgrades, and the function still
		// holds the same credentials production's does.
		lambdaFunction(acctStaging, "us-east-1", "slack-notifier", "nodejs16.x", "x86_64",
			256, 15, 1_048_576, true, d(2022, 4, 26),
			tags("environment", "staging", "owner", "platform")),
		// The row that declines to answer. This bucket has no Block Public
		// Access configuration at all — S3 returns
		// NoSuchPublicAccessBlockConfiguration, which is data, not a failure —
		// and its policy is not public. Public ACLs would still be live, and
		// the census does not call GetBucketAcl, so it says nothing rather than
		// a reassuring false. Versioning is Suspended, the state that keeps
		// billing for every version already written.
		s3Bucket(acctStaging, "us-east-1", "acme-staging-uploads", "AES256", ptr(false),
			"Suspended", "", nil, ptr(false), d(2023, 1, 24),
			nil), // no tags at all

		// ── staging account · eu-west-1 ─────────────────────────────────
		res(acctStaging, "eu-west-1", model.ServiceRDS, "instance", "gdpr-test-db",
			"postgres", "15.4", "db.t3.medium", storage(50), false, "available",
			"gdpr-test-db.c2r6fwq8vhte.eu-west-1.rds.amazonaws.com", d(2023, 5, 16),
			tags("environment", "staging", "owner", "compliance")),
		res(acctStaging, "eu-west-1", model.ServiceRDS, "instance", "data-residency-poc",
			"mariadb", "10.11.6", "db.t3.small", storage(20), false, "available",
			"data-residency-poc.c2r6fwq8vhte.eu-west-1.rds.amazonaws.com", d(2024, 3, 7),
			nil), // no tags at all
		res(acctStaging, "eu-west-1", model.ServiceElastiCache, "cluster", "gdpr-cache",
			"redis", "", "cache.t4g.micro", nil, false, "available",
			"gdpr-cache.kx3q9f.ng.0001.euw1.cache.amazonaws.com", d(2023, 5, 16),
			tags("environment", "staging", "owner", "compliance")),
	}
}

// applyExposure fills exposure fields the way the real scanners report them:
// the RDS family and Redshift always carry all three, Aurora clusters have no
// public-accessibility flag, Redshift serverless only reports public access,
// grouped and standalone ElastiCache Redis report encryption and snapshot
// retention, and serverless caches, memcached and DynamoDB report nothing. A
// few fixtures are deliberately risky so the report has exposure rows to show.
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
			// Memcached is excluded because it has neither at-rest encryption
			// nor snapshots to retain, and serverless caches are excluded
			// whatever engine they run, because DescribeServerlessCaches
			// reports neither flag. Everything else — redis and the valkey
			// forks of it alike — gets both, since DescribeReplicationGroups
			// reports both for all of them. Naming redis alone would leave a
			// valkey row silently missing two values the API does return.
			if r.Attr(model.AttrEngine) != "memcached" && r.Type != model.TypeElastiCacheServerlessCache {
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
//
// class carries whatever the service calls the dimension it is sized by, which
// for DynamoDB is not an instance class at all; see below.
//
// storageGB is a pointer for the same reason a scanner reads the SDK pointer
// and never the converted value: nil is a service that reported no size, zero
// is a service that reported zero, and deciding from the value would silently
// turn the second into the first.
func res(account, region, svc, shape, name, engine, version, class string,
	storageGB *int32, multiAZ bool, status, endpoint string,
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
	// A table has no instance class, and the scanner does not invent one: it
	// records on-demand vs provisioned under DynamoDB's own name for it. The
	// fixture has to key it the same way or the report is exercised against a
	// column no scan fills.
	if svc == model.ServiceDynamoDB {
		r.SetAttr(model.AttrBillingMode, class)
	} else {
		r.SetAttr(model.AttrInstanceClass, class)
	}
	r.SetAttr(model.AttrEndpoint, endpoint)
	// Multi-AZ mirrors which control planes actually report it: the RDS family
	// and provisioned Redshift/ElastiCache clusters do; DynamoDB and the
	// serverless/standalone shapes never do, so the key stays absent — the bag
	// equivalent of the nil that used to mean "not reported".
	if reportsMultiAZ(svc, shape) {
		r.SetBoolAttr(model.AttrMultiAZ, &multiAZ)
	}
	if storageGB != nil {
		r.SetMeasure(model.MeasureSizeBytes, int64(*storageGB)<<30)
	}
	return r
}

// storage names a size the service reported, in whole gibibytes. Passing nil to
// res() instead says the service reported none — an Aurora cluster is the case
// the fixture carries, since its storage belongs to a shared volume that
// DescribeDBClusters attributes to no row.
func storage(gibibytes int32) *int32 { return ptr(gibibytes) }

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
		// The three shapes have three ARN resource types, and the ARN is both
		// the diff's match key and the cost join's key — a replication group
		// written as a cluster would match nothing on either side of either.
		switch shape {
		case "cluster":
			return fmt.Sprintf("arn:aws:elasticache:%s:%s:replicationgroup:%s", region, account, name)
		case "serverless":
			return fmt.Sprintf("arn:aws:elasticache:%s:%s:serverlesscache:%s", region, account, name)
		default:
			return fmt.Sprintf("arn:aws:elasticache:%s:%s:cluster:%s", region, account, name)
		}
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
	r.SetAttr(model.AttrVPCID, vpcID(account, region))
	r.SetAttr(model.AttrSubnetID, subnetID(account, az))
	// Sorted, like the scanner sorts them, so the fixture cannot drift into a
	// value shape a real scan would never produce. The attachment only: the
	// volumes are their own rows, and summing their sizes here would count the
	// same storage twice estate-wide.
	ids := append([]string(nil), volumes...)
	sort.Strings(ids)
	r.SetAttr(model.AttrEBSVolumeIDs, strings.Join(ids, ","))
	return r
}

// ebsVolume builds an EBS volume row. iops and throughput are pointers because
// volume types differ in what they report — st1 and sc1 report neither — and
// the fixture has to be able to express "AWS said nothing" as distinct from
// "AWS said zero". Passing no attachments makes the volume unattached, which is
// the finding the demo exists to show.
func ebsVolume(account, region, az, id, volumeType string, sizeGiB int32,
	iops, throughput *int32, encrypted bool, instances []string,
	created time.Time, t map[string]string) model.Resource {
	name := t["Name"]
	if name == "" {
		name = id
	}
	// An unattached volume is "available"; an attached one is "in-use". That is
	// EBS's own vocabulary, and the whole waste signal, so it is derived from
	// the attachments rather than passed in and allowed to disagree with them.
	status := "available"
	if len(instances) > 0 {
		status = "in-use"
	}
	r := model.Resource{
		ARN:       scanners.EBSVolumeARN("aws", region, account, id),
		Service:   model.ServiceEBS,
		Type:      model.TypeEBSVolume,
		Name:      name,
		Status:    status,
		Region:    region,
		AccountID: account,
		CreatedAt: &created,
		Tags:      t,
		// Unlike the instance, a volume can answer this one honestly: encryption
		// at rest is a property of the volume itself. PubliclyAccessible stays
		// nil — a volume has no network identity to expose.
		Encrypted: ptr(encrypted),
	}
	r.SetAttr(model.AttrVolumeType, volumeType)
	r.SetAttr(model.AttrAvailabilityZone, az)
	ids := append([]string(nil), instances...)
	sort.Strings(ids)
	r.SetAttr(model.AttrAttachedInstanceIDs, strings.Join(ids, ","))
	// Provisioned size is billed size for a volume, so this is the one EBS row
	// type where size_bytes is answerable. Snapshots are the opposite case.
	r.SetMeasure(model.MeasureSizeBytes, int64(sizeGiB)<<30)
	if iops != nil {
		r.SetMeasure(model.MeasureIOPS, int64(*iops))
	}
	if throughput != nil {
		r.SetMeasure(model.MeasureThroughputMiBps, int64(*throughput))
	}
	return r
}

// ebsSnapshot builds an EBS snapshot row.
//
// sourceExists is a *bool with three meanings, which is the point of the
// signature: true and false are verdicts the scanner reached because it
// enumerated the region's volumes completely, and nil is the case where it
// could not — the region's DescribeVolumes failed, so the row states the source
// volume ID AWS reported and refuses to say whether that volume is still there.
// The fixture carries one of each so the guardrail is visible in the demo and
// not only in the tests.
func ebsSnapshot(account, region, id, sourceVolumeID string, sourceVolumeGiB int32,
	fullSnapshotBytes int64, encrypted bool, backingImages []string,
	sourceExists *bool, started time.Time, t map[string]string) model.Resource {
	name := t["Name"]
	if name == "" {
		name = id
	}
	r := model.Resource{
		// No account ID in a snapshot ARN — that is AWS's shape, not an
		// oversight. See scanners.EBSSnapshotARN.
		ARN:       scanners.EBSSnapshotARN("aws", region, id),
		Service:   model.ServiceEBS,
		Type:      model.TypeEBSSnapshot,
		Name:      name,
		Status:    "completed",
		Region:    region,
		AccountID: account,
		CreatedAt: &started,
		Tags:      t,
		Encrypted: ptr(encrypted),
	}
	r.SetAttr(model.AttrStorageTier, "standard")
	r.SetAttr(model.AttrSourceVolumeID, sourceVolumeID)
	images := append([]string(nil), backingImages...)
	sort.Strings(images)
	r.SetAttr(model.AttrBackingImageIDs, strings.Join(images, ","))
	if sourceExists != nil {
		r.SetAttr(model.AttrSourceVolumeExists, strconv.FormatBool(*sourceExists))
	}
	// Deliberately no size_bytes. A snapshot is incremental and no API reports
	// what it actually bills; these two are the honest neighbours of that
	// number and neither is a substitute for it, which is why they are named
	// after what they measure and never summed.
	r.SetMeasure(model.MeasureSourceVolumeBytes, int64(sourceVolumeGiB)<<30)
	r.SetMeasure(model.MeasureFullSnapshotBytes, fullSnapshotBytes)
	return r
}

// natGateway builds a NAT gateway row. An empty publicIP makes it a private
// gateway, which holds no billable address — distinct from a public one whose
// address the scan failed to read, and expressible only as an absent key.
func natGateway(account, region, az, id, publicIP string,
	created time.Time, t map[string]string) model.Resource {
	name := t["Name"]
	if name == "" {
		name = id
	}
	connectivity := "public"
	if publicIP == "" {
		connectivity = "private"
	}
	r := model.Resource{
		ARN:       scanners.NATGatewayARN("aws", region, account, id),
		Service:   model.ServiceNATGateway,
		Type:      model.TypeNATGateway,
		Name:      name,
		Status:    "available",
		Region:    region,
		AccountID: account,
		CreatedAt: &created,
		Tags:      t,
		// Both nil, as the scanner leaves them: a NAT gateway accepts no inbound
		// connections, and it stores nothing.
	}
	r.SetAttr(model.AttrConnectivityType, connectivity)
	r.SetAttr(model.AttrPublicIP, publicIP)
	r.SetAttr(model.AttrAvailabilityZone, az)
	r.SetAttr(model.AttrVPCID, vpcID(account, region))
	r.SetAttr(model.AttrSubnetID, subnetID(account, az))
	// Zonal, like almost every NAT gateway in existence — which is the point:
	// the cost is per gateway, and the classic layout puts one in every AZ.
	r.SetMeasure(model.MeasureAvailabilityZoneCount, 1)
	// No data-processed measure, matching the scanner: the bytes are a
	// CloudWatch question and a zero would read as "nothing went through this".
	return r
}

// elasticIP builds an Elastic IP row. An empty holder makes it unassociated,
// which is the finding: since February 2024 an address bills whether or not
// anything is using it.
func elasticIP(account, region, allocationID, ip, holder, subnetID string,
	t map[string]string) model.Resource {
	name := t["Name"]
	if name == "" {
		name = ip
	}
	status := "unassociated"
	if holder != "" {
		status = "associated"
	}
	r := model.Resource{
		ARN:       scanners.ElasticIPARN("aws", region, account, allocationID),
		Service:   model.ServicePublicIP,
		Type:      model.TypeEIP,
		Name:      name,
		Status:    status,
		Region:    region,
		AccountID: account,
		// No CreatedAt, as the scanner records none: DescribeAddresses reports
		// no allocation time, and how long the address has sat idle is exactly
		// the question a reader brings to this row.
		Tags: t,
		// PubliclyAccessible stays nil. Whatever holds the address is already
		// counted as exposed on its own row; repeating it here would make a
		// metric about risk into a metric about addresses.
	}
	r.SetAttr(model.AttrPublicIP, ip)
	r.SetAttr(model.AttrAssociatedWith, holder)
	r.SetAttr(model.AttrSubnetID, subnetID)
	return r
}

// autoAssignedIP builds a public IPv4 row for an address that has no allocation
// behind it — the kind an instance is handed at launch.
//
// It is in the fixture because it is the reason the scanner makes two calls:
// DescribeAddresses never returns these, and in most accounts they are the
// majority. A census built on that one call reports a confident undercount.
func autoAssignedIP(account, region, az, interfaceID, ip, instanceID string,
	t map[string]string) model.Resource {
	name := t["Name"]
	if name == "" {
		name = ip
	}
	r := model.Resource{
		ARN:       scanners.NetworkInterfaceARN("aws", region, account, interfaceID),
		Service:   model.ServicePublicIP,
		Type:      model.TypeNetworkInterface,
		Name:      name,
		Status:    "associated",
		Region:    region,
		AccountID: account,
		Tags:      t,
	}
	r.SetAttr(model.AttrPublicIP, ip)
	r.SetAttr(model.AttrAssociatedWith, instanceID)
	r.SetAttr(model.AttrAvailabilityZone, az)
	r.SetAttr(model.AttrVPCID, vpcID(account, region))
	r.SetAttr(model.AttrSubnetID, subnetID(account, az))
	return r
}

// loadBalancerV2 builds an application or network load balancer row.
//
// targetGroups is a *int because the count has three meanings and the fixture
// has to carry all three: a positive count, a complete zero — a load balancer
// billing by the hour with nowhere to send traffic — and nil, where the
// region's target group listing failed and the scanner refuses to write a zero
// it did not observe.
func loadBalancerV2(account, region, lbType, name, scheme, dnsName string,
	azCount int, targetGroups *int, created time.Time, t map[string]string) model.Resource {
	// The v2 ARN's opaque suffix is fixture data like any other ID; what matters
	// is the shape, since it is the diff match key and the cost join key.
	shortType := lbType[:3]
	suffix := lbSuffix(account, region, name)
	arn := "arn:aws:elasticloadbalancing:" + region + ":" + account +
		":loadbalancer/" + shortType + "/" + name + "/" + suffix
	r := model.Resource{
		ARN:                arn,
		Service:            model.ServiceELB,
		Type:               model.TypeLoadBalancerV2,
		Name:               name,
		Status:             "active",
		Region:             region,
		AccountID:          account,
		CreatedAt:          &created,
		Tags:               t,
		PubliclyAccessible: ptr(scheme == "internet-facing"),
	}
	r.SetAttr(model.AttrScheme, scheme)
	r.SetAttr(model.AttrLoadBalancerType, lbType)
	r.SetAttr(model.AttrEndpoint, dnsName)
	r.SetAttr(model.AttrVPCID, vpcID(account, region))
	r.SetMeasure(model.MeasureAvailabilityZoneCount, int64(azCount))
	if targetGroups != nil {
		arns := make([]string, 0, *targetGroups)
		for i := range *targetGroups {
			arns = append(arns, fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:targetgroup/%s-%d/%s",
				region, account, name, i+1, suffix))
		}
		r.SetAttr(model.AttrTargetGroupARNs, strings.Join(arns, ","))
		r.SetMeasure(model.MeasureTargetGroupCount, int64(*targetGroups))
	}
	// No registered_instance_count: counting targets on a v2 load balancer is
	// one request per target group, which the scanner does not make.
	return r
}

// classicLoadBalancer builds a Classic Load Balancer row. The v1 API returns
// registered instances inline, so unlike its successor this row can answer the
// idle question from the same response that named it — and zero instances is a
// load balancer from 2016 still billing for nothing.
func classicLoadBalancer(account, region, name, scheme, dnsName string,
	azCount, instances int, created time.Time, t map[string]string) model.Resource {
	r := model.Resource{
		ARN:     scanners.ClassicLoadBalancerARN("aws", region, account, name),
		Service: model.ServiceELB,
		Type:    model.TypeLoadBalancer,
		Name:    name,
		// Empty, as the scanner leaves it: the v1 API has no state field, and
		// "active" would be a guess that happens to be right most of the time.
		Status:             "",
		Region:             region,
		AccountID:          account,
		CreatedAt:          &created,
		Tags:               t,
		PubliclyAccessible: ptr(scheme == "internet-facing"),
	}
	r.SetAttr(model.AttrScheme, scheme)
	r.SetAttr(model.AttrLoadBalancerType, "classic")
	r.SetAttr(model.AttrEndpoint, dnsName)
	r.SetAttr(model.AttrVPCID, vpcID(account, region))
	r.SetMeasure(model.MeasureAvailabilityZoneCount, int64(azCount))
	r.SetMeasure(model.MeasureRegisteredInstanceCount, int64(instances))
	return r
}

// lambdaFunction builds a Lambda function row.
//
// runtime doubles as the package-type discriminator, because in the API the
// two are exclusive: a zip function reports a runtime identifier and an image
// function reports none, its runtime being sealed inside a container blueprint
// cannot open. Passing "" here therefore produces the row a container-image
// function really produces — no runtime key, and so no lifecycle verdict.
//
// CreatedAt is nil on every row, as it is on every real one: Lambda reports
// only when a function last changed.
func lambdaFunction(account, region, name, runtime, arch string,
	memoryMB, timeoutSec int, codeSize int64, inVPC bool,
	modified time.Time, t map[string]string) model.Resource {
	r := model.Resource{
		ARN:       "arn:aws:lambda:" + region + ":" + account + ":function:" + name,
		Service:   model.ServiceLambda,
		Type:      model.TypeLambdaFunction,
		Name:      name,
		Status:    "Active",
		Region:    region,
		AccountID: account,
		Tags:      t,
	}
	packageType := "Zip"
	if runtime == "" {
		packageType = "Image"
	}
	r.SetAttr(model.AttrRuntime, runtime)
	r.SetAttr(model.AttrPackageType, packageType)
	r.SetAttr(model.AttrArchitecture, arch)
	// Lambda's own format, down to the four-digit offset — the fixture carries
	// the string the API returns rather than a tidier rendering of it.
	r.SetAttr(model.AttrLastModified, modified.Format("2006-01-02T15:04:05.000-0700"))
	if inVPC {
		r.SetAttr(model.AttrVPCID, vpcID(account, region))
		r.SetAttr(model.AttrSubnetID, subnetID(account, region+"a"))
	}
	r.SetMeasure(model.MeasureMemoryMB, int64(memoryMB))
	r.SetMeasure(model.MeasureTimeoutSeconds, int64(timeoutSec))
	// Stored unconditionally, including the zero an image function reports:
	// its bytes live in ECR and none of them count against the region's code
	// storage quota. That zero is a fact about where the code is, not a gap.
	r.SetMeasure(model.MeasureCodeSizeBytes, codeSize)
	// Every function gets the 512 MB AWS gives it by default; the one that
	// asked for more says so at its own callsite.
	r.SetMeasure(model.MeasureEphemeralStorageMB, 512)
	return r
}

// The Block Public Access shapes the fixture uses.
//
// bpaLocked is what AWS applies to every bucket created since April 2023.
// bpaPolicyLive is the dangerous one and the reason all four settings are
// recorded separately: it looks locked down — three switches on — but
// RestrictPublicBuckets is off, so a public policy attached before the others
// went on is still serving.
var (
	bpaLocked = &scanners.S3PublicAccess{
		BlockACLs: ptr(true), IgnoreACLs: ptr(true), BlockPolicy: ptr(true), Restrict: ptr(true),
	}
	bpaPolicyLive = &scanners.S3PublicAccess{
		BlockACLs: ptr(true), IgnoreACLs: ptr(true), BlockPolicy: ptr(true), Restrict: ptr(false),
	}
)

// s3Bucket builds an S3 bucket row.
//
// Every reported field is passed as the API reports it, with the zero value
// meaning "not reported": an empty sse is a bucket whose encryption
// configuration the scan never got, a nil access is one with no Block Public
// Access configuration at all, an empty versioning is a bucket that never
// enabled it. Each becomes an absent key, which is the only way this census
// distinguishes those from a configured "off".
//
// No size and no object count. A real scan leaves both absent too — they come
// from CloudWatch, and only with --metrics. See AddMetrics.
func s3Bucket(account, region, name, sse string, bucketKey *bool,
	versioning, mfaDelete string, access *scanners.S3PublicAccess, policyPublic *bool,
	created time.Time, t map[string]string) model.Resource {
	r := model.Resource{
		// Neither region nor account, exactly as S3 writes it: bucket names are
		// globally unique, so the ARN has nothing else to carry.
		ARN:       "arn:aws:s3:::" + name,
		Service:   model.ServiceS3,
		Type:      model.TypeS3Bucket,
		Name:      name,
		Region:    region,
		AccountID: account,
		CreatedAt: &created,
		Tags:      t,
		// Status stays empty. A bucket has no lifecycle state to report.
	}
	r.SetAttr(model.AttrSSEAlgorithm, sse)
	r.SetBoolAttr(model.AttrBucketKeyEnabled, bucketKey)
	if sse != "" {
		// True or absent, never false — the same rule the scanner applies, and
		// for the same reason: S3 encrypts new objects whether or not a bucket
		// asks, so "no configuration" is not "unencrypted".
		r.Encrypted = ptr(true)
	}
	r.SetAttr(model.AttrVersioning, versioning)
	r.SetAttr(model.AttrMFADelete, mfaDelete)
	// Through the scanner's own derivation rather than a copy of it, so the
	// fixture cannot call a bucket safe on evidence the scanner would refuse.
	scanners.ApplyS3PublicAccess(&r, access, policyPublic)
	return r
}

// The three lookups below are total: every one has a deterministic answer for
// a key the storyboard never wrote down. That matters because SnapshotN
// invents accounts and names, and a map miss returns "", which SetAttr drops —
// generated rows would come away with no VPC and no subnet, and a generated
// load balancer's ARN would end in a bare slash. A synthesized id is not a
// weaker fixture than a written one: both are made up, and the census only
// requires that they be shaped like the real thing and stable across runs.

// The opaque suffix AWS appends to a v2 load balancer ARN, fixed per fixture
// load balancer so the ARNs — and therefore the diff and the cost join — are
// stable across runs.
var demoLBSuffixes = map[string]string{
	"web":              "0a1b2c3d4e5f6071",
	"legacy-checkout":  "0a1b2c3d4e5f6072",
	"internal-grpc":    "0a1b2c3d4e5f6073",
	"batch-api":        "0b2c3d4e5f6a7074",
	"staging-frontend": "0d4e5f6a7b8c9075",
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

// vpcID names the VPC an account's resources occupy in one region.
func vpcID(account, region string) string {
	key := account + "/" + region
	if id, ok := demoVPCs[key]; ok {
		return id
	}
	return synthID("vpc-", key, 17)
}

// subnetID names the subnet in one availability zone. Callers pass the zone,
// not the region, because two rows in the same VPC and different zones must
// not land on the same subnet.
func subnetID(account, az string) string {
	key := account + "/" + az
	if id, ok := demoSubnets[key]; ok {
		return id
	}
	return synthID("subnet-", key, 17)
}

// lbSuffix returns the opaque tail of a load balancer's ARN. The written-down
// ones are keyed by name alone, which is unique across the storyboard; the
// synthesized ones take the account and region too, since a generator is free
// to reuse a name in another account and the ARN has to stay unique.
func lbSuffix(account, region, name string) string {
	if s, ok := demoLBSuffixes[name]; ok {
		return s
	}
	return synthID("", account+"/"+region+"/"+name, 16)
}

// synthID builds an AWS-shaped identifier from a key. FNV-1a is used for its
// determinism and not for any other property: these ids are only ever compared
// for equality, and the census needs the same key to produce the same id on
// every run or the diff would report drift the fixture did not have.
//
// The width is exact in both directions. %0*x pads to a minimum and never
// truncates, so a caller asking for nine digits would otherwise get however
// many the hash happened to need — and an id wider than the seventeen hex
// digits EC2 hands out is not an id anyone would recognise. Taking the low
// digits keeps the widths AWS uses; the callers above ask for sixteen and
// seventeen, which a 64-bit hash already fits, so only narrower ones change.
func synthID(prefix, key string, hexDigits int) string {
	h := fnv.New64a()
	// Hash.Write never returns an error.
	_, _ = h.Write([]byte(key))
	s := fmt.Sprintf("%0*x", hexDigits, h.Sum64())
	return prefix + s[len(s)-hexDigits:]
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
