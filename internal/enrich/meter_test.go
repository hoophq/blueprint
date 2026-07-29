package enrich

import (
	"errors"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"

	"github.com/hoophq/blueprint/internal/model"
)

// The billed unit is the metric requested, not the call it rode in on. A user
// checking this figure against their bill needs it to be the series count.
func TestMeterCountsSeriesNotCalls(t *testing.T) {
	const total = 1201
	names := make([]string, total)
	resources := make([]model.Resource, total)
	for i := range resources {
		names[i] = "db" + strconv.Itoa(i)
		resources[i] = instance("111111111111", "us-east-1", names[i])
	}
	api := &fakeCW{listFn: knows(names...)}

	m, _ := run(t, api, resources)

	got := m.Meter()
	if got.Series != total {
		t.Errorf("Series = %d, want %d", got.Series, total)
	}
	if got.GetCalls != 3 {
		t.Errorf("GetCalls = %d, want 3", got.GetCalls)
	}
	// Discovery is free and reported apart from the billed count, so a reader
	// cannot mistake it for spend.
	if got.DiscoverCalls != 1 {
		t.Errorf("DiscoverCalls = %d, want 1", got.DiscoverCalls)
	}
}

// A call AWS refused is not a call AWS charged for. Counting it would inflate
// the one number the user reconciles against their bill — and the gap is
// already in the ledger.
func TestMeterSkipsFailedCalls(t *testing.T) {
	api := &fakeCW{
		listFn: knows("db1"),
		getFn: func(*cloudwatch.GetMetricDataInput) (*cloudwatch.GetMetricDataOutput, error) {
			return nil, errors.New("ThrottlingException: rate exceeded")
		},
	}
	resources := []model.Resource{instance("111111111111", "us-east-1", "db1")}

	m, failures := run(t, api, resources)

	if len(failures) != 1 {
		t.Fatalf("got %d ledger entries, want 1: %+v", len(failures), failures)
	}
	got := m.Meter()
	if got.Series != 0 || got.GetCalls != 0 {
		t.Errorf("meter = %+v, want no billed series or calls", got)
	}
}

// Discovery filtering shrinks the batch, and the meter has to shrink with it —
// the point of paying for ListMetrics is paying less for GetMetricData.
func TestMeterReflectsDiscoveryFiltering(t *testing.T) {
	api := &fakeCW{listFn: knows("db1")}
	resources := []model.Resource{
		instance("111111111111", "us-east-1", "db1"),
		instance("111111111111", "us-east-1", "db2"),
	}

	m, _ := run(t, api, resources)

	if got := m.Meter().Series; got != 1 {
		t.Errorf("Series = %d, want 1 — db2 has no series to buy", got)
	}
}

func TestMeterIsZeroWhenNothingWasQueried(t *testing.T) {
	api := &fakeCW{}
	m, _ := run(t, api, nil)

	if got := (m.Meter()); got != (Meter{}) {
		t.Errorf("meter = %+v, want zero", got)
	}
}

func TestChargeUSD(t *testing.T) {
	tests := []struct {
		metrics int
		want    string
	}{
		// The figure the CLI prints after a small scan. Rendering it as "0.01"
		// would overstate the charge by nearly a cent; rendering it as "0.00"
		// would claim the call was free.
		{1200, "0.012"},
		{100_000, "1.00"},
		{500, "0.005"},
		// A single metric costs a thousandth of a cent, and saying so is more
		// honest than rounding it to nothing.
		{1, "0.00001"},
		{0, "0.00"},
		{999, "0.00999"},
		{1000, "0.01"},
		{250_000, "2.50"},
		{123_456, "1.23456"},
	}
	for _, tt := range tests {
		if got := ChargeUSD(tt.metrics); got != tt.want {
			t.Errorf("ChargeUSD(%d) = %q, want %q", tt.metrics, got, tt.want)
		}
	}
}

// The meter is written from every scope's goroutine, so the race detector has
// to see them overlap on it.
func TestMeterIsSafeAcrossScopes(t *testing.T) {
	var resources []model.Resource
	for _, region := range []string{"us-east-1", "us-west-2", "eu-west-1", "sa-east-1"} {
		resources = append(resources, instance("111111111111", region, "db1"))
	}
	api := &fakeCW{listFn: knows("db1")}

	m, _ := run(t, api, resources)

	if got := m.Meter().Series; got != len(resources) {
		t.Errorf("Series = %d, want %d", got, len(resources))
	}
}
