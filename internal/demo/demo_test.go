package demo

import (
	"strings"
	"testing"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scanners"
)

// The fixture is the first thing most people see, so it has to be honest in
// the same way a real scan is: it may not carry a value the service does not
// report. EC2 is where that bites — it has no engine, no size AWS will tell
// you from DescribeInstances, and no retention setting at all — so a fixture
// that filled those in would teach the report to render numbers no scan can
// produce.
func TestSnapshotEC2InstancesReportOnlyWhatEC2Reports(t *testing.T) {
	snap := Snapshot("test")

	var seen int
	for _, r := range snap.Resources {
		if r.Service != model.ServiceEC2 {
			continue
		}
		seen++
		if r.Type != model.TypeEC2Instance {
			t.Errorf("%s: type = %q, want %q", r.Name, r.Type, model.TypeEC2Instance)
		}
		if got := r.Attr(model.AttrEngine); got != "" {
			t.Errorf("%s: engine = %q, want none — EC2 reports no data engine", r.Name, got)
		}
		if v, ok := r.Measure(model.MeasureSizeBytes); ok {
			t.Errorf("%s: size_bytes = %d, want absent — attached volumes are their own rows", r.Name, v)
		}
		if v, ok := r.Measure(model.MeasureBackupRetentionDays); ok {
			t.Errorf("%s: backup_retention_days = %d, want absent — EC2 has no retention setting", r.Name, v)
		}
		if r.Encrypted != nil {
			t.Errorf("%s: encrypted = %v, want nil — encryption is a per-volume property", r.Name, *r.Encrypted)
		}
		if r.PubliclyAccessible == nil {
			t.Errorf("%s: publicly_accessible is nil, want a reported value", r.Name)
		}
		// The endpoint is the private name; a public address is the exposure
		// flag above, never something to dial.
		if ep := r.Attr(model.AttrEndpoint); !strings.Contains(ep, ".internal") {
			t.Errorf("%s: endpoint = %q, want a private DNS name", r.Name, ep)
		}
		// ARNs come from the scanner's builder, so a fixture row is keyed the
		// way a scanned row is — the diff and the cost join depend on it.
		id := r.ARN[strings.LastIndex(r.ARN, "/")+1:]
		if want := scanners.EC2InstanceARN("aws", r.Region, r.AccountID, id); r.ARN != want {
			t.Errorf("ARN = %q, want %q", r.ARN, want)
		}
	}
	if seen == 0 {
		t.Fatal("no EC2 instances in the fixture")
	}
}

// An instance with no Name tag is named by its ID, the way the console shows
// it — the fixture exercises that path rather than tagging everything.
func TestSnapshotHasAnUntaggedEC2Instance(t *testing.T) {
	for _, r := range Snapshot("test").Resources {
		if r.Service == model.ServiceEC2 && strings.HasPrefix(r.Name, "i-") {
			if r.Tags["Name"] != "" {
				t.Errorf("%s: named by ID but carries a Name tag %q", r.ARN, r.Tags["Name"])
			}
			return
		}
	}
	t.Error("no EC2 instance falls back to its instance ID for a name")
}

// A snapshot's billed size is incremental and no AWS API reports it, so no
// snapshot row may carry size_bytes — that is the key every renderer prints as
// "Size", and filling it with the source volume's size would put a number on
// the page that overstates the bill by however much the snapshot deduplicated.
func TestSnapshotEBSSnapshotsHaveNoSizeBytes(t *testing.T) {
	var seen int
	for _, r := range Snapshot("test").Resources {
		if r.Type != model.TypeEBSSnapshot {
			continue
		}
		seen++
		if v, ok := r.Measure(model.MeasureSizeBytes); ok {
			t.Errorf("%s: size_bytes = %d, want absent — a snapshot's billed size is not reported by any API", r.Name, v)
		}
		// The two honest neighbours are both there, and both named after what
		// they actually measure.
		if _, ok := r.Measure(model.MeasureSourceVolumeBytes); !ok {
			t.Errorf("%s: source_volume_bytes absent", r.Name)
		}
		if _, ok := r.Measure(model.MeasureFullSnapshotBytes); !ok {
			t.Errorf("%s: full_snapshot_bytes absent", r.Name)
		}
	}
	if seen == 0 {
		t.Fatal("no EBS snapshots in the fixture")
	}
}

