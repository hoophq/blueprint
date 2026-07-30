// Package model defines the normalized resource model shared by all scanners
// and renderers. Fields are control-plane metadata only — never credentials.
package model

import (
	"sort"
	"strconv"
	"time"
)

// Service identifiers used in Resource.Service.
const (
	ServiceRDS         = "rds"
	ServiceAurora      = "aurora"
	ServiceDocumentDB  = "documentdb"
	ServiceNeptune     = "neptune"
	ServiceDynamoDB    = "dynamodb"
	ServiceElastiCache = "elasticache"
	ServiceRedshift    = "redshift"
	ServiceEC2         = "ec2"
	// ServiceEBS is block storage. It is scanned through the EC2 API but kept
	// separate from ServiceEC2 so the failure ledger can say which of the two
	// blueprint could not read — losing DescribeVolumes in a region tells the
	// reader something quite different from losing DescribeInstances there.
	ServiceEBS = "ebs"
	// ServiceNATGateway, ServicePublicIP and ServiceELB are the network line
	// items — each an hourly charge that accrues whether or not anything is
	// using it. They are separate services for the same reason EBS is separate
	// from EC2: they are separate describe calls, they fail independently, and
	// a ledger saying "NAT gateways in eu-west-1" is worth more than one saying
	// "some networking".
	ServiceNATGateway = "natgateway"
	// ServicePublicIP covers billable public IPv4 addresses. Since February
	// 2024 AWS charges for every one of them, in use or not, which is what
	// makes them worth a census row of their own rather than a field on
	// whatever they happen to be attached to.
	ServicePublicIP = "publicip"
	ServiceELB      = "elb"
	// ServiceLambda is the one service here that bills nothing at rest. A
	// function nobody invokes costs nothing, so it will never appear in a cost
	// report and never draw a second look — which is exactly why an unpatched
	// runtime survives in one for years. Its census row is a lifecycle finding
	// rather than a spend finding.
	ServiceLambda = "lambda"
	// ServiceS3 is object storage. Its buckets are the one resource in this
	// census whose cost is invisible from the control plane entirely: a bucket
	// with a petabyte in it and an empty one describe identically, and the only
	// free source for the difference is a daily CloudWatch metric. So an S3 row
	// leaves the scan with its configuration and picks up its size later, or
	// not at all — see internal/enrich.
	ServiceS3 = "s3"
)

// Resource.Type values. These are CloudFormation type names — AWS's own
// vocabulary for "what kind of thing is this", already defined for every
// service blueprint will ever scan. The alternative (an invented enum like
// instance|cluster|table) has to be extended and re-argued for each new
// service, and means nothing outside this tool.
const (
	TypeRDSInstance                 = "AWS::RDS::DBInstance"
	TypeRDSCluster                  = "AWS::RDS::DBCluster"
	TypeDocDBInstance               = "AWS::DocDB::DBInstance"
	TypeDocDBCluster                = "AWS::DocDB::DBCluster"
	TypeNeptuneInstance             = "AWS::Neptune::DBInstance"
	TypeNeptuneCluster              = "AWS::Neptune::DBCluster"
	TypeDynamoDBTable               = "AWS::DynamoDB::Table"
	TypeElastiCacheReplicationGroup = "AWS::ElastiCache::ReplicationGroup"
	TypeElastiCacheCacheCluster     = "AWS::ElastiCache::CacheCluster"
	TypeElastiCacheServerlessCache  = "AWS::ElastiCache::ServerlessCache"
	TypeRedshiftCluster             = "AWS::Redshift::Cluster"
	TypeRedshiftServerlessWorkgroup = "AWS::RedshiftServerless::Workgroup"
	TypeEC2Instance                 = "AWS::EC2::Instance"
	TypeEBSVolume                   = "AWS::EC2::Volume"
	TypeEBSSnapshot                 = "AWS::EC2::Snapshot"
	TypeNATGateway                  = "AWS::EC2::NatGateway"
	// TypeEIP is an allocated Elastic IP. TypeNetworkInterface is the other way
	// a public IPv4 address gets billed: auto-assigned to an ENI at launch,
	// with no EIP allocation behind it. Both are census rows under
	// ServicePublicIP because both are the same charge; the type says which
	// AWS resource is holding the address, since only one of the two is
	// something you can release on its own.
	TypeEIP              = "AWS::EC2::EIP"
	TypeNetworkInterface = "AWS::EC2::NetworkInterface"
	TypeLoadBalancerV2   = "AWS::ElasticLoadBalancingV2::LoadBalancer"
	TypeLoadBalancer     = "AWS::ElasticLoadBalancing::LoadBalancer"
	TypeLambdaFunction   = "AWS::Lambda::Function"
	TypeS3Bucket         = "AWS::S3::Bucket"
)

