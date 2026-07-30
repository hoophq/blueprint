package model

import (
	"strings"
	"time"
)

// lifecycle identifies what a resource's end-of-life is judged against: the
// census service, the platform it runs, and that platform's major version.
//
// Keying by service as well as platform is what lets this table outgrow
// databases. "Engine" describes nothing about a Lambda function or an EKS
// cluster, and the same platform name can carry different dates under
// different services — AWS extends support for a runtime on its own schedule,
// independent of what the upstream project published. Service is the
// component that keeps those apart.
type lifecycle struct {
	Service  string
	Platform string
	Version  string
}

// eolDates maps a lifecycle identity to the published end-of-life date for
// that platform — the date after which nobody is shipping security patches for
// it. Table snapshot: 2026-07 — update alongside each release.
//
// Who publishes that date depends on who owns the lifecycle, and the two are
// not interchangeable. For an RDS engine it is the upstream community: AWS
// tracks it closely enough that the upstream date is the honest one. For a
// Lambda runtime it is AWS itself, which routinely patches a runtime for years
// after the language project has stopped. Reading either date onto the other's
// resources would be wrong in both directions, which is what the service
// component of the key is for.
//
// Scope is deliberately narrow: only platforms with an unambiguous published
// date that applies to the build AWS actually runs. Aurora
// variants, DocumentDB, Neptune, ElastiCache, Redshift, and DynamoDB are
// excluded — AWS runs their lifecycles on its own calendar, so flagging them
// by upstream dates would be wrong more often than right. Under service
// keying that exclusion is structural rather than incidental: an Aurora
// cluster cannot match the rds/mysql rows even though it speaks MySQL.
var eolDates = map[lifecycle]string{
	{ServiceRDS, "mysql", "5.5"}: "2018-12-03",
	{ServiceRDS, "mysql", "5.6"}: "2021-02-05",
	{ServiceRDS, "mysql", "5.7"}: "2023-10-31",
	{ServiceRDS, "mysql", "8.0"}: "2026-04-30",

	{ServiceRDS, "postgres", "9.6"}: "2021-11-11",
	{ServiceRDS, "postgres", "10"}:  "2022-11-10",
	{ServiceRDS, "postgres", "11"}:  "2023-11-09",
	{ServiceRDS, "postgres", "12"}:  "2024-11-14",
	{ServiceRDS, "postgres", "13"}:  "2025-11-13",
	{ServiceRDS, "postgres", "14"}:  "2026-11-12",

	{ServiceRDS, "mariadb", "10.3"}: "2023-05-25",
	{ServiceRDS, "mariadb", "10.4"}: "2024-06-18",
	{ServiceRDS, "mariadb", "10.5"}: "2025-06-24",
	{ServiceRDS, "mariadb", "10.6"}: "2026-07-06",

	// Lambda runtimes, from AWS's "Deprecated runtimes" table, keyed by the
	// runtime identifier whole (see lifecyclePlatform). The date is the
	// deprecation date — the day AWS stops patching the runtime — not the later
	// dates on which it starts blocking function creates and updates. Those two
	// describe what you may still do with the function; this one describes
	// whether anyone is fixing its CVEs, which is the question the red pill
	// answers.
	//
	// Unlike the RDS rows above, these are AWS's own dates rather than the
	// upstream project's. That is the point of keying by service: Lambda ran
	// Python 3.8 for two years past python.org's EOL, and reporting the
	// upstream date would have flagged a runtime AWS was still patching.
	//
	// Only the deprecated table is transcribed. AWS also publishes projected
	// deprecation dates for supported runtimes, and they are deliberately left
	// out: AWS labels them subject to change, and a future date baked into a
	// binary is inert until the day it silently becomes an assertion — at which
	// point a year-old build would state a deprecation that may have moved,
	// with nothing on the page to say the date was a forecast. A runtime AWS
	// has not yet deprecated gets no verdict here.
	{ServiceLambda, lambdaRuntime, "nodejs20.x"}:     "2026-04-30",
	{ServiceLambda, lambdaRuntime, "ruby3.2"}:        "2026-03-31",
	{ServiceLambda, lambdaRuntime, "python3.9"}:      "2025-12-15",
	{ServiceLambda, lambdaRuntime, "nodejs18.x"}:     "2025-09-01",
	{ServiceLambda, lambdaRuntime, "dotnet6"}:        "2024-12-20",
	{ServiceLambda, lambdaRuntime, "python3.8"}:      "2024-10-14",
	{ServiceLambda, lambdaRuntime, "nodejs16.x"}:     "2024-06-12",
	{ServiceLambda, lambdaRuntime, "dotnet7"}:        "2024-05-14",
	{ServiceLambda, lambdaRuntime, "java8"}:          "2024-01-08",
	{ServiceLambda, lambdaRuntime, "go1.x"}:          "2024-01-08",
	{ServiceLambda, lambdaRuntime, "provided"}:       "2024-01-08",
	{ServiceLambda, lambdaRuntime, "ruby2.7"}:        "2023-12-07",
	{ServiceLambda, lambdaRuntime, "nodejs14.x"}:     "2023-12-04",
	{ServiceLambda, lambdaRuntime, "python3.7"}:      "2023-12-04",
	{ServiceLambda, lambdaRuntime, "dotnetcore3.1"}:  "2023-04-03",
	{ServiceLambda, lambdaRuntime, "nodejs12.x"}:     "2023-03-31",
	{ServiceLambda, lambdaRuntime, "python3.6"}:      "2022-07-18",
	{ServiceLambda, lambdaRuntime, "dotnet5.0"}:      "2022-05-10",
	{ServiceLambda, lambdaRuntime, "dotnetcore2.1"}:  "2022-01-05",
	{ServiceLambda, lambdaRuntime, "nodejs10.x"}:     "2021-07-30",
	{ServiceLambda, lambdaRuntime, "ruby2.5"}:        "2021-07-30",
	{ServiceLambda, lambdaRuntime, "python2.7"}:      "2021-07-15",
	{ServiceLambda, lambdaRuntime, "nodejs8.10"}:     "2020-03-06",
	{ServiceLambda, lambdaRuntime, "nodejs4.3"}:      "2020-03-05",
	{ServiceLambda, lambdaRuntime, "nodejs4.3-edge"}: "2020-03-05",
	{ServiceLambda, lambdaRuntime, "nodejs6.10"}:     "2019-08-12",
	{ServiceLambda, lambdaRuntime, "dotnetcore1.0"}:  "2019-06-27",
	{ServiceLambda, lambdaRuntime, "dotnetcore2.0"}:  "2019-05-30",
	{ServiceLambda, lambdaRuntime, "nodejs"}:         "2016-08-30",
}

