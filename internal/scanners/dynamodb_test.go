package scanners

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/hoophq/blueprint/internal/model"
)

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
	if r.Service != model.ServiceDynamoDB || r.Type != model.TypeDynamoDBTable {
		t.Errorf("unexpected service/type: %+v", r)
	}
	if got := r.Attr(model.AttrEngine); got != "dynamodb" {
		t.Errorf("engine = %q, want dynamodb", got)
	}
	if r.Name != "orders" || r.Status != "ACTIVE" {
		t.Errorf("unexpected name/status: %+v", r)
	}
	// Exact bytes, not a rounded GB figure: DynamoDB reports the real number
	// and rounding it up made every non-empty table look like at least 1 GB.
	if got, ok := r.Measure(model.MeasureSizeBytes); !ok || got != 3<<30+1 {
		t.Errorf("size_bytes = (%d, %v), want (%d, true)", got, ok, 3<<30+1)
	}
	// Billing mode is not an instance class — it gets its own key so the two
	// concepts never masquerade as each other.
	if got := r.Attr(model.AttrBillingMode); got != "PAY_PER_REQUEST" {
		t.Errorf("billing_mode = %q, want PAY_PER_REQUEST", got)
	}
	if got := r.Attr(model.AttrInstanceClass); got != "" {
		t.Errorf("instance_class = %q, want absent (DynamoDB has no instances)", got)
	}
	if r.CreatedAt == nil || !r.CreatedAt.Equal(created) {
		t.Errorf("unexpected CreatedAt: %v", r.CreatedAt)
	}
	if r.Tags["env"] != "prod" {
		t.Errorf("unexpected tags: %v", r.Tags)
	}
}

// Zero bytes is something DynamoDB says, not something it declines to say.
// The figure refreshes roughly every six hours, so a zero can mean an empty
// table or one too new to have been measured — the census cannot tell which,
// and the honest record is the number the service returned. Dropping the key
// would claim DynamoDB reported nothing, which is a different and false
// statement, and it would hide every genuinely empty table.
func TestTableResourceKeepsReportedZeroSize(t *testing.T) {
	sized := func(size *int64) model.Resource {
		return tableResource(ddbtypes.TableDescription{
			TableArn:       aws.String("arn:aws:dynamodb:us-east-1:123456789012:table/t"),
			TableName:      aws.String("t"),
			TableSizeBytes: size,
		}, nil, "us-east-1", "123456789012")
	}

	r := sized(aws.Int64(0))
	got, ok := r.Measure(model.MeasureSizeBytes)
	if !ok || got != 0 {
		t.Errorf("size_bytes = (%d, %v) for a reported zero, want (0, true)", got, ok)
	}

	// And a description that carries no size at all stays key-absent, so the
	// two remain distinguishable in the artifact.
	silent := sized(nil)
	if v, ok := silent.Measure(model.MeasureSizeBytes); ok {
		t.Errorf("size_bytes = (%d, true) with no TableSizeBytes, want not reported", v)
	}
}