// Attribute keys used in Resource.Attributes. Keys are named after the AWS
// API field they came from, lowercased, so a reader can trace any value back
// to the describe response that produced it.
const (
	AttrEngine        = "engine"
	AttrEngineVersion = "engine_version"
	AttrInstanceClass = "instance_class"
	AttrBillingMode   = "billing_mode"
	AttrEndpoint      = "endpoint" // host only, never a connection string
	AttrMultiAZ       = "multi_az"
	// AttrPlatform is the operating-system family a compute resource runs, as
	// the service names it — EC2 fills it from PlatformDetails, the same string
	// that appears on the bill. It is distinct from AttrEngine, which names a
	// managed data engine the service is responsible for; nothing may infer one
	// from the other, and a resource whose service does not report an OS has no
	// platform key rather than a guessed one.
	AttrPlatform = "platform"
	// AttrAvailabilityZone is the single AZ a zonal resource sits in. Multi-AZ
	// resources report AttrMultiAZ instead and have no AZ of their own.
	AttrAvailabilityZone = "availability_zone"
	// AttrVPCID and AttrSubnetID place a resource in its network. Absent for
	// resources outside a VPC (EC2-Classic remnants, or a service that does not
	// report placement) rather than blank.
	AttrVPCID    = "vpc_id"
	AttrSubnetID = "subnet_id"
	// AttrEBSVolumeIDs lists the EBS volumes attached to an instance, comma
	// separated and sorted. It records the attachment relationship only — the
	// volumes are their own census rows with their own sizes, so an instance
	// must never carry their bytes as a size measure or an estate-wide storage
	// total would count the same volume twice.
	AttrEBSVolumeIDs = "ebs_volume_ids"
	// AttrVolumeType is the EBS volume type ("gp2", "gp3", "io2", "st1", ...).
	// Read alongside MeasureIOPS and MeasureThroughputMiBps it is the whole of
	// the gp2-to-gp3 question, stated as three numbers rather than a verdict:
	// what the volume is, and what it was actually provisioned to deliver.
	AttrVolumeType = "volume_type"
	// AttrAttachedInstanceIDs lists the instances a volume is attached to,
	// comma separated and sorted. Usually one; multi-attach io1/io2 volumes
	// report several. Absent means AWS reported no attachments, which for a
	// volume in state "available" is the definitive unattached signal — the
	// volume still bills at the full per-GiB rate. That is a fact, and the
	// conclusion drawn from it is the reader's.
	AttrAttachedInstanceIDs = "attached_instance_ids"
	// AttrSourceVolumeID is the volume a snapshot was taken from, as AWS
	// reports it. Absent when the snapshot has no source volume in this
	// account — a copied or imported snapshot, which AWS marks with a
	// placeholder rather than a real volume ID.
	AttrSourceVolumeID = "source_volume_id"
	// AttrSourceVolumeExists says whether that source volume was still present
	// in this region's census. It is derived rather than reported, so it is
	// written only when the derivation was sound — both the volume and image
	// enumerations ran to completion. If either failed the key is absent and
	// the failure ledger says why, because "I could not finish looking" must
	// never render as "it is gone".
	AttrSourceVolumeExists = "source_volume_exists"
	// AttrBackingImageIDs lists the account's own AMIs whose block device
	// mappings reference this snapshot, comma separated and sorted. A snapshot
	// backing an AMI is not an orphan however long its source volume has been
	// gone, which is why deleting on source_volume_exists=false alone is a
	// mistake this key exists to prevent.
	//
	// Unlike AttrSourceVolumeExists this is written whenever any were found,
	// complete enumeration or not: it is positive evidence, and withholding a
	// known "this snapshot backs an AMI" is the one error that could get a
	// snapshot deleted. Its absence is correspondingly weaker — it means no
	// backing AMI was seen, which is only "there is none" when the ledger
	// shows nothing failed.
	AttrBackingImageIDs = "backing_image_ids"
	// AttrStorageTier is a snapshot's storage tier ("standard", "archive").
	// Archived snapshots bill differently and take hours to restore.
	AttrStorageTier = "storage_tier"
	// AttrConnectivityType distinguishes a public NAT gateway from a private
	// one. Both bill by the hour; only the public one holds a billable IPv4.
	AttrConnectivityType = "connectivity_type"
	// AttrPublicIP is the billable IPv4 address itself.
	AttrPublicIP = "public_ip"
	// AttrAssociatedWith names what a public IPv4 or a NAT gateway address is
	// attached to — an instance ID, a network interface ID, a NAT gateway ID.
	// Absent means AWS reported no association, which for an Elastic IP is the
	// finding: an unassociated EIP bills exactly the same as a working one.
	AttrAssociatedWith = "associated_with"
	// AttrScheme is a load balancer's "internet-facing" or "internal".
	AttrScheme = "scheme"
	// AttrLoadBalancerType is "application", "network", "gateway" for the v2
	// API, and "classic" for the original one.
	AttrLoadBalancerType = "load_balancer_type"
	// AttrTargetGroupARNs lists the target groups pointed at a v2 load
	// balancer, comma separated and sorted.
	//
	// It is written only when the target group enumeration finished, for the
	// same reason source_volume_exists is: an empty list means "nothing is
	// attached to this load balancer", which is a delete signal, and a
	// truncated list is indistinguishable from an empty one. If the call
	// failed the key is absent and the ledger says why.
	AttrTargetGroupARNs = "target_group_arns"
	// AttrRuntime is a Lambda function's runtime identifier exactly as AWS
	// names it — "python3.12", "nodejs18.x", "java8.al2", "provided.al2". It is
	// deliberately not split into AttrEngine and AttrEngineVersion: AWS's
	// deprecation table is published against these identifiers whole, and
	// re-joining two halves to look a date up would let a parsing slip produce
	// a lifecycle verdict for a runtime that does not exist.
	//
	// Absent means AWS reported no runtime, which for a container-image
	// function is the correct answer rather than a gap — the runtime lives
	// inside an image blueprint cannot see, so no lifecycle verdict can honestly
	// be drawn. AttrPackageType says which case a reader is looking at.
	AttrRuntime = "runtime"
	// AttrPackageType is "Zip" or "Image". It is the difference between a
	// function whose runtime AWS manages and patches, and one whose base image
	// the owner is responsible for rebuilding — the same deprecation date
	// applies to both, but only one of them gets an email about it.
	AttrPackageType = "package_type"
	// AttrArchitecture is the instruction set a function runs on ("x86_64",
	// "arm64"), comma separated on the rare function that declares both. Lambda
	// prices arm64 roughly 20% below x86_64 for identical work, so it is a
	// standing price difference visible from a describe call alone.
	AttrArchitecture = "architecture"
	// AttrLastModified is when a function's code or configuration last changed,
	// RFC 3339 as Lambda reports it. It is emphatically not a creation time and
	// must never be read into CreatedAt: a function untouched since 2019 and one
	// created yesterday are the opposite findings, and Lambda reports no
	// creation time at all.
	AttrLastModified = "last_modified"
	// AttrSSEAlgorithm is the default server-side encryption a bucket applies
	// to new objects, under the name S3 gives it: "AES256" (SSE-S3),
	// "aws:kms", or "aws:kms:dsse". Absent means S3 reported no default
	// encryption configuration, which is not the same as unencrypted — see the
	// Encrypted field's contract and the s3 scanner for why the core flag stays
	// nil there.
	AttrSSEAlgorithm = "sse_algorithm"
	// AttrBucketKeyEnabled is S3 Bucket Keys, which cut the KMS request charge
	// on an SSE-KMS bucket by up to 99% by deriving a per-bucket key instead of
	// calling KMS per object. It is a pure cost lever with no behavioural
	// difference, which is why a bucket that never turned it on is a finding
	// and false must survive to the page.
	AttrBucketKeyEnabled = "bucket_key_enabled"
	// AttrVersioning is a bucket's versioning state, verbatim: "Enabled" or
	// "Suspended". Absent means never enabled — S3 answers that case with an
	// empty status rather than a third word, and inventing one ("Disabled")
	// would put a value in the census that no API ever said.
	//
	// It belongs in a cost census because noncurrent versions are billed and
	// invisible: they do not appear in a bucket listing, and a bucket with
	// versioning on and no lifecycle rule grows forever.
	AttrVersioning = "versioning"
	// AttrMFADelete is whether deleting a version requires MFA. Almost always
	// disabled; recorded because it is in the same response and is the one
	// setting that distinguishes a versioned bucket kept for compliance from
	// one versioned by accident.
	AttrMFADelete = "mfa_delete"
	// The four Block Public Access settings, each under the name S3 gives it.
	// They are four keys rather than one summary because they do different
	// jobs and the difference decides whether a bucket is reachable:
	// AttrBlockPublicACLs and AttrBlockPublicPolicy reject public
	// configurations at write time, while AttrIgnorePublicACLs and
	// AttrRestrictPublicBuckets neutralize the ones already there. Only the
	// second pair says anything about a bucket that is public today.
	AttrBlockPublicACLs       = "block_public_acls"
	AttrIgnorePublicACLs      = "ignore_public_acls"
	AttrBlockPublicPolicy     = "block_public_policy"
	AttrRestrictPublicBuckets = "restrict_public_buckets"
	// AttrPolicyIsPublic is S3's own verdict on whether a bucket's policy makes
	// it public, from GetBucketPolicyStatus. It is recorded beside the core
	// PubliclyAccessible flag rather than folded into it because the two answer
	// different questions: this one is about the policy alone, while the flag
	// has to account for Block Public Access overriding it.
	AttrPolicyIsPublic = "policy_is_public"
)

