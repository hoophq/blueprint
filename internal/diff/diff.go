// Package diff compares two census snapshots so recurring scans can answer
// "what changed since last time": new databases, removed databases, and
// field-level drift on the ones present in both.
package diff

import (
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/hoophq/blueprint/internal/model"
)

// Result of comparing a fresh snapshot against a previous one.
type Result struct {
	Added   []model.Resource
	Removed []model.Resource
	Changed []ResourceDiff
}

// ResourceDiff is one database present in both snapshots with drifted fields.
type ResourceDiff struct {
	Resource model.Resource // the current (new) state
	Fields   []FieldChange
}

// FieldChange is one drifted field, with values rendered as strings.
type FieldChange struct {
	Field string
	Old   string
	New   string
}

// Empty reports whether the two snapshots were identical for diff purposes.
func (r Result) Empty() bool {
	return len(r.Added) == 0 && len(r.Removed) == 0 && len(r.Changed) == 0
}

// Compare matches resources by ARN — the only identifier that is stable and
// unique across scans — and reports additions, removals, and field drift.
// Output slices are sorted by ARN for deterministic rendering.
func Compare(old, current *model.Snapshot) Result {
	prev := make(map[string]model.Resource, len(old.Resources))
	for _, r := range old.Resources {
		prev[r.ARN] = r
	}

	var res Result
	seen := make(map[string]bool, len(current.Resources))
	for _, r := range current.Resources {
		seen[r.ARN] = true
		o, ok := prev[r.ARN]
		if !ok {
			res.Added = append(res.Added, r)
			continue
		}
		if fields := fieldChanges(o, r); len(fields) > 0 {
			res.Changed = append(res.Changed, ResourceDiff{Resource: r, Fields: fields})
		}
	}
	for _, r := range old.Resources {
		if !seen[r.ARN] {
			res.Removed = append(res.Removed, r)
		}
	}

	sort.Slice(res.Added, func(i, j int) bool { return res.Added[i].ARN < res.Added[j].ARN })
	sort.Slice(res.Removed, func(i, j int) bool { return res.Removed[i].ARN < res.Removed[j].ARN })
	sort.Slice(res.Changed, func(i, j int) bool { return res.Changed[i].Resource.ARN < res.Changed[j].Resource.ARN })
	return res
}

// fieldChanges lists drift on the fields a DBA acts on. Tags are covered
// through the derived environment/owner pair rather than raw tag maps, and
// eol/eol_date are excluded: they derive from engine_version and would only
// duplicate that change.
func fieldChanges(o, n model.Resource) []FieldChange {
	var out []FieldChange
	add := func(field, oldV, newV string) {
		if oldV != newV {
			out = append(out, FieldChange{Field: field, Old: oldV, New: newV})
		}
	}
	add("engine", o.Engine, n.Engine)
	add("engine_version", o.EngineVersion, n.EngineVersion)
	add("instance_class", o.InstanceClass, n.InstanceClass)
	add("storage_gb", strconv.FormatInt(int64(o.StorageGB), 10), strconv.FormatInt(int64(n.StorageGB), 10))
	add("multi_az", strconv.FormatBool(o.MultiAZ), strconv.FormatBool(n.MultiAZ))
	add("status", o.Status, n.Status)
	add("environment", o.Environment, n.Environment)
	add("owner", o.Owner, n.Owner)
	return out
}

// maxListed caps per-bucket detail lines so a huge drift doesn't flood the
// terminal; the counts in the header always cover everything.
const maxListed = 20

// Write renders the diff as a terminal section. label names the baseline
// (typically the previous census filename).
func (r Result) Write(w io.Writer, label string) {
	fmt.Fprintf(w, "\n━━ changes vs %s ━━\n", label)
	if r.Empty() {
		fmt.Fprintf(w, "  no changes\n")
		return
	}
	fmt.Fprintf(w, "  +%d new  ·  −%d removed  ·  ~%d changed\n", len(r.Added), len(r.Removed), len(r.Changed))
	writeList(w, "+", r.Added)
	writeList(w, "−", r.Removed)
	for i, c := range r.Changed {
		if i == maxListed {
			fmt.Fprintf(w, "  ~ … and %d more changed\n", len(r.Changed)-maxListed)
			break
		}
		for _, f := range c.Fields {
			fmt.Fprintf(w, "  ~ %s (%s, %s): %s %s → %s\n",
				c.Resource.Name, c.Resource.Service, c.Resource.Region,
				f.Field, orDash(f.Old), orDash(f.New))
		}
	}
}

func writeList(w io.Writer, sign string, list []model.Resource) {
	for i, r := range list {
		if i == maxListed {
			fmt.Fprintf(w, "  %s … and %d more\n", sign, len(list)-maxListed)
			return
		}
		fmt.Fprintf(w, "  %s %s (%s %s, %s)\n", sign, r.Name, r.Service, r.Engine, r.Region)
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
