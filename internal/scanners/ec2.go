package scanners

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// ec2Scanner censuses EC2 instances. One paginated DescribeInstances call
// covers everything: unlike RDS or DynamoDB, EC2 returns tags inline, so there
// is no per-resource follow-up and no tag-failure aggregation to do.
//
// What an instance runs is not this scanner's business. EC2 reports the shape
// of the box and the OS family it was billed as; anything about the software
// on it — that a name contains "postgres", that port 3306 is open — would be a
// guess wearing the costume of a finding, and the census does not make guesses
// (honesty guardrail: reported, never inferred).
type ec2Scanner struct{}

func init() { scan.Register(ec2Scanner{}) }

func (ec2Scanner) Service() string { return "ec2" }

func (ec2Scanner) Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error) {
	client := ec2.NewFromConfig(cfg)
	var out []model.Resource

	pages := ec2.NewDescribeInstancesPaginator(client, &ec2.DescribeInstancesInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			// Partial results per the Scanner contract: whatever paginated
			// successfully is real, and the runner ledgers the gap.
			return out, fmt.Errorf("describe instances: %w", err)
		}
		out = append(out, instancesFromReservations(page.Reservations, region, accountID)...)
	}
	return out, nil
}

// instancesFromReservations flattens a page of reservations into census rows.
// Reservations are a launch-request grouping, not a thing anyone operates, so
// they are unwrapped rather than recorded.
func instancesFromReservations(reservations []ec2types.Reservation, region, accountID string) []model.Resource {
	var out []model.Resource
	for _, res := range reservations {
		for _, inst := range res.Instances {
			if instanceTerminated(inst) {
				continue
			}
			out = append(out, ec2InstanceResource(inst, region, accountID))
		}
	}
	return out
}

// instanceTerminated reports whether an instance has been destroyed. AWS keeps
// terminated instances visible for about an hour after the fact; they own
// nothing and cost nothing, so counting them would inflate the census and
// churn the diff twice — once when they appear, once when AWS forgets them.
//
// The check is on the state name AWS returned, not on a code range: an
// instance whose state is missing entirely is kept, because "no state
// reported" is not evidence of termination.
func instanceTerminated(i ec2types.Instance) bool {
	return i.State != nil && i.State.Name == ec2types.InstanceStateNameTerminated
}

