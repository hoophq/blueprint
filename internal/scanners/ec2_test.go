package scanners

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/hoophq/blueprint/internal/model"
)

const testAccount = "123456789012"

func TestEC2InstanceARN(t *testing.T) {
	// The separator is a slash, not a colon: EC2 instance ARNs are
	// arn:…:instance/i-abc, and the diff and cost join both key on this exact
	// string, so the shape is pinned rather than eyeballed.
	got := EC2InstanceARN("aws", "us-east-1", testAccount, "i-0abc123")
	want := "arn:aws:ec2:us-east-1:" + testAccount + ":instance/i-0abc123"
	if got != want {
		t.Errorf("EC2InstanceARN = %q, want %q", got, want)
	}
	got = EC2InstanceARN("aws-us-gov", "us-gov-west-1", testAccount, "i-0abc123")
	want = "arn:aws-us-gov:ec2:us-gov-west-1:" + testAccount + ":instance/i-0abc123"
	if got != want {
		t.Errorf("EC2InstanceARN = %q, want %q", got, want)
	}
}

func TestInstancePartition(t *testing.T) {
	// An ARN AWS actually returned beats the region rule, even when the two
	// disagree — AWS's answer is evidence, the region rule is reconstruction.
	withProfile := ec2types.Instance{
		IamInstanceProfile: &ec2types.IamInstanceProfile{
			Arn: aws.String("arn:aws-us-gov:iam::" + testAccount + ":instance-profile/app"),
		},
	}
	if got := instancePartition(withProfile, "us-east-1"); got != "aws-us-gov" {
		t.Errorf("profile ARN partition = %q, want aws-us-gov", got)
	}

	withOutpost := ec2types.Instance{
		OutpostArn: aws.String("arn:aws-cn:outposts:cn-north-1:" + testAccount + ":outpost/op-1"),
	}
	if got := instancePartition(withOutpost, "cn-north-1"); got != "aws-cn" {
		t.Errorf("outpost ARN partition = %q, want aws-cn", got)
	}

	// The common case: no ARN anywhere in the response, so the region decides.
	// Defaulting to "aws" here would mis-key every GovCloud instance.
	if got := instancePartition(ec2types.Instance{}, "us-gov-west-1"); got != "aws-us-gov" {
		t.Errorf("bare instance in GovCloud = %q, want aws-us-gov", got)
	}
	if got := instancePartition(ec2types.Instance{}, "us-east-1"); got != "aws" {
		t.Errorf("bare instance in commercial = %q, want aws", got)
	}
	// Presence and usability are separate questions, and the answers differ.
	//
	// Presence is the pointer: a nil ARN was never reported. Usability is the
	// parse: a string AWS did report, but that does not name a partition, is
	// equally no answer. Both must lose to the region rule, because the
	// alternative is reading a bare "aws" out of a non-ARN and mis-keying every
	// instance in a non-commercial partition — a failure with no error attached
	// to it. Each of these would read "aws" if presence alone decided.
	unusable := map[string]ec2types.Instance{
		"nil profile ARN":   {IamInstanceProfile: &ec2types.IamInstanceProfile{}},
		"empty profile ARN": {IamInstanceProfile: &ec2types.IamInstanceProfile{Arn: aws.String("")}},
		"non-ARN profile":   {IamInstanceProfile: &ec2types.IamInstanceProfile{Arn: aws.String("instance-profile/app")}},
		"partitionless ARN": {IamInstanceProfile: &ec2types.IamInstanceProfile{Arn: aws.String("arn::iam::x:y")}},
		"empty outpost ARN": {OutpostArn: aws.String("")},
		"non-ARN outpost":   {OutpostArn: aws.String("op-1")},
	}
	for name, inst := range unusable {
		if got := instancePartition(inst, "us-gov-west-1"); got != "aws-us-gov" {
			t.Errorf("%s in GovCloud = %q, want aws-us-gov from the region rule", name, got)
		}
	}
}

