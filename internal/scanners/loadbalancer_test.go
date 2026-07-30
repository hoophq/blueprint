package scanners

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing/types"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"github.com/hoophq/blueprint/internal/model"
)

// The v2 ARN AWS returns for an application load balancer, used as the join key
// against the region's target groups.
const testALBARN = "arn:aws:elasticloadbalancing:us-east-1:" + testAccount +
	":loadbalancer/app/web/0a1b2c3d4e5f6789"

// A classic ARN has no type segment before the name, which is exactly how the
// two generations are told apart in an artifact.
func TestClassicLoadBalancerARN(t *testing.T) {
	got := ClassicLoadBalancerARN("aws", "us-east-1", testAccount, "legacy-web")
	want := "arn:aws:elasticloadbalancing:us-east-1:" + testAccount + ":loadbalancer/legacy-web"
	if got != want {
		t.Errorf("ClassicLoadBalancerARN = %q, want %q", got, want)
	}
	got = ClassicLoadBalancerARN("aws-cn", "cn-north-1", testAccount, "legacy-web")
	want = "arn:aws-cn:elasticloadbalancing:cn-north-1:" + testAccount + ":loadbalancer/legacy-web"
	if got != want {
		t.Errorf("ClassicLoadBalancerARN = %q, want %q", got, want)
	}
}

func TestLoadBalancerV2Resource(t *testing.T) {
	created := time.Date(2022, 11, 4, 8, 0, 0, 0, time.UTC)
	lb := elbv2types.LoadBalancer{
		LoadBalancerArn:  aws.String(testALBARN),
		LoadBalancerName: aws.String("web"),
		DNSName:          aws.String("web-123.us-east-1.elb.amazonaws.com"),
		Scheme:           elbv2types.LoadBalancerSchemeEnumInternetFacing,
		Type:             elbv2types.LoadBalancerTypeEnumApplication,
		State:            &elbv2types.LoadBalancerState{Code: elbv2types.LoadBalancerStateEnumActive},
		VpcId:            aws.String("vpc-0abc"),
		CreatedTime:      &created,
		AvailabilityZones: []elbv2types.AvailabilityZone{
			{ZoneName: aws.String("us-east-1a")},
			{ZoneName: aws.String("us-east-1b")},
		},
	}
	tags := map[string]string{"env": "prod"}

	r := loadBalancerV2Resource(lb, "us-east-1", testAccount, tags,
		[]string{"arn:aws:elasticloadbalancing:us-east-1:" + testAccount + ":targetgroup/web/aaaa"}, true)

	if r.ARN != testALBARN {
		t.Errorf("ARN = %q, want AWS's own ARN verbatim", r.ARN)
	}
	if r.Service != model.ServiceELB {
		t.Errorf("Service = %q, want %q", r.Service, model.ServiceELB)
	}
	if r.Type != model.TypeLoadBalancerV2 {
		t.Errorf("Type = %q, want %q", r.Type, model.TypeLoadBalancerV2)
	}
	if r.Status != string(elbv2types.LoadBalancerStateEnumActive) {
		t.Errorf("Status = %q, want active", r.Status)
	}
	if r.CreatedAt == nil || !r.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", r.CreatedAt, created)
	}
	if r.Tags["env"] != "prod" {
		t.Errorf("Tags = %v, want the batched DescribeTags result", r.Tags)
	}
	if got := r.Attr(model.AttrLoadBalancerType); got != "application" {
		t.Errorf("load_balancer_type = %q, want application", got)
	}
	if got := r.Attr(model.AttrEndpoint); got != "web-123.us-east-1.elb.amazonaws.com" {
		t.Errorf("endpoint = %q, want the DNS name", got)
	}
	if got, ok := r.Measure(model.MeasureAvailabilityZoneCount); !ok || got != 2 {
		t.Errorf("availability_zone_count = %d (present %t), want 2", got, ok)
	}
	if got, ok := r.Measure(model.MeasureTargetGroupCount); !ok || got != 1 {
		t.Errorf("target_group_count = %d (present %t), want 1", got, ok)
	}
}

// Scheme is AWS's own answer to the question the exposure flag asks, so it is
// passed through — and an internal load balancer must not be counted as exposed
// merely for having been found.
func TestLoadBalancerSchemeDrivesExposure(t *testing.T) {
	cases := []struct {
		scheme string
		want   *bool
	}{
		{"internet-facing", aws.Bool(true)},
		{"internal", aws.Bool(false)},
		// An empty or unrecognised scheme leaves the flag unanswered rather than
		// defaulting a load balancer into or out of the exposure count.
		{"", nil},
		{"something-aws-added-later", nil},
	}
	for _, tc := range cases {
		got := schemePubliclyAccessible(tc.scheme)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("scheme %q: PubliclyAccessible = %v, want nil", tc.scheme, *got)
		case tc.want != nil && got == nil:
			t.Errorf("scheme %q: PubliclyAccessible = nil, want %v", tc.scheme, *tc.want)
		case tc.want != nil && got != nil && *got != *tc.want:
			t.Errorf("scheme %q: PubliclyAccessible = %v, want %v", tc.scheme, *got, *tc.want)
		}
	}
}

