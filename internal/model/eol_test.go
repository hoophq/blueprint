package model

import (
	"testing"
	"time"
)

func TestDeriveEOL(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		engine, version string
		wantEOL         bool
		wantDate        string
	}{
		{"mysql", "5.7.44", true, "2023-10-31"},
		{"mysql", "5.6.51", true, "2021-02-05"},
		{"mysql", "8.0.35", true, "2026-04-30"},
		{"postgres", "9.6.24", true, "2021-11-11"},
		{"postgres", "13.13", true, "2025-11-13"},
		{"postgres", "14.11", false, ""}, // EOL 2026-11-12 has not passed yet
		{"postgres", "15.4", false, ""},
		{"mariadb", "10.5.9", true, "2025-06-24"},
		{"mariadb", "10.11.6", false, ""},
		// Excluded engines: AWS-managed lifecycles, never flagged.
		{"aurora-mysql", "8.0.mysql_aurora.3.05.2", false, ""},
		{"aurora-postgresql", "11.9", false, ""},
		{"redis", "6.0.5", false, ""},
		{"docdb", "3.6.0", false, ""},
		{"dynamodb", "", false, ""},
		{"mysql", "", false, ""},
	}
	for _, c := range cases {
		r := Resource{Engine: c.engine, EngineVersion: c.version}
		r.DeriveEOL(now)
		if r.EOL != c.wantEOL || r.EOLDate != c.wantDate {
			t.Errorf("DeriveEOL(%s %s) = (%v, %q), want (%v, %q)",
				c.engine, c.version, r.EOL, r.EOLDate, c.wantEOL, c.wantDate)
		}
	}
}

func TestDeriveEOLFutureDateNotFlagged(t *testing.T) {
	// The same version must not be flagged before its date passes.
	r := Resource{Engine: "mysql", EngineVersion: "8.0.35"}
	r.DeriveEOL(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if r.EOL {
		t.Errorf("mysql 8.0 flagged EOL before 2026-04-30")
	}
}