// Measure keys used in Resource.Measures.
const (
	MeasureSizeBytes           = "size_bytes"
	MeasureBackupRetentionDays = "backup_retention_days"
	MeasureBaseCapacityRPU     = "base_capacity_rpu"
	// MeasureFreeStorageBytes is the unused space left on an instance's
	// volume, observed from CloudWatch rather than reported by a describe
	// call — so it always carries an observation time (see AsOfSuffix).
	MeasureFreeStorageBytes = "free_storage_bytes"
	// MeasureIOPS and MeasureThroughputMiBps are what a volume is provisioned
	// to deliver, reported only for the volume types that carry them. A gp3
	// left at its 3000/125 baseline reports those numbers, and zero is a real
	// value wherever AWS returns one.
	MeasureIOPS            = "iops"
	MeasureThroughputMiBps = "throughput_mibps"
	// MeasureSourceVolumeBytes and MeasureFullSnapshotBytes are the two sizes
	// AWS reports for a snapshot, and neither is what the snapshot costs.
	//
	// No API reports a snapshot's billed size. EBS snapshots are incremental:
	// the charge is for blocks this snapshot is the only remaining owner of,
	// which depends on every other snapshot of the same volume and on the
	// order they are deleted in. VolumeSize is the source volume's provisioned
	// size, unchanged by how much of it was ever written.
	// FullSnapshotSizeInBytes is the size of all written blocks — what a full
	// restore would produce, and an upper bound the actual charge is usually
	// far below.
	//
	// So these are deliberately not MeasureSizeBytes: summing them into a
	// storage or cost total would produce a confident number that is wrong by
	// an unknowable factor, most badly in exactly the accounts with the most
	// snapshots. They are named for what AWS measured, and they are per-row
	// facts only.
	MeasureSourceVolumeBytes = "source_volume_bytes"
	MeasureFullSnapshotBytes = "full_snapshot_bytes"
	// MeasureTargetGroupCount and MeasureRegisteredInstanceCount are the
	// structural idle signal for a load balancer: how many places it can send
	// traffic, counted from describe calls alone. Zero is the whole point of
	// recording them and must survive to the page — a load balancer with
	// nothing behind it bills the same hourly rate as one serving production.
	//
	// Neither is a verdict about traffic. "Idle" in the sense of "nobody is
	// calling it" is a CloudWatch question, and answering it here from a
	// structural count would be an inference dressed as a fact: a load
	// balancer with healthy targets and no requests is idle, and one with no
	// targets may have been drained thirty seconds ago.
	//
	// They are not two views of the same number, and only one appears per row.
	// The classic API returns registered instances inline, so that count is
	// free and always complete. The v2 APIs answer the same question one
	// request per target group, which is the N+1 the scanner is shaped to
	// avoid — so a v2 row carries the target group count instead, and leaves
	// the instance count absent rather than filling it with a number nothing
	// measured.
	MeasureTargetGroupCount        = "target_group_count"
	MeasureRegisteredInstanceCount = "registered_instance_count"
	// MeasureAvailabilityZoneCount is how many AZs a load balancer or NAT
	// gateway deployment spans. NAT gateways are per-AZ and each one bills
	// separately, which is the forgotten multiplier the issue behind this
	// scanner names.
	MeasureAvailabilityZoneCount = "availability_zone_count"
	// MeasureMemoryMB and MeasureTimeoutSeconds are what a Lambda function is
	// configured to be allowed, not what it used. Lambda bills GB-seconds, so
	// memory is a direct multiplier on every invocation's price and timeout is
	// the ceiling on how long one can bill for — a function at 10240 MB that
	// needs 256 is paying 40x, and no describe call will say so. What it
	// actually consumed is a CloudWatch question and is left to the enrich
	// stage; guessing it from the configuration would be inventing the finding.
	MeasureMemoryMB       = "memory_mb"
	MeasureTimeoutSeconds = "timeout_seconds"
	// MeasureCodeSizeBytes is the deployment package size. Deliberately not
	// MeasureSizeBytes: that key means stored data, and folding code artifacts
	// into an estate-wide storage total would mix two unrelated things. It is
	// its own quota — 75 GB of code storage per region — which is the number
	// this measure adds up to.
	//
	// Zero is a real value and is stored. A container-image function reports 0
	// here because its code is in ECR, not in Lambda, and that zero is the
	// finding rather than a gap.
	MeasureCodeSizeBytes = "code_size_bytes"
	// MeasureEphemeralStorageMB is the /tmp allocation. Everything above the
	// 512 MB default is separately billed per GB-second, so it is a charge that
	// exists whether or not the function ever writes a file.
	MeasureEphemeralStorageMB = "ephemeral_storage_mb"
	// MeasureObjectCount is how many objects a bucket holds, observed from
	// CloudWatch rather than reported by any describe call — so it carries an
	// observation time (see AsOfSuffix), and an absent key means the metric had
	// no datapoint in the window rather than an empty bucket.
	//
	// The distinction is the whole value of the key. A brand-new bucket and a
	// bucket S3 stopped publishing for look identical from the control plane,
	// and writing 0 for either would turn "we do not know" into "we checked and
	// it is empty" — the exact claim someone deletes a bucket on.
	MeasureObjectCount = "object_count"
)

