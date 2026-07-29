package scanners

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/hoophq/blueprint/internal/model"
)

// gib is the byte count of one GiB, spelled out so the size assertions below
// read as "8 GiB" rather than as a magic constant.
const gib = int64(1) << 30

func TestEBSVolumeARN(t *testing.T) {
	got := EBSVolumeARN("aws", "us-east-1", testAccount, "vol-0abc123")
	want := "arn:aws:ec2:us-east-1:" + testAccount + ":volume/vol-0abc123"
	if got != want {
		t.Errorf("EBSVolumeARN = %q, want %q", got, want)
	}
	got = EBSVolumeARN("aws-cn", "cn-north-1", testAccount, "vol-0abc123")
	want = "arn:aws-cn:ec2:cn-north-1:" + testAccount + ":volume/vol-0abc123"
	if got != want {
		t.Errorf("EBSVolumeARN = %q, want %q", got, want)
	}
}

// The empty account field is the whole point of this test. It looks like a bug
// and is AWS's own format; anyone "fixing" it re-keys every snapshot in the
// census, so the shape is pinned character for character.
func TestEBSSnapshotARNOmitsTheAccount(t *testing.T) {
	got := EBSSnapshotARN("aws", "us-east-1", "snap-0abc123")
	want := "arn:aws:ec2:us-east-1::snapshot/snap-0abc123"
	if got != want {
		t.Errorf("EBSSnapshotARN = %q, want %q", got, want)
	}
	if got := EBSSnapshotARN("aws", "us-east-1", "snap-0abc123"); strings.Contains(got, testAccount) {
		t.Errorf("EBSSnapshotARN = %q, must not carry the account ID", got)
	}
}

func TestEBSVolumeResource(t *testing.T) {
	created := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	v := ec2types.Volume{
		VolumeId:         aws.String("vol-0abc123"),
		State:            ec2types.VolumeStateInUse,
		VolumeType:       ec2types.VolumeTypeGp3,
		AvailabilityZone: aws.String("us-east-1a"),
		Size:             aws.Int32(8),
		Iops:             aws.Int32(3000),
		Throughput:       aws.Int32(125),
		Encrypted:        aws.Bool(true),
		CreateTime:       &created,
		Attachments: []ec2types.VolumeAttachment{
			{InstanceId: aws.String("i-0000000000000bbbb")},
			{InstanceId: aws.String("i-0000000000000aaaa")},
		},
		Tags: []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("data")}},
	}

	r := ebsVolumeResource(v, "us-east-1", testAccount)

	if r.Service != model.ServiceEBS || r.Type != model.TypeEBSVolume {
		t.Errorf("service/type = %q/%q, want %q/%q", r.Service, r.Type, model.ServiceEBS, model.TypeEBSVolume)
	}
	if r.Name != "data" {
		t.Errorf("Name = %q, want the Name tag", r.Name)
	}
	if r.Status != "in-use" {
		t.Errorf("Status = %q, want in-use", r.Status)
	}
	if r.Encrypted == nil || !*r.Encrypted {
		t.Error("Encrypted must be reported on the volume — it is the row that can honestly answer it")
	}
	// The instance declines this question; the volume must not also decline it,
	// or nothing in the census answers it.
	if r.PubliclyAccessible != nil {
		t.Error("PubliclyAccessible must stay nil: a volume has no network identity")
	}
	if got, want := r.Attr(model.AttrVolumeType), "gp3"; got != want {
		t.Errorf("volume_type = %q, want %q", got, want)
	}
	// Sorted, so the attribute does not churn the diff when AWS reorders the
	// attachment list between scans.
	if got, want := r.Attr(model.AttrAttachedInstanceIDs), "i-0000000000000aaaa,i-0000000000000bbbb"; got != want {
		t.Errorf("attached_instance_ids = %q, want %q", got, want)
	}
	if got, ok := r.Measure(model.MeasureSizeBytes); !ok || got != 8*gib {
		t.Errorf("size_bytes = %d (reported %v), want %d", got, ok, 8*gib)
	}
	if got, ok := r.Measure(model.MeasureIOPS); !ok || got != 3000 {
		t.Errorf("iops = %d (reported %v), want 3000", got, ok)
	}
	if got, ok := r.Measure(model.MeasureThroughputMiBps); !ok || got != 125 {
		t.Errorf("throughput_mibps = %d (reported %v), want 125", got, ok)
	}
	if r.CreatedAt == nil || !r.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", r.CreatedAt, created)
	}
}