func ec2InstanceResource(i ec2types.Instance, region, accountID string) model.Resource {
	id := aws.ToString(i.InstanceId)
	tags := toTagMap(i.Tags, func(t ec2types.Tag) (*string, *string) { return t.Key, t.Value })

	status := ""
	if i.State != nil {
		status = string(i.State.Name)
	}
	// The Name tag is what the console shows and what operators call the box,
	// but it is optional and need not be unique, so the instance ID backs it
	// up. Falling back to the ID keeps every row identifiable without
	// inventing a name for an untagged instance.
	name := tags["Name"]
	if name == "" {
		name = id
	}

	r := model.Resource{
		ARN:       EC2InstanceARN(instancePartition(i, region), region, accountID, id),
		Service:   model.ServiceEC2,
		Type:      model.TypeEC2Instance,
		Name:      name,
		Status:    status,
		Region:    region,
		AccountID: accountID,
		// LaunchTime is the last time the instance was started, not the first
		// time it was created — stopping and starting a box moves it forward.
		// It is the only time EC2 reports, and it is the one the console shows,
		// so it is recorded as-is rather than dressed up as a creation date.
		CreatedAt: i.LaunchTime,
		Tags:      tags,
		// A public IPv4 address is an exposure signal, so it sets the flag
		// rather than becoming an endpoint. The claim is narrow and deliberate:
		// AWS reported a public IPv4 on this instance right now. It is not a
		// reachability verdict — security groups and NACLs still gate traffic —
		// and it does not cover IPv6 or a load balancer in front. A stopped
		// instance without an Elastic IP has released its address and honestly
		// reports false until it starts again.
		PubliclyAccessible: aws.Bool(i.PublicIpAddress != nil),
		// Encrypted stays nil on purpose. Encryption at rest is a property of
		// each attached volume, not of the instance, and the volumes are their
		// own census rows (they carry their own flag). Answering here would
		// mean collapsing several volumes into one verdict — "false" hiding an
		// encrypted volume, "true" hiding a plaintext one — so the instance
		// declines to answer a question it is not the right subject for.
	}

	r.SetAttr(model.AttrInstanceClass, string(i.InstanceType))
	// PlatformDetails is the OS family AWS billed the instance as ("Linux/UNIX",
	// "Windows", "Red Hat Enterprise Linux", …), recorded verbatim. The adjacent
	// Platform field is deliberately unused: it says "windows" and is empty for
	// everything else, so reading it would mean writing "linux" for a machine
	// AWS never called Linux.
	r.SetAttr(model.AttrPlatform, aws.ToString(i.PlatformDetails))
	// Private DNS only. A census records where a resource sits, not how to
	// reach it, and the public name is the exposure signal handled above.
	r.SetAttr(model.AttrEndpoint, aws.ToString(i.PrivateDnsName))
	if i.Placement != nil {
		r.SetAttr(model.AttrAvailabilityZone, aws.ToString(i.Placement.AvailabilityZone))
	}
	r.SetAttr(model.AttrVPCID, aws.ToString(i.VpcId))
	r.SetAttr(model.AttrSubnetID, aws.ToString(i.SubnetId))
	// The attachment relationship, and only that. Attached volumes are their
	// own rows in this census with their own sizes, so summing their bytes into
	// a size measure here would count the same storage twice estate-wide.
	r.SetAttr(model.AttrEBSVolumeIDs, attachedVolumeIDs(i.BlockDeviceMappings))
	// No backup-retention measure: EC2 has no retention setting to report.
	// Whether a box is backed up lives in AWS Backup or in a snapshot
	// lifecycle policy, neither of which DescribeInstances can see, so the key
	// stays absent rather than claiming zero days of retention.
	return r
}

// attachedVolumeIDs joins the EBS volume IDs attached to an instance, sorted
// so the value is stable across scans regardless of the order AWS lists the
// block devices in. Instance-store devices have no EBS block and no ID to
// record, so they are absent from this list — which is why it names EBS.
func attachedVolumeIDs(mappings []ec2types.InstanceBlockDeviceMapping) string {
	ids := make([]string, 0, len(mappings))
	for _, m := range mappings {
		if m.Ebs == nil {
			continue
		}
		if id := aws.ToString(m.Ebs.VolumeId); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// instancePartition determines which AWS partition an instance's ARN belongs
// to, preferring an ARN AWS itself returned over any reconstruction.
//
// DescribeInstances carries no ARN for the instance, but it may carry one for
// something attached to it, and those live in the same partition. When there
// is none — the common case for a plain instance — the region name is the only
// evidence left. See arn.go for why "aws" is not an acceptable default here.
func instancePartition(i ec2types.Instance, region string) string {
	if i.IamInstanceProfile != nil {
		if arn := aws.ToString(i.IamInstanceProfile.Arn); arn != "" {
			return arnPartition(arn)
		}
	}
	if arn := aws.ToString(i.OutpostArn); arn != "" {
		return arnPartition(arn)
	}
	return partitionForRegion(region)
}

// EC2InstanceARN builds an instance ARN: DescribeInstances does not return
// one. Exported so the demo fixture builds ARNs with the same shape.
//
// The census depends on this format being exactly right — the ARN is both the
// key the diff matches on and the key cost enrichment joins on, so a wrong
// shape makes every instance look new on every scan and silently unpriced.
func EC2InstanceARN(partition, region, accountID, instanceID string) string {
	return fmt.Sprintf("arn:%s:ec2:%s:%s:instance/%s", partition, region, accountID, instanceID)
}
