package scanners

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// selfOwner scopes the snapshot and image listings to this account. It is not
// an optimization: unscoped, DescribeSnapshots returns every public snapshot in
// the region — hundreds of thousands of rows belonging to strangers.
const selfOwner = "self"

// noSourceVolume is the placeholder AWS puts in a snapshot's VolumeId when
// there is no source volume behind it — a snapshot copied from another region
// or imported from outside AWS. It is a real value in the response and it names
// nothing, so asking whether "vol-ffffffff" still exists would flag every
// copied snapshot in the account as an orphan.
const noSourceVolume = "vol-ffffffff"

// ebsScanner censuses EBS volumes and snapshots — the cheapest high-value waste
// find in AWS, at one paginated call each.
//
// It is registered as its own service rather than folded into ec2Scanner so the
// failure ledger can name which listing was lost. Losing DescribeVolumes in a
// region and losing DescribeInstances there are different gaps in a census, and
// a reader deciding whether to trust a storage figure needs to know which one
// happened.
//
// What it will not do is convert a fact into a verdict. An unattached volume is
// reported as state "available" with no attachments, not as "waste"; a gp2
// volume's provisioned IOPS are reported as a number, not as a migration
// recommendation. The numbers make those cases obvious, and the reader's
// context — a volume detached for a migration this afternoon looks identical to
// one abandoned in 2019 — is the part this tool does not have.
type ebsScanner struct{}

func init() { scan.Register(ebsScanner{}) }

func (ebsScanner) Service() string { return model.ServiceEBS }

func (ebsScanner) Scan(ctx context.Context, cfg aws.Config, region, accountID string) ([]model.Resource, error) {
	client := ec2.NewFromConfig(cfg)

	// All three listings run even if an earlier one failed, because they answer
	// independent questions: a lost DescribeVolumes should not also cost the
	// reader their snapshot inventory. What it does cost is the orphan join,
	// which snapshotJoin refuses to compute on partial input.
	volumes, volumesErr := describeVolumes(ctx, client)
	images, imagesErr := describeSelfImages(ctx, client)
	snapshots, snapshotsErr := describeSelfSnapshots(ctx, client)

	out := make([]model.Resource, 0, len(volumes)+len(snapshots))
	for _, v := range volumes {
		out = append(out, ebsVolumeResource(v, region, accountID))
	}
	join := newSnapshotJoin(volumes, volumesErr == nil, images, imagesErr == nil)
	for _, s := range snapshots {
		out = append(out, ebsSnapshotResource(s, region, accountID, join))
	}
	// Partial results per the Scanner contract: every row above is real, and
	// the runner ledgers whatever gaps these errors describe. errors.Join is
	// nil when all three succeeded.
	return out, errors.Join(volumesErr, imagesErr, snapshotsErr)
}

func describeVolumes(ctx context.Context, client *ec2.Client) ([]ec2types.Volume, error) {
	var out []ec2types.Volume
	pages := ec2.NewDescribeVolumesPaginator(client, &ec2.DescribeVolumesInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return out, fmt.Errorf("describe volumes: %w", err)
		}
		out = append(out, page.Volumes...)
	}
	return out, nil
}

func describeSelfSnapshots(ctx context.Context, client *ec2.Client) ([]ec2types.Snapshot, error) {
	var out []ec2types.Snapshot
	pages := ec2.NewDescribeSnapshotsPaginator(client, &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{selfOwner},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return out, fmt.Errorf("describe snapshots: %w", err)
		}
		out = append(out, page.Snapshots...)
	}
	return out, nil
}

// describeSelfImages lists the account's own AMIs, which are not census rows —
// they are read solely to learn which snapshots are backing one, and so must
// not be deleted no matter how long their source volume has been gone.
func describeSelfImages(ctx context.Context, client *ec2.Client) ([]ec2types.Image, error) {
	var out []ec2types.Image
	pages := ec2.NewDescribeImagesPaginator(client, &ec2.DescribeImagesInput{
		Owners: []string{selfOwner},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return out, fmt.Errorf("describe images: %w", err)
		}
		out = append(out, page.Images...)
	}
	return out, nil
}

// snapshotJoin holds the two enumerations a snapshot is judged against and,
// the part that actually matters, whether both of them finished.
//
// The orphan question — is this snapshot's source volume gone, and does it back
// no AMI — is answered by absence from two lists. Absence from a list that was
// cut short by an API error is not evidence of anything, and a snapshot wrongly
// called an orphan is a snapshot someone deletes. So the derived verdict is
// gated on complete, and on nothing else.
type snapshotJoin struct {
	liveVolumes   map[string]bool
	backingImages map[string][]string
	complete      bool
}

