package scanners

import "strings"

// Partition helpers shared by the scanners that have to build an ARN because
// the describe call does not return one.
//
// Getting the partition wrong is not cosmetic. The ARN is the key the diff
// matches resources on and the key cost enrichment joins on, so a GovCloud
// instance written as arn:aws:… matches nothing: it looks newly created on
// every scan and never picks up a cost figure. Both failures are silent.

// partitionFromARN reads the partition out of an ARN ("arn:PARTITION:..."),
// reporting whether the input was an ARN naming one at all.
//
// The bool matters to any caller that has a second source of evidence. A string
// that does not parse is not a partition of "aws" — it is no answer, and a
// caller that can fall back to the region name must be able to tell those
// apart. Callers with nothing to fall back on want arnPartition instead.
func partitionFromARN(arn string) (string, bool) {
	parts := strings.SplitN(arn, ":", 3)
	if len(parts) == 3 && parts[0] == "arn" && parts[1] != "" {
		return parts[1], true
	}
	return "", false
}

// arnPartition extracts the partition from an ARN ("arn:PARTITION:...");
// empty or malformed input falls back to the default "aws" partition.
//
// Preferred over partitionForRegion whenever the response carries any ARN at
// all, because it is AWS's own answer rather than this tool's reconstruction.
func arnPartition(arn string) string {
	if partition, ok := partitionFromARN(arn); ok {
		return partition
	}
	return "aws"
}

// regionPartitions maps a region-name prefix to its partition, longest prefix
// winning. The prefixes do not overlap ambiguously — "us-isob-" is not a
// prefix of "us-iso-" and vice versa — but they are matched longest-first
// anyway so a future region cannot quietly fall into the wrong bucket.
var regionPartitions = []struct{ prefix, partition string }{
	{"us-gov-", "aws-us-gov"},
	{"us-isob-", "aws-iso-b"},
	{"us-isof-", "aws-iso-f"},
	{"eu-isoe-", "aws-iso-e"},
	{"us-iso-", "aws-iso"},
	{"cn-", "aws-cn"},
}

// partitionForRegion derives the partition from a region name.
//
// This is a last resort, used only when a describe response contains no ARN to
// read the partition from — unlike Redshift (whose clusters always carry a
// namespace ARN), an EC2 instance commonly has no ARN-bearing field at all, so
// defaulting to "aws" would mis-key every resource in GovCloud and China
// rather than none. The mapping is a documented AWS naming rule, not a guess
// about the resource: region names in those partitions are prefixed by
// construction.
func partitionForRegion(region string) string {
	for _, p := range regionPartitions {
		if strings.HasPrefix(region, p.prefix) {
			return p.partition
		}
	}
	return "aws"
}
