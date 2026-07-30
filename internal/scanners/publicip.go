package scanners

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// Status values for a public IPv4 row. AWS reports no status for an address, so
// these are this tool's words — but they are read off the same response that
// named the address, from the association fields AWS filled in beside it, not
// joined against a second listing that might have been cut short.
const (
	addressAssociated   = "associated"
	addressUnassociated = "unassociated"
)

// publicIPScanner censuses billable public IPv4 addresses.
//
// Since February 2024 AWS charges for every public IPv4 address it has given
// you — roughly $3.60 a month each, whether or not anything is using it. That
// turned a free-if-attached resource into a line item that scales with how
// messy the account is, and nothing in the console adds it up.
//
// # Why two calls
//
// DescribeAddresses alone undercounts, and does so silently. It returns
// Elastic IPs: addresses you explicitly allocated. It does not return the
// address an instance was auto-assigned at launch, because there is no
// allocation behind it — and in most accounts those are the majority. A census
// built on that one call would report a confident number that is too low,
// which is worse than reporting nothing.
//
// So the addresses are gathered from both sides: allocations from
// DescribeAddresses, and everything actually bound to a network interface from
// DescribeNetworkInterfaces. The two overlap — an associated EIP appears in
// both — and the overlap is removed by the address itself, which is unique
// across AWS at any moment.
//
// Both calls run even if one fails, and the errors are joined. When one is
// lost, the rows from the other are still real, the count is knowably
// incomplete, and the failure ledger is where that is said rather than left for
// the reader to infer from a suspiciously round number.
type publicIPScanner struct{}

func init() { scan.Register(publicIPScanner{}) }

func (publicIPScanner) Service() string { return model.ServicePublicIP }

func (publicIPScanner) Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error) {
	client := ec2.NewFromConfig(cfg)

	addresses, addressesErr := describeAddresses(ctx, client)
	interfaces, interfacesErr := describeNetworkInterfaces(ctx, client)

	return mergeAddresses(addresses, interfaces, region, accountID),
		errors.Join(addressesErr, interfacesErr)
}

// mergeAddresses folds the two listings into one row per billable address.
//
// The allocations come first so that an Elastic IP is described by the call
// that knows the most about it, and the interfaces then contribute only what
// that call could not have returned: the auto-assigned addresses.
func mergeAddresses(addresses []ec2types.Address, interfaces []ec2types.NetworkInterface,
	region, accountID string) []model.Resource {

	partition := partitionForRegion(region)
	out := make([]model.Resource, 0, len(addresses))

	// seen is keyed by the address, which is the billable unit and is globally
	// unique at any instant — so it is the only key on which "this is the same
	// charge" is a safe thing to say. Keying on the holder would not work: a
	// network interface can hold several addresses.
	seen := make(map[string]bool, len(addresses))
	for _, a := range addresses {
		ip := aws.ToString(a.PublicIp)
		if ip == "" {
			// A Wavelength carrier IP or an Outposts customer-owned IP, neither
			// of which is a billable AWS public IPv4. AWS reports those in their
			// own fields and leaves PublicIp nil, so this filter is reading its
			// answer rather than guessing at one.
			continue
		}
		seen[ip] = true
		out = append(out, elasticIPResource(a, ip, partition, region, accountID))
	}

	for _, eni := range interfaces {
		for _, assoc := range interfaceAssociations(eni) {
			ip := aws.ToString(assoc.PublicIp)
			if ip == "" || seen[ip] {
				continue
			}
			seen[ip] = true
			out = append(out, interfaceAddressResource(eni, assoc, ip, partition, region, accountID))
		}
	}
	return out
}

// describeAddresses lists the account's Elastic IPs. Unlike most EC2 listings
// this one is not paginated — AWS returns every address in a single response —
// so there is no partial-page case to carry.
func describeAddresses(ctx context.Context, client *ec2.Client) ([]ec2types.Address, error) {
	resp, err := client.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
	if err != nil {
		return nil, fmt.Errorf("describe addresses: %w", err)
	}
	return resp.Addresses, nil
}

func describeNetworkInterfaces(ctx context.Context, client *ec2.Client) ([]ec2types.NetworkInterface, error) {
	var out []ec2types.NetworkInterface
	pages := ec2.NewDescribeNetworkInterfacesPaginator(client, &ec2.DescribeNetworkInterfacesInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return out, fmt.Errorf("describe network interfaces: %w", err)
		}
		out = append(out, page.NetworkInterfaces...)
	}
	return out, nil
}

// interfaceAssociations returns every public-address association on an
// interface: the one on the primary private address, plus one per secondary
// private address that has an Elastic IP bound to it.
//
// Both sources are read because they are not the same set. The top-level
// Association is the primary address only, and an interface with five EIPs on
// secondary addresses is five separate charges. They overlap — the primary
// normally appears in both — and the caller drops duplicates by address.
func interfaceAssociations(eni ec2types.NetworkInterface) []ec2types.NetworkInterfaceAssociation {
	assocs := make([]ec2types.NetworkInterfaceAssociation, 0, len(eni.PrivateIpAddresses)+1)
	if eni.Association != nil {
		assocs = append(assocs, *eni.Association)
	}
	for _, p := range eni.PrivateIpAddresses {
		if p.Association != nil {
			assocs = append(assocs, *p.Association)
		}
	}
	return assocs
}

