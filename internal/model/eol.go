package model

import (
	"strings"
	"time"
)

// eolDates maps engine → major version → upstream (community) end-of-life
// date. Table snapshot: 2026-07 — update alongside each release.
//
// Scope is deliberately narrow: only engines whose upstream project publishes
// unambiguous EOL dates that apply to the RDS-managed build. Aurora variants,
// DocumentDB, Neptune, ElastiCache, Redshift, and DynamoDB are excluded — AWS
// runs their lifecycles on its own calendar, so flagging them by upstream
// dates would be wrong more often than right.
var eolDates = map[string]map[string]string{
	"mysql": {
		"5.5": "2018-12-03",
		"5.6": "2021-02-05",
		"5.7": "2023-10-31",
		"8.0": "2026-04-30",
	},
	"postgres": {
		"9.6": "2021-11-11",
		"10":  "2022-11-10",
		"11":  "2023-11-09",
		"12":  "2024-11-14",
		"13":  "2025-11-13",
		"14":  "2026-11-12",
	},
	"mariadb": {
		"10.3": "2023-05-25",
		"10.4": "2024-06-18",
		"10.5": "2025-06-24",
		"10.6": "2026-07-06",
	},
}

// DeriveEOL fills EOL and EOLDate by matching the resource's engine major
// version against the baked-in upstream EOL table. Only dates that have
// already passed at `now` flag the resource, so future dates can sit in the
// table safely.
func (r *Resource) DeriveEOL(now time.Time) {
	r.EOL = false
	r.EOLDate = ""
	table, ok := eolDates[r.Engine]
	if !ok || r.EngineVersion == "" {
		return
	}
	date, ok := table[engineMajor(r.Engine, r.EngineVersion)]
	if !ok {
		return
	}
	if d, err := time.Parse("2006-01-02", date); err == nil && !d.After(now) {
		r.EOL = true
		r.EOLDate = date
	}
}

// engineMajor extracts the version component the EOL table is keyed by:
// postgres uses single-number majors from v10 on ("13.13" → "13") but
// two-component majors before ("9.6.24" → "9.6"); mysql and mariadb always
// use two components ("5.7.44" → "5.7").
func engineMajor(engine, version string) string {
	parts := strings.Split(version, ".")
	if engine == "postgres" && parts[0] != "9" {
		return parts[0]
	}
	if len(parts) < 2 {
		return parts[0]
	}
	return parts[0] + "." + parts[1]
}
