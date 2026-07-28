package scanners

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	redshifttypes "github.com/aws/aws-sdk-go-v2/service/redshift/types"
	rsstypes "github.com/aws/aws-sdk-go-v2/service/redshiftserverless/types"

	"github.com/hoophq/blueprint/internal/model"
)

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
	if r.Service != model.ServiceRedshift || r.Type != model.TypeRedshiftCluster {
		t.Errorf("unexpected service/type: %+v", r)
	}
	if got := r.Attr(model.AttrEngine); got != "redshift" {
		t.Errorf("engine = %q, want redshift", got)
	}
	if got := r.Attr(model.AttrInstanceClass); got != "ra3.xlplus" {
		t.Errorf("instance_class = %q, want ra3.xlplus", got)
	}
	if got := r.Attr(model.AttrEngineVersion); got != "1.0" {
		t.Errorf("engine_version = %q, want 1.0", got)
	}
	if got := r.Attr(model.AttrMultiAZ); got != "true" {
		t.Errorf("multi_az = %q, want \"true\" for \"Enabled\"", got)
	}
	if got, ok := r.Measure(model.MeasureSizeBytes); !ok || got != 1025*1024*1024 {
		t.Errorf("size_bytes = (%d, %v), want (%d, true)", got, ok, 1025*1024*1024)
	}
	if got := r.Attr(model.AttrEndpoint); got != "analytics.redshift.amazonaws.com" {
		t.Errorf("unexpected endpoint: %q", got)
	}
	if r.Tags["env"] != "prod" {
		t.Errorf("unexpected tags: %v", r.Tags)
	}

	// Capacity the cluster reported as zero is stored as zero; capacity it did
	// not report at all leaves the key absent. Collapsing the two would let a
	// cluster whose storage figure has not landed yet read as an empty one.
	c.TotalStorageCapacityInMegaBytes = aws.Int64(0)
	r = redshiftClusterResource(c, "us-east-1", "123456789012")
	if got, ok := r.Measure(model.MeasureSizeBytes); !ok || got != 0 {
		t.Errorf("size_bytes = (%d, %v) for a reported zero, want (0, true)", got, ok)
	}
	c.TotalStorageCapacityInMegaBytes = nil
	r = redshiftClusterResource(c, "us-east-1", "123456789012")
	if v, ok := r.Measure(model.MeasureSizeBytes); ok {
		t.Errorf("size_bytes = (%d, true) with no capacity reported, want absent", v)
	}
	c.TotalStorageCapacityInMegaBytes = aws.Int64(1025)

	// Nil endpoint and absent MultiAZ must not panic or mislead: a field the
	// API did not report leaves the key absent rather than reading as false.
	c.Endpoint = nil
	c.MultiAZ = nil
	r = redshiftClusterResource(c, "us-east-1", "123456789012")
	if _, ok := r.Attributes[model.AttrEndpoint]; ok {
		t.Errorf("expected no endpoint attribute, got %q", r.Attr(model.AttrEndpoint))
	}
	if _, ok := r.Attributes[model.AttrMultiAZ]; ok {
		t.Errorf("expected no multi_az attribute, got %q", r.Attr(model.AttrMultiAZ))
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
	if r.Service != model.ServiceRedshift || r.Type != model.TypeRedshiftServerlessWorkgroup {
		t.Errorf("unexpected service/type: %+v", r)
	}
	if got := r.Attr(model.AttrEngine); got != "redshift-serverless" {
		t.Errorf("engine = %q, want redshift-serverless", got)
	}
	if r.Name != "etl" || r.Status != "AVAILABLE" {
		t.Errorf("unexpected name/status: %+v", r)
	}
	// Base capacity is a number of RPUs, kept as a measure rather than
	// pre-formatted into the instance-class slot it does not belong in.
	if got, ok := r.Measure(model.MeasureBaseCapacityRPU); !ok || got != 8 {
		t.Errorf("base_capacity_rpu = (%d, %v), want (8, true)", got, ok)
	}
	if got := r.Attr(model.AttrEndpoint); got != "etl.123456789012.us-east-1.redshift-serverless.amazonaws.com" {
		t.Errorf("unexpected endpoint: %q", got)
	}

	// No base capacity or endpoint: the keys stay absent.
	w.BaseCapacity = nil
	w.Endpoint = nil
	r = workgroupResource(w, "us-east-1", "123456789012")
	if v, ok := r.Measure(model.MeasureBaseCapacityRPU); ok {
		t.Errorf("base_capacity_rpu = (%d, true), want not reported", v)
	}
	if _, ok := r.Attributes[model.AttrEndpoint]; ok {
		t.Errorf("expected no endpoint attribute, got %q", r.Attr(model.AttrEndpoint))
	}
}