// AsOfSuffix names the attribute that carries a measure's observation time:
// measure "free_storage_bytes" is timestamped by attribute
// "free_storage_bytes_as_of", holding RFC 3339 UTC.
//
// Describe-call measures are true as of the scan. Metric measures are not:
// CloudWatch daily statistics lag 24–48h, and a stopped resource stops
// publishing entirely, so its newest datapoint can be far older than the
// scan. Rendering a two-day-old reading as if it were current is the same
// class of lie as rendering an unreported value as zero, so the timestamp
// travels with the value instead of being implied by GeneratedAt.
//
// It is a suffixed attribute rather than a field because measures live in an
// open bag: a parallel map of times would have to be threaded through the
// diff, CSV flattening, and report for a value only some measures have.
const AsOfSuffix = "_as_of"

// Resource is one discovered AWS resource, normalized across services.
//
// The struct splits into a narrow core and a typed attribute bag. The core
// holds the handful of fields that mean the same thing for every AWS resource
// — identity, placement, ownership, lifecycle — and that the renderers, diff,
// and summary all index on. Everything service-specific lives in Attributes
// (strings) and Measures (integers), keyed by the AWS API field it came from.
//
// That split is what lets a new scanner land without touching this file: an
// S3 bucket or an EC2 instance carries its own keys instead of forcing a new
// column that every other service leaves blank.
//
// Key absence in the bag carries the same "not reported" signal the core's
// pointer fields carry (honesty guardrail): a service that does not report
// multi-AZ has no multi_az key, which is distinct from multi_az="false".
// Measures are int64 rather than float64 so values round-trip through JSON
// byte-for-byte, and both maps are deterministic in the artifact because
// encoding/json sorts map keys.
type Resource struct {
	ARN string `json:"arn"`
	// Service is the census service bucket ("rds", "dynamodb", ...); Type is
	// the CloudFormation type name ("AWS::RDS::DBCluster").
	Service string `json:"service"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Status  string `json:"status,omitempty"`
	// Region is omitted for global resources (IAM, S3 buckets addressed
	// globally, ...) rather than guessed as us-east-1.
	Region    string            `json:"region,omitempty"`
	AccountID string            `json:"account_id"`
	CreatedAt *time.Time        `json:"created_at,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	// Environment and Owner are tag-derived only (PRD honesty guardrail:
	// imported, never inferred). Empty means "no matching tag".
	Environment string `json:"environment,omitempty"`
	Owner       string `json:"owner,omitempty"`
	// EOL marks resources whose end-of-life date has passed, per the table
	// baked into the binary (see eol.go for scope and exclusions); EOLDate
	// carries that date as YYYY-MM-DD. Whose end of life it is depends on the
	// service — an RDS engine's date is the community's, a Lambda runtime's is
	// AWS's own, and the two disagree by years — so the table records the date
	// its own publisher gave and this field claims only that it has passed.
	// The verdict lives in the core
	// because every renderer reads it for every service, while the inputs it
	// is derived from — the service's platform and version — stay in the bag
	// under the names AWS gives them.
	EOL     bool   `json:"eol,omitempty"`
	EOLDate string `json:"eol_date,omitempty"`
	// Exposure booleans straight from the describe responses, pointer-typed
	// because absence must stay distinguishable from a healthy value: nil
	// means the service does not report the field (honesty guardrail —
	// reported, never inferred). They stay in the core rather than the bag
	// because the summary and the report's exposure count read them for every
	// resource type.
	PubliclyAccessible *bool `json:"publicly_accessible,omitempty"`
	Encrypted          *bool `json:"encrypted,omitempty"`
	// Cost is what a cost source reported this resource costs, present only
	// when one actually reported a figure for it. Nil means no source priced
	// it — never that the resource is free. It is a pointer to a typed struct
	// rather than a bag key because an amount is meaningless without the
	// currency, method, and window that qualify it, and those must travel
	// together or not at all. See ResourceCost.
	//
	// Adding it does not bump SchemaVersion, on the same reasoning as
	// Snapshot.Cost: it changes no existing field's representation, and the
	// diff does not read it, so it cannot fabricate drift across the boundary
	// the version guards. An older baseline simply has no costs to compare.
	Cost *ResourceCost `json:"cost,omitempty"`
	// CostUnavailable names why no source priced this resource, when a source
	// looked and came back empty. It is the absence made explicit: a blank cost
	// cell with a reason beside it cannot be misread as zero spend, which a
	// blank cell on its own invites. Empty means nothing looked — no cost stage
	// ran, or it stopped before reaching this resource — which is a different
	// statement from "looked and found nothing" and must not be collapsed into
	// it. Only a source that actually queried for this resource may set it, and
	// only to something that source can support; render never invents one.
	CostUnavailable string `json:"cost_unavailable,omitempty"`

	Attributes map[string]string `json:"attributes,omitempty"`
	Measures   map[string]int64  `json:"measures,omitempty"`
}

