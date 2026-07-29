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