func TestEC2InstanceResource(t *testing.T) {
	launched := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	i := ec2types.Instance{
		InstanceId:      aws.String("i-0abc123"),
		InstanceType:    ec2types.InstanceTypeM5Large,
		PlatformDetails: aws.String("Red Hat Enterprise Linux"),
		PrivateDnsName:  aws.String("ip-10-0-1-5.ec2.internal"),
		PublicIpAddress: aws.String("203.0.113.10"),
		LaunchTime:      aws.Time(launched),
		State:           &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Placement:       &ec2types.Placement{AvailabilityZone: aws.String("us-east-1a")},
		VpcId:           aws.String("vpc-01"),
		SubnetId:        aws.String("subnet-01"),
		BlockDeviceMappings: []ec2types.InstanceBlockDeviceMapping{
			{Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String("vol-zzz")}},
			{Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String("vol-aaa")}},
		},
		Tags: []ec2types.Tag{
			{Key: aws.String("Name"), Value: aws.String("api-worker")},
			{Key: aws.String("env"), Value: aws.String("prod")},
		},
	}

	r := ec2InstanceResource(i, "us-east-1", testAccount)

	if want := "arn:aws:ec2:us-east-1:" + testAccount + ":instance/i-0abc123"; r.ARN != want {
		t.Errorf("ARN = %q, want %q", r.ARN, want)
	}
	if r.Service != model.ServiceEC2 || r.Type != model.TypeEC2Instance {
		t.Errorf("unexpected service/type: %q/%q", r.Service, r.Type)
	}
	if r.Name != "api-worker" {
		t.Errorf("Name = %q, want api-worker", r.Name)
	}
	if r.Status != "running" {
		t.Errorf("Status = %q, want running", r.Status)
	}
	if r.CreatedAt == nil || !r.CreatedAt.Equal(launched) {
		t.Errorf("CreatedAt = %v, want %v", r.CreatedAt, launched)
	}
	if r.Tags["env"] != "prod" {
		t.Errorf("tags not imported: %v", r.Tags)
	}
	if got := r.Attr(model.AttrInstanceClass); got != "m5.large" {
		t.Errorf("instance_class = %q, want m5.large", got)
	}
	if got := r.Attr(model.AttrPlatform); got != "Red Hat Enterprise Linux" {
		t.Errorf("platform = %q, want the verbatim PlatformDetails string", got)
	}
	if got := r.Attr(model.AttrEndpoint); got != "ip-10-0-1-5.ec2.internal" {
		t.Errorf("endpoint = %q, want the private DNS name", got)
	}
	if got := r.Attr(model.AttrAvailabilityZone); got != "us-east-1a" {
		t.Errorf("availability_zone = %q, want us-east-1a", got)
	}
	if got := r.Attr(model.AttrVPCID); got != "vpc-01" {
		t.Errorf("vpc_id = %q, want vpc-01", got)
	}
	if got := r.Attr(model.AttrSubnetID); got != "subnet-01" {
		t.Errorf("subnet_id = %q, want subnet-01", got)
	}
	// Sorted, not in the order AWS listed them, so the value does not churn
	// the diff when AWS reorders the block devices.
	if got := r.Attr(model.AttrEBSVolumeIDs); got != "vol-aaa,vol-zzz" {
		t.Errorf("ebs_volume_ids = %q, want vol-aaa,vol-zzz", got)
	}
	if r.PubliclyAccessible == nil || !*r.PubliclyAccessible {
		t.Errorf("PubliclyAccessible = %v, want true for an instance with a public IPv4", r.PubliclyAccessible)
	}
}

// The public DNS name is never recorded as the endpoint: it is the exposure
// signal, and the flag is where exposure is reported.
func TestEC2InstanceEndpointIsPrivateDNSOnly(t *testing.T) {
	i := ec2types.Instance{
		InstanceId:      aws.String("i-1"),
		PrivateDnsName:  aws.String("ip-10-0-1-5.ec2.internal"),
		PublicDnsName:   aws.String("ec2-203-0-113-10.compute-1.amazonaws.com"),
		PublicIpAddress: aws.String("203.0.113.10"),
	}
	r := ec2InstanceResource(i, "us-east-1", testAccount)
	if got := r.Attr(model.AttrEndpoint); got != "ip-10-0-1-5.ec2.internal" {
		t.Errorf("endpoint = %q, want the private DNS name", got)
	}
	for k, v := range r.Attributes {
		if v == "ec2-203-0-113-10.compute-1.amazonaws.com" {
			t.Errorf("public DNS leaked into attribute %q", k)
		}
	}
}

func TestEC2InstanceNameFallsBackToInstanceID(t *testing.T) {
	i := ec2types.Instance{InstanceId: aws.String("i-0nameless")}
	if got := ec2InstanceResource(i, "us-east-1", testAccount).Name; got != "i-0nameless" {
		t.Errorf("Name = %q, want the instance ID", got)
	}
	// An empty Name tag is not a name either.
	i.Tags = []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("")}}
	if got := ec2InstanceResource(i, "us-east-1", testAccount).Name; got != "i-0nameless" {
		t.Errorf("Name = %q, want the instance ID for an empty Name tag", got)
	}
}

func TestEC2InstanceWithoutPublicIPIsNotExposed(t *testing.T) {
	i := ec2types.Instance{InstanceId: aws.String("i-1")}
	r := ec2InstanceResource(i, "us-east-1", testAccount)
	// False, not nil: DescribeInstances is authoritative about whether the
	// instance holds a public IPv4 right now, so "no address" is a reported
	// fact rather than a gap in what AWS told us.
	if r.PubliclyAccessible == nil {
		t.Fatal("PubliclyAccessible = nil, want a reported false")
	}
	if *r.PubliclyAccessible {
		t.Error("PubliclyAccessible = true for an instance with no public IPv4")
	}
}