// TLS termination is a listener property and is about data in transit, which is
// a different question from the one the Encrypted field asks.
func TestLoadBalancerLeavesEncryptionUnanswered(t *testing.T) {
	r := loadBalancerV2Resource(elbv2types.LoadBalancer{
		LoadBalancerArn: aws.String(testALBARN),
		Scheme:          elbv2types.LoadBalancerSchemeEnumInternetFacing,
	}, "us-east-1", testAccount, nil, nil, true)

	if r.Encrypted != nil {
		t.Errorf("Encrypted = %v, want nil", *r.Encrypted)
	}
}

// A load balancer AWS reported no state for gets an empty status. "active" is
// right most of the time, which is exactly what makes guessing it dangerous.
func TestLoadBalancerV2StatusIsEmptyWhenAWSReportedNone(t *testing.T) {
	if got := loadBalancerV2Status(nil); got != "" {
		t.Errorf("loadBalancerV2Status(nil) = %q, want empty", got)
	}
	r := loadBalancerV2Resource(elbv2types.LoadBalancer{
		LoadBalancerArn: aws.String(testALBARN),
	}, "us-east-1", testAccount, nil, nil, true)
	if r.Status != "" {
		t.Errorf("Status = %q, want empty", r.Status)
	}
}

// The finding: a load balancer billing by the hour with nowhere to send
// traffic. A complete zero is a fact and must be stored.
func TestLoadBalancerV2StoresACompleteZeroTargetGroupCount(t *testing.T) {
	r := loadBalancerV2Resource(elbv2types.LoadBalancer{
		LoadBalancerArn: aws.String(testALBARN),
	}, "us-east-1", testAccount, nil, nil, true)

	got, ok := r.Measure(model.MeasureTargetGroupCount)
	if !ok {
		t.Fatal("target_group_count absent; a complete zero is the whole finding")
	}
	if got != 0 {
		t.Errorf("target_group_count = %d, want 0", got)
	}
	if _, ok := r.Attributes[model.AttrTargetGroupARNs]; ok {
		t.Errorf("target_group_arns = %q, want absent when there are none",
			r.Attr(model.AttrTargetGroupARNs))
	}
}

// And the inverse: a listing cut short by an API error produces zero for every
// load balancer in the region, which reads identically to the finding above.
// Neither key may be written on that evidence.
func TestLoadBalancerV2OmitsTargetGroupsWhenTheListingFailed(t *testing.T) {
	r := loadBalancerV2Resource(elbv2types.LoadBalancer{
		LoadBalancerArn: aws.String(testALBARN),
	}, "us-east-1", testAccount, nil, nil, false)

	if got, ok := r.Measure(model.MeasureTargetGroupCount); ok {
		t.Errorf("target_group_count = %d, want absent: an unknown count is not zero", got)
	}
	if _, ok := r.Attributes[model.AttrTargetGroupARNs]; ok {
		t.Errorf("target_group_arns = %q, want absent", r.Attr(model.AttrTargetGroupARNs))
	}
}

// Counting targets on a v2 load balancer means one request per target group,
// which this scanner deliberately does not make. The key stays absent rather
// than carrying a number nothing measured.
func TestLoadBalancerV2ReportsNoRegisteredInstanceCount(t *testing.T) {
	r := loadBalancerV2Resource(elbv2types.LoadBalancer{
		LoadBalancerArn: aws.String(testALBARN),
	}, "us-east-1", testAccount, nil, nil, true)

	if got, ok := r.Measure(model.MeasureRegisteredInstanceCount); ok {
		t.Errorf("registered_instance_count = %d, want absent: DescribeTargetHealth is not called", got)
	}
}

