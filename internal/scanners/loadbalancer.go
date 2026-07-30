package scanners

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing/types"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// describeTagsBatch is the number of load balancers AWS accepts per DescribeTags
// call, on both the v1 and v2 APIs.
const describeTagsBatch = 20

// classicLoadBalancerType is the value written to load_balancer_type for a
// Classic Load Balancer. The v1 API reports no type — there was only ever one —
// so unlike "application" and "network" this string is this tool's, chosen so
// the attribute can be grouped on without a special case for the empty value.
const classicLoadBalancerType = "classic"

// loadBalancerScanner censuses load balancers on both ELB APIs.
//
// A load balancer bills by the hour from the moment it exists. One left behind
// a decommissioned service keeps charging at the same rate as one serving
// production, and the console page it appears on is sorted by name, not by
// whether anything is behind it.
//
// # What "idle" is allowed to mean here
//
// The structural question — how many places can this thing send traffic — is
// answerable from describe calls, and is recorded: target_group_count for the
// v2 APIs, registered_instance_count for classic. Zero is the finding.
//
// The behavioural question — is anybody calling it — is not answerable here.
// That lives in CloudWatch, and a load balancer with healthy targets and no
// requests for a month is idle while one with no targets may have been drained
// thirty seconds ago. So the counts are reported as counts, and the word
// "idle" does not appear in the output.
//
// Per-target health (DescribeTargetHealth) is deliberately not called: it is
// one request per target group, which is the N+1 this scanner is shaped to
// avoid. Target groups themselves are listed region-wide in a single paginated
// chain and matched to their load balancers client-side.
type loadBalancerScanner struct{}

func init() { scan.Register(loadBalancerScanner{}) }

func (loadBalancerScanner) Service() string { return model.ServiceELB }

func (loadBalancerScanner) Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error) {
	v2Client := elbv2.NewFromConfig(cfg)
	v1Client := elb.NewFromConfig(cfg)

	// Every listing runs regardless of what failed before it. The two APIs
	// describe disjoint sets of load balancers, so losing one must not cost the
	// reader the other, and the target group listing only decides which
	// attributes a v2 row can carry.
	v2LBs, v2Err := describeLoadBalancersV2(ctx, v2Client)
	targetGroups, targetGroupsErr := describeTargetGroups(ctx, v2Client)
	classicLBs, classicErr := describeClassicLoadBalancers(ctx, v1Client)

	v2Tags, v2TagsErr := describeLoadBalancerTagsV2(ctx, v2Client, v2LBs)
	classicTags, classicTagsErr := describeClassicLoadBalancerTags(ctx, v1Client, classicLBs)

	// Target groups name the load balancers they are attached to, not the other
	// way round, so the region-wide listing is inverted once here rather than
	// re-scanned per load balancer.
	byLoadBalancer := targetGroupsByLoadBalancer(targetGroups)

	out := make([]model.Resource, 0, len(v2LBs)+len(classicLBs))
	for _, lb := range v2LBs {
		arn := aws.ToString(lb.LoadBalancerArn)
		out = append(out, loadBalancerV2Resource(lb, region, accountID,
			v2Tags[arn], byLoadBalancer[arn], targetGroupsErr == nil))
	}
	for _, lb := range classicLBs {
		name := aws.ToString(lb.LoadBalancerName)
		out = append(out, classicLoadBalancerResource(lb, region, accountID, classicTags[name]))
	}

	return out, errors.Join(v2Err, targetGroupsErr, classicErr, v2TagsErr, classicTagsErr)
}

func describeLoadBalancersV2(ctx context.Context, client *elbv2.Client) ([]elbv2types.LoadBalancer, error) {
	var out []elbv2types.LoadBalancer
	pages := elbv2.NewDescribeLoadBalancersPaginator(client, &elbv2.DescribeLoadBalancersInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return out, fmt.Errorf("describe load balancers: %w", err)
		}
		out = append(out, page.LoadBalancers...)
	}
	return out, nil
}

// describeTargetGroups lists every target group in the region in one paginated
// chain. Asking per load balancer would be the same data at one request each;
// the response already carries LoadBalancerArns, so the association is a local
// join rather than a reason to call the API again.
func describeTargetGroups(ctx context.Context, client *elbv2.Client) ([]elbv2types.TargetGroup, error) {
	var out []elbv2types.TargetGroup
	pages := elbv2.NewDescribeTargetGroupsPaginator(client, &elbv2.DescribeTargetGroupsInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return out, fmt.Errorf("describe target groups: %w", err)
		}
		out = append(out, page.TargetGroups...)
	}
	return out, nil
}

