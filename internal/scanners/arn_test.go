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
