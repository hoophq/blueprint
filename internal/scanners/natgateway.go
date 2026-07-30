package scanners

import (
	"context"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// natGatewayScanner censuses NAT gateways.
//
// A NAT gateway is the archetypal silent line item: roughly $32 a month before
// a byte of data processing, and invisible in every console view organised
// around instances. Nothing about it changes when the traffic through it stops,
// so nothing about it draws attention when it should be deleted. The forgotten
// multiplier is that the classic deployment is one per availability zone, so a
// three-AZ VPC that nobody uses any more is three of them.
//
// One paginated call, tags inline, no follow-ups.
type natGatewayScanner struct{}

func init() { scan.Register(natGatewayScanner{}) }

func (natGatewayScanner) Service() string { return model.ServiceNATGateway }

func (natGatewayScanner) Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error) {
	client := ec2.NewFromConfig(cfg)
	var out []model.Resource

	pages := ec2.NewDescribeNatGatewaysPaginator(client, &ec2.DescribeNatGatewaysInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			// Partial results per the Scanner contract: the gateways already
			// paged in are real, and the runner ledgers the gap.
			return out, fmt.Errorf("describe nat gateways: %w", err)
		}
		for _, gw := range page.NatGateways {
			if gw.State == ec2types.NatGatewayStateDeleted {
				// AWS keeps deleted gateways in this listing for hours. They
				// bill nothing and own nothing, so counting them would inflate
				// the census and churn the diff twice — once when they appear
				// and once when AWS forgets them.
				//
				// "deleting" is deliberately not filtered: a gateway mid
				// deletion is still billing. Only the terminal state counts,
				// and a gateway reporting no state at all is kept, because
				// silence is not evidence of deletion.
				continue
			}
			out = append(out, natGatewayResource(gw, region, accountID))
		}
	}
	return out, nil
}

func natGatewayResource(gw ec2types.NatGateway, region, accountID string) model.Resource {
	id := aws.ToString(gw.NatGatewayId)
	tags := toTagMap(gw.Tags, func(t ec2types.Tag) (*string, *string) { return t.Key, t.Value })

	name := tags["Name"]
	if name == "" {
		name = id
	}

	r := model.Resource{
		ARN:       NATGatewayARN(partitionForRegion(region), region, accountID, id),
		Service:   model.ServiceNATGateway,
		Type:      model.TypeNATGateway,
		Name:      name,
		Status:    string(gw.State),
		Region:    region,
		AccountID: accountID,
		CreatedAt: gw.CreateTime,
		Tags:      tags,
		// PubliclyAccessible stays nil. A public NAT gateway holds a public IPv4
		// address, but it accepts no inbound connections — that is the entire
		// reason it exists — so answering true would file it alongside a
		// database open to the world in the exposure count. Encrypted stays nil
		// too: it moves packets and stores nothing, so there is no data at rest
		// to have an opinion about.
	}

	r.SetAttr(model.AttrConnectivityType, string(gw.ConnectivityType))
	r.SetAttr(model.AttrVPCID, aws.ToString(gw.VpcId))
	r.SetAttr(model.AttrSubnetID, aws.ToString(gw.SubnetId))
	// The public addresses this gateway holds. Each is separately billable as a
	// public IPv4 and appears again as its own row under ServicePublicIP;
	// recording it here is the join between the two, not a second count.
	r.SetAttr(model.AttrPublicIP, joinIDs(gw.NatGatewayAddresses,
		// Presence is the pointer: a private NAT gateway's address entry carries
		// a private IP and a nil public one, so it contributes nothing and the
		// attribute ends up absent. That is not "its address is unknown", it is
		// "it has no public address to bill".
		func(a ec2types.NatGatewayAddress) *string { return a.PublicIp }))

	// The gateway itself reports no availability zone — only its addresses do,
	// one per zone. A zonal gateway has exactly one; a regional gateway spans
	// several, which is the whole difference in what it bills.
	zones := natGatewayZones(gw.NatGatewayAddresses)
	if len(zones) > 0 {
		// Only on evidence. A count of zero here would read as "spans no zones",
		// which is false for every NAT gateway that exists; if AWS named no zone
		// on any address, the honest answer is that the key is absent.
		r.SetMeasure(model.MeasureAvailabilityZoneCount, int64(len(zones)))
		if len(zones) == 1 {
			// Singular, so it is only written when there is a single answer. A
			// regional gateway leaves it absent and lets the count speak rather
			// than picking one of its zones to stand for all of them.
			r.SetAttr(model.AttrAvailabilityZone, zones[0])
		}
	}

	// No data-processed measure. DescribeNatGateways reports none — the bytes a
	// gateway has moved are a CloudWatch question — and a zero here would read
	// as "nothing went through this", which is the one thing a reader would act
	// on.
	return r
}

// natGatewayZones returns the distinct availability zones a gateway's addresses
// sit in, sorted.
func natGatewayZones(addresses []ec2types.NatGatewayAddress) []string {
	zones := make([]string, 0, len(addresses))
	for _, a := range addresses {
		if a.AvailabilityZone == nil {
			continue
		}
		if az := *a.AvailabilityZone; az != "" {
			zones = append(zones, az)
		}
	}
	slices.Sort(zones)
	return slices.Compact(zones)
}

// NATGatewayARN builds a NAT gateway ARN: DescribeNatGateways does not return
// one. Exported so the demo fixture builds ARNs with the same shape.
func NATGatewayARN(partition, region, accountID, natGatewayID string) string {
	return fmt.Sprintf("arn:%s:ec2:%s:%s:natgateway/%s", partition, region, accountID, natGatewayID)
}
