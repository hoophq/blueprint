package diff

import (
	"strings"
	"testing"
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

func res(arn, name, engine, version, status string) model.Resource {
	r := model.Resource{
		ARN: arn, Name: name, Service: model.ServiceRDS, Type: model.TypeRDSInstance,
		Status: status, Region: "us-east-1", AccountID: "111111111111",
	}
	r.SetAttr(model.AttrEngine, engine)
	r.SetAttr(model.AttrEngineVersion, version)
	return r
}

func TestCompare(t *testing.T) {
	old := &model.Snapshot{Resources: []model.Resource{
		res("arn:a", "kept-same", "postgres", "15.4", "available"),
		res("arn:b", "upgraded", "postgres", "13.13", "available"),
		res("arn:c", "gone", "mysql", "5.7.44", "available"),
	}}
	cur := &model.Snapshot{Resources: []model.Resource{
		res("arn:a", "kept-same", "postgres", "15.4", "available"),
		res("arn:b", "upgraded", "postgres", "16.2", "available"),
		res("arn:d", "brand-new", "mysql", "8.4.3", "creating"),
	}}

	d := Compare(old, cur)
	if d.Empty() {
		t.Fatal("diff reported empty, want changes")
	}
	if len(d.Added) != 1 || d.Added[0].Name != "brand-new" {
		t.Errorf("Added = %v, want [brand-new]", names(d.Added))
	}
	if len(d.Removed) != 1 || d.Removed[0].Name != "gone" {
		t.Errorf("Removed = %v, want [gone]", names(d.Removed))
	}
	if len(d.Changed) != 1 {
		t.Fatalf("Changed has %d entries, want 1", len(d.Changed))
	}
	c := d.Changed[0]
	if c.Resource.Name != "upgraded" || len(c.Fields) != 1 {
		t.Fatalf("Changed[0] = %s with %d fields, want upgraded with 1", c.Resource.Name, len(c.Fields))
	}
	if f := c.Fields[0]; f.Field != "engine_version" || f.Old != "13.13" || f.New != "16.2" {
		t.Errorf("field change = %+v, want engine_version 13.13 → 16.2", f)
	}
}

func TestCompareIdenticalIsEmpty(t *testing.T) {
	snap := &model.Snapshot{Resources: []model.Resource{
		res("arn:a", "same", "postgres", "15.4", "available"),
	}}
	if d := Compare(snap, snap); !d.Empty() {
		t.Errorf("identical snapshots produced diff: %+v", d)
	}
}

func TestWriteRendersBuckets(t *testing.T) {
	old := &model.Snapshot{Resources: []model.Resource{
		res("arn:b", "drifted", "postgres", "13.13", "available"),
		res("arn:c", "gone", "mysql", "5.7.44", "stopped"),
	}}
	cur := &model.Snapshot{Resources: []model.Resource{
		res("arn:b", "drifted", "postgres", "16.2", "available"),
		res("arn:d", "brand-new", "mysql", "8.4.3", "creating"),
	}}
	var sb strings.Builder
	Compare(old, cur).Write(&sb, "prev.json")
	out := sb.String()
	for _, needle := range []string{
		"changes vs prev.json",
		"+1 new  ·  −1 removed  ·  ~1 changed",
		"+ brand-new (rds mysql, us-east-1)",
		"− gone (rds mysql, us-east-1)",
		"~ drifted (rds, us-east-1): engine_version 13.13 → 16.2",
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("diff output missing %q\n---\n%s", needle, out)
		}
	}
}

// The diff walks the attribute bag generically, so a key no core field knows
// about still drifts. That is what keeps this package working for scanners
// that do not exist yet — nobody has to come back here and add a case.
func TestCompareDriftsOnUnknownBagKeys(t *testing.T) {
	before := res("arn:a", "widget", "postgres", "15.4", "available")
	before.SetAttr("some_future_key", "old-value")
	after := res("arn:a", "widget", "postgres", "15.4", "available")
	after.SetAttr("some_future_key", "new-value")

	d := Compare(
		&model.Snapshot{Resources: []model.Resource{before}},
		&model.Snapshot{Resources: []model.Resource{after}},
	)
	if len(d.Changed) != 1 || len(d.Changed[0].Fields) != 1 {
		t.Fatalf("Changed = %+v, want one resource with one field", d.Changed)
	}
	if f := d.Changed[0].Fields[0]; f.Field != "some_future_key" || f.Old != "old-value" || f.New != "new-value" {
		t.Errorf("field change = %+v, want some_future_key old-value → new-value", f)
	}
}