func newSnapshotJoin(volumes []ec2types.Volume, volumesComplete bool, images []ec2types.Image, imagesComplete bool) snapshotJoin {
	return snapshotJoin{
		liveVolumes:   volumeIDSet(volumes),
		backingImages: imageSnapshotIDs(images),
		complete:      volumesComplete && imagesComplete,
	}
}

func volumeIDSet(volumes []ec2types.Volume) map[string]bool {
	live := make(map[string]bool, len(volumes))
	for _, v := range volumes {
		if v.VolumeId == nil {
			continue
		}
		if id := *v.VolumeId; id != "" {
			live[id] = true
		}
	}
	return live
}

// imageSnapshotIDs inverts the AMI list into snapshot ID -> the AMIs that
// reference it, sorted and deduplicated so the attribute is stable across scans
// however AWS happened to order the response.
func imageSnapshotIDs(images []ec2types.Image) map[string][]string {
	backing := make(map[string][]string)
	for _, img := range images {
		// Presence is the pointer throughout: an AMI AWS did not name cannot be
		// recorded as backing anything, and a device mapping with no EBS block
		// is instance store, which references no snapshot.
		if img.ImageId == nil {
			continue
		}
		imageID := *img.ImageId
		if imageID == "" {
			continue
		}
		for _, m := range img.BlockDeviceMappings {
			if m.Ebs == nil || m.Ebs.SnapshotId == nil {
				continue
			}
			if snapshotID := *m.Ebs.SnapshotId; snapshotID != "" {
				backing[snapshotID] = append(backing[snapshotID], imageID)
			}
		}
	}
	for snapshotID, imageIDs := range backing {
		slices.Sort(imageIDs)
		backing[snapshotID] = slices.Compact(imageIDs)
	}
	return backing
}

func ebsVolumeResource(v ec2types.Volume, region, accountID string) model.Resource {
	id := aws.ToString(v.VolumeId)
	tags := toTagMap(v.Tags, func(t ec2types.Tag) (*string, *string) { return t.Key, t.Value })
	// The Name tag is what the console shows, but volumes are tagged far less
	// often than instances, so the ID backs it up rather than leaving the row
	// nameless.
	name := tags["Name"]
	if name == "" {
		name = id
	}

	r := model.Resource{
		ARN:     EBSVolumeARN(partitionFromARNs(region, v.KmsKeyId, v.OutpostArn), region, accountID, id),
		Service: model.ServiceEBS,
		Type:    model.TypeEBSVolume,
		Name:    name,
		// "available" — the unattached state, and the one worth reading twice.
		// It is recorded as AWS's own status string rather than translated into
		// a finding.
		Status:    string(v.State),
		Region:    region,
		AccountID: accountID,
		CreatedAt: v.CreateTime,
		Tags:      tags,
		// This is where encryption at rest is honestly answerable: it is a
		// property of the volume, which is why the instance row declines the
		// question and defers to these rows.
		Encrypted: v.Encrypted,
		// PubliclyAccessible stays nil. A volume has no network identity to be
		// exposed on — it is reachable only through the instance it is attached
		// to — so "false" would answer a question that was never asked of it,
		// and would count toward an exposure total it does not belong in.
	}

	r.SetAttr(model.AttrVolumeType, string(v.VolumeType))
	r.SetAttr(model.AttrAvailabilityZone, aws.ToString(v.AvailabilityZone))
	r.SetAttr(model.AttrAttachedInstanceIDs, attachedInstanceIDs(v.Attachments))
	// Iops and Throughput are reported only for the volume types that carry
	// them; SetMeasureInt32 leaves the key absent for the rest and stores a
	// reported zero as zero. Together with volume_type they are the whole of
	// the gp2-to-gp3 case, stated as numbers.
	r.SetMeasureInt32(model.MeasureIOPS, v.Iops)
	r.SetMeasureInt32(model.MeasureThroughputMiBps, v.Throughput)
	// Provisioned size is the billed size: EBS charges for GiB-months of
	// allocation, not of use. That is what makes it a genuine size_bytes, and
	// what makes an "available" volume cost exactly as much as an attached one.
	setGiBMeasure(&r, model.MeasureSizeBytes, v.Size)
	return r
}