// An unattached volume is the headline finding of this scanner, and it is a
// finding made of two absences — no attachments, state "available". Neither may
// be dressed up: the attribute is absent rather than empty-stringed, and the
// status is AWS's word rather than a verdict.
func TestEBSVolumeUnattached(t *testing.T) {
	v := ec2types.Volume{
		VolumeId: aws.String("vol-0orphan"),
		State:    ec2types.VolumeStateAvailable,
		Size:     aws.Int32(500),
	}
	r := ebsVolumeResource(v, "us-east-1", testAccount)

	if _, present := r.Attributes[model.AttrAttachedInstanceIDs]; present {
		t.Error("attached_instance_ids must be absent for an unattached volume, not empty")
	}
	if r.Status != "available" {
		t.Errorf("Status = %q, want available", r.Status)
	}
	// Untagged volumes are the common case; the row must still be identifiable.
	if r.Name != "vol-0orphan" {
		t.Errorf("Name = %q, want the volume ID as fallback", r.Name)
	}
	// Nothing in this response reported encryption, so nothing may claim it.
	if r.Encrypted != nil {
		t.Error("Encrypted must stay nil when AWS did not report it")
	}
}

// The recurring bug this project keeps catching: a `> 0` filter that turns
// "reported as zero" into "not reported". A gp3 volume can genuinely sit at
// zero provisioned throughput in the response, and a zero-GiB size — however
// unlikely — is a finding about the volume, not a reason to drop the key.
func TestEBSVolumeStoresReportedZeros(t *testing.T) {
	v := ec2types.Volume{
		VolumeId:   aws.String("vol-0zero"),
		Size:       aws.Int32(0),
		Iops:       aws.Int32(0),
		Throughput: aws.Int32(0),
	}
	r := ebsVolumeResource(v, "us-east-1", testAccount)

	for _, key := range []string{model.MeasureSizeBytes, model.MeasureIOPS, model.MeasureThroughputMiBps} {
		got, ok := r.Measure(key)
		if !ok {
			t.Errorf("%s: a reported zero was dropped", key)
			continue
		}
		if got != 0 {
			t.Errorf("%s = %d, want 0", key, got)
		}
	}
}

// The mirror image: a volume type that carries no IOPS or throughput leaves
// those keys absent, so absent and zero stay distinguishable end to end.
func TestEBSVolumeOmitsUnreportedMeasures(t *testing.T) {
	v := ec2types.Volume{VolumeId: aws.String("vol-0st1"), VolumeType: ec2types.VolumeTypeSt1}
	r := ebsVolumeResource(v, "us-east-1", testAccount)

	for _, key := range []string{model.MeasureSizeBytes, model.MeasureIOPS, model.MeasureThroughputMiBps} {
		if _, ok := r.Measure(key); ok {
			t.Errorf("%s present, but AWS reported no value for it", key)
		}
	}
}

// gp2 with provisioned IOPS and throughput in the same response is the
// gp2-to-gp3 case. The scanner's job is to state all three numbers and stop
// short of the recommendation.
func TestEBSVolumeGp2ReportsProvisionedPerformance(t *testing.T) {
	v := ec2types.Volume{
		VolumeId:   aws.String("vol-0gp2"),
		VolumeType: ec2types.VolumeTypeGp2,
		State:      ec2types.VolumeStateInUse,
		Size:       aws.Int32(1000),
		Iops:       aws.Int32(3000),
	}
	r := ebsVolumeResource(v, "us-east-1", testAccount)

	if got := r.Attr(model.AttrVolumeType); got != "gp2" {
		t.Errorf("volume_type = %q, want gp2", got)
	}
	if got, ok := r.Measure(model.MeasureIOPS); !ok || got != 3000 {
		t.Errorf("iops = %d (reported %v), want 3000", got, ok)
	}
	// No verdict attribute anywhere on the row — the reader draws it.
	for key := range r.Attributes {
		switch key {
		case "recommendation", "waste", "upgrade_to", "suggested_volume_type":
			t.Errorf("scanner emitted a verdict attribute %q", key)
		}
	}
}

