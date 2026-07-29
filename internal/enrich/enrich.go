// Package enrich holds the post-scan stages that fill in resource facts no
// single scanner can supply.
//
// A scanner sees one (account, region, service) unit at a time. That is the
// right shape for a describe API and the wrong shape for anything answering a
// question about the estate as a whole: CloudWatch GetMetricData carries 500
// queries per call and Cost Optimization Hub returns a flat organization-wide
// list. Asked from inside Scan(), both degenerate into many narrow calls — and
// for the billed ones, into many narrow charges.
//
// Each stage implements scan.Enricher and is wired in by the CLI only when the
// user opts in, which is what keeps the paid ones off by default.
package enrich
