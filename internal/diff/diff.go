// Package diff compares two census snapshots so recurring scans can answer
// "what changed since last time": new resources, removed resources, and
// field-level drift on the ones present in both.
package diff

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

// Result of comparing a fresh snapshot against a previous one.
type Result struct {
	Added   []model.Resource
	Removed []model.Resource
	Changed []ResourceDiff
	// Cost is spend movement, computed and rendered apart from field drift.
	// See internal/diff/cost.go for why it has to be, and why it is
	// deliberately absent from Empty.
	Cost CostDrift
}

// ResourceDiff is one resource present in both snapshots with drifted fields.
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

// Empty reports whether the estate was identical for diff purposes.
//
// Cost is excluded on purpose, and this is the load-bearing spot: Empty gates
// --fail-on-change, and spend moves without the estate moving — AWS restates
// bills for weeks and a modelled rate is remodelled continuously. Counting it
// here would make --fail-on-change return non-zero on every run and stop
// meaning anything. Spend movement is reported in its own section instead.
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
	res.Cost = costDrift(old, current)
	return res
}

// fieldChanges lists drift on every core field, every tag, and every
// attribute and measure either side reports. Walking the bag generically is
// what keeps the diff working for services that do not exist yet: a new
// scanner's keys drift without anyone extending this function.
//
// The core is compared exhaustively rather than selectively. ARN is the match
// key, so it cannot drift — but everything else can, and a field left out
// here is a change the census watched happen and stayed silent about. Name in
// particular: it comes from a mutable Name tag for the resource types the
// census is growing into (EBS volumes, NAT gateways), so it moves while the
// ARN holds still.
//
// Only eol/eol_date are excluded: they derive from the platform and version
// attributes and would report the same upgrade twice.
func fieldChanges(o, n model.Resource) []FieldChange {
	var out []FieldChange
	add := func(field, oldV, newV string) {
		if oldV != newV {
			out = append(out, FieldChange{Field: field, Old: oldV, New: newV})
		}
	}
	add("name", o.Name, n.Name)
	add("service", o.Service, n.Service)
	add("type", o.Type, n.Type)
	add("status", o.Status, n.Status)
	add("region", o.Region, n.Region)
	add("account_id", o.AccountID, n.AccountID)
	add("created_at", timePtrStr(o.CreatedAt), timePtrStr(n.CreatedAt))
	add("environment", o.Environment, n.Environment)
	add("owner", o.Owner, n.Owner)
	add("publicly_accessible", boolPtrStr(o.PubliclyAccessible), boolPtrStr(n.PubliclyAccessible))
	add("encrypted", boolPtrStr(o.Encrypted), boolPtrStr(n.Encrypted))
	// Tags drift per key rather than as one blob, so the line names what
	// actually changed. The prefix keeps a tag called "engine" from colliding
	// with the attribute of that name. Retagging shows up twice — once here and
	// once as environment/owner — which is the honest reading: the tag changed
	// and the field the census derives from it changed with it.
	for _, k := range unionKeys(o.Tags, n.Tags) {
		add("tag:"+k, tagStr(o.Tags, k), tagStr(n.Tags, k))
	}
	for _, k := range unionKeys(o.Attributes, n.Attributes) {
		// An observation timestamp advances on every scan by definition, so
		// diffing it would mark every metric-bearing resource as changed on
		// every run and bury the drift that is real. The measure it belongs to
		// is still compared below — the reading moving is drift, the clock
		// moving is not.
		if strings.HasSuffix(k, model.AsOfSuffix) {
			continue
		}
		add(k, o.Attributes[k], n.Attributes[k])
	}
	for _, k := range unionKeys(o.Measures, n.Measures) {
		add(k, measureStr(o, k), measureStr(n, k))
	}
	return out
}

// unionKeys returns every key present in either map, sorted so the drift
// lines come out in the same order on every run.
func unionKeys[V any](a, b map[string]V) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// tagStr renders a tag value for comparison: quoted when the resource carries
// the tag, empty (shown as "—") when it does not.
//
// The quotes are what keep an empty tag distinguishable from a missing one.
// Scanners store tag values exactly as AWS returns them, and AWS accepts a tag
// with no value, so "untagged" and "tagged with nothing" are different events —
// but a plain map index renders both as "", and neither transition would drift.
// Quoting is used rather than a sentinel because tags are the one census field
// whose value is arbitrary user text: any marker chosen to mean "absent" could
// itself be somebody's tag value, while quoting maps distinct values to
// distinct strings for all of them. Core fields come from AWS identifiers and
// enums, so they stay unquoted.
func tagStr(tags map[string]string, key string) string {
	v, ok := tags[key]
	if !ok {
		return ""
	}
	return strconv.Quote(v)
}

// measureStr renders a measure for comparison, using empty (shown as "—") for
// a key the resource does not report so "not reported" never drifts against a
// real zero.
func measureStr(r model.Resource, key string) string {
	v, ok := r.Measure(key)
	if !ok {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// timePtrStr renders a creation timestamp for comparison, empty when the
// service did not report one. UTC keeps a baseline written in another zone
// from reading as drift.
//
// Nanosecond precision is kept because the realistic way a creation time moves
// is a resource being destroyed and rebuilt under the same ARN — a table or
// bucket recreated with the same name — and second-truncated formatting would
// hide that when the rebuild lands inside the same second. Trailing zeros are
// dropped by this layout, which is fine for equality: identical instants always
// format identically.
func timePtrStr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// boolPtrStr renders a tri-state boolean for field comparison: empty when the
// service did not report the field (rendered as "—" by orDash), so "not
// reported" never drifts against an explicit false.
func boolPtrStr(v *bool) string {
	if v == nil {
		return ""
	}
	return strconv.FormatBool(*v)
}

// maxListed caps per-bucket detail lines so a huge drift doesn't flood the
// terminal; the counts in the header always cover everything.
const maxListed = 20

// Write renders the diff as terminal sections. label names the baseline
// (typically the previous census filename).
//
// Spend gets its own section below the estate changes, printed even when the
// estate itself held still — a bill that moved while nothing was created or
// destroyed is the interesting case, not a reason to stay quiet.
func (r Result) Write(w io.Writer, label string) {
	r.writeResources(w, label)
	r.Cost.WriteCost(w, label)
}

func (r Result) writeResources(w io.Writer, label string) {
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
		fmt.Fprintf(w, "  %s %s (%s %s, %s)\n", sign, r.Name, r.Service, r.Attr(model.AttrEngine), r.Region)
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
