package render

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/hoophq/blueprint/internal/model"
)

func TestTerminalFailureRollup(t *testing.T) {
	snap := &model.Snapshot{
		Failures: []model.Failure{
			{AccountID: "1", Region: "us-west-2", Service: "rds", Error: "AccessDenied"},
			{AccountID: "1", Region: "us-east-1", Service: "rds", Error: "AccessDenied"},
			// Duplicate unit (e.g. --regions us-east-1,us-east-1): the region
			// must not repeat in the rolled-up line.
			{AccountID: "1", Region: "us-east-1", Service: "rds", Error: "AccessDenied"},
			{AccountID: "1", Region: "", Service: "cost", Error: "boom"},
		},
	}
	var buf bytes.Buffer
	Terminal(&buf, snap, nil)
	out := buf.String()

	// Identical (account, service, error) across regions rolls up into one
	// line with a sorted, de-duplicated region list; the header still counts
	// every unit.
	if !strings.Contains(out, "4 scan unit(s) failed") {
		t.Errorf("header does not count all units:\n%s", out)
	}
	if !strings.Contains(out, "- 1/rds in us-east-1, us-west-2: AccessDenied") {
		t.Errorf("missing rolled-up failure line:\n%s", out)
	}
	if strings.Count(out, "AccessDenied") != 1 {
		t.Errorf("rolled-up error listed more than once:\n%s", out)
	}
	// A region-less (global) unit renders without dangling separators.
	if !strings.Contains(out, "- 1/cost: boom") {
		t.Errorf("global failure line malformed:\n%s", out)
	}
}

func TestTerminalFailureCap(t *testing.T) {
	snap := &model.Snapshot{}
	for i := range maxFailuresListed + 5 {
		snap.Failures = append(snap.Failures, model.Failure{
			AccountID: "1", Region: "us-east-1", Service: "rds",
			Error: fmt.Sprintf("error-%03d", i),
		})
	}
	var buf bytes.Buffer
	Terminal(&buf, snap, nil)
	out := buf.String()
	if got := strings.Count(out, "    - "); got != maxFailuresListed {
		t.Errorf("listed %d failure lines, want cap at %d", got, maxFailuresListed)
	}
	if !strings.Contains(out, "and 5 more") {
		t.Errorf("missing overflow line:\n%s", out)
	}
}

func TestTerminalServiceTieBreak(t *testing.T) {
	snap := &model.Snapshot{
		Resources: []model.Resource{
			{ARN: "a1", Service: "rds", Region: "us-east-1", AccountID: "1"},
			{ARN: "a2", Service: "dynamodb", Region: "us-east-1", AccountID: "1"},
		},
	}
	var buf bytes.Buffer
	Terminal(&buf, snap, nil)
	// Equal counts must order by name, not map iteration.
	if !strings.Contains(buf.String(), "by service: dynamodb 1 · rds 1") {
		t.Errorf("equal-count services not name-ordered:\n%s", buf.String())
	}
}
