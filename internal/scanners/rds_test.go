package scanners

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"

	"github.com/hoophq/blueprint/internal/model"
)

func TestClassifyEngine(t *testing.T) {
	cases := map[string]string{
		"aurora-postgresql": model.ServiceAurora,
		"aurora-mysql":      model.ServiceAurora,
		"docdb":             model.ServiceDocumentDB,
		"neptune":           model.ServiceNeptune,
		"postgres":          model.ServiceRDS,
		"mysql":             model.ServiceRDS,
		"sqlserver-ee":      model.ServiceRDS,
		"oracle-se2":        model.ServiceRDS,
		"mariadb":           model.ServiceRDS,
	}
	for engine, want := range cases {
		if got := classifyEngine(engine); got != want {
			t.Errorf("classifyEngine(%q) = %q, want %q", engine, got, want)
		}
	}
}

func TestInstanceResourceSkipsNilEndpoint(t *testing.T) {
	inst := rdstypes.DBInstance{
		DBInstanceArn:        aws.String("arn:aws:rds:us-east-1:1:db:x"),
		DBInstanceIdentifier: aws.String("x"),
		Engine:               aws.String("postgres"),
	}
	r := instanceResource(inst, "us-east-1", "1")
	if got := r.Attr(model.AttrEndpoint); got != "" {
		t.Errorf("expected no endpoint attribute, got %q", got)
	}
	if r.Type != model.TypeRDSInstance || r.Service != model.ServiceRDS {
		t.Errorf("unexpected type/service: %+v", r)
	}
}

// The RDS control plane serves four services; the CloudFormation type must
// name the one the resource actually belongs to, not the shared API.
func TestRDSTypesFollowTheEngine(t *testing.T) {
	cases := []struct {
		engine       string
		wantInstance string
		wantCluster  string
	}{
		{"postgres", model.TypeRDSInstance, model.TypeRDSCluster},
		{"aurora-mysql", model.TypeRDSInstance, model.TypeRDSCluster},
		{"docdb", model.TypeDocDBInstance, model.TypeDocDBCluster},
		{"neptune", model.TypeNeptuneInstance, model.TypeNeptuneCluster},
	}
	for _, c := range cases {
		inst := instanceResource(rdstypes.DBInstance{
			DBInstanceIdentifier: aws.String("i"), Engine: aws.String(c.engine),
		}, "us-east-1", "1")
		if inst.Type != c.wantInstance {
			t.Errorf("instance %q type = %q, want %q", c.engine, inst.Type, c.wantInstance)
		}
		cl := clusterResource(rdstypes.DBCluster{
			DBClusterIdentifier: aws.String("c"), Engine: aws.String(c.engine),
		}, "us-east-1", "1")
		if cl.Type != c.wantCluster {
			t.Errorf("cluster %q type = %q, want %q", c.engine, cl.Type, c.wantCluster)
		}
	}
}

// AllocatedStorage is reported in GiB; the census stores exact bytes so every
// service's size is comparable in one column.
func TestInstanceResourceStorageInBytes(t *testing.T) {
	r := instanceResource(rdstypes.DBInstance{
		DBInstanceIdentifier: aws.String("sized"),
		Engine:               aws.String("mysql"),
		AllocatedStorage:     aws.Int32(100),
	}, "us-east-1", "1")
	got, ok := r.Measure(model.MeasureSizeBytes)
	if !ok || got != 100*1024*1024*1024 {
		t.Errorf("size_bytes = (%d, %v), want (%d, true)", got, ok, 100*1024*1024*1024)
	}

	// Unreported storage (Aurora, serverless) must leave the key absent rather
	// than claim a 0-byte database.
	r = instanceResource(rdstypes.DBInstance{
		DBInstanceIdentifier: aws.String("aurora-member"),
		Engine:               aws.String("aurora-mysql"),
	}, "us-east-1", "1")
	if v, ok := r.Measure(model.MeasureSizeBytes); ok {
		t.Errorf("size_bytes = (%d, true) with no AllocatedStorage, want not reported", v)
	}
}

func TestRdsTagKV(t *testing.T) {
	m := toTagMap([]rdstypes.Tag{{Key: aws.String("env"), Value: aws.String("prod")}}, rdsTagKV)
	if m["env"] != "prod" {
		t.Errorf("unexpected tag map: %v", m)
	}
}

func TestInstanceResourceExposureFields(t *testing.T) {
	pub, enc := true, false
	days := int32(0)
	inst := rdstypes.DBInstance{
		DBInstanceIdentifier:  aws.String("exposed-db"),
		Engine:                aws.String("mysql"),
		PubliclyAccessible:    &pub,
		StorageEncrypted:      &enc,
		BackupRetentionPeriod: &days,
	}
	r := instanceResource(inst, "us-east-1", "1")
	if r.PubliclyAccessible == nil || !*r.PubliclyAccessible {
		t.Error("PubliclyAccessible not passed through")
	}
	if r.Encrypted == nil || *r.Encrypted {
		t.Error("StorageEncrypted=false not passed through")
	}
	if v, ok := r.Measure(model.MeasureBackupRetentionDays); !ok || v != 0 {
		t.Errorf("BackupRetentionPeriod=0 not passed through: (%d, %v)", v, ok)
	}
	if !r.Exposed() {
		t.Error("public + unencrypted + no backups must read as exposed")
	}

	// Fields the API did not return stay unreported (tri-state honesty).
	r = instanceResource(rdstypes.DBInstance{DBInstanceIdentifier: aws.String("bare")}, "us-east-1", "1")
	if r.PubliclyAccessible != nil || r.Encrypted != nil {
		t.Error("absent API fields must stay nil, not default to a value")
	}
	if _, ok := r.Measure(model.MeasureBackupRetentionDays); ok {
		t.Error("absent BackupRetentionPeriod must leave the measure absent, not 0")
	}
	if r.Exposed() {
		t.Error("a resource that reported nothing must not read as exposed")
	}
}
