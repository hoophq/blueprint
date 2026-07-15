package scanners

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/hoophq/dbcensus/internal/model"
)

func TestBytesToGB(t *testing.T) {
	const gb = int64(1 << 30)
	cases := map[int64]int32{
		0:          0,
		-1:         0,
		1:          1, // any non-empty table registers at least 1 GB
		gb - 1:     1,
		gb:         1,
		gb + 1:     2,
		5 * gb:     5,
		5*gb + 512: 6,
	}
	for bytes, want := range cases {
		if got := bytesToGB(bytes); got != want {
			t.Errorf("bytesToGB(%d) = %d, want %d", bytes, got, want)
		}
	}
}

func TestBillingMode(t *testing.T) {
	if got := billingMode(nil); got != "PROVISIONED" {
		t.Errorf("billingMode(nil) = %q, want PROVISIONED", got)
	}
	if got := billingMode(&ddbtypes.BillingModeSummary{}); got != "PROVISIONED" {
		t.Errorf("billingMode(empty) = %q, want PROVISIONED", got)
	}
	summary := &ddbtypes.BillingModeSummary{BillingMode: ddbtypes.BillingModePayPerRequest}
	if got := billingMode(summary); got != "PAY_PER_REQUEST" {
		t.Errorf("billingMode(pay-per-request) = %q, want PAY_PER_REQUEST", got)
	}
}

func TestTableResource(t *testing.T) {
	created := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	desc := ddbtypes.TableDescription{
		TableArn:           aws.String("arn:aws:dynamodb:us-east-1:123456789012:table/orders"),
		TableName:          aws.String("orders"),
		TableSizeBytes:     aws.Int64(3<<30 + 1),
		TableStatus:        ddbtypes.TableStatusActive,
		CreationDateTime:   &created,
		BillingModeSummary: &ddbtypes.BillingModeSummary{BillingMode: ddbtypes.BillingModePayPerRequest},
	}
	tags := map[string]string{"env": "prod"}
	r := tableResource(desc, tags, "us-east-1", "123456789012")

	if r.ARN != "arn:aws:dynamodb:us-east-1:123456789012:table/orders" {
		t.Errorf("unexpected ARN: %q", r.ARN)
	}
	if r.Service != model.ServiceDynamoDB || r.Kind != "table" || r.Engine != "dynamodb" {
		t.Errorf("unexpected service/kind/engine: %+v", r)
	}
	if r.Name != "orders" || r.Status != "ACTIVE" {
		t.Errorf("unexpected name/status: %+v", r)
	}
	if r.StorageGB != 4 {
		t.Errorf("StorageGB = %d, want 4 (rounded up)", r.StorageGB)
	}
	if r.InstanceClass != "PAY_PER_REQUEST" {
		t.Errorf("InstanceClass = %q, want PAY_PER_REQUEST", r.InstanceClass)
	}
	if r.CreatedAt == nil || !r.CreatedAt.Equal(created) {
		t.Errorf("unexpected CreatedAt: %v", r.CreatedAt)
	}
	if r.Tags["env"] != "prod" {
		t.Errorf("unexpected tags: %v", r.Tags)
	}
}
