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
	if r.Endpoint != "" {
		t.Errorf("expected empty endpoint, got %q", r.Endpoint)
	}
	if r.Kind != "instance" || r.Service != model.ServiceRDS {
		t.Errorf("unexpected kind/service: %+v", r)
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
	if r.BackupRetentionDays == nil || *r.BackupRetentionDays != 0 {
		t.Error("BackupRetentionPeriod=0 not passed through")
	}

	// Fields the API did not return stay nil (tri-state honesty).
	r = instanceResource(rdstypes.DBInstance{DBInstanceIdentifier: aws.String("bare")}, "us-east-1", "1")
	if r.PubliclyAccessible != nil || r.Encrypted != nil || r.BackupRetentionDays != nil {
		t.Error("absent API fields must stay nil, not default to a value")
	}
}
