package scanners

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/hoophq/blueprint/internal/model"
)

func TestPublicIPARNs(t *testing.T) {
	got := ElasticIPARN("aws", "us-east-1", testAccount, "eipalloc-0abc123")
	want := "arn:aws:ec2:us-east-1:" + testAccount + ":elastic-ip/eipalloc-0abc123"
	if got != want {
		t.Errorf("ElasticIPARN = %q, want %q", got, want)
	}
	got = NetworkInterfaceARN("aws-us-gov", "us-gov-west-1", testAccount, "eni-0abc123")
	want = "arn:aws-us-gov:ec2:us-gov-west-1:" + testAccount + ":network-interface/eni-0abc123"
	if got != want {
		t.Errorf("NetworkInterfaceARN = %q, want %q", got, want)
	}
}

// An unassociated Elastic IP bills exactly what a working one does, and is the
// single most common finding this scanner exists to surface.
func TestElasticIPUnassociated(t *testing.T) {
	r := elasticIPResource(ec2types.Address{
		PublicIp:     aws.String("203.0.113.1"),
		AllocationId: aws.String("eipalloc-0abc123"),
	}, "203.0.113.1", "aws", "us-east-1", testAccount)

	if r.Status != addressUnassociated {
		t.Errorf("Status = %q, want %q", r.Status, addressUnassociated)
	}
	if r.Type != model.TypeEIP {
		t.Errorf("Type = %q, want %q", r.Type, model.TypeEIP)
	}
	if _, ok := r.Attributes[model.AttrAssociatedWith]; ok {
		t.Errorf("associated_with = %q, want absent for an address holding nothing",
			r.Attr(model.AttrAssociatedWith))
	}
	// The address stands in for a missing Name tag so the row is identifiable.
	if r.Name != "203.0.113.1" {
		t.Errorf("Name = %q, want the address", r.Name)
	}
}

// DescribeAddresses reports no allocation time, and how long an address has sat
// idle is precisely the question a reader brings. A scan-time stand-in would
// answer it wrongly rather than leave it open.
func TestElasticIPHasNoInventedCreationTime(t *testing.T) {
	r := elasticIPResource(ec2types.Address{
		PublicIp:     aws.String("203.0.113.1"),
		AllocationId: aws.String("eipalloc-0abc123"),
	}, "203.0.113.1", "aws", "us-east-1", testAccount)

	if r.CreatedAt != nil {
		t.Errorf("CreatedAt = %v, want nil: AWS reports no allocation time", r.CreatedAt)
	}
}

// The exposure flag counts resources reachable from the internet. The instance
// holding this address already carries that; setting it here too would count
// one exposure twice and turn a risk metric into an address count.
func TestPublicIPLeavesExposureToTheResourceHoldingIt(t *testing.T) {
	eip := elasticIPResource(ec2types.Address{
		PublicIp:     aws.String("203.0.113.1"),
		AllocationId: aws.String("eipalloc-0abc123"),
		InstanceId:   aws.String("i-0000000000000aaaa"),
	}, "203.0.113.1", "aws", "us-east-1", testAccount)
	if eip.PubliclyAccessible != nil {
		t.Errorf("PubliclyAccessible = %v, want nil", *eip.PubliclyAccessible)
	}

	auto := interfaceAddressResource(
		ec2types.NetworkInterface{NetworkInterfaceId: aws.String("eni-0abc123")},
		ec2types.NetworkInterfaceAssociation{PublicIp: aws.String("203.0.113.2")},
		"203.0.113.2", "aws", "us-east-1", testAccount)
	if auto.PubliclyAccessible != nil {
		t.Errorf("PubliclyAccessible = %v, want nil", *auto.PubliclyAccessible)
	}
}

// The overlap between the two listings is the normal case, not an edge case: an
// associated Elastic IP is returned by both. One address is one charge and must
// be one row.
func TestMergeAddressesDeduplicatesTheOverlap(t *testing.T) {
	addresses := []ec2types.Address{{
		PublicIp:           aws.String("203.0.113.1"),
		AllocationId:       aws.String("eipalloc-0abc123"),
		NetworkInterfaceId: aws.String("eni-0abc123"),
		InstanceId:         aws.String("i-0000000000000aaaa"),
	}}
	interfaces := []ec2types.NetworkInterface{{
		NetworkInterfaceId: aws.String("eni-0abc123"),
		Association: &ec2types.NetworkInterfaceAssociation{
			PublicIp:     aws.String("203.0.113.1"),
			AllocationId: aws.String("eipalloc-0abc123"),
		},
	}}

	got := mergeAddresses(addresses, interfaces, "us-east-1", testAccount)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: the same address must not bill twice", len(got))
	}
	if got[0].Type != model.TypeEIP {
		t.Errorf("Type = %q, want %q from the allocation listing", got[0].Type, model.TypeEIP)
	}
}