func describeClassicLoadBalancers(ctx context.Context, client *elb.Client) ([]elbtypes.LoadBalancerDescription, error) {
	var out []elbtypes.LoadBalancerDescription
	pages := elb.NewDescribeLoadBalancersPaginator(client, &elb.DescribeLoadBalancersInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return out, fmt.Errorf("describe classic load balancers: %w", err)
		}
		out = append(out, page.LoadBalancerDescriptions...)
	}
	return out, nil
}

// targetGroupsByLoadBalancer inverts the region's target groups into load
// balancer ARN -> the target group ARNs pointed at it.
//
// A target group with no load balancer is skipped rather than bucketed under
// the empty string: it is an orphan of a different kind and not what this
// scanner reports on.
func targetGroupsByLoadBalancer(groups []elbv2types.TargetGroup) map[string][]string {
	byLB := make(map[string][]string)
	for _, tg := range groups {
		arn := aws.ToString(tg.TargetGroupArn)
		if arn == "" {
			continue
		}
		for _, lbARN := range tg.LoadBalancerArns {
			if lbARN == "" {
				continue
			}
			byLB[lbARN] = append(byLB[lbARN], arn)
		}
	}
	for lbARN, arns := range byLB {
		slices.Sort(arns)
		byLB[lbARN] = slices.Compact(arns)
	}
	return byLB
}

// describeLoadBalancerTagsV2 fetches tags for the v2 load balancers, twenty per
// call — the API's own limit, and the reason this is a handful of requests
// rather than one per load balancer.
//
// Tags are worth the extra calls because environment and owner are imported
// from them and from nothing else. Silently leaving them nil would not just
// lose two fields, it would count every load balancer as untagged and quietly
// worsen the tag-hygiene numbers the report leads with. A failure here is
// therefore summarised into the ledger rather than swallowed, and whatever
// batches did succeed are kept.
func describeLoadBalancerTagsV2(ctx context.Context, client *elbv2.Client,
	lbs []elbv2types.LoadBalancer) (map[string]map[string]string, error) {

	arns := make([]string, 0, len(lbs))
	for _, lb := range lbs {
		if arn := aws.ToString(lb.LoadBalancerArn); arn != "" {
			arns = append(arns, arn)
		}
	}

	byARN := make(map[string]map[string]string, len(arns))
	var failures tagFailures
	for batch := range slices.Chunk(arns, describeTagsBatch) {
		resp, err := client.DescribeTags(ctx, &elbv2.DescribeTagsInput{ResourceArns: batch})
		failures.record(err)
		if err != nil {
			continue
		}
		for _, d := range resp.TagDescriptions {
			arn := aws.ToString(d.ResourceArn)
			if arn == "" {
				continue
			}
			byARN[arn] = toTagMap(d.Tags, func(t elbv2types.Tag) (*string, *string) { return t.Key, t.Value })
		}
	}
	return byARN, failures.err()
}

// describeClassicLoadBalancerTags is the v1 equivalent, keyed by name because
// the classic API has no ARNs.
func describeClassicLoadBalancerTags(ctx context.Context, client *elb.Client,
	lbs []elbtypes.LoadBalancerDescription) (map[string]map[string]string, error) {

	names := make([]string, 0, len(lbs))
	for _, lb := range lbs {
		if name := aws.ToString(lb.LoadBalancerName); name != "" {
			names = append(names, name)
		}
	}

	byName := make(map[string]map[string]string, len(names))
	var failures tagFailures
	for batch := range slices.Chunk(names, describeTagsBatch) {
		resp, err := client.DescribeTags(ctx, &elb.DescribeTagsInput{LoadBalancerNames: batch})
		failures.record(err)
		if err != nil {
			continue
		}
		for _, d := range resp.TagDescriptions {
			name := aws.ToString(d.LoadBalancerName)
			if name == "" {
				continue
			}
			byName[name] = toTagMap(d.Tags, func(t elbtypes.Tag) (*string, *string) { return t.Key, t.Value })
		}
	}
	return byName, failures.err()
}

