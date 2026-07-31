package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hoophq/blueprint/internal/demo"
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

// --demo-scale generates resources. Combined with a real scan that would mean
// a census containing rows AWS never returned, so the combination is refused
// rather than ignored — loudly, at the point of the mistake.
func TestDemoScaleValidates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scale   int
		demo    bool
		wantErr string
	}{
		{"unset, real scan", 0, false, ""},
		{"unset, demo", 0, true, ""},
		{"scaled demo", 20000, true, ""},
		{"scaled real scan", 20000, false, "--demo"},
		{"negative, demo", -1, true, "at least 1"},
		{"at the ceiling", demo.MaxScale, true, ""},
		// Well past what a browser can open, and far enough past it that the
		// generator would panic allocating the slice rather than report.
		{"over the ceiling", demo.MaxScale + 1, true, "capped at"},
		// Missing --demo is the more serious of the two mistakes and is the
		// one reported, even though the count is also nonsense.
		{"negative, real scan", -1, false, "--demo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDemoScale(tc.scale, tc.demo)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("validateDemoScale(%d, %t) = %v, want nil", tc.scale, tc.demo, err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("validateDemoScale(%d, %t) = nil, want an error mentioning %q", tc.scale, tc.demo, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("validateDemoScale(%d, %t) = %v, want an error mentioning %q", tc.scale, tc.demo, err, tc.wantErr)
			}
		})
	}
}

// The flag has to be wired to the fixture, not merely registered. Running the
// command is the only way to catch a --demo-scale that parses into a variable
// nobody reads — which would leave every scaled run silently reporting the
// storyboard.
func TestDemoScaleReachesTheCensus(t *testing.T) {
	storyboard := len(demo.Snapshot("test").Resources)

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"unscaled --demo is the storyboard", nil, storyboard},
		{"scaled up", []string{"--demo-scale", "300"}, 300},
		// Below the storyboard the fixture cannot shrink — it is curated, every
		// row earns its place — so the floor is the storyboard itself.
		{"below the storyboard", []string{"--demo-scale", "10"}, storyboard},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			cmd := scanCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(append([]string{
				"--demo", "--no-open", "--no-history", "--formats", "json", "-o", out,
			}, tc.args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("scan: %v", err)
			}

			files, err := filepath.Glob(filepath.Join(out, "*.json"))
			if err != nil || len(files) != 1 {
				t.Fatalf("want exactly one census JSON in %s, got %v (%v)", out, files, err)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var snap model.Snapshot
			if err := json.Unmarshal(data, &snap); err != nil {
				t.Fatal(err)
			}
			if got := len(snap.Resources); got != tc.want {
				t.Errorf("census holds %d resources, want %d", got, tc.want)
			}
		})
	}
}

// Scaling is a --demo affordance and the help has to say so, since the flag
// silently doing nothing on a real scan is precisely what it must not look
// like it does.
func TestDemoScaleFlagDefaultsAndHelp(t *testing.T) {
	f := scanCmd().Flags().Lookup("demo-scale")
	if f == nil {
		t.Fatal("--demo-scale flag is missing")
	}
	if f.DefValue != "0" {
		t.Errorf("--demo-scale defaults to %q, want 0 so that --demo alone stays the curated fixture", f.DefValue)
	}
	if !strings.Contains(f.Usage, "--demo") {
		t.Errorf("--demo-scale help does not mention --demo: %q", f.Usage)
	}
}