func ebsSnapshotResource(s ec2types.Snapshot, region, accountID string, join snapshotJoin) model.Resource {
	id := aws.ToString(s.SnapshotId)
	tags := toTagMap(s.Tags, func(t ec2types.Tag) (*string, *string) { return t.Key, t.Value })
	name := tags["Name"]
	if name == "" {
		name = id
	}

	r := model.Resource{
		ARN:     EBSSnapshotARN(partitionFromARNs(region, s.KmsKeyId, s.OutpostArn), region, id),
		Service: model.ServiceEBS,
		Type:    model.TypeEBSSnapshot,
		Name:    name,
		Status:  string(s.State),
		Region:  region,
		// The account owns the snapshot even though its ARN does not say so;
		// see EBSSnapshotARN.
		AccountID: accountID,
		// StartTime is when the snapshot was initiated, which for a snapshot is
		// also when it was created — unlike an instance's LaunchTime, nothing
		// moves it forward later.
		CreatedAt: s.StartTime,
		Tags:      tags,
		Encrypted: s.Encrypted,
	}

	r.SetAttr(model.AttrStorageTier, string(s.StorageTier))
	// Deliberately not MeasureSizeBytes, either of them. Neither number is what
	// this snapshot costs, and no API reports what it costs; see the measure
	// declarations in the model for why summing them produces a confident wrong
	// answer rather than an approximate right one.
	setGiBMeasure(&r, model.MeasureSourceVolumeBytes, s.VolumeSize)
	r.SetMeasureInt64(model.MeasureFullSnapshotBytes, s.FullSnapshotSizeInBytes)

	source := aws.ToString(s.VolumeId)
	if source != noSourceVolume {
		r.SetAttr(model.AttrSourceVolumeID, source)
	}
	// Positive evidence, recorded whether or not the enumerations finished: a
	// snapshot known to back an AMI must say so, since that is the fact that
	// stops someone deleting it.
	r.SetAttr(model.AttrBackingImageIDs, strings.Join(join.backingImages[id], ","))
	// The derived verdict, recorded only on complete input. A snapshot whose
	// source volume this scan simply failed to enumerate is not an orphan; it
	// is unexamined, and the failure ledger is where that is said.
	if join.complete && source != "" && source != noSourceVolume {
		r.SetAttr(model.AttrSourceVolumeExists, strconv.FormatBool(join.liveVolumes[source]))
	}
	return r
}

// attachedInstanceIDs joins the instances a volume is attached to, sorted and
// deduplicated so the value is stable across scans. Empty for an unattached
// volume, which SetAttr turns into an absent key — and which, paired with state
// "available", is the definitive unattached signal.
func attachedInstanceIDs(attachments []ec2types.VolumeAttachment) string {
	ids := make([]string, 0, len(attachments))
	for _, a := range attachments {
		// Presence is the pointer; usability is then a separate question, and
		// an attachment AWS reported with an empty instance ID names no
		// instance. Appending it would render "i-a,,i-b" — a list with a hole
		// in it. That is not the zero-measure case the honesty guardrail
		// protects: a zero-byte volume is a fact about storage, an unnamed
		// attachment is not a fact about anything.
		if a.InstanceId == nil {
			continue
		}
		if id := *a.InstanceId; id != "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return strings.Join(slices.Compact(ids), ",")
}

// setGiBMeasure records a size AWS reports in whole GiB as bytes.
//
// Presence is the pointer, as everywhere: a nil size is one AWS did not report
// and leaves the key absent, while a reported 0 is stored as 0 — a real finding
// that must survive to the renderer. The widening happens before the shift so a
// volume larger than 2 GiB does not overflow the int32 the SDK hands back.
func setGiBMeasure(r *model.Resource, key string, gib *int32) {
	if gib == nil {
		return
	}
	r.SetMeasure(key, int64(*gib)<<30)
}

// EBSVolumeARN builds a volume ARN: DescribeVolumes does not return one.
// Exported so the demo fixture builds ARNs with the same shape.
func EBSVolumeARN(partition, region, accountID, volumeID string) string {
	return fmt.Sprintf("arn:%s:ec2:%s:%s:volume/%s", partition, region, accountID, volumeID)
}

// EBSSnapshotARN builds a snapshot ARN, which — unlike a volume's — carries no
// account ID. The field is genuinely empty in AWS's own format:
//
//	arn:aws:ec2:us-east-1:123456789012:volume/vol-0abc
//	arn:aws:ec2:us-east-1::snapshot/snap-0abc
//
// This looks like a bug and is not one; snapshots and AMIs both omit it, and an
// IAM policy written as ec2:*:ACCOUNT:snapshot/* silently matches nothing. The
// census depends on the shape being exactly right for the same reason: the ARN
// is both the key --compare matches on and the key cost enrichment joins on, so
// "correcting" this would make every snapshot look new on every scan and go
// permanently unpriced.
func EBSSnapshotARN(partition, region, snapshotID string) string {
	return fmt.Sprintf("arn:%s:ec2:%s::snapshot/%s", partition, region, snapshotID)
}