// The undercount this scanner exists to prevent: an auto-assigned address never
// appears in DescribeAddresses, because there is no allocation behind it.
func TestMergeAddressesFindsAutoAssignedAddresses(t *testing.T) {
	interfaces := []ec2types.NetworkInterface{{
		NetworkInterfaceId: aws.String("eni-0abc123"),
		AvailabilityZone:   aws.String("us-east-1a"),
		VpcId:              aws.String("vpc-0abc"),
		SubnetId:           aws.String("subnet-0abc"),
		Attachment:         &ec2types.NetworkInterfaceAttachment{InstanceId: aws.String("i-0000000000000aaaa")},
		Association:        &ec2types.NetworkInterfaceAssociation{PublicIp: aws.String("203.0.113.7")},
	}}

	got := mergeAddresses(nil, interfaces, "us-east-1", testAccount)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	r := got[0]
	if r.Type != model.TypeNetworkInterface {
		t.Errorf("Type = %q, want %q: no allocation stands behind this address",
			r.Type, model.TypeNetworkInterface)
	}
	if want := NetworkInterfaceARN("aws", "us-east-1", testAccount, "eni-0abc123"); r.ARN != want {
		t.Errorf("ARN = %q, want %q", r.ARN, want)
	}
	if got := r.Attr(model.AttrAssociatedWith); got != "i-0000000000000aaaa" {
		t.Errorf("associated_with = %q, want the instance, which is more specific than the interface", got)
	}
	if got := r.Attr(model.AttrPublicIP); got != "203.0.113.7" {
		t.Errorf("public_ip = %q, want 203.0.113.7", got)
	}
}

// An interface can hold several public addresses — one per private address with
// an Elastic IP bound to it — and each is a separate charge.
func TestMergeAddressesCountsSecondaryAddresses(t *testing.T) {
	interfaces := []ec2types.NetworkInterface{{
		NetworkInterfaceId: aws.String("eni-0abc123"),
		Association:        &ec2types.NetworkInterfaceAssociation{PublicIp: aws.String("203.0.113.7")},
		PrivateIpAddresses: []ec2types.NetworkInterfacePrivateIpAddress{
			// The primary, repeated here by AWS — deduplicated by address.
			{Association: &ec2types.NetworkInterfaceAssociation{PublicIp: aws.String("203.0.113.7")}},
			{Association: &ec2types.NetworkInterfaceAssociation{
				PublicIp:     aws.String("203.0.113.8"),
				AllocationId: aws.String("eipalloc-0secondary"),
			}},
		},
	}}

	got := mergeAddresses(nil, interfaces, "us-east-1", testAccount)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: a secondary Elastic IP bills on its own", len(got))
	}
}

// Reaching an Elastic IP through the interface listing means DescribeAddresses
// did not return it — normally because it failed. It must still be written with
// the ARN it would otherwise have had, or the diff reports a delete and a
// create every time that listing recovers.
func TestElasticIPKeepsItsARNWhenOnlyTheInterfaceListingSawIt(t *testing.T) {
	interfaces := []ec2types.NetworkInterface{{
		NetworkInterfaceId: aws.String("eni-0abc123"),
		Association: &ec2types.NetworkInterfaceAssociation{
			PublicIp:     aws.String("203.0.113.1"),
			AllocationId: aws.String("eipalloc-0abc123"),
		},
	}}

	viaInterface := mergeAddresses(nil, interfaces, "us-east-1", testAccount)
	if len(viaInterface) != 1 {
		t.Fatalf("got %d rows, want 1", len(viaInterface))
	}

	viaAddresses := mergeAddresses([]ec2types.Address{{
		PublicIp:           aws.String("203.0.113.1"),
		AllocationId:       aws.String("eipalloc-0abc123"),
		NetworkInterfaceId: aws.String("eni-0abc123"),
	}}, nil, "us-east-1", testAccount)
	if len(viaAddresses) != 1 {
		t.Fatalf("got %d rows, want 1", len(viaAddresses))
	}

	if viaInterface[0].ARN != viaAddresses[0].ARN {
		t.Errorf("ARN differs by which listing found the address: %q vs %q",
			viaInterface[0].ARN, viaAddresses[0].ARN)
	}
	if viaInterface[0].Type != model.TypeEIP {
		t.Errorf("Type = %q, want %q", viaInterface[0].Type, model.TypeEIP)
	}
}

// A carrier or customer-owned IP is not a billable AWS public IPv4, and AWS
// says so by leaving PublicIp nil rather than by any flag this tool interprets.
func TestMergeAddressesSkipsAddressesWithoutAPublicIPv4(t *testing.T) {
	addresses := []ec2types.Address{{
		AllocationId: aws.String("eipalloc-0carrier"),
		CarrierIp:    aws.String("198.51.100.4"),
	}}
	interfaces := []ec2types.NetworkInterface{{
		NetworkInterfaceId: aws.String("eni-0private"),
		Association:        &ec2types.NetworkInterfaceAssociation{CarrierIp: aws.String("198.51.100.5")},
	}}

	if got := mergeAddresses(addresses, interfaces, "us-east-1", testAccount); len(got) != 0 {
		t.Errorf("got %d rows, want 0: neither address is a billable public IPv4", len(got))
	}
}

func TestAssociationHolderPrefersTheInstance(t *testing.T) {
	got := associationHolder(aws.String("i-0000000000000aaaa"), aws.String("eni-0abc123"))
	if got != "i-0000000000000aaaa" {
		t.Errorf("associationHolder = %q, want the instance", got)
	}
	if got := associationHolder(nil, aws.String("eni-0abc123")); got != "eni-0abc123" {
		t.Errorf("associationHolder = %q, want the interface", got)
	}
	if got := associationHolder(nil, nil); got != "" {
		t.Errorf("associationHolder = %q, want empty", got)
	}
}