// Attr returns the attribute for key, or "" when the resource does not report
// it. Callers that must tell "absent" from "empty" should read Attributes
// directly — SetAttr never stores an empty value, so the two coincide.
func (r *Resource) Attr(key string) string { return r.Attributes[key] }

// SetAttr records an attribute, dropping empty values so a field the service
// did not report stays key-absent instead of becoming an empty string.
func (r *Resource) SetAttr(key, value string) {
	if key == "" || value == "" {
		return
	}
	if r.Attributes == nil {
		r.Attributes = make(map[string]string)
	}
	r.Attributes[key] = value
}

// SetBoolAttr records a tri-state boolean: a nil pointer leaves the key
// absent, preserving "the service does not report this".
func (r *Resource) SetBoolAttr(key string, v *bool) {
	if v == nil {
		return
	}
	r.SetAttr(key, strconv.FormatBool(*v))
}

// Measure returns the measure for key and whether the resource reported it.
// The bool matters: 0 backup-retention days means backups are off, which is
// not the same as a service that has no retention setting at all.
func (r *Resource) Measure(key string) (int64, bool) {
	v, ok := r.Measures[key]
	return v, ok
}

// SetMeasure records a measure. Zero is stored, because for several keys zero
// is a real (and alarming) reading rather than an absence.
func (r *Resource) SetMeasure(key string, v int64) {
	if key == "" {
		return
	}
	if r.Measures == nil {
		r.Measures = make(map[string]int64)
	}
	r.Measures[key] = v
}

