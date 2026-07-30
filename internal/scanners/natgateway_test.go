package scanners

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/hoophq/blueprint/internal/model"
)

func TestNATGatewayARN(t *testing.T) {
	got := NATGatewayARN("aws", "us-east-1", testAccount, "nat-0abc123")
	want := "arn:aws:ec2:us-east-1:" + testAccount + ":natgateway/nat-0abc123"
	if got != want {
		t.Errorf("NATGatewayARN = %q, want %q", got, want)
	}
	got = NATGatewayARN("aws-cn", "cn-north-1", testAccount, "nat-0abc123")
	want = "arn:aws-cn:ec2:cn-north-1:" + testAccount + ":natgateway/nat-0abc123"
	if got != want {
		t.Errorf("NATGatewayARN = %q, want %q", got, want)
	}
}

func TestNATGatewayResource(t *testing.T) {
	created := time.Date(2023, 6, 1, 9, 30, 0, 0, time.UTC)
	gw := ec2types.NatGateway{
		NatGatewayId:     aws.String("nat-0abc123"),
		State:            ec2types.NatGatewayStateAvailable,
		ConnectivityType: ec2types.ConnectivityTypePublic,
		VpcId:            aws.String("vpc-0abc"),
		SubnetId:         aws.String("subnet-0abc"),
		CreateTime:       &created,
		NatGatewayAddresses: []ec2types.NatGatewayAddress{
			{PublicIp: aws.String("203.0.113.9"), AvailabilityZone: aws.String("us-east-1a")},
		},
		Tags: []ec2types.Tag{
			{Key: aws.String("Name"), Value: aws.String("egress-a")},
			{Key: aws.String("env"), Value: aws.String("prod")},
		},
	}

	r := natGatewayResource(gw, "us-east-1", testAccount)

	if r.Service != model.ServiceNATGateway {
		t.Errorf("Service = %q, want %q", r.Service, model.ServiceNATGateway)
	}
	if r.Type != model.TypeNATGateway {
		t.Errorf("Type = %q, want %q", r.Type, model.TypeNATGateway)
	}
	if r.Name != "egress-a" {
		t.Errorf("Name = %q, want the Name tag", r.Name)
	}
	if r.Status != string(ec2types.NatGatewayStateAvailable) {
		t.Errorf("Status = %q, want %q", r.Status, ec2types.NatGatewayStateAvailable)
	}
	if r.CreatedAt == nil || !r.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", r.CreatedAt, created)
	}
	if got := r.Attr(model.AttrConnectivityType); got != string(ec2types.ConnectivityTypePublic) {
		t.Errorf("connectivity_type = %q, want public", got)
	}
	if got := r.Attr(model.AttrPublicIP); got != "203.0.113.9" {
		t.Errorf("public_ip = %q, want the gateway address", got)
	}
	if got := r.Attr(model.AttrAvailabilityZone); got != "us-east-1a" {
		t.Errorf("availability_zone = %q, want us-east-1a", got)
	}
	if got, ok := r.Measure(model.MeasureAvailabilityZoneCount); !ok || got != 1 {
		t.Errorf("availability_zone_count = %d (present %t), want 1", got, ok)
	}
}

// A NAT gateway is not exposed and stores nothing, and answering either
// question would file it beside a database open to the internet.
func TestNATGatewayLeavesExposureAndEncryptionUnanswered(t *testing.T) {
	r := natGatewayResource(ec2types.NatGateway{
		NatGatewayId: aws.String("nat-0abc123"),
		State:        ec2types.NatGatewayStateAvailable,
		NatGatewayAddresses: []ec2types.NatGatewayAddress{
			{PublicIp: aws.String("203.0.113.9")},
		},
	}, "us-east-1", testAccount)

	if r.PubliclyAccessible != nil {
		t.Errorf("PubliclyAccessible = %v, want nil even though it holds a public address", *r.PubliclyAccessible)
	}
	if r.Encrypted != nil {
		t.Errorf("Encrypted = %v, want nil: a NAT gateway stores nothing", *r.Encrypted)
	}
}

