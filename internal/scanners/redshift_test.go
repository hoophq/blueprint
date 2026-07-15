package scanners

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	redshifttypes "github.com/aws/aws-sdk-go-v2/service/redshift/types"
	rsstypes "github.com/aws/aws-sdk-go-v2/service/redshiftserverless/types"

	"github.com/hoophq/blueprint/internal/model"
)

func TestMbToGB(t *testing.T) {
	cases := map[int64]int32{
		0:    0,
		-1:   0,
		1:    1,
		1023: 1,
		1024: 1,
		1025: 2,
		2048: 2,
	}
	for mb, want := range cases {
		if got := mbToGB(mb); got != want {
			t.Errorf("mbToGB(%d) = %d, want %d", mb, got, want)
		}
	}
}

func TestRedshiftClusterARN(t *testing.T) {
	got := RedshiftClusterARN("aws", "us-east-1", "123456789012", "analytics")
	want := "arn:aws:redshift:us-east-1:123456789012:cluster:analytics"
	if got != want {
		t.Errorf("RedshiftClusterARN = %q, want %q", got, want)
	}
	got = RedshiftClusterARN("aws-us-gov", "us-gov-west-1", "123456789012", "analytics")
	want = "arn:aws-us-gov:redshift:us-gov-west-1:123456789012:cluster:analytics"
	if got != want {
		t.Errorf("RedshiftClusterARN = %q, want %q", got, want)
	}
}

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

func TestRedshiftClusterResource(t *testing.T) {
	c := redshifttypes.Cluster{
		ClusterIdentifier:               aws.String("analytics"),
		ClusterVersion:                  aws.String("1.0"),
		NodeType:                        aws.String("ra3.xlplus"),
		ClusterStatus:                   aws.String("available"),
		MultiAZ:                         aws.String("Enabled"),
		TotalStorageCapacityInMegaBytes: aws.Int64(1025),
		Endpoint:                        &redshifttypes.Endpoint{Address: aws.String("analytics.redshift.amazonaws.com")},
		Tags:                            []redshifttypes.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}
	r := redshiftClusterResource(c, "us-east-1", "123456789012")
	if r.ARN != "arn:aws:redshift:us-east-1:123456789012:cluster:analytics" {
		t.Errorf("unexpected ARN: %q", r.ARN)
	}
	if r.Service != model.ServiceRedshift || r.Kind != "cluster" || r.Engine != "redshift" {
		t.Errorf("unexpected service/kind/engine: %+v", r)
	}
	if r.InstanceClass != "ra3.xlplus" || r.EngineVersion != "1.0" {
		t.Errorf("unexpected class/version: %+v", r)
	}
	if !r.MultiAZ {
		t.Error("expected MultiAZ true for \"Enabled\"")
	}
	if r.StorageGB != 2 {
		t.Errorf("StorageGB = %d, want 2 (rounded up)", r.StorageGB)
	}
	if r.Endpoint != "analytics.redshift.amazonaws.com" {
		t.Errorf("unexpected endpoint: %q", r.Endpoint)
	}
	if r.Tags["env"] != "prod" {
		t.Errorf("unexpected tags: %v", r.Tags)
	}

	// Nil endpoint and absent MultiAZ must not panic or mislead.
	c.Endpoint = nil
	c.MultiAZ = nil
	r = redshiftClusterResource(c, "us-east-1", "123456789012")
	if r.Endpoint != "" || r.MultiAZ {
		t.Errorf("expected empty endpoint and MultiAZ false, got %+v", r)
	}

	// The ARN partition is derived from ClusterNamespaceArn when present.
	c.ClusterNamespaceArn = aws.String("arn:aws-us-gov:redshift:us-gov-west-1:123456789012:namespace:ns-1")
	r = redshiftClusterResource(c, "us-gov-west-1", "123456789012")
	if want := "arn:aws-us-gov:redshift:us-gov-west-1:123456789012:cluster:analytics"; r.ARN != want {
		t.Errorf("ARN = %q, want %q", r.ARN, want)
	}
}

func TestWorkgroupResource(t *testing.T) {
	w := rsstypes.Workgroup{
		WorkgroupArn:  aws.String("arn:aws:redshift-serverless:us-east-1:123456789012:workgroup/wg-1"),
		WorkgroupName: aws.String("etl"),
		Status:        rsstypes.WorkgroupStatusAvailable,
		BaseCapacity:  aws.Int32(8),
		Endpoint:      &rsstypes.Endpoint{Address: aws.String("etl.123456789012.us-east-1.redshift-serverless.amazonaws.com")},
	}
	r := workgroupResource(w, "us-east-1", "123456789012")
	if r.Service != model.ServiceRedshift || r.Kind != "serverless" || r.Engine != "redshift-serverless" {
		t.Errorf("unexpected service/kind/engine: %+v", r)
	}
	if r.Name != "etl" || r.Status != "AVAILABLE" {
		t.Errorf("unexpected name/status: %+v", r)
	}
	if r.InstanceClass != "8 RPU" {
		t.Errorf("InstanceClass = %q, want \"8 RPU\"", r.InstanceClass)
	}
	if r.Endpoint != "etl.123456789012.us-east-1.redshift-serverless.amazonaws.com" {
		t.Errorf("unexpected endpoint: %q", r.Endpoint)
	}

	// No base capacity or endpoint: fields stay empty.
	w.BaseCapacity = nil
	w.Endpoint = nil
	r = workgroupResource(w, "us-east-1", "123456789012")
	if r.InstanceClass != "" || r.Endpoint != "" {
		t.Errorf("expected empty class/endpoint, got %+v", r)
	}
}