// SetMeasureInt32 records a measure from the *int32 the AWS SDKs hand back,
// leaving the key absent when the service did not report it. The pointer's
// presence is the test, never the value: filtering out zero would collapse
// "reported as zero" into "not reported", which is the distinction the bag
// exists to keep.
func (r *Resource) SetMeasureInt32(key string, v *int32) {
	if v == nil {
		return
	}
	r.SetMeasure(key, int64(*v))
}

// SetMeasureInt64 is SetMeasureInt32 for the *int64 fields (byte counts,
// mostly), with the same pointer-presence rule.
func (r *Resource) SetMeasureInt64(key string, v *int64) {
	if v == nil {
		return
	}
	r.SetMeasure(key, *v)
}

// SetObservedMeasure records a measure read from a time series together with
// the instant AWS observed it, stored under key+AsOfSuffix in UTC.
//
// A zero observation time leaves the measure unwritten. That is stricter than
// SetMeasure, and deliberately so: an untimed datapoint is one whose staleness
// cannot be judged, and for metrics — where the newest reading may predate the
// scan by days — an unjudgeable value is worse than an absent one. Callers
// with a value that is true as of the scan want SetMeasure.
func (r *Resource) SetObservedMeasure(key string, v int64, at time.Time) {
	if key == "" || at.IsZero() {
		return
	}
	r.SetMeasure(key, v)
	r.SetAttr(key+AsOfSuffix, at.UTC().Format(time.RFC3339))
}