func TestClassicLoadBalancerResource(t *testing.T) {
	created := time.Date(2016, 2, 9, 14, 0, 0, 0, time.UTC)
	lb := elbtypes.LoadBalancerDescription{
		LoadBalancerName:  aws.String("legacy-web"),
		DNSName:           aws.String("legacy-web-123.us-east-1.elb.amazonaws.com"),
		Scheme:            aws.String("internal"),
		VPCId:             aws.String("vpc-0abc"),
		CreatedTime:       &created,
		AvailabilityZones: []string{"us-east-1a"},
		Instances: []elbtypes.Instance{
			{InstanceId: aws.String("i-0000000000000aaaa")},
			{InstanceId: aws.String("i-0000000000000bbbb")},
		},
	}

	r := classicLoadBalancerResource(lb, "us-east-1", testAccount, map[string]string{"owner": "platform"})

	want := ClassicLoadBalancerARN("aws", "us-east-1", testAccount, "legacy-web")
	if r.ARN != want {
		t.Errorf("ARN = %q, want %q", r.ARN, want)
	}
	if r.Type != model.TypeLoadBalancer {
		t.Errorf("Type = %q, want %q", r.Type, model.TypeLoadBalancer)
	}
	if got := r.Attr(model.AttrLoadBalancerType); got != classicLoadBalancerType {
		t.Errorf("load_balancer_type = %q, want %q", got, classicLoadBalancerType)
	}
	if r.PubliclyAccessible == nil || *r.PubliclyAccessible {
		t.Errorf("PubliclyAccessible = %v, want false for an internal load balancer", r.PubliclyAccessible)
	}
	if got, ok := r.Measure(model.MeasureRegisteredInstanceCount); !ok || got != 2 {
		t.Errorf("registered_instance_count = %d (present %t), want 2", got, ok)
	}
	if r.Tags["owner"] != "platform" {
		t.Errorf("Tags = %v, want the batched DescribeTags result", r.Tags)
	}
	// The v1 API has no state field, so there is nothing honest to put here.
	if r.Status != "" {
		t.Errorf("Status = %q, want empty: the classic API reports no state", r.Status)
	}
}

// Registered instances arrive in the same response that named the load
// balancer, so the count is always complete — and zero means a classic load
// balancer still billing with nothing behind it.
func TestClassicLoadBalancerStoresZeroRegisteredInstances(t *testing.T) {
	r := classicLoadBalancerResource(elbtypes.LoadBalancerDescription{
		LoadBalancerName: aws.String("abandoned"),
	}, "us-east-1", testAccount, nil)

	got, ok := r.Measure(model.MeasureRegisteredInstanceCount)
	if !ok {
		t.Fatal("registered_instance_count absent; zero is the finding, not an absence")
	}
	if got != 0 {
		t.Errorf("registered_instance_count = %d, want 0", got)
	}
}

// Every load balancer is in at least one zone, so zero zones is AWS having
// listed none rather than a fact about the deployment.
func TestLoadBalancerOmitsAZCountWhenNoneWereListed(t *testing.T) {
	v2 := loadBalancerV2Resource(elbv2types.LoadBalancer{
		LoadBalancerArn: aws.String(testALBARN),
	}, "us-east-1", testAccount, nil, nil, true)
	if got, ok := v2.Measure(model.MeasureAvailabilityZoneCount); ok {
		t.Errorf("availability_zone_count = %d, want absent", got)
	}

	classic := classicLoadBalancerResource(elbtypes.LoadBalancerDescription{
		LoadBalancerName: aws.String("legacy-web"),
	}, "us-east-1", testAccount, nil)
	if got, ok := classic.Measure(model.MeasureAvailabilityZoneCount); ok {
		t.Errorf("availability_zone_count = %d, want absent", got)
	}
}

// Target groups name their load balancers, so the region-wide listing is
// inverted once rather than re-fetched per load balancer.
func TestTargetGroupsByLoadBalancer(t *testing.T) {
	const otherARN = "arn:aws:elasticloadbalancing:us-east-1:" + testAccount +
		":loadbalancer/net/api/1234567890abcdef"
	tgPrefix := "arn:aws:elasticloadbalancing:us-east-1:" + testAccount + ":targetgroup/"

	groups := []elbv2types.TargetGroup{
		{TargetGroupArn: aws.String(tgPrefix + "b"), LoadBalancerArns: []string{testALBARN}},
		{TargetGroupArn: aws.String(tgPrefix + "a"), LoadBalancerArns: []string{testALBARN, otherARN}},
		// A target group attached to nothing is an orphan of a different kind,
		// and must not be bucketed under the empty string.
		{TargetGroupArn: aws.String(tgPrefix + "orphan")},
		// Nor may an unnamed group contribute a hole to anyone's list.
		{LoadBalancerArns: []string{testALBARN}},
	}

	byLB := targetGroupsByLoadBalancer(groups)
	if len(byLB) != 2 {
		t.Fatalf("got %d load balancers, want 2: the orphan must not appear", len(byLB))
	}
	// Sorted, because AWS promises no order and the artifact has to be stable.
	got := byLB[testALBARN]
	if len(got) != 2 || got[0] != tgPrefix+"a" || got[1] != tgPrefix+"b" {
		t.Errorf("target groups = %v, want [%sa %sb]", got, tgPrefix, tgPrefix)
	}
	if len(byLB[otherARN]) != 1 {
		t.Errorf("target groups for the network load balancer = %v, want one", byLB[otherARN])
	}
	if _, ok := byLB[""]; ok {
		t.Error("empty load balancer ARN key present")
	}
}
