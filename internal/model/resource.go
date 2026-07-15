// Package model defines the normalized resource model shared by all scanners
// and renderers. Fields are control-plane metadata only — never credentials.
package model

import (
	"sort"
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

// Resource is one discovered managed database, normalized across services.
type Resource struct {
	ARN           string            `json:"arn"`
	Service       string            `json:"service"`
	Kind          string            `json:"kind"` // instance | cluster | table | serverless
	Name          string            `json:"name"`
	Engine        string            `json:"engine"`
	EngineVersion string            `json:"engine_version,omitempty"`
	InstanceClass string            `json:"instance_class,omitempty"`
	StorageGB     int32             `json:"storage_gb,omitempty"`
	MultiAZ       bool              `json:"multi_az,omitempty"`
	Status        string            `json:"status,omitempty"`
	Endpoint      string            `json:"endpoint,omitempty"` // host only
	Region        string            `json:"region"`
	AccountID     string            `json:"account_id"`
	CreatedAt     *time.Time        `json:"created_at,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	// Environment and Owner are tag-derived only (PRD honesty guardrail:
	// imported, never inferred). Empty means "no matching tag".
	Environment string `json:"environment,omitempty"`
	Owner       string `json:"owner,omitempty"`
	// EOL marks engines whose upstream end-of-life date has passed, per the
	// table baked into the binary (see eol.go for scope and exclusions);
	// EOLDate carries that date as YYYY-MM-DD.
	EOL     bool   `json:"eol,omitempty"`
	EOLDate string `json:"eol_date,omitempty"`
	// Exposure booleans straight from the describe responses, pointer-typed
	// because absence must stay distinguishable from a healthy value: nil
	// means the service does not report the field (honesty guardrail —
	// reported, never inferred).
	PubliclyAccessible  *bool  `json:"publicly_accessible,omitempty"`
	Encrypted           *bool  `json:"encrypted,omitempty"`
	BackupRetentionDays *int32 `json:"backup_retention_days,omitempty"`
}

// Exposed reports whether any collected exposure flag is in its risky state:
// publicly accessible, storage not encrypted, or automated backups disabled.
func (r *Resource) Exposed() bool {
	return (r.PubliclyAccessible != nil && *r.PubliclyAccessible) ||
		(r.Encrypted != nil && !*r.Encrypted) ||
		(r.BackupRetentionDays != nil && *r.BackupRetentionDays == 0)
}

// Failure records a scan unit the tool could NOT see, so coverage claims
// stay honest ("could not scan X: AccessDenied").
type Failure struct {
	AccountID string `json:"account_id,omitempty"`
	Region    string `json:"region,omitempty"`
	Service   string `json:"service"`
	Error     string `json:"error"`
}

// Snapshot is the complete result of one scan run — the unit all renderers
// consume and the JSON artifact written to disk.
type Snapshot struct {
	Version     string     `json:"version"`
	GeneratedAt time.Time  `json:"generated_at"`
	Accounts    []string   `json:"accounts"`
	Regions     []string   `json:"regions"`
	Resources   []Resource `json:"resources"`
	Failures    []Failure  `json:"failures,omitempty"`
}

// Summary holds the sprawl numbers shown in the terminal and report header.
type Summary struct {
	Total        int
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
		Engines:      map[string]int{},
		Services:     map[string]int{},
		Regions:      map[string]int{},
		Accounts:     map[string]int{},
		Environments: map[string]int{},
		Failures:     len(s.Failures),
	}
	for _, r := range s.Resources {
		sum.Total++
		sum.Engines[r.Engine]++
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
		if r.BackupRetentionDays != nil && *r.BackupRetentionDays == 0 {
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
// makes JSON artifacts byte-for-byte deterministic.
func (s *Snapshot) Finalize() {
	now := time.Now()
	for i := range s.Resources {
		s.Resources[i].DeriveEnvOwner()
		s.Resources[i].DeriveEOL(now)
	}
	s.Sort()
	sort.Strings(s.Regions)
	sort.Strings(s.Accounts)
}

// Sort orders resources deterministically: account, region, service, name.
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
		return a.Name < b.Name
	})
}