// Every core field is compared, not just the handful the database-era diff
// happened to list. A resource whose ARN holds still while its name, scope, or
// creation time moves is drift the census watched happen — staying silent
// about it would be the same failure as not scanning at all.
func TestCompareDriftsOnEveryCoreField(t *testing.T) {
	created := time.Date(2020, 5, 7, 12, 0, 0, 0, time.UTC)
	recreated := time.Date(2026, 1, 9, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		field   string
		mutate  func(*model.Resource)
		wantOld string
		wantNew string
	}{
		// Names come from a mutable Name tag on the resource types the census is
		// growing into, so the same volume can be renamed under a stable ARN.
		{"name", func(r *model.Resource) { r.Name = "renamed" }, "widget", "renamed"},
		{"service", func(r *model.Resource) { r.Service = model.ServiceAurora }, model.ServiceRDS, model.ServiceAurora},
		{"type", func(r *model.Resource) { r.Type = model.TypeRDSCluster }, model.TypeRDSInstance, model.TypeRDSCluster},
		{"status", func(r *model.Resource) { r.Status = "stopped" }, "available", "stopped"},
		{"region", func(r *model.Resource) { r.Region = "eu-west-1" }, "us-east-1", "eu-west-1"},
		{"account_id", func(r *model.Resource) { r.AccountID = "222222222222" }, "111111111111", "222222222222"},
		{"created_at", func(r *model.Resource) { r.CreatedAt = &recreated },
			created.Format(time.RFC3339), recreated.Format(time.RFC3339)},
		{"environment", func(r *model.Resource) { r.Environment = "staging" }, "production", "staging"},
		{"owner", func(r *model.Resource) { r.Owner = "platform" }, "payments", "platform"},
		{"publicly_accessible", func(r *model.Resource) { r.PubliclyAccessible = new(true) }, "false", "true"},
		{"encrypted", func(r *model.Resource) { r.Encrypted = nil }, "true", ""},
		{"tag:app", func(r *model.Resource) { r.Tags = map[string]string{"app": "billing"} }, "orders", "billing"},
		{"tag:added", func(r *model.Resource) { r.Tags["added"] = "yes" }, "", "yes"},
	}

	base := func() model.Resource {
		r := res("arn:a", "widget", "postgres", "15.4", "available")
		r.CreatedAt = &created
		r.Environment, r.Owner = "production", "payments"
		r.PubliclyAccessible, r.Encrypted = new(false), new(true)
		r.Tags = map[string]string{"app": "orders"}
		return r
	}

	for _, c := range cases {
		before, after := base(), base()
		c.mutate(&after)

		d := Compare(
			&model.Snapshot{Resources: []model.Resource{before}},
			&model.Snapshot{Resources: []model.Resource{after}},
		)
		if len(d.Changed) != 1 {
			t.Errorf("%s: Changed = %+v, want one resource", c.field, d.Changed)
			continue
		}
		fields := d.Changed[0].Fields
		if len(fields) != 1 {
			t.Errorf("%s: got %d field changes, want 1: %+v", c.field, len(fields), fields)
			continue
		}
		if f := fields[0]; f.Field != c.field || f.Old != c.wantOld || f.New != c.wantNew {
			t.Errorf("field change = %+v, want %s %q → %q", f, c.field, c.wantOld, c.wantNew)
		}
	}
}

// Retagging the environment moves both the tag and the field derived from it,
// and the diff reports both: one is what AWS holds, the other is what the
// census concluded from it.
func TestCompareReportsTagAndDerivedFieldTogether(t *testing.T) {
	before := res("arn:a", "widget", "postgres", "15.4", "available")
	before.Tags = map[string]string{"environment": "production"}
	before.Environment = "production"
	after := res("arn:a", "widget", "postgres", "15.4", "available")
	after.Tags = map[string]string{"environment": "staging"}
	after.Environment = "staging"

	d := Compare(
		&model.Snapshot{Resources: []model.Resource{before}},
		&model.Snapshot{Resources: []model.Resource{after}},
	)
	if len(d.Changed) != 1 {
		t.Fatalf("Changed = %+v, want one resource", d.Changed)
	}
	got := map[string]string{}
	for _, f := range d.Changed[0].Fields {
		got[f.Field] = f.Old + " → " + f.New
	}
	for field, want := range map[string]string{
		"environment":     "production → staging",
		"tag:environment": "production → staging",
	} {
		if got[field] != want {
			t.Errorf("%s = %q, want %q (all: %v)", field, got[field], want, got)
		}
	}
}

// A measure the service stopped reporting must not drift against a real zero:
// "not reported" renders as an empty side, never as 0.
func TestCompareSeparatesUnreportedMeasuresFromZero(t *testing.T) {
	silent := res("arn:a", "widget", "postgres", "15.4", "available")
	zero := res("arn:a", "widget", "postgres", "15.4", "available")
	zero.SetMeasure(model.MeasureBackupRetentionDays, 0)

	d := Compare(
		&model.Snapshot{Resources: []model.Resource{silent}},
		&model.Snapshot{Resources: []model.Resource{zero}},
	)
	if len(d.Changed) != 1 || len(d.Changed[0].Fields) != 1 {
		t.Fatalf("Changed = %+v, want one resource with one field", d.Changed)
	}
	f := d.Changed[0].Fields[0]
	if f.Field != model.MeasureBackupRetentionDays || f.Old != "" || f.New != "0" {
		t.Errorf("field change = %+v, want backup_retention_days \"\" → 0", f)
	}

	// And two resources that both stay silent do not drift at all.
	if d := Compare(
		&model.Snapshot{Resources: []model.Resource{silent}},
		&model.Snapshot{Resources: []model.Resource{silent}},
	); !d.Empty() {
		t.Errorf("two unreported measures drifted against each other: %+v", d)
	}
}

func TestWriteEmpty(t *testing.T) {
	var sb strings.Builder
	Result{}.Write(&sb, "prev.json")
	if !strings.Contains(sb.String(), "no changes") {
		t.Errorf("empty diff output = %q, want it to say no changes", sb.String())
	}
}

func names(rs []model.Resource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}