// A private NAT gateway has addresses, and none of them are public. The
// attribute must be absent rather than empty-but-present, and the count must
// still be recorded from the zones those addresses do report.
func TestNATGatewayPrivateReportsNoPublicIP(t *testing.T) {
	r := natGatewayResource(ec2types.NatGateway{
		NatGatewayId:     aws.String("nat-0private"),
		State:            ec2types.NatGatewayStateAvailable,
		ConnectivityType: ec2types.ConnectivityTypePrivate,
		NatGatewayAddresses: []ec2types.NatGatewayAddress{
			{PrivateIp: aws.String("10.0.1.5"), AvailabilityZone: aws.String("us-east-1b")},
		},
	}, "us-east-1", testAccount)

	if _, ok := r.Attributes[model.AttrPublicIP]; ok {
		t.Errorf("public_ip present (%q); a private gateway has no public address to bill",
			r.Attr(model.AttrPublicIP))
	}
	if got, ok := r.Measure(model.MeasureAvailabilityZoneCount); !ok || got != 1 {
		t.Errorf("availability_zone_count = %d (present %t), want 1", got, ok)
	}
	// With no Name tag the identifier stands in, so the row is still findable.
	if r.Name != "nat-0private" {
		t.Errorf("Name = %q, want the gateway ID", r.Name)
	}
}

// The zone lives on the addresses, one per zone, and that is the only place it
// is reported. A gateway spanning several must count them all and must not pick
// one to stand for the rest.
func TestNATGatewayZonesAreDistinctAndSorted(t *testing.T) {
	addresses := []ec2types.NatGatewayAddress{
		{AvailabilityZone: aws.String("us-east-1c")},
		{AvailabilityZone: aws.String("us-east-1a")},
		{AvailabilityZone: aws.String("us-east-1a")},
		{AvailabilityZone: nil},
		{AvailabilityZone: aws.String("")},
	}
	got := natGatewayZones(addresses)
	want := []string{"us-east-1a", "us-east-1c"}
	if len(got) != len(want) {
		t.Fatalf("natGatewayZones = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("natGatewayZones = %v, want %v", got, want)
		}
	}

	r := natGatewayResource(ec2types.NatGateway{
		NatGatewayId:        aws.String("nat-0regional"),
		State:               ec2types.NatGatewayStateAvailable,
		NatGatewayAddresses: addresses,
	}, "us-east-1", testAccount)

	if got, ok := r.Measure(model.MeasureAvailabilityZoneCount); !ok || got != 2 {
		t.Errorf("availability_zone_count = %d (present %t), want 2", got, ok)
	}
	if _, ok := r.Attributes[model.AttrAvailabilityZone]; ok {
		t.Errorf("availability_zone = %q; a multi-zone gateway must not name one of them",
			r.Attr(model.AttrAvailabilityZone))
	}
}

// A gateway AWS named no zone for gets no count. Zero would read as "spans no
// zones", which is false for every NAT gateway that exists.
func TestNATGatewayWithoutZonesOmitsTheCount(t *testing.T) {
	r := natGatewayResource(ec2types.NatGateway{
		NatGatewayId:        aws.String("nat-0abc123"),
		State:               ec2types.NatGatewayStateAvailable,
		NatGatewayAddresses: []ec2types.NatGatewayAddress{{PublicIp: aws.String("203.0.113.9")}},
	}, "us-east-1", testAccount)

	if got, ok := r.Measure(model.MeasureAvailabilityZoneCount); ok {
		t.Errorf("availability_zone_count = %d, want absent when no zone was reported", got)
	}
}

// The bytes a gateway has moved are a CloudWatch question. A zero here would
// read as "nothing went through this", which is the one thing a reader acts on.
func TestNATGatewayReportsNoDataProcessed(t *testing.T) {
	r := natGatewayResource(ec2types.NatGateway{
		NatGatewayId: aws.String("nat-0abc123"),
		State:        ec2types.NatGatewayStateAvailable,
	}, "us-east-1", testAccount)

	for key := range r.Measures {
		if key != model.MeasureAvailabilityZoneCount {
			t.Errorf("unexpected measure %q; a NAT gateway describe reports no volume", key)
		}
	}
}