func TestEBSVolumePartition(t *testing.T) {
	// A KMS key ARN AWS returned beats the region rule.
	withKey := ec2types.Volume{
		VolumeId: aws.String("vol-1"),
		KmsKeyId: aws.String("arn:aws-us-gov:kms:us-gov-west-1:" + testAccount + ":key/abc"),
	}
	if got := ebsVolumeResource(withKey, "us-east-1", testAccount).ARN; !strings.HasPrefix(got, "arn:aws-us-gov:") {
		t.Errorf("ARN = %q, want the aws-us-gov partition from the KMS ARN", got)
	}
	// Nothing usable reported: the region rule decides, and must not be
	// outranked by a malformed string reading as a bare "aws".
	unusable := map[string]ec2types.Volume{
		"no evidence":     {VolumeId: aws.String("vol-1")},
		"empty KMS ARN":   {VolumeId: aws.String("vol-1"), KmsKeyId: aws.String("")},
		"non-ARN KMS key": {VolumeId: aws.String("vol-1"), KmsKeyId: aws.String("abc-123")},
		"empty outpost":   {VolumeId: aws.String("vol-1"), OutpostArn: aws.String("")},
	}
	for name, v := range unusable {
		if got := ebsVolumeResource(v, "us-gov-west-1", testAccount).ARN; !strings.HasPrefix(got, "arn:aws-us-gov:") {
			t.Errorf("%s in GovCloud: ARN = %q, want the region rule's aws-us-gov", name, got)
		}
	}
}

func TestEBSSnapshotResource(t *testing.T) {
	started := time.Date(2023, 1, 5, 9, 30, 0, 0, time.UTC)
	s := ec2types.Snapshot{
		SnapshotId:              aws.String("snap-0abc123"),
		VolumeId:                aws.String("vol-0live"),
		State:                   ec2types.SnapshotStateCompleted,
		StorageTier:             ec2types.StorageTierStandard,
		VolumeSize:              aws.Int32(100),
		FullSnapshotSizeInBytes: aws.Int64(42_000_000),
		Encrypted:               aws.Bool(false),
		StartTime:               &started,
	}
	join := newSnapshotJoin([]ec2types.Volume{{VolumeId: aws.String("vol-0live")}}, true, nil, true)

	r := ebsSnapshotResource(s, "us-east-1", testAccount, join)

	if r.Service != model.ServiceEBS || r.Type != model.TypeEBSSnapshot {
		t.Errorf("service/type = %q/%q, want %q/%q", r.Service, r.Type, model.ServiceEBS, model.TypeEBSSnapshot)
	}
	if r.AccountID != testAccount {
		t.Errorf("AccountID = %q — the ARN omits it, the row must not", r.AccountID)
	}
	if got, want := r.Attr(model.AttrSourceVolumeID), "vol-0live"; got != want {
		t.Errorf("source_volume_id = %q, want %q", got, want)
	}
	if got, want := r.Attr(model.AttrSourceVolumeExists), "true"; got != want {
		t.Errorf("source_volume_exists = %q, want %q", got, want)
	}
	if got, want := r.Attr(model.AttrStorageTier), "standard"; got != want {
		t.Errorf("storage_tier = %q, want %q", got, want)
	}
	if got, ok := r.Measure(model.MeasureSourceVolumeBytes); !ok || got != 100*gib {
		t.Errorf("source_volume_bytes = %d (reported %v), want %d", got, ok, 100*gib)
	}
	if got, ok := r.Measure(model.MeasureFullSnapshotBytes); !ok || got != 42_000_000 {
		t.Errorf("full_snapshot_bytes = %d (reported %v), want 42000000", got, ok)
	}
	if r.Encrypted == nil || *r.Encrypted {
		t.Error("a reported false must survive as false, not as absent")
	}
}

// The honesty landmine named in the issue. No API reports a snapshot's billed
// size, so neither number it does report may wear the key that renderers treat
// as a size — MeasureSizeBytes is the one every renderer shows as "Size", and
// putting an upper bound there invites summing it.
func TestEBSSnapshotHasNoSizeBytes(t *testing.T) {
	s := ec2types.Snapshot{
		SnapshotId:              aws.String("snap-0abc123"),
		VolumeSize:              aws.Int32(16_000),
		FullSnapshotSizeInBytes: aws.Int64(1_500_000_000),
	}
	r := ebsSnapshotResource(s, "us-east-1", testAccount, snapshotJoin{})

	if v, ok := r.Measure(model.MeasureSizeBytes); ok {
		t.Errorf("snapshot carries size_bytes = %d; neither reported size is the billed size", v)
	}
}