// The fixture has to show the guardrail, not just describe it: a region whose
// volume enumeration failed produces snapshot rows with no orphan verdict, and
// a ledger entry saying why. Either half alone is a different story — a
// verdictless row with no explanation looks like a bug, and a ledger entry with
// confident verdicts beside it looks like the failure did not matter.
func TestSnapshotWithheldOrphanVerdictIsExplainedByTheLedger(t *testing.T) {
	snap := Snapshot("test")

	ebsFailed := map[string]bool{}
	for _, f := range snap.Failures {
		if f.Service == model.ServiceEBS {
			ebsFailed[f.AccountID+"/"+f.Region] = true
		}
	}
	if len(ebsFailed) == 0 {
		t.Fatal("no EBS scan failure in the fixture — the withheld-verdict case cannot be shown without one")
	}

	var withheld int
	for _, r := range snap.Resources {
		if r.Type != model.TypeEBSSnapshot {
			continue
		}
		_, hasVerdict := r.Attributes[model.AttrSourceVolumeExists]
		scopeFailed := ebsFailed[r.AccountID+"/"+r.Region]
		switch {
		case scopeFailed && hasVerdict:
			t.Errorf("%s: source_volume_exists = %q in a scope whose EBS scan failed — "+
				"an incomplete list is not evidence the volume is gone",
				r.Name, r.Attr(model.AttrSourceVolumeExists))
		case scopeFailed:
			withheld++
			// The row still carries what AWS did report.
			if r.Attr(model.AttrSourceVolumeID) == "" {
				t.Errorf("%s: no source_volume_id — the withheld verdict should cost only the verdict", r.Name)
			}
		case !hasVerdict:
			t.Errorf("%s: no source_volume_exists, but nothing failed in %s/%s",
				r.Name, r.AccountID, r.Region)
		}
	}
	if withheld == 0 {
		t.Error("no snapshot exercises the withheld-verdict path")
	}
}

// The row that stops someone deleting a snapshot they need: source volume gone,
// so it reads like an orphan, but an AMI is built on it. Both facts are on the
// row, because either one alone is misleading.
func TestSnapshotHasAnAMIBackedSnapshotThatLooksOrphaned(t *testing.T) {
	for _, r := range Snapshot("test").Resources {
		if r.Type != model.TypeEBSSnapshot {
			continue
		}
		if r.Attr(model.AttrSourceVolumeExists) != "false" {
			continue
		}
		if r.Attr(model.AttrBackingImageIDs) != "" {
			return // found it
		}
	}
	t.Error("no snapshot has a dead source volume and a backing AMI — " +
		"the demo cannot show why source_volume_exists=false is not a delete signal")
}

// The headline waste find, and the shape it takes: state "available" and no
// attachment key at all. Absent, not empty — an empty attached_instance_ids
// would be a list with nothing in it rather than a volume with nothing on it.
func TestSnapshotHasUnattachedEBSVolumes(t *testing.T) {
	var unattached int
	for _, r := range Snapshot("test").Resources {
		if r.Type != model.TypeEBSVolume {
			continue
		}
		attached, has := r.Attributes[model.AttrAttachedInstanceIDs]
		if has && attached == "" {
			t.Errorf("%s: attached_instance_ids present but empty, want the key absent", r.Name)
		}
		if has {
			if r.Status != "in-use" {
				t.Errorf("%s: attached to %q but status = %q, want in-use", r.Name, attached, r.Status)
			}
			continue
		}
		unattached++
		if r.Status != "available" {
			t.Errorf("%s: unattached but status = %q, want available", r.Name, r.Status)
		}
		if _, ok := r.Measure(model.MeasureSizeBytes); !ok {
			t.Errorf("%s: no size_bytes — an unattached volume bills by the GiB, which is the whole point", r.Name)
		}
	}
	if unattached == 0 {
		t.Error("no unattached EBS volume in the fixture")
	}
}