// DeriveEOL fills EOL and EOLDate by matching the resource's lifecycle
// identity against the baked-in upstream EOL table. Only dates that have
// already passed at `now` flag the resource, so future dates can sit in the
// table safely. Resources whose service reports no lifecycle platform (most
// of AWS) never flag.
func (r *Resource) DeriveEOL(now time.Time) {
	r.EOL = false
	r.EOLDate = ""
	platform, version := r.lifecyclePlatform()
	if platform == "" || version == "" {
		return
	}
	date, ok := eolDates[lifecycle{r.Service, platform, version}]
	if !ok {
		return
	}
	if d, err := time.Parse("2006-01-02", date); err == nil && !d.After(now) {
		r.EOL = true
		r.EOLDate = date
	}
}

// lifecyclePlatform returns the platform and version this resource's
// lifecycle is keyed by, read from whichever attribute the service reports it
// under. The bag is keyed by AWS's own field names — RDS says "engine", Lambda
// says "runtime" — so the mapping from AWS's word for it to a platform+version
// pair belongs here rather than in every scanner. Adding a service to the
// lifecycle story is a case here plus rows in eolDates.
//
// The version returned is already in the form eolDates is keyed by, because
// how much of a version string is significant is a per-platform question and
// only the platform's own case knows the answer. RDS publishes dates per major
// version and reports a full one, so it narrows. Lambda publishes them against
// whole runtime identifiers, so it narrows nothing — see below.
func (r *Resource) lifecyclePlatform() (platform, version string) {
	switch r.Service {
	case ServiceRDS:
		engine := r.Attr(AttrEngine)
		return engine, majorVersion(engine, r.Attr(AttrEngineVersion))
	case ServiceLambda:
		// Passed through whole and matched literally. AWS publishes Lambda
		// deprecations against the runtime identifier itself ("python3.8",
		// "nodejs18.x", "java8.al2", "provided.al2"), and there is no
		// decomposition of those into a language and a version that AWS
		// endorses: "provided.al2" names an OS, "nodejs18.x" carries a
		// wildcard, "nodejs" carries no version at all. Splitting them here
		// would invent a scheme and then depend on it, and the failure mode is
		// the bad one — a parse that lands on the wrong row states a
		// deprecation date for a runtime that never had one.
		//
		// So the platform slot holds a constant and every row in the table is
		// AWS's own string, copied. A runtime blueprint has no row for gets no
		// verdict, which is the correct answer for anything AWS has not
		// deprecated and for anything published after this binary was built.
		return lambdaRuntime, r.Attr(AttrRuntime)
	}
	return "", ""
}

// lambdaRuntime fills the platform slot for every Lambda row: the runtime
// identifier is indivisible, so it all lives in the version and the platform
// names the field it came from.
const lambdaRuntime = "runtime"

// majorVersion extracts the version component the RDS rows are keyed by:
// postgres uses single-number majors from v10 on ("13.13" → "13") but
// two-component majors before ("9.6.24" → "9.6"); mysql and mariadb always
// use two components ("5.7.44" → "5.7").
func majorVersion(platform, version string) string {
	parts := strings.Split(version, ".")
	if platform == "postgres" && parts[0] != "9" {
		return parts[0]
	}
	if len(parts) < 2 {
		return parts[0]
	}
	return parts[0] + "." + parts[1]
}