func loadBalancerV2Resource(lb elbv2types.LoadBalancer, region, accountID string,
	tags map[string]string, targetGroupARNs []string, targetGroupsComplete bool) model.Resource {

	arn := aws.ToString(lb.LoadBalancerArn)
	scheme := string(lb.Scheme)

	r := model.Resource{
		// AWS returns the ARN here, so it is used verbatim — including its
		// partition, which is a better answer than anything reconstructed from
		// the region name.
		ARN:     arn,
		Service: model.ServiceELB,
		Type:    model.TypeLoadBalancerV2,
		Name:    aws.ToString(lb.LoadBalancerName),
		// State is a pointer and its Code is an enum; a load balancer AWS
		// reported no state for gets an empty status rather than an invented
		// "active".
		Status:    loadBalancerV2Status(lb.State),
		Region:    region,
		AccountID: accountID,
		CreatedAt: lb.CreatedTime,
		Tags:      tags,
		// Scheme is AWS's own answer to exactly the question this flag asks, so
		// it is passed through rather than declined. Encrypted stays nil: TLS
		// termination is a listener property and is about data in transit, which
		// is a different question from the one this field asks.
		PubliclyAccessible: schemePubliclyAccessible(scheme),
	}

	r.SetAttr(model.AttrScheme, scheme)
	r.SetAttr(model.AttrLoadBalancerType, string(lb.Type))
	r.SetAttr(model.AttrVPCID, aws.ToString(lb.VpcId))
	r.SetAttr(model.AttrEndpoint, aws.ToString(lb.DNSName))
	setAvailabilityZoneCount(&r, len(lb.AvailabilityZones))

	// Both keys are gated on the enumeration having finished, and for the same
	// reason: zero target groups is a delete signal, and a listing cut short by
	// an API error produces exactly that number for every load balancer in the
	// region. A complete zero is stored and must survive to the page; an
	// unknown one leaves both keys absent, and the ledger says why.
	if targetGroupsComplete {
		r.SetAttr(model.AttrTargetGroupARNs, joinIDs(targetGroupARNs, func(s string) *string { return &s }))
		r.SetMeasure(model.MeasureTargetGroupCount, int64(len(targetGroupARNs)))
	}
	// No registered_instance_count. Counting targets means DescribeTargetHealth
	// once per target group; the key stays absent rather than being filled with
	// a number this scanner did not measure.
	return r
}

func classicLoadBalancerResource(lb elbtypes.LoadBalancerDescription, region, accountID string,
	tags map[string]string) model.Resource {

	name := aws.ToString(lb.LoadBalancerName)
	scheme := aws.ToString(lb.Scheme)

	r := model.Resource{
		ARN:     ClassicLoadBalancerARN(partitionForRegion(region), region, accountID, name),
		Service: model.ServiceELB,
		Type:    model.TypeLoadBalancer,
		Name:    name,
		// The v1 API reports no state field at all, so there is nothing to put
		// here. Empty is the honest answer; "active" would be a guess that
		// happens to be right most of the time.
		Status:             "",
		Region:             region,
		AccountID:          accountID,
		CreatedAt:          lb.CreatedTime,
		Tags:               tags,
		PubliclyAccessible: schemePubliclyAccessible(scheme),
	}

	r.SetAttr(model.AttrScheme, scheme)
	r.SetAttr(model.AttrLoadBalancerType, classicLoadBalancerType)
	r.SetAttr(model.AttrVPCID, aws.ToString(lb.VPCId))
	r.SetAttr(model.AttrEndpoint, aws.ToString(lb.DNSName))
	setAvailabilityZoneCount(&r, len(lb.AvailabilityZones))
	// Registered instances come back inline on the classic API, so the count is
	// free and, unlike the v2 case, always complete — it was in the same
	// response that named the load balancer. Zero is stored, and zero is a
	// classic load balancer still billing with nothing behind it.
	r.SetMeasure(model.MeasureRegisteredInstanceCount, int64(len(lb.Instances)))
	return r
}

// loadBalancerV2Status reads the state code AWS reported, or empty if it
// reported no state. Presence is the pointer, as everywhere.
func loadBalancerV2Status(state *elbv2types.LoadBalancerState) string {
	if state == nil {
		return ""
	}
	return string(state.Code)
}

// schemePubliclyAccessible turns a load balancer's scheme into the exposure
// flag. Only AWS's two documented values are answers; anything else — an empty
// scheme, or a value added after this was written — leaves the flag nil rather
// than defaulting a resource into or out of the exposure count on a string this
// tool did not recognise.
func schemePubliclyAccessible(scheme string) *bool {
	switch scheme {
	case "internet-facing":
		return aws.Bool(true)
	case "internal":
		return aws.Bool(false)
	default:
		return nil
	}
}

// setAvailabilityZoneCount records how many zones a load balancer spans. Zero
// means AWS listed no zones, which is not a fact about the deployment — every
// load balancer is in at least one — so the key is left absent instead.
func setAvailabilityZoneCount(r *model.Resource, zones int) {
	if zones == 0 {
		return
	}
	r.SetMeasure(model.MeasureAvailabilityZoneCount, int64(zones))
}

// ClassicLoadBalancerARN builds a Classic Load Balancer ARN: the v1 API returns
// only a name. Unlike a v2 ARN it has no type segment before the name, which is
// how the two are told apart:
//
//	arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/legacy-web
//	arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/web/0a1b2c3d
//
// Exported so the demo fixture builds ARNs with the same shape.
func ClassicLoadBalancerARN(partition, region, accountID, name string) string {
	return fmt.Sprintf("arn:%s:elasticloadbalancing:%s:%s:loadbalancer/%s", partition, region, accountID, name)
}