// Volume types differ in what they report: st1 and sc1 report no IOPS and no
// throughput. The fixture carries one so the report is exercised against an
// absent measure and not only against zero.
func TestSnapshotHasAVolumeReportingNoPerformanceNumbers(t *testing.T) {
	for _, r := range Snapshot("test").Resources {
		if r.Type != model.TypeEBSVolume {
			continue
		}
		_, hasIOPS := r.Measure(model.MeasureIOPS)
		_, hasThroughput := r.Measure(model.MeasureThroughputMiBps)
		if !hasIOPS && !hasThroughput {
			return // found it
		}
	}
	t.Error("every EBS volume reports IOPS or throughput — the absent-measure path is never exercised")
}

// Every EBS row is keyed the way the scanner keys it, snapshots included. The
// snapshot ARN's empty account field is AWS's shape, and this pins the fixture
// to it: the ARN is the diff's match key and the cost join's key, so a fixture
// that "fixed" it would be testing a shape no scan produces.
func TestSnapshotEBSARNsMatchTheScannerBuilders(t *testing.T) {
	for _, r := range Snapshot("test").Resources {
		id := r.ARN[strings.LastIndex(r.ARN, "/")+1:]
		switch r.Type {
		case model.TypeEBSVolume:
			if want := scanners.EBSVolumeARN("aws", r.Region, r.AccountID, id); r.ARN != want {
				t.Errorf("ARN = %q, want %q", r.ARN, want)
			}
		case model.TypeEBSSnapshot:
			if want := scanners.EBSSnapshotARN("aws", r.Region, id); r.ARN != want {
				t.Errorf("ARN = %q, want %q", r.ARN, want)
			}
			if strings.Contains(r.ARN, r.AccountID) {
				t.Errorf("ARN = %q carries the account ID; snapshot ARNs do not", r.ARN)
			}
			// The row still knows which account owns it, even though the ARN
			// cannot say so.
			if r.AccountID == "" {
				t.Errorf("%s: no account ID on the row", r.ARN)
			}
		}
	}
}

// Storage the estate is paying for is counted once. An instance names its
// volumes and carries none of their bytes; the volume carries the bytes. The
// two agree, or a scan failure says why they could not.
func TestSnapshotEBSAttachmentsAreConsistentWithInstances(t *testing.T) {
	snap := Snapshot("test")

	volumeRows := map[string]model.Resource{}
	ebsFailed := map[string]bool{}
	for _, f := range snap.Failures {
		if f.Service == model.ServiceEBS {
			ebsFailed[f.AccountID+"/"+f.Region] = true
		}
	}
	for _, r := range snap.Resources {
		if r.Type == model.TypeEBSVolume {
			volumeRows[r.ARN[strings.LastIndex(r.ARN, "/")+1:]] = r
		}
	}

	for _, r := range snap.Resources {
		if r.Type != model.TypeEC2Instance {
			continue
		}
		instanceID := r.ARN[strings.LastIndex(r.ARN, "/")+1:]
		for _, volumeID := range strings.Split(r.Attr(model.AttrEBSVolumeIDs), ",") {
			if volumeID == "" {
				continue
			}
			vol, ok := volumeRows[volumeID]
			if !ok {
				// Only forgivable when the ledger says the volumes in this
				// scope were never enumerated.
				if !ebsFailed[r.AccountID+"/"+r.Region] {
					t.Errorf("instance %s names volume %s, which has no row and no scan failure to explain it",
						instanceID, volumeID)
				}
				continue
			}
			if !strings.Contains(vol.Attr(model.AttrAttachedInstanceIDs), instanceID) {
				t.Errorf("volume %s is attached to %q, but instance %s claims it",
					volumeID, vol.Attr(model.AttrAttachedInstanceIDs), instanceID)
			}
			if vol.Region != r.Region || vol.AccountID != r.AccountID {
				t.Errorf("volume %s is in %s/%s but instance %s is in %s/%s",
					volumeID, vol.AccountID, vol.Region, instanceID, r.AccountID, r.Region)
			}
		}
	}
}
