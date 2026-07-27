package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hoophq/blueprint/internal/model"
)

// A --compare baseline from a different census schema must be refused, not
// diffed: field representations change between schemas (e.g. multi_az going
// from an omitted bool to a pointer), so a cross-schema diff would report
// format changes as drift on every resource.
func TestCompareAgainstRefusesSchemaMismatch(t *testing.T) {
	prev := &model.Snapshot{ // Schema zero value = pre-versioning artifact
		Accounts: []string{"1"},
		Regions:  []string{"us-east-1"},
	}
	data, err := json.Marshal(prev)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "old-census.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	snap := &model.Snapshot{Schema: model.SchemaVersion}

	err = compareAgainst(cmd, snap, path, false)
	if err == nil {
		t.Fatal("compareAgainst diffed across schemas, want refusal")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("refusal does not explain the schema mismatch: %v", err)
	}

	// Same schema on both sides diffs normally.
	prev.Schema = model.SchemaVersion
	data, err = json.Marshal(prev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compareAgainst(cmd, snap, path, false); err != nil {
		t.Errorf("same-schema compare failed: %v", err)
	}
}
