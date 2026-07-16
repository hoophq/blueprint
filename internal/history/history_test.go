package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoophq/blueprint/internal/model"
)

func snap(genAt time.Time, accounts, regions []string, names ...string) *model.Snapshot {
	s := &model.Snapshot{
		Version:     "test",
		GeneratedAt: genAt,
		Accounts:    accounts,
		Regions:     regions,
	}
	for _, n := range names {
		s.Resources = append(s.Resources, model.Resource{
			ARN: "arn:aws:rds:us-east-1:1:db:" + n, Name: n,
			Service: model.ServiceRDS, Kind: "instance", Engine: "postgres",
			Region: "us-east-1", AccountID: "1",
		})
	}
	return s
}

func TestScopeKeyBucketsByAccountsAndRegions(t *testing.T) {
	a := snap(time.Now(), []string{"1", "2"}, []string{"us-east-1"})
	b := snap(time.Now(), []string{"2", "1"}, []string{"us-east-1"}) // order must not matter
	c := snap(time.Now(), []string{"1"}, []string{"us-east-1"})
	d := snap(time.Now(), []string{"1", "2"}, []string{"us-east-1", "eu-west-1"})
	if ScopeKey(a) != ScopeKey(b) {
		t.Error("account order changed the scope key")
	}
	if ScopeKey(a) == ScopeKey(c) {
		t.Error("different accounts produced the same scope key")
	}
	if ScopeKey(a) == ScopeKey(d) {
		t.Error("different regions produced the same scope key")
	}
}

func TestSaveAndLatestRoundTrip(t *testing.T) {
	root := t.TempDir()
	acct := []string{"111111111111"}
	reg := []string{"us-east-1"}

	if prev, err := Latest(root, ScopeKey(snap(time.Now(), acct, reg))); err != nil || prev != nil {
		t.Fatalf("Latest on empty bucket = (%v, %v), want (nil, nil)", prev, err)
	}

	older := snap(time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC), acct, reg, "orders")
	newer := snap(time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC), acct, reg, "orders", "billing")
	for _, s := range []*model.Snapshot{older, newer} {
		if _, err := Save(root, s); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	got, err := Latest(root, ScopeKey(newer))
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(got.Resources) != 2 || !got.GeneratedAt.Equal(newer.GeneratedAt) {
		t.Errorf("Latest returned %d resources at %v, want the newer snapshot (2 at %v)",
			len(got.Resources), got.GeneratedAt, newer.GeneratedAt)
	}
}

func TestSavePrunesBeyondKeep(t *testing.T) {
	root := t.TempDir()
	acct := []string{"111111111111"}
	reg := []string{"us-east-1"}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < keepPerScope+5; i++ {
		if _, err := Save(root, snap(base.Add(time.Duration(i)*time.Hour), acct, reg)); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	files, err := scopeFiles(root, ScopeKey(snap(base, acct, reg)))
	if err != nil {
		t.Fatalf("scopeFiles: %v", err)
	}
	if len(files) != keepPerScope {
		t.Errorf("bucket has %d files after pruning, want %d", len(files), keepPerScope)
	}
	// The survivors must be the newest ones.
	latest, err := Latest(root, ScopeKey(snap(base, acct, reg)))
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	want := base.Add(time.Duration(keepPerScope+4) * time.Hour)
	if !latest.GeneratedAt.Equal(want) {
		t.Errorf("latest after pruning is %v, want %v", latest.GeneratedAt, want)
	}
}

func TestDirEnvOverride(t *testing.T) {
	t.Setenv("BLUEPRINT_HISTORY_DIR", filepath.Join(os.TempDir(), "bp-hist-test"))
	d, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if d != filepath.Join(os.TempDir(), "bp-hist-test") {
		t.Errorf("Dir = %q, want the env override", d)
	}
}