// MeasureAsOf returns when key's value was observed. The bool is false for a
// measure that carries no observation time — every describe-sourced measure,
// which is current as of the scan.
func (r *Resource) MeasureAsOf(key string) (time.Time, bool) {
	raw := r.Attr(key + AsOfSuffix)
	if raw == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC(), true
}

// Exposed reports whether any collected exposure flag is in its risky state:
// publicly accessible, storage not encrypted, or automated backups disabled.
func (r *Resource) Exposed() bool {
	if r.PubliclyAccessible != nil && *r.PubliclyAccessible {
		return true
	}
	if r.Encrypted != nil && !*r.Encrypted {
		return true
	}
	// Absent means the service has no retention setting; only an explicit 0
	// means backups are off.
	if days, ok := r.Measure(MeasureBackupRetentionDays); ok && days == 0 {
		return true
	}
	return false
}

// Failure records a scan unit the tool could NOT see, so coverage claims
// stay honest ("could not scan X: AccessDenied").
type Failure struct {
	AccountID string `json:"account_id,omitempty"`
	Region    string `json:"region,omitempty"`
	Service   string `json:"service"`
	Error     string `json:"error"`
	// Time is when the tool recorded the failure, in UTC.
	//
	// Snapshot.GeneratedAt is stamped when the run starts, so on its own it
	// bounds a failure only from below. For an organization-wide census that
	// runs for minutes — and for the billed Cost Explorer calls, where the
	// question "when did this throttle?" has a price attached — "sometime
	// after the scan began" is too wide to line up against a CloudTrail event
	// or a throttling window. This is the instant that correlates.
	//
	// Adding it does not bump SchemaVersion: it is a new field rather than a
	// change to an existing one, and the diff does not read the ledger, so it
	// cannot fabricate drift across the boundary the version guards. Zero
	// means an entry that predates the field, and is omitted rather than
	// written out as year 1.
	Time time.Time `json:"time,omitzero"`
}

// SchemaVersion is the census artifact schema written by this binary.
// Snapshots produced before versioning carry an implicit 0. Diffing across
// schema versions is refused: field representation changes (schema 2 moved
// engine/class/storage into the attribute bag and replaced kind with a
// CloudFormation type name) would otherwise surface as fabricated resource
// drift on every row.
const SchemaVersion = 2