func elasticIPResource(a ec2types.Address, ip, partition, region, accountID string) model.Resource {
	tags := toTagMap(a.Tags, func(t ec2types.Tag) (*string, *string) { return t.Key, t.Value })
	name := tags["Name"]
	if name == "" {
		name = ip
	}

	// The allocation ID is the address's identity and what the ARN is built on.
	// Falling back to the address itself covers the EC2-Classic shape, where
	// there is no allocation — retired in 2022, but an ARN that collides or
	// changes between scans would break the diff match and the cost join
	// silently, and the fallback costs one line.
	id := aws.ToString(a.AllocationId)
	if id == "" {
		id = ip
	}

	// Read straight off this address's own fields, not joined against anything.
	holder := associationHolder(a.InstanceId, a.NetworkInterfaceId)
	status := addressUnassociated
	if holder != "" {
		status = addressAssociated
	}

	r := model.Resource{
		ARN:       ElasticIPARN(partition, region, accountID, id),
		Service:   model.ServicePublicIP,
		Type:      model.TypeEIP,
		Name:      name,
		Status:    status,
		Region:    region,
		AccountID: accountID,
		// No CreatedAt. DescribeAddresses reports no allocation time, and an
		// unassociated EIP's age is exactly what a reader wants here — so
		// inventing one from the scan time would answer the question wrongly
		// rather than leave it open.
		Tags: tags,
		// PubliclyAccessible stays nil, which reads oddly for a public address
		// until you see what the flag counts. It marks resources exposed to the
		// internet, and an instance that has a public IP is already flagged on
		// its own row. Setting it here would count the same exposure twice and
		// make a metric about risk into a metric about addresses.
	}

	r.SetAttr(model.AttrPublicIP, ip)
	r.SetAttr(model.AttrAssociatedWith, holder)
	r.SetAttr(model.AttrSubnetID, aws.ToString(a.SubnetId))
	return r
}

func interfaceAddressResource(eni ec2types.NetworkInterface, assoc ec2types.NetworkInterfaceAssociation,
	ip, partition, region, accountID string) model.Resource {

	tags := toTagMap(eni.TagSet, func(t ec2types.Tag) (*string, *string) { return t.Key, t.Value })
	name := tags["Name"]
	if name == "" {
		name = ip
	}

	var instanceID *string
	if eni.Attachment != nil {
		instanceID = eni.Attachment.InstanceId
	}

	// An address reached through an interface is by definition held by that
	// interface, so this is never empty and the row is never "unassociated" —
	// the unassociated case only exists for an allocation with nothing behind
	// it, which is what DescribeAddresses reports.
	holder := associationHolder(instanceID, eni.NetworkInterfaceId)

	// Which resource is holding the address decides the row's type and ARN, and
	// the association says so: an allocation ID means this is an Elastic IP.
	//
	// Reaching one here means DescribeAddresses did not return it — normally
	// because it failed, since an EIP that call listed was deduplicated away
	// above. Writing it as an Elastic IP anyway is what keeps the ARN identical
	// across that failure. Typing it by which call happened to find it would
	// give the same address two different ARNs on consecutive scans, and the
	// diff would report a delete and a create every time the listing recovered.
	//
	// Keying the other case on the interface is unambiguous: an auto-assigned
	// public IPv4 goes to the primary private address only, so an interface
	// contributes at most one row that is not an Elastic IP.
	arn := NetworkInterfaceARN(partition, region, accountID, aws.ToString(eni.NetworkInterfaceId))
	resourceType := model.TypeNetworkInterface
	if allocationID := aws.ToString(assoc.AllocationId); allocationID != "" {
		arn = ElasticIPARN(partition, region, accountID, allocationID)
		resourceType = model.TypeEIP
	}

	r := model.Resource{
		ARN:       arn,
		Service:   model.ServicePublicIP,
		Type:      resourceType,
		Name:      name,
		Status:    addressAssociated,
		Region:    region,
		AccountID: accountID,
		Tags:      tags,
	}

	r.SetAttr(model.AttrPublicIP, ip)
	r.SetAttr(model.AttrAssociatedWith, holder)
	r.SetAttr(model.AttrAvailabilityZone, aws.ToString(eni.AvailabilityZone))
	r.SetAttr(model.AttrVPCID, aws.ToString(eni.VpcId))
	r.SetAttr(model.AttrSubnetID, aws.ToString(eni.SubnetId))
	return r
}

// associationHolder names the most specific thing AWS said is using an address:
// the instance if there is one, otherwise the network interface. Empty when it
// named neither, which for an Elastic IP is the finding — an address nothing is
// attached to bills exactly what a working one does.
func associationHolder(instanceID, networkInterfaceID *string) string {
	if id := aws.ToString(instanceID); id != "" {
		return id
	}
	return aws.ToString(networkInterfaceID)
}

// ElasticIPARN builds an Elastic IP ARN, keyed on the allocation ID.
// Exported so the demo fixture builds ARNs with the same shape.
func ElasticIPARN(partition, region, accountID, allocationID string) string {
	return fmt.Sprintf("arn:%s:ec2:%s:%s:elastic-ip/%s", partition, region, accountID, allocationID)
}

// NetworkInterfaceARN builds a network interface ARN. Exported so the demo
// fixture builds ARNs with the same shape.
func NetworkInterfaceARN(partition, region, accountID, interfaceID string) string {
	return fmt.Sprintf("arn:%s:ec2:%s:%s:network-interface/%s", partition, region, accountID, interfaceID)
}