// "I could not finish looking" must never render as "it is gone". A snapshot
// judged against a truncated volume list has no orphan verdict at all.
func TestEBSSnapshotWithholdsOrphanVerdictOnPartialData(t *testing.T) {
	s := ec2types.Snapshot{SnapshotId: aws.String("snap-0abc"), VolumeId: aws.String("vol-0gone")}

	cases := map[string]snapshotJoin{
		"volumes truncated": newSnapshotJoin(nil, false, nil, true),
		"images truncated":  newSnapshotJoin(nil, true, nil, false),
		"both truncated":    newSnapshotJoin(nil, false, nil, false),
	}
	for name, join := range cases {
		r := ebsSnapshotResource(s, "us-east-1", testAccount, join)
		if _, present := r.Attributes[model.AttrSourceVolumeExists]; present {
			t.Errorf("%s: source_volume_exists was written on incomplete input", name)
		}
		// The reported fact survives; only the derived verdict is withheld.
		if got := r.Attr(model.AttrSourceVolumeID); got != "vol-0gone" {
			t.Errorf("%s: source_volume_id = %q, want it recorded regardless", name, got)
		}
	}

	// Complete input, source volume genuinely absent: now the verdict lands.
	r := ebsSnapshotResource(s, "us-east-1", testAccount, newSnapshotJoin(nil, true, nil, true))
	if got := r.Attr(model.AttrSourceVolumeExists); got != "false" {
		t.Errorf("source_volume_exists = %q, want false on complete input", got)
	}
}

