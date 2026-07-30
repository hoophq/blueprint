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

// Lambda's rows are keyed by AWS's runtime identifier whole, so this walks the
// real identifiers rather than a synthetic platform+version pair.
func TestDeriveEOLLambdaRuntime(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		runtime  string
		wantEOL  bool
		wantDate string
	}{
		// Deprecated: AWS has stopped patching these.
		{"python3.8", true, "2024-10-14"},
		{"python2.7", true, "2021-07-15"},
		{"nodejs16.x", true, "2024-06-12"},
		{"nodejs18.x", true, "2025-09-01"},
		{"go1.x", true, "2024-01-08"},
		{"java8", true, "2024-01-08"},
		{"provided", true, "2024-01-08"},
		{"dotnetcore3.1", true, "2023-04-03"},
		{"nodejs4.3-edge", true, "2020-03-05"},
		// The identifier with no version suffix at all — Node.js 0.10. It keys
		// on itself, which is the whole point of not splitting.
		{"nodejs", true, "2016-08-30"},
		// Supported at the table snapshot: no row, so no verdict. AWS publishes
		// projected dates for these and they are deliberately not transcribed.
		{"python3.12", false, ""},
		{"python3.10", false, ""},
		{"provided.al2", false, ""},
		{"java8.al2", false, ""},
		{"nodejs22.x", false, ""},
		{"dotnet8", false, ""},
		// A runtime published after this binary was built. No row, no verdict —
		// never a guess from the shape of the string.
		{"python4.0", false, ""},
		// Container-image function: AWS reports no runtime, so the attribute is
		// absent and nothing may be concluded about the base image inside it.
		{"", false, ""},
	}
	for _, c := range cases {
		r := Resource{Service: ServiceLambda}
		r.SetAttr(AttrRuntime, c.runtime)
		r.DeriveEOL(now)
		if r.EOL != c.wantEOL || r.EOLDate != c.wantDate {
			t.Errorf("DeriveEOL(lambda %q) = (%v, %q), want (%v, %q)",
				c.runtime, r.EOL, r.EOLDate, c.wantEOL, c.wantDate)
		}
	}
}

// The identifier is matched literally: no lowercasing, no trimming, no
// splitting on the dot. A near-miss must miss, because the failure mode of a
// fuzzy match is stating a deprecation date for a runtime that never had one.
func TestDeriveEOLLambdaMatchesTheIdentifierLiterally(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	for _, runtime := range []string{"Python3.8", "python3.8 ", "python3", "python", "3.8", "python3.80"} {
		r := Resource{Service: ServiceLambda}
		r.SetAttr(AttrRuntime, runtime)
		r.DeriveEOL(now)
		if r.EOL {
			t.Errorf("DeriveEOL(lambda %q) flagged EOL %q; only exact identifiers may match", runtime, r.EOLDate)
		}
	}
}

// Lambda reads the runtime attribute, not the engine one, and vice versa. A
// resource carrying the other service's attribute gets no verdict rather than
// a cross-wired one.
func TestDeriveEOLLambdaAndRDSDoNotShareAttributes(t *testing.T) {
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

	lambdaWithEngine := Resource{Service: ServiceLambda}
	lambdaWithEngine.SetAttr(AttrEngine, "mysql")
	lambdaWithEngine.SetAttr(AttrEngineVersion, "5.7.44")
	lambdaWithEngine.DeriveEOL(now)
	if lambdaWithEngine.EOL {
		t.Errorf("a lambda carrying an engine attribute flagged EOL %q", lambdaWithEngine.EOLDate)
	}

	rdsWithRuntime := Resource{Service: ServiceRDS}
	rdsWithRuntime.SetAttr(AttrRuntime, "python3.8")
	rdsWithRuntime.DeriveEOL(now)
	if rdsWithRuntime.EOL {
		t.Errorf("an RDS instance carrying a runtime attribute flagged EOL %q", rdsWithRuntime.EOLDate)
	}
}

// Every Lambda row is a date AWS has already reached. AWS also publishes
// projected deprecation dates for runtimes it still supports, and those are
// excluded on purpose: a forecast baked into a binary is inert until the day it
// silently becomes an assertion, at which point an old build states a date that
// may have moved, with nothing on the page to say it was ever a forecast.
//
// This fails the moment someone transcribes the supported-runtimes table, which
// is the intent — it should be a decision, not a copy-paste.
func TestLambdaEOLTableCarriesNoForecasts(t *testing.T) {
	// The table's own snapshot date, from the eolDates doc comment.
	snapshot := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	for key, date := range eolDates {
		if key.Service != ServiceLambda {
			continue
		}
		if key.Platform != lambdaRuntime {
			t.Errorf("lambda row %q has platform %q, want %q — the identifier is indivisible and lives in the version",
				key.Version, key.Platform, lambdaRuntime)
		}
		d, err := time.Parse("2006-01-02", date)
		if err != nil {
			t.Errorf("lambda row %q has unparseable date %q: %v", key.Version, date, err)
			continue
		}
		if d.After(snapshot) {
			t.Errorf("lambda row %q is dated %s, after the table snapshot: only runtimes AWS has already deprecated belong here, never projected dates",
				key.Version, date)
		}
	}
}