// Snapshot is the complete result of one scan run — the unit all renderers
// consume and the JSON artifact written to disk.
type Snapshot struct {
	// Schema is the artifact schema version (SchemaVersion at write time).
	Schema      int       `json:"schema"`
	Version     string    `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Accounts    []string  `json:"accounts"`
	Regions     []string  `json:"regions"`
	// Services lists the scanner services this scan attempted — the coverage
	// scope, independent of what was found. Part of the history scope key so
	// a scan with fewer scanners never diffs against a wider baseline (which
	// would report every unscanned resource as removed).
	Services  []string   `json:"services,omitempty"`
	Resources []Resource `json:"resources"`
	Failures  []Failure  `json:"failures,omitempty"`
	// Cost is the billing rollup for these accounts, present only when the
	// scan was run with --costs. Nil means cost was not collected — never that
	// spend was zero. Adding it does not bump SchemaVersion: it changes no
	// existing field's representation, and the diff does not read it, so it
	// cannot fabricate drift across the boundary that the version guards.
	Cost *CostReport `json:"cost,omitempty"`
}

// Summary holds the sprawl numbers shown in the terminal and report header.
type Summary struct {
	Total        int
	Types        map[string]int
	Engines      map[string]int
	Services     map[string]int
	Regions      map[string]int
	Accounts     map[string]int
	Environments map[string]int
	NoOwner      int
	NoEnv        int
	EOL          int
	Public       int
	Unencrypted  int
	NoBackups    int
	Exposed      int
	Failures     int
}

// Summarize computes the sprawl summary for a snapshot.
func (s *Snapshot) Summarize() Summary {
	sum := Summary{
		Types:        map[string]int{},
		Engines:      map[string]int{},
		Services:     map[string]int{},
		Regions:      map[string]int{},
		Accounts:     map[string]int{},
		Environments: map[string]int{},
		Failures:     len(s.Failures),
	}
	for i := range s.Resources {
		r := &s.Resources[i]
		sum.Total++
		sum.Types[r.Type]++
		// Most AWS resources have no engine; counting the empty bucket would
		// report "1 engine" for an estate of S3 buckets.
		if engine := r.Attr(AttrEngine); engine != "" {
			sum.Engines[engine]++
		}
		sum.Services[r.Service]++
		sum.Regions[r.Region]++
		sum.Accounts[r.AccountID]++
		if r.Owner == "" {
			sum.NoOwner++
		}
		if r.Environment == "" {
			sum.NoEnv++
		} else {
			sum.Environments[r.Environment]++
		}
		if r.EOL {
			sum.EOL++
		}
		if r.PubliclyAccessible != nil && *r.PubliclyAccessible {
			sum.Public++
		}
		if r.Encrypted != nil && !*r.Encrypted {
			sum.Unencrypted++
		}
		if days, ok := r.Measure(MeasureBackupRetentionDays); ok && days == 0 {
			sum.NoBackups++
		}
		if r.Exposed() {
			sum.Exposed++
		}
	}
	return sum
}

// Finalize prepares a snapshot for output: derives tag-based fields on every
// resource and sorts Resources, Regions, and Accounts. Regions and Accounts
// are collected from map/API iteration upstream, so sorting here is what
// makes JSON artifacts byte-for-byte deterministic. The attribute bags need
// no sorting — encoding/json emits map keys in sorted order.
func (s *Snapshot) Finalize() {
	now := time.Now()
	for i := range s.Resources {
		s.Resources[i].DeriveEnvOwner()
		s.Resources[i].DeriveEOL(now)
	}
	s.Sort()
	s.SortFailures()
	sort.Strings(s.Regions)
	sort.Strings(s.Accounts)
	sort.Strings(s.Services)
}

// SortFailures orders the failure ledger deterministically. The runner
// appends failures in goroutine-completion order, so without this the JSON
// artifact would differ between two identical runs. Exported because the CLI
// appends org-mode pre-scan failures after Finalize and must re-sort.
func (s *Snapshot) SortFailures() {
	sort.Slice(s.Failures, func(i, j int) bool {
		a, b := s.Failures[i], s.Failures[j]
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Error != b.Error {
			return a.Error < b.Error
		}
		// Time is the final tie-break, not part of the ledger's identity: two
		// entries alike in every other field must still land in a fixed order,
		// because sort.Slice is unstable and the artifact has to be
		// byte-for-byte reproducible for a given snapshot.
		return a.Time.Before(b.Time)
	})
}

// Sort orders resources deterministically: account, region, service, name,
// with ARN as the final tie-break. The tie-break is load-bearing: names come
// from optional tags for some resource types, so (account, region, service,
// name) is not unique, the input order is goroutine-completion order, and
// sort.Slice is unstable — without a unique key the JSON artifact would stop
// being byte-for-byte deterministic.
func (s *Snapshot) Sort() {
	sort.Slice(s.Resources, func(i, j int) bool {
		a, b := s.Resources[i], s.Resources[j]
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ARN < b.ARN
	})
}
