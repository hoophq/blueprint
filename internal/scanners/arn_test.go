package scanners

import "testing"

func TestArnPartition(t *testing.T) {
	cases := map[string]string{
		"arn:aws:redshift:us-east-1:1:namespace:ns":            "aws",
		"arn:aws-us-gov:redshift:us-gov-west-1:1:namespace:ns": "aws-us-gov",
		"arn:aws-cn:redshift:cn-north-1:1:namespace:ns":        "aws-cn",
		"":       "aws", // absent ClusterNamespaceArn falls back
		"arn::x": "aws", // empty partition falls back
		"bogus":  "aws",
		"a:b":    "aws",
	}
	for arn, want := range cases {
		if got := arnPartition(arn); got != want {
			t.Errorf("arnPartition(%q) = %q, want %q", arn, got, want)
		}
	}
}

// arnPartition answers "aws" for anything it cannot read, which is right for a
// caller with no other evidence and wrong for one that has a region to fall
// back on. partitionFromARN is where those two are told apart, so what it must
// pin is the bool: every unreadable input reports false rather than a partition
// that happens to be the commercial one.
func TestPartitionFromARN(t *testing.T) {
	readable := map[string]string{
		"arn:aws:ec2:us-east-1:1:instance/i-1":            "aws",
		"arn:aws-us-gov:iam::1:instance-profile/app":      "aws-us-gov",
		"arn:aws-cn:outposts:cn-north-1:1:outpost/op-1":   "aws-cn",
		"arn:aws-iso-b:ec2:us-isob-east-1:1:instance/i-1": "aws-iso-b",
	}
	for arn, want := range readable {
		got, ok := partitionFromARN(arn)
		if !ok || got != want {
			t.Errorf("partitionFromARN(%q) = (%q, %v), want (%q, true)", arn, got, ok, want)
		}
	}

	// None of these is an ARN naming a partition. Reporting "aws" with ok=true
	// for any of them would let a malformed field outrank the region rule.
	for _, arn := range []string{"", "bogus", "a:b", "arn::x", "arn::iam::1:role/r", "notarn:aws:ec2:::"} {
		if got, ok := partitionFromARN(arn); ok {
			t.Errorf("partitionFromARN(%q) = (%q, true), want ok=false", arn, got)
		}
	}
}

func TestPartitionForRegion(t *testing.T) {
	cases := map[string]string{
		"us-east-1":       "aws",
		"eu-west-1":       "aws",
		"sa-east-1":       "aws",
		"us-gov-west-1":   "aws-us-gov",
		"us-gov-east-1":   "aws-us-gov",
		"cn-north-1":      "aws-cn",
		"cn-northwest-1":  "aws-cn",
		"us-iso-east-1":   "aws-iso",
		"us-isob-east-1":  "aws-iso-b",
		"us-isof-south-1": "aws-iso-f",
		"eu-isoe-west-1":  "aws-iso-e",
		// An unknown region is not evidence of an unusual partition; the
		// commercial default is the only honest answer.
		"": "aws",
	}
	for region, want := range cases {
		if got := partitionForRegion(region); got != want {
			t.Errorf("partitionForRegion(%q) = %q, want %q", region, got, want)
		}
	}
}

// The iso prefixes are near-anagrams of each other, so pin that no region
// falls into a neighbour's partition — a silent mis-key would make every
// resource in that region look new on every scan.
func TestPartitionForRegionIsoPrefixesDoNotCollide(t *testing.T) {
	if partitionForRegion("us-isob-east-1") == partitionForRegion("us-iso-east-1") {
		t.Error("us-isob-* and us-iso-* resolved to the same partition")
	}
	if partitionForRegion("us-isof-south-1") == partitionForRegion("us-iso-east-1") {
		t.Error("us-isof-* and us-iso-* resolved to the same partition")
	}
}
