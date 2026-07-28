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
)

// Measure keys used in Resource.Measures.
const (
	MeasureSizeBytes           = "size_bytes"
	MeasureBackupRetentionDays = "backup_retention_days"
	MeasureBaseCapacityRPU     = "base_capacity_rpu"
)

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
	// EOL marks resources whose upstream end-of-life date has passed, per the
	// table baked into the binary (see eol.go for scope and exclusions);
	// EOLDate carries that date as YYYY-MM-DD.
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
// leaving the key absent when the service did not report it.
func (r *Resource) SetMeasureInt32(key string, v *int32) {
	if v == nil {
		return
	}
	r.SetMeasure(key, int64(*v))
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
		return a.Error < b.Error
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
