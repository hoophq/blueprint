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

// eolDates maps a lifecycle identity to the upstream (community) end-of-life
// date for that platform. Table snapshot: 2026-07 — update alongside each
// release.
//
// Scope is deliberately narrow: only platforms whose upstream project
// publishes unambiguous EOL dates that apply to the AWS-managed build. Aurora
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
	date, ok := eolDates[lifecycle{r.Service, platform, majorVersion(platform, version)}]
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
// under. The bag is keyed by AWS's own field names — RDS says "engine", and
// Lambda will say "runtime" — so the mapping from AWS's word for it to a
// platform+version pair belongs here rather than in every scanner. Adding a
// service to the lifecycle story is a case here plus rows in eolDates.
func (r *Resource) lifecyclePlatform() (platform, version string) {
	switch r.Service {
	case ServiceRDS:
		return r.Attr(AttrEngine), r.Attr(AttrEngineVersion)
	}
	return "", ""
}

// majorVersion extracts the version component the EOL table is keyed by:
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