// Everything EC2 does not report must stay key-absent rather than render as a
// zero or an empty string, and nothing may be inferred to fill the gap.
func TestEC2InstanceUnreportedFieldsStayAbsent(t *testing.T) {
	r := ec2InstanceResource(ec2types.Instance{InstanceId: aws.String("i-1")}, "us-east-1", testAccount)

	for _, key := range []string{
		model.AttrPlatform,
		model.AttrEndpoint,
		model.AttrAvailabilityZone,
		model.AttrVPCID,
		model.AttrSubnetID,
		model.AttrEBSVolumeIDs,
		model.AttrInstanceClass,
	} {
		if _, ok := r.Attributes[key]; ok {
			t.Errorf("attribute %q present for an instance that reported nothing", key)
		}
	}
	// Encryption is a property of each attached volume, so the instance
	// declines to answer rather than summarising several volumes into one bit.
	if r.Encrypted != nil {
		t.Errorf("Encrypted = %v, want nil at the instance level", *r.Encrypted)
	}
	// No engine: guessing what software runs on a box from its shape or name
	// is exactly the inference the census refuses to make.
	if _, ok := r.Attributes[model.AttrEngine]; ok {
		t.Error("engine attribute set — EC2 must never claim what runs on the box")
	}
	// EC2 reports no retention setting; claiming zero days would read as "no
	// backups configured", which this scanner cannot know.
	if _, ok := r.Measures[model.MeasureBackupRetentionDays]; ok {
		t.Error("backup_retention_days set — EC2 reports no retention")
	}
	// Attached volumes are their own census rows; summing them here would
	// double-count storage estate-wide.
	if _, ok := r.Measures[model.MeasureSizeBytes]; ok {
		t.Error("size_bytes set — attached EBS must not be summed onto the instance")
	}
	if r.Status != "" {
		t.Errorf("Status = %q, want empty when no state was reported", r.Status)
	}
}

// Platform comes from PlatformDetails only. The adjacent Platform enum is
// "windows" or empty, so reading it would mean writing "linux" for a machine
// AWS never called Linux.
func TestEC2InstancePlatformIsNotInferredFromTheWindowsFlag(t *testing.T) {
	i := ec2types.Instance{
		InstanceId: aws.String("i-1"),
		Platform:   ec2types.PlatformValuesWindows,
	}
	r := ec2InstanceResource(i, "us-east-1", testAccount)
	if _, ok := r.Attributes[model.AttrPlatform]; ok {
		t.Errorf("platform = %q, want absent when PlatformDetails is unset", r.Attr(model.AttrPlatform))
	}
}

func TestInstancesFromReservationsSkipsTerminated(t *testing.T) {
	state := func(n ec2types.InstanceStateName) *ec2types.InstanceState {
		return &ec2types.InstanceState{Name: n}
	}
	reservations := []ec2types.Reservation{
		{Instances: []ec2types.Instance{
			{InstanceId: aws.String("i-running"), State: state(ec2types.InstanceStateNameRunning)},
			{InstanceId: aws.String("i-terminated"), State: state(ec2types.InstanceStateNameTerminated)},
		}},
		{Instances: []ec2types.Instance{
			{InstanceId: aws.String("i-stopped"), State: state(ec2types.InstanceStateNameStopped)},
			{InstanceId: aws.String("i-shutting-down"), State: state(ec2types.InstanceStateNameShuttingDown)},
			// No state reported is not evidence of termination.
			{InstanceId: aws.String("i-stateless")},
		}},
	}

	got := instancesFromReservations(reservations, "us-east-1", testAccount)
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	want := []string{"i-running", "i-stopped", "i-shutting-down", "i-stateless"}
	if len(names) != len(want) {
		t.Fatalf("instances = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("instance %d = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestAttachedVolumeIDs(t *testing.T) {
	mappings := []ec2types.InstanceBlockDeviceMapping{
		{Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String("vol-b")}},
		// An instance-store device has no EBS block and no volume to name.
		{DeviceName: aws.String("/dev/sdb")},
		{Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String("vol-a")}},
		// A mapping AWS returned without an ID contributes nothing rather
		// than an empty slot in the list — whether the ID is absent (nil) or
		// reported as an empty string. The two are distinguished at the
		// pointer, but neither names a volume, so "vol-a,,vol-b" is not an
		// honest rendering of either.
		{Ebs: &ec2types.EbsInstanceBlockDevice{}},
		{Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String("")}},
	}
	if got := attachedVolumeIDs(mappings); got != "vol-a,vol-b" {
		t.Errorf("attachedVolumeIDs = %q, want vol-a,vol-b", got)
	}
	if got := attachedVolumeIDs(nil); got != "" {
		t.Errorf("attachedVolumeIDs(nil) = %q, want empty", got)
	}
}

func TestEC2ScannerRegistration(t *testing.T) {
	if got := (ec2Scanner{}).Service(); got != model.ServiceEC2 {
		t.Errorf("Service() = %q, want %q", got, model.ServiceEC2)
	}
}