// A snapshot backing an AMI is not an orphan however long its source volume has
// been gone. This is the cross-check that stops the false flag, so it is pinned
// both ways: recorded when found, and recorded even on incomplete input,
// because withholding it is the error that gets a snapshot deleted.
func TestEBSSnapshotBackingImages(t *testing.T) {
	s := ec2types.Snapshot{SnapshotId: aws.String("snap-0amibacked"), VolumeId: aws.String("vol-0gone")}
	images := []ec2types.Image{
		{
			ImageId: aws.String("ami-0bbbb"),
			BlockDeviceMappings: []ec2types.BlockDeviceMapping{
				{Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-0amibacked")}},
			},
		},
		{
			ImageId: aws.String("ami-0aaaa"),
			BlockDeviceMappings: []ec2types.BlockDeviceMapping{
				{Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-0amibacked")}},
				// Instance-store mapping: no EBS block, references no snapshot.
				{DeviceName: aws.String("/dev/sdb")},
			},
		},
	}

	r := ebsSnapshotResource(s, "us-east-1", testAccount, newSnapshotJoin(nil, true, images, true))
	if got, want := r.Attr(model.AttrBackingImageIDs), "ami-0aaaa,ami-0bbbb"; got != want {
		t.Errorf("backing_image_ids = %q, want %q (sorted)", got, want)
	}
	// The source volume is gone AND the snapshot backs two AMIs. Both facts
	// must be on the row, or the first one alone reads as "safe to delete".
	if got := r.Attr(model.AttrSourceVolumeExists); got != "false" {
		t.Errorf("source_volume_exists = %q, want false", got)
	}

	// Truncated volume list: the verdict is withheld, the AMI evidence is not.
	partial := ebsSnapshotResource(s, "us-east-1", testAccount, newSnapshotJoin(nil, false, images, false))
	if got, want := partial.Attr(model.AttrBackingImageIDs), "ami-0aaaa,ami-0bbbb"; got != want {
		t.Errorf("backing_image_ids on partial input = %q, want %q", got, want)
	}

	// A snapshot backing nothing has no key at all, rather than an empty one.
	other := ec2types.Snapshot{SnapshotId: aws.String("snap-0plain")}
	plain := ebsSnapshotResource(other, "us-east-1", testAccount, newSnapshotJoin(nil, true, images, true))
	if _, present := plain.Attributes[model.AttrBackingImageIDs]; present {
		t.Error("backing_image_ids must be absent when no AMI references the snapshot")
	}
}

// AWS's own placeholder for "this snapshot has no source volume" is a real
// volume-shaped string. Treating it as an ID would ask whether vol-ffffffff
// still exists — it never did — and flag every copied snapshot as an orphan.
func TestEBSSnapshotPlaceholderSourceVolume(t *testing.T) {
	s := ec2types.Snapshot{SnapshotId: aws.String("snap-0copied"), VolumeId: aws.String(noSourceVolume)}
	r := ebsSnapshotResource(s, "us-east-1", testAccount, newSnapshotJoin(nil, true, nil, true))

	if _, present := r.Attributes[model.AttrSourceVolumeID]; present {
		t.Error("the placeholder volume ID must not be recorded as a source volume")
	}
	if _, present := r.Attributes[model.AttrSourceVolumeExists]; present {
		t.Error("a snapshot with no source volume has no source-volume verdict to give")
	}
}

func TestEBSSnapshotStoresReportedZeros(t *testing.T) {
	s := ec2types.Snapshot{
		SnapshotId:              aws.String("snap-0empty"),
		VolumeSize:              aws.Int32(0),
		FullSnapshotSizeInBytes: aws.Int64(0),
	}
	r := ebsSnapshotResource(s, "us-east-1", testAccount, snapshotJoin{})

	// A snapshot of an entirely unwritten volume genuinely reports zero written
	// bytes. Dropping it would hide the emptiest snapshots in the account.
	for _, key := range []string{model.MeasureSourceVolumeBytes, model.MeasureFullSnapshotBytes} {
		got, ok := r.Measure(key)
		if !ok {
			t.Errorf("%s: a reported zero was dropped", key)
			continue
		}
		if got != 0 {
			t.Errorf("%s = %d, want 0", key, got)
		}
	}
}

func TestEBSSnapshotOmitsUnreportedSizes(t *testing.T) {
	s := ec2types.Snapshot{SnapshotId: aws.String("snap-0pending"), State: ec2types.SnapshotStatePending}
	r := ebsSnapshotResource(s, "us-east-1", testAccount, snapshotJoin{})

	for _, key := range []string{model.MeasureSourceVolumeBytes, model.MeasureFullSnapshotBytes} {
		if _, ok := r.Measure(key); ok {
			t.Errorf("%s present, but AWS reported no value for it", key)
		}
	}
}

func TestAttachedInstanceIDs(t *testing.T) {
	got := attachedInstanceIDs([]ec2types.VolumeAttachment{
		{InstanceId: aws.String("i-0c")},
		{InstanceId: nil},                // AWS reported no instance
		{InstanceId: aws.String("")},     // reported, and names nothing
		{InstanceId: aws.String("i-0a")}, // multi-attach io2
		{InstanceId: aws.String("i-0a")}, // duplicated in the response
	})
	if want := "i-0a,i-0c"; got != want {
		t.Errorf("attachedInstanceIDs = %q, want %q", got, want)
	}
	if got := attachedInstanceIDs(nil); got != "" {
		t.Errorf("attachedInstanceIDs(nil) = %q, want empty so SetAttr omits the key", got)
	}
}

func TestSetGiBMeasure(t *testing.T) {
	var r model.Resource
	setGiBMeasure(&r, "k", nil)
	if _, ok := r.Measure("k"); ok {
		t.Error("a nil size must leave the key absent")
	}

	setGiBMeasure(&r, "zero", aws.Int32(0))
	if got, ok := r.Measure("zero"); !ok || got != 0 {
		t.Errorf("zero = %d (reported %v), want a stored 0", got, ok)
	}

	// The widening must happen before the shift: 4 GiB overflows int32 bytes,
	// and 16 TiB is an ordinary io2 volume.
	setGiBMeasure(&r, "big", aws.Int32(16_384))
	if got, want := mustMeasure(t, &r, "big"), 16_384*gib; got != want {
		t.Errorf("16384 GiB = %d, want %d", got, want)
	}
	setGiBMeasure(&r, "four", aws.Int32(4))
	if got, want := mustMeasure(t, &r, "four"), 4*gib; got != want {
		t.Errorf("4 GiB = %d, want %d", got, want)
	}
}

func mustMeasure(t *testing.T, r *model.Resource, key string) int64 {
	t.Helper()
	v, ok := r.Measure(key)
	if !ok {
		t.Fatalf("%s not reported", key)
	}
	return v
}

func TestVolumeIDSet(t *testing.T) {
	live := volumeIDSet([]ec2types.Volume{
		{VolumeId: aws.String("vol-1")},
		{VolumeId: nil},
		{VolumeId: aws.String("")},
	})
	if !live["vol-1"] {
		t.Error("vol-1 missing from the live set")
	}
	if live[""] {
		t.Error("an unnamed volume must not enter the live set — it would make every unnamed source look alive")
	}
	if len(live) != 1 {
		t.Errorf("live set has %d entries, want 1", len(live))
	}
}

// The scanner is registered under its own service name so the failure ledger
// can say which of the two EC2-API listings was lost.
func TestEBSScannerService(t *testing.T) {
	if got := (ebsScanner{}).Service(); got != model.ServiceEBS {
		t.Errorf("Service() = %q, want %q", got, model.ServiceEBS)
	}
	if got := (ebsScanner{}).Service(); got == (ec2Scanner{}).Service() {
		t.Error("ebs and ec2 must be distinct services in the failure ledger")
	}
}
