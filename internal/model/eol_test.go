package model

import (
	"testing"
	"time"
)

func TestDeriveEOL(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		service, engine, version string
		wantEOL                  bool
		wantDate                 string
	}{
		{ServiceRDS, "mysql", "5.7.44", true, "2023-10-31"},
		{ServiceRDS, "mysql", "5.6.51", true, "2021-02-05"},
		{ServiceRDS, "mysql", "8.0.35", true, "2026-04-30"},
		{ServiceRDS, "postgres", "9.6.24", true, "2021-11-11"},
		{ServiceRDS, "postgres", "13.13", true, "2025-11-13"},
		{ServiceRDS, "postgres", "14.11", false, ""}, // EOL 2026-11-12 has not passed yet
		{ServiceRDS, "postgres", "15.4", false, ""},
		{ServiceRDS, "mariadb", "10.5.9", true, "2025-06-24"},
		{ServiceRDS, "mariadb", "10.11.6", false, ""},
		// Excluded services: AWS-managed lifecycles, never flagged.
		{ServiceAurora, "aurora-mysql", "8.0.mysql_aurora.3.05.2", false, ""},
		{ServiceAurora, "aurora-postgresql", "11.9", false, ""},
		{ServiceElastiCache, "redis", "6.0.5", false, ""},
		{ServiceDocumentDB, "docdb", "3.6.0", false, ""},
		{ServiceDynamoDB, "dynamodb", "", false, ""},
		{ServiceRDS, "mysql", "", false, ""},
	}
	for _, c := range cases {
		r := Resource{Service: c.service}
		r.SetAttr(AttrEngine, c.engine)
		r.SetAttr(AttrEngineVersion, c.version)
		r.DeriveEOL(now)
		if r.EOL != c.wantEOL || r.EOLDate != c.wantDate {
			t.Errorf("DeriveEOL(%s %s %s) = (%v, %q), want (%v, %q)",
				c.service, c.engine, c.version, r.EOL, r.EOLDate, c.wantEOL, c.wantDate)
		}
	}
}

// The table is keyed by service as well as platform, so a service AWS runs on
// its own lifecycle calendar cannot inherit an upstream date by speaking the
// same dialect. Aurora is the live case; this pins the rule for every service
// the census has yet to learn.
func TestDeriveEOLIsScopedToService(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	for _, svc := range []string{ServiceAurora, ServiceElastiCache, ServiceRedshift, "some-future-service"} {
		r := Resource{Service: svc}
		r.SetAttr(AttrEngine, "mysql")
		r.SetAttr(AttrEngineVersion, "5.7.44")
		r.DeriveEOL(now)
		if r.EOL {
			t.Errorf("%s inherited the rds/mysql 5.7 EOL date", svc)
		}
	}

	// The same platform under RDS still flags, so the loop above is not passing
	// because nothing flags at all.
	r := Resource{Service: ServiceRDS}
	r.SetAttr(AttrEngine, "mysql")
	r.SetAttr(AttrEngineVersion, "5.7.44")
	r.DeriveEOL(now)
	if !r.EOL || r.EOLDate != "2023-10-31" {
		t.Errorf("rds mysql 5.7 = (%v, %q), want (true, 2023-10-31)", r.EOL, r.EOLDate)
	}
}

func TestDeriveEOLFutureDateNotFlagged(t *testing.T) {
	// The same version must not be flagged before its date passes.
	r := Resource{Service: ServiceRDS}
	r.SetAttr(AttrEngine, "mysql")
	r.SetAttr(AttrEngineVersion, "8.0.35")
	r.DeriveEOL(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if r.EOL {
		t.Errorf("mysql 8.0 flagged EOL before 2026-04-30")
	}
}
