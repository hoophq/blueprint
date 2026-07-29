package cost

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/smithy-go"

	"github.com/hoophq/blueprint/internal/model"
)

// fakeCE serves canned pages and records every request it was given, so the
// tests can assert on what would have been billed as well as what came back.
type fakeCE struct {
	// pages are returned in order; the token wiring is checked against calls.
	pages []*costexplorer.GetCostAndUsageOutput
	err   error
	calls []*costexplorer.GetCostAndUsageInput
}

func (f *fakeCE) GetCostAndUsage(_ context.Context, in *costexplorer.GetCostAndUsageInput, _ ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error) {
	// Copy: Collect mutates the input between pages, and a stored pointer
	// would show every call the last page's token.
	cp := *in
	f.calls = append(f.calls, &cp)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.pages) == 0 {
		return &costexplorer.GetCostAndUsageOutput{}, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

// group builds one response group: dimension value, account, amount.
func group(name, account, amt string) cetypes.Group {
	return cetypes.Group{
		Keys: []string{name, account},
		Metrics: map[string]cetypes.MetricValue{
			"AmortizedCost": {Amount: aws.String(amt), Unit: aws.String("USD")},
		},
	}
}

// noUnit strips the currency from a group, which is how Cost Explorer reports
// an amount whose unit this tool has no name for.
func noUnit(g cetypes.Group) cetypes.Group {
	m := g.Metrics["AmortizedCost"]
	m.Unit = nil
	g.Metrics["AmortizedCost"] = m
	return g
}

func page(groups ...cetypes.Group) *costexplorer.GetCostAndUsageOutput {
	return &costexplorer.GetCostAndUsageOutput{
		ResultsByTime: []cetypes.ResultByTime{{Groups: groups}},
	}
}

func testOptions() Options {
	return Options{
		Accounts:      []string{"111111111111", "222222222222"},
		CallerAccount: "111111111111",
		Metric:        DefaultMetric,
		Window:        model.CostWindow{Start: "2026-06-01", End: "2026-07-01", Label: "2026-06"},
		MaxRequests:   DefaultMaxRequests,
	}
}

// usd returns the sole USD entry, failing if the report has any other shape.
func usd(t *testing.T, r *model.CostReport) model.CostByCurrency {
	t.Helper()
	if r == nil {
		t.Fatal("report is nil")
	}
	if len(r.Currencies) != 1 {
		t.Fatalf("got %d currencies, want 1: %+v", len(r.Currencies), r.Currencies)
	}
	if r.Currencies[0].Currency != "USD" {
		t.Fatalf("currency = %q, want USD", r.Currencies[0].Currency)
	}
	return r.Currencies[0]
}

func TestCollectPartitionsSpend(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		// Call 1: record types.
		page(
			group("Usage", "111111111111", "100.00"),
			group("Usage", "222222222222", "50.00"),
			group("Tax", "111111111111", "15.00"),
			group("Credit", "111111111111", "-20.00"),
			group("SavingsPlanRecurringFee", "111111111111", "30.00"),
		),
		// Call 2: services, already filtered to attributed record types.
		page(
			group("Amazon Relational Database Service", "111111111111", "90.00"),
			group("Amazon DynamoDB", "111111111111", "10.00"),
			group("Amazon DynamoDB", "222222222222", "50.00"),
		),
	}}

	report, failures := Collect(context.Background(), f, testOptions())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	cur := usd(t, report)

	// 100 + 50 + 15 - 20 + 30
	if cur.Total != "175.00" {
		t.Errorf("Total = %q, want 175.00", cur.Total)
	}
	if cur.Attributed != "150.00" {
		t.Errorf("Attributed = %q, want 150.00", cur.Attributed)
	}
	// 15 - 20 + 30
	if cur.Unattributed != "25.00" {
		t.Errorf("Unattributed = %q, want 25.00", cur.Unattributed)
	}
	assertPartition(t, cur)

	// A commitment fee is account-level spend: named, not folded into a
	// service and not dropped.
	wantRecords := map[string]string{"Tax": "15.00", "Credit": "-20.00", "SavingsPlanRecurringFee": "30.00"}
	if got := toMap(cur.UnattributedRecords); !sameMap(got, wantRecords) {
		t.Errorf("UnattributedRecords = %v, want %v", got, wantRecords)
	}
	wantServices := map[string]string{
		"Amazon Relational Database Service": "90.00",
		"Amazon DynamoDB":                    "60.00",
	}
	if got := toMap(cur.Services); !sameMap(got, wantServices) {
		t.Errorf("Services = %v, want %v", got, wantServices)
	}
	wantAccounts := map[string]string{"111111111111": "125.00", "222222222222": "50.00"}
	if got := toMap(cur.Accounts); !sameMap(got, wantAccounts) {
		t.Errorf("Accounts = %v, want %v", got, wantAccounts)
	}

	if report.Metric != "AmortizedCost" {
		t.Errorf("Metric = %q, want AmortizedCost", report.Metric)
	}
	if report.Meter.Requests != 2 {
		t.Errorf("Requests = %d, want 2", report.Meter.Requests)
	}
	if report.Meter.EstimatedChargeUSD != "0.02" {
		t.Errorf("EstimatedChargeUSD = %q, want 0.02", report.Meter.EstimatedChargeUSD)
	}
	if report.Meter.Capped {
		t.Error("Capped is true for a run well under budget")
	}
}

// An unrecognized record type must surface under its own name rather than
// being silently absorbed into service usage.
func TestCollectNamesUnknownRecordTypes(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(
			group("Usage", "111111111111", "10.00"),
			group("SomeFutureRecordType", "111111111111", "7.00"),
		),
		page(group("Amazon DynamoDB", "111111111111", "10.00")),
	}}
	cur := usd(t, mustCollect(t, f, testOptions()))
	if cur.Unattributed != "7.00" {
		t.Errorf("Unattributed = %q, want 7.00", cur.Unattributed)
	}
	if got := toMap(cur.UnattributedRecords)["SomeFutureRecordType"]; got != "7.00" {
		t.Errorf("unknown record type not reported by name: %v", cur.UnattributedRecords)
	}
	assertPartition(t, cur)
}

// The payer-credentials trap: Cost Explorer answers for the whole
// organization, so spend from accounts this census never scanned must not be
// counted as if it had been.
func TestCollectDropsAccountsOutsideTheCensus(t *testing.T) {
	opts := testOptions()
	opts.Accounts = []string{"111111111111"}
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(
			group("Usage", "111111111111", "10.00"),
			group("Usage", "999999999999", "9999.00"),
		),
		page(
			group("Amazon DynamoDB", "111111111111", "10.00"),
			group("Amazon DynamoDB", "999999999999", "9999.00"),
		),
	}}
	cur := usd(t, mustCollect(t, f, opts))
	if cur.Total != "10.00" {
		t.Errorf("Total = %q, want 10.00 — an unscanned account's spend leaked in", cur.Total)
	}
	if _, ok := toMap(cur.Accounts)["999999999999"]; ok {
		t.Error("an account outside the census appears in the breakdown")
	}
}

// Every request is grouped by linked account so the census restriction can be
// applied to the response, and both dimensions are used because Cost Explorer
// allows no more than two.
func TestCollectRequestShape(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{page(), page()}}
	if _, failures := Collect(context.Background(), f, testOptions()); len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if len(f.calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(f.calls))
	}
	for i, in := range f.calls {
		if len(in.GroupBy) != 2 {
			t.Fatalf("call %d: %d groupings, want 2", i, len(in.GroupBy))
		}
		if got := aws.ToString(in.GroupBy[1].Key); got != "LINKED_ACCOUNT" {
			t.Errorf("call %d: second grouping = %q, want LINKED_ACCOUNT", i, got)
		}
		if in.Granularity != cetypes.GranularityMonthly {
			t.Errorf("call %d: granularity = %q, want MONTHLY", i, in.Granularity)
		}
		if got := aws.ToString(in.TimePeriod.Start); got != "2026-06-01" {
			t.Errorf("call %d: start = %q", i, got)
		}
		// End is exclusive, so a June report ends on 1 July.
		if got := aws.ToString(in.TimePeriod.End); got != "2026-07-01" {
			t.Errorf("call %d: end = %q", i, got)
		}
		if len(in.Metrics) != 1 || in.Metrics[0] != "AmortizedCost" {
			t.Errorf("call %d: metrics = %v", i, in.Metrics)
		}
	}
	if got := aws.ToString(f.calls[0].GroupBy[0].Key); got != "RECORD_TYPE" {
		t.Errorf("first call groups by %q, want RECORD_TYPE", got)
	}
	if got := aws.ToString(f.calls[1].GroupBy[0].Key); got != "SERVICE" {
		t.Errorf("second call groups by %q, want SERVICE", got)
	}
	// Only the service call is narrowed to attributed record types; the
	// record-type call must see everything or the total would be partial.
	rec := f.calls[0].Filter
	if rec == nil || rec.Dimensions == nil || len(rec.And) != 0 {
		t.Fatalf("record-type call filter = %+v, want a single dimension", rec)
	}
	if rec.Dimensions.Key != cetypes.DimensionLinkedAccount {
		t.Errorf("record-type call filters on %q, want LINKED_ACCOUNT only", rec.Dimensions.Key)
	}
	svc := f.calls[1].Filter
	if svc == nil || len(svc.And) != 2 {
		t.Fatalf("service call filter = %+v, want two ANDed dimensions", svc)
	}
	if got := svc.And[1].Dimensions.Key; got != cetypes.DimensionRecordType {
		t.Errorf("service call second filter key = %q, want RECORD_TYPE", got)
	}
	if got := svc.And[1].Dimensions.Values; len(got) != len(attributedRecordTypes) {
		t.Errorf("service call record-type values = %v, want the attributed set", got)
	}
}

// Past the filter-value limit the request goes out unfiltered, and the
// response-side restriction is what keeps the report honest.
func TestCollectFallsBackToResponseSideAccountFilter(t *testing.T) {
	opts := testOptions()
	opts.Accounts = nil
	for i := 0; i < maxFilterAccounts+1; i++ {
		opts.Accounts = append(opts.Accounts, "acct")
	}
	opts.Accounts[0] = "111111111111"
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(group("Usage", "111111111111", "1.00"), group("Usage", "999999999999", "500.00")),
		page(group("Amazon DynamoDB", "111111111111", "1.00")),
	}}
	cur := usd(t, mustCollect(t, f, opts))
	if cur.Total != "1.00" {
		t.Errorf("Total = %q, want 1.00", cur.Total)
	}
	if f.calls[0].Filter != nil {
		t.Error("account filter was still sent past the value limit")
	}
}

func TestCollectPaginates(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		{
			ResultsByTime: []cetypes.ResultByTime{{Groups: []cetypes.Group{group("Usage", "111111111111", "1.00")}}},
			NextPageToken: aws.String("page-2"),
		},
		page(group("Usage", "111111111111", "2.00")),
		page(group("Amazon DynamoDB", "111111111111", "3.00")),
	}}
	report := mustCollect(t, f, testOptions())
	cur := usd(t, report)
	if cur.Total != "3.00" {
		t.Errorf("Total = %q, want 3.00 — a page was dropped", cur.Total)
	}
	if got := aws.ToString(f.calls[1].NextPageToken); got != "page-2" {
		t.Errorf("second call token = %q, want page-2", got)
	}
	// The token must not survive into the next query, or the service call
	// would resume the record-type query's pagination.
	if got := aws.ToString(f.calls[2].NextPageToken); got != "" {
		t.Errorf("third call carried a stale token %q", got)
	}
	// Three billed requests, three cents.
	if report.Meter.Requests != 3 || report.Meter.EstimatedChargeUSD != "0.03" {
		t.Errorf("meter = %+v, want 3 requests / 0.03", report.Meter)
	}
}

// The budget is a spending cap, so it must stop the run and say so rather
// than paginate on.
func TestCollectStopsAtRequestBudget(t *testing.T) {
	opts := testOptions()
	opts.MaxRequests = 2
	endless := func() *costexplorer.GetCostAndUsageOutput {
		return &costexplorer.GetCostAndUsageOutput{
			ResultsByTime: []cetypes.ResultByTime{{Groups: []cetypes.Group{group("Usage", "111111111111", "1.00")}}},
			NextPageToken: aws.String("more"),
		}
	}
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{endless(), endless(), endless(), endless()}}

	report, failures := Collect(context.Background(), f, opts)
	if len(f.calls) != 2 {
		t.Errorf("issued %d requests with a budget of 2 — the cap did not hold", len(f.calls))
	}
	if report == nil {
		t.Fatal("report is nil; a run that spent money must disclose it")
	}
	if !report.Meter.Capped {
		t.Error("Capped is false after the budget stopped the run")
	}
	if report.Meter.Requests != 2 || report.Meter.EstimatedChargeUSD != "0.02" {
		t.Errorf("meter = %+v, want 2 requests / 0.02", report.Meter)
	}
	assertLedger(t, failures, "ce_pagination_incomplete")
	// The pages that were read are discarded, not published. A truncated sum
	// carries no mark saying so, and reconciling one against an invoice yields
	// a gap the reader cannot tell from a real finding.
	if len(report.Currencies) != 0 {
		t.Errorf("a truncated run published amounts: %+v", report.Currencies)
	}
	if report.Estimated != nil {
		t.Error("Estimated is set with no rollup to describe")
	}
}

// Finishing on the last permitted request is a complete run. Capped means the
// budget stopped something, not that the allowance was fully used.
func TestCollectDoesNotFlagCapWhenItFinishesOnBudget(t *testing.T) {
	opts := testOptions()
	opts.MaxRequests = 2
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(group("Usage", "111111111111", "10.00")),
		page(group("Amazon DynamoDB", "111111111111", "10.00")),
	}}

	report, failures := Collect(context.Background(), f, opts)
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if report.Meter.Requests != 2 {
		t.Errorf("meter = %+v, want 2 requests", report.Meter)
	}
	if report.Meter.Capped {
		t.Error("Capped is true after a run that completed within its budget")
	}
	if cur := usd(t, report); cur.Total != "10.00" {
		t.Errorf("Total = %q, want 10.00", cur.Total)
	}
}

// The record-type call succeeding does not make a failed service call
// publishable: Attributed would stand next to an empty breakdown that no
// longer sums to it.
func TestCollectDiscardsWhenTheServiceQueryFails(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(group("Usage", "111111111111", "100.00")),
		{
			ResultsByTime: []cetypes.ResultByTime{{Groups: []cetypes.Group{
				group("Amazon DynamoDB", "111111111111", "40.00"),
			}}},
			NextPageToken: aws.String("more"),
		},
	}}
	opts := testOptions()
	opts.MaxRequests = 2

	report, failures := Collect(context.Background(), f, opts)
	assertLedger(t, failures, "ce_pagination_incomplete")
	if report == nil {
		t.Fatal("report is nil; a run that spent money must disclose it")
	}
	if len(report.Currencies) != 0 {
		t.Errorf("published a rollup built on a truncated service query: %+v", report.Currencies)
	}
	if report.Meter.Requests != 2 {
		t.Errorf("meter = %+v, want 2 requests", report.Meter)
	}
}

// A failed record-type call means the rollup is going to be discarded, so
// paying for the service call on top of it buys the user nothing.
func TestCollectDoesNotBillForAnUnusableSecondQuery(t *testing.T) {
	f := &fakeCE{err: apiErr("ThrottlingException", "Rate exceeded")}
	report, _ := Collect(context.Background(), f, testOptions())
	if len(f.calls) != 1 {
		t.Errorf("issued %d requests after the first one failed, want 1", len(f.calls))
	}
	if report.Meter.Requests != 1 {
		t.Errorf("meter = %+v, want 1 request", report.Meter)
	}
}

// A closed month stays estimated for a while after it ends. Estimated figures
// move and reconcile to no invoice, so the report says so and the ledger makes
// it visible in the terminal.
func TestCollectFlagsEstimatedData(t *testing.T) {
	estimatedPage := func(groups ...cetypes.Group) *costexplorer.GetCostAndUsageOutput {
		return &costexplorer.GetCostAndUsageOutput{
			ResultsByTime: []cetypes.ResultByTime{{Groups: groups, Estimated: true}},
		}
	}
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		estimatedPage(group("Usage", "111111111111", "10.00")),
		page(group("Amazon DynamoDB", "111111111111", "10.00")),
	}}

	report, failures := Collect(context.Background(), f, testOptions())
	assertLedger(t, failures, "ce_estimated_data")
	if report.Estimated == nil || !*report.Estimated {
		t.Errorf("Estimated = %v, want true", report.Estimated)
	}
	// Flagged, not withheld: the figures are the best AWS has and are still
	// worth reporting once the reader knows they will move.
	if cur := usd(t, report); cur.Total != "10.00" {
		t.Errorf("Total = %q, want 10.00", cur.Total)
	}
}

// Finalized data is a positive statement too, so it is written down rather
// than left to be inferred from a missing key.
func TestCollectRecordsFinalizedData(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(group("Usage", "111111111111", "10.00")),
		page(group("Amazon DynamoDB", "111111111111", "10.00")),
	}}
	report := mustCollect(t, f, testOptions())
	if report.Estimated == nil {
		t.Fatal("Estimated is nil after a published rollup")
	}
	if *report.Estimated {
		t.Error("Estimated is true for data AWS did not flag")
	}
}

func TestCollectClassifiesErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantKind string
	}{
		{"not enabled", apiErr("DataUnavailableException", "no data"), "ce_not_enabled"},
		{"not enabled via message", apiErr("AccessDeniedException", "Cost Explorer is not enabled for this account"), "ce_not_enabled"},
		{"access denied", apiErr("AccessDeniedException", "User is not authorized to perform ce:GetCostAndUsage"), "ce_access_denied"},
		{"linked account", apiErr("AccessDeniedException", "Linked account access to billing data is disabled"), "ce_linked_account_access_disabled"},
		{"throttled", apiErr("ThrottlingException", "Rate exceeded"), "ce_throttled"},
		{"bad token", apiErr("InvalidNextTokenException", "token expired"), "ce_pagination_incomplete"},
		{"unknown", errors.New("connection reset"), "ce_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCE{err: tc.err}
			report, failures := Collect(context.Background(), f, testOptions())
			assertLedger(t, failures, tc.wantKind)
			// The first call still cost a cent even though it failed.
			if report == nil {
				t.Fatal("report is nil; a failed but billed run must still disclose the charge")
			}
			if report.Meter.Requests == 0 {
				t.Error("meter reports zero requests after a billed call")
			}
			if len(report.Currencies) != 0 {
				t.Errorf("failed run reported amounts: %+v", report.Currencies)
			}
		})
	}
}

// The failure ledger's region is empty because Cost Explorer is global, and
// the account is the caller's — the failure is theirs, not the subjects'.
func TestCollectLedgerShape(t *testing.T) {
	f := &fakeCE{err: apiErr("ThrottlingException", "Rate exceeded")}
	_, failures := Collect(context.Background(), f, testOptions())
	if len(failures) == 0 {
		t.Fatal("no ledger entry")
	}
	for _, f := range failures {
		if f.Region != "" {
			t.Errorf("Region = %q, want empty for a global service", f.Region)
		}
		if f.Service != Service {
			t.Errorf("Service = %q, want %q", f.Service, Service)
		}
		if f.AccountID != "111111111111" {
			t.Errorf("AccountID = %q, want the caller's", f.AccountID)
		}
	}
}

// A metric with no unit is a currency this tool does not know. Calling it USD
// would silently add foreign amounts to dollars.
func TestCollectDoesNotAssumeUSD(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(noUnit(group("Usage", "111111111111", "10.00")), group("Usage", "222222222222", "5.00")),
		page(
			noUnit(group("Amazon DynamoDB", "111111111111", "10.00")),
			group("Amazon DynamoDB", "222222222222", "5.00"),
		),
	}}
	report := mustCollect(t, f, testOptions())
	if len(report.Currencies) != 2 {
		t.Fatalf("got %d currencies, want 2 (USD and unknown): %+v", len(report.Currencies), report.Currencies)
	}
	// Sorted, so the empty currency comes first.
	if report.Currencies[0].Currency != "" || report.Currencies[0].Total != "10.00" {
		t.Errorf("unknown-currency entry = %+v", report.Currencies[0])
	}
	if report.Currencies[1].Currency != "USD" || report.Currencies[1].Total != "5.00" {
		t.Errorf("USD entry = %+v", report.Currencies[1])
	}
}

// The service breakdown and the attributed total come from two separately
// billed queries, so the model's promise that one sums to the other spans two
// round trips against data that can move between them. Both calls succeeding
// is not enough to publish.
func TestCollectDiscardsWhenTheBreakdownsDoNotReconcile(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(group("Usage", "111111111111", "100.00")),
		// The service query saw less than the record query did.
		page(group("Amazon DynamoDB", "111111111111", "40.00")),
	}}

	report, failures := Collect(context.Background(), f, testOptions())
	assertLedger(t, failures, "ce_breakdown_mismatch")
	if len(report.Currencies) != 0 {
		t.Errorf("published a breakdown that does not sum to its own total: %+v", report.Currencies)
	}
	// The message carries both figures, because "they disagree" without
	// saying by how much leaves the reader unable to judge whether it was a
	// rounding artifact or half the bill.
	if got := failures[0].Error; !strings.Contains(got, "40.00") || !strings.Contains(got, "100.00") {
		t.Errorf("ledger entry omits the two figures that disagree: %q", got)
	}
	// Both requests were still issued and are still disclosed.
	if report.Meter.Requests != 2 {
		t.Errorf("meter = %+v, want 2 requests", report.Meter)
	}
}

// A currency in one query and not the other is a mismatch, not a currency
// that quietly checks out against nothing.
func TestCollectReconcilesAcrossCurrencies(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(group("Usage", "111111111111", "10.00")),
		page(noUnit(group("Amazon DynamoDB", "111111111111", "10.00"))),
	}}
	_, failures := Collect(context.Background(), f, testOptions())
	assertLedger(t, failures, "ce_breakdown_mismatch")
	if got := failures[0].Error; !strings.Contains(got, "unreported currency") {
		t.Errorf("ledger entry does not name the currency that failed to reconcile: %q", got)
	}
}

// Reconciling compares money, not formatting: the same amount written at
// different precision is not a discrepancy.
func TestCollectReconcilesAcrossDecimalWidths(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(group("Usage", "111111111111", "10")),
		page(group("Amazon DynamoDB", "111111111111", "10.0000000")),
	}}
	report, failures := Collect(context.Background(), f, testOptions())
	if len(failures) != 0 {
		t.Fatalf("10 and 10.0000000 were treated as different amounts: %+v", failures)
	}
	if cur := usd(t, report); cur.Attributed != "10.00" {
		t.Errorf("Attributed = %q, want 10.00", cur.Attributed)
	}
}

// Unattributed spend is not part of the promise: it has no service breakdown
// to reconcile against, so tax and credits must not look like a discrepancy.
func TestCollectDoesNotReconcileUnattributedSpend(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(
			group("Usage", "111111111111", "10.00"),
			group("Tax", "111111111111", "3.00"),
			group("Credit", "111111111111", "-1.00"),
		),
		page(group("Amazon DynamoDB", "111111111111", "10.00")),
	}}
	cur := usd(t, mustCollect(t, f, testOptions()))
	if cur.Total != "12.00" || cur.Attributed != "10.00" || cur.Unattributed != "2.00" {
		t.Errorf("partition = total %q, attributed %q, unattributed %q; want 12.00 / 10.00 / 2.00",
			cur.Total, cur.Attributed, cur.Unattributed)
	}
}

// Cost Explorer returns every metric it was asked for, including an explicit
// zero, so a group missing the metric means the metric is unavailable for this
// window — not that the group cost nothing. Skipping such groups would quietly
// understate the total, so the run reports nothing and says why.
func TestCollectFailsWhenTheMetricIsUnavailable(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(
			cetypes.Group{Keys: []string{"Usage", "111111111111"}, Metrics: map[string]cetypes.MetricValue{}},
			group("Tax", "111111111111", "1.00"),
		),
		page(),
	}}

	report, failures := Collect(context.Background(), f, testOptions())
	assertLedger(t, failures, "ce_metric_unavailable")
	if len(report.Currencies) != 0 {
		t.Errorf("published a total that is short by the groups it dropped: %+v", report.Currencies)
	}
	// The ledger entry names the alternatives rather than picking one: a
	// silently substituted metric answers a different question than the one
	// asked, under the label of the one asked.
	if got := failures[0].Error; !strings.Contains(got, "--cost-metric") {
		t.Errorf("ledger entry does not point at the flag that fixes it: %q", got)
	}
	// …but only for accounts this census covers. An organization with more
	// accounts than the request filter can carry gets an unfiltered response,
	// and aborting because some unrelated account lacks the metric would fail
	// a run over data that was never going to be reported.
	f = &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(
			cetypes.Group{Keys: []string{"Usage", "999999999999"}, Metrics: map[string]cetypes.MetricValue{}},
			group("Usage", "111111111111", "1.00"),
		),
		page(group("Amazon DynamoDB", "111111111111", "1.00")),
	}}
	cur := usd(t, mustCollect(t, f, testOptions()))
	if cur.Total != "1.00" {
		t.Errorf("Total = %q, want 1.00 from the in-scope account alone", cur.Total)
	}

	// A group present but carrying a nil amount is the same absence.
	f = &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(cetypes.Group{
			Keys:    []string{"Usage", "111111111111"},
			Metrics: map[string]cetypes.MetricValue{"AmortizedCost": {Unit: aws.String("USD")}},
		}),
	}}
	_, failures = Collect(context.Background(), f, testOptions())
	assertLedger(t, failures, "ce_metric_unavailable")
}

// A reported zero is a real finding and survives; the metric being absent is
// what makes a figure disappear.
func TestCollectKeepsReportedZero(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(group("Usage", "111111111111", "0")),
		page(group("Amazon DynamoDB", "111111111111", "0")),
	}}
	cur := usd(t, mustCollect(t, f, testOptions()))
	if got := toMap(cur.Services)["Amazon DynamoDB"]; got != "0" {
		t.Errorf("Services[Amazon DynamoDB] = %q, want the reported 0 verbatim", got)
	}
	if cur.Total != "0.00" {
		t.Errorf("Total = %q, want 0.00", cur.Total)
	}
}

// A malformed amount is refused rather than coerced: a bill is not a place to
// guess.
func TestCollectRejectsNonDecimalAmounts(t *testing.T) {
	f := &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
		page(cetypes.Group{
			Keys:    []string{"Usage", "111111111111"},
			Metrics: map[string]cetypes.MetricValue{"AmortizedCost": {Amount: aws.String("1e9"), Unit: aws.String("USD")}},
		}),
	}}
	_, failures := Collect(context.Background(), f, testOptions())
	assertLedger(t, failures, "ce_failed")
}

func TestCollectRejectsUnknownMetric(t *testing.T) {
	opts := testOptions()
	opts.Metric = "AmortizedCost" // the AWS name, not the flag value
	f := &fakeCE{}
	report, failures := Collect(context.Background(), f, opts)
	if report != nil {
		t.Error("a rejected metric produced a report")
	}
	if len(f.calls) != 0 {
		t.Error("a rejected metric still spent money")
	}
	assertLedger(t, failures, "ce_invalid_metric")
}

// No accounts means nothing to ask about — and nothing to charge for.
func TestCollectWithNoAccountsSpendsNothing(t *testing.T) {
	opts := testOptions()
	opts.Accounts = nil
	f := &fakeCE{}
	report, failures := Collect(context.Background(), f, opts)
	if report != nil || len(failures) != 0 {
		t.Errorf("got report=%v failures=%v, want nothing", report, failures)
	}
	if len(f.calls) != 0 {
		t.Error("issued a billed request with no accounts to report on")
	}
}

func TestCollectIsDeterministic(t *testing.T) {
	build := func() *fakeCE {
		return &fakeCE{pages: []*costexplorer.GetCostAndUsageOutput{
			page(
				group("Tax", "222222222222", "1.00"),
				group("Usage", "111111111111", "2.00"),
				group("Credit", "111111111111", "-1.00"),
			),
			page(
				group("Amazon Redshift", "222222222222", "1.00"),
				group("Amazon DynamoDB", "111111111111", "1.00"),
			),
		}}
	}
	a := mustCollect(t, build(), testOptions())
	b := mustCollect(t, build(), testOptions())
	if got, want := jsonish(a), jsonish(b); got != want {
		t.Errorf("two identical runs produced different reports:\n%s\n%s", got, want)
	}
	cur := usd(t, a)
	assertSorted(t, "Services", cur.Services)
	assertSorted(t, "UnattributedRecords", cur.UnattributedRecords)
	assertSorted(t, "Accounts", cur.Accounts)
}

func TestLastFullMonth(t *testing.T) {
	for _, tc := range []struct {
		now              string
		start, end, name string
	}{
		{"2026-07-28T13:45:00Z", "2026-06-01", "2026-07-01", "2026-06"},
		// The first instant of a month still reports the month before it.
		{"2026-07-01T00:00:00Z", "2026-06-01", "2026-07-01", "2026-06"},
		{"2026-07-31T23:59:59Z", "2026-06-01", "2026-07-01", "2026-06"},
		// Year boundary.
		{"2026-01-15T00:00:00Z", "2025-12-01", "2026-01-01", "2025-12"},
		// March back to February, and a leap February at that: naive
		// day-arithmetic lands on 3 March instead of 1 February.
		{"2024-03-31T12:00:00Z", "2024-02-01", "2024-03-01", "2024-02"},
		{"2026-03-31T12:00:00Z", "2026-02-01", "2026-03-01", "2026-02"},
	} {
		now, err := time.Parse(time.RFC3339, tc.now)
		if err != nil {
			t.Fatal(err)
		}
		got := LastFullMonth(now)
		if got.Start != tc.start || got.End != tc.end || got.Label != tc.name {
			t.Errorf("LastFullMonth(%s) = %+v, want %s..%s %q", tc.now, got, tc.start, tc.end, tc.name)
		}
	}
}

// A non-UTC clock must not shift the window: a scan run at 21:00 in New York
// on 30 June and one run at 09:00 in Tokyo on 1 July are the same instant and
// must produce the same report.
func TestLastFullMonthUsesUTC(t *testing.T) {
	instant := time.Date(2026, 7, 1, 0, 30, 0, 0, time.UTC)
	utc := LastFullMonth(instant)
	for _, name := range []string{"America/New_York", "Asia/Tokyo", "Pacific/Kiritimati"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skipf("no tzdata for %s", name)
		}
		if got := LastFullMonth(instant.In(loc)); got != utc {
			t.Errorf("LastFullMonth in %s = %+v, want %+v", name, got, utc)
		}
	}
}

func TestMetricsAndValidMetric(t *testing.T) {
	if !ValidMetric(DefaultMetric) {
		t.Fatalf("DefaultMetric %q is not a valid metric", DefaultMetric)
	}
	for _, m := range Metrics() {
		if !ValidMetric(m) {
			t.Errorf("Metrics() lists %q but ValidMetric rejects it", m)
		}
	}
	for _, bad := range []string{"", "AmortizedCost", "Amortized", "usage"} {
		if ValidMetric(bad) {
			t.Errorf("ValidMetric(%q) = true", bad)
		}
	}
}

// The Cost Explorer client must not inherit the shared config's retryer: on a
// per-request-billed API, a silent retry multiplies the user's charge and
// makes the meter a lower bound instead of a count.
func TestClientDisablesRetriesAndPinsRegion(t *testing.T) {
	shared := aws.Config{
		Region:  "eu-west-1",
		Retryer: func() aws.Retryer { return retryerWithAttempts{8} },
	}
	c := Client(shared)
	opts := c.Options()
	if opts.Region != ceRegion {
		t.Errorf("region = %q, want %q — Cost Explorer is global", opts.Region, ceRegion)
	}
	if got := opts.Retryer.MaxAttempts(); got != 1 {
		t.Errorf("MaxAttempts = %d, want 1", got)
	}
	// The caller's own config must be left alone.
	if shared.Region != "eu-west-1" {
		t.Errorf("Client mutated the shared config's region to %q", shared.Region)
	}
	if got := shared.Retryer().MaxAttempts(); got != 8 {
		t.Errorf("Client mutated the shared config's retryer to %d attempts", got)
	}
}

// --- helpers ---

type retryerWithAttempts struct{ n int }

func (r retryerWithAttempts) IsErrorRetryable(error) bool { return false }
func (r retryerWithAttempts) MaxAttempts() int            { return r.n }
func (r retryerWithAttempts) RetryDelay(int, error) (time.Duration, error) {
	return 0, nil
}
func (r retryerWithAttempts) GetRetryToken(context.Context, error) (func(error) error, error) {
	return func(error) error { return nil }, nil
}
func (r retryerWithAttempts) GetInitialToken() func(error) error {
	return func(error) error { return nil }
}

func apiErr(code, msg string) error {
	return &smithy.GenericAPIError{Code: code, Message: msg}
}

func mustCollect(t *testing.T, f *fakeCE, opts Options) *model.CostReport {
	t.Helper()
	report, failures := Collect(context.Background(), f, opts)
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if report == nil {
		t.Fatal("report is nil")
	}
	return report
}

func assertLedger(t *testing.T, failures []model.Failure, kind string) {
	t.Helper()
	for _, f := range failures {
		if strings.HasPrefix(f.Error, kind+": ") {
			return
		}
	}
	t.Errorf("no ledger entry of kind %q in %+v", kind, failures)
}

// assertPartition re-checks the invariant the type documents, with exact
// arithmetic rather than by trusting the builder that produced it.
func assertPartition(t *testing.T, cur model.CostByCurrency) {
	t.Helper()
	a, err := parseAmount(cur.Attributed)
	if err != nil {
		t.Fatalf("Attributed %q: %v", cur.Attributed, err)
	}
	u, err := parseAmount(cur.Unattributed)
	if err != nil {
		t.Fatalf("Unattributed %q: %v", cur.Unattributed, err)
	}
	total, err := parseAmount(cur.Total)
	if err != nil {
		t.Fatalf("Total %q: %v", cur.Total, err)
	}
	if got := new(big.Rat).Add(a.rat, u.rat); got.Cmp(total.rat) != 0 {
		t.Errorf("Attributed + Unattributed = %v, want Total %v", got, total.rat)
	}
	if got := sumNamed(t, cur.Services); got.Cmp(a.rat) != 0 {
		t.Errorf("Services sum to %v, want Attributed %v", got, a.rat)
	}
	if got := sumNamed(t, cur.UnattributedRecords); got.Cmp(u.rat) != 0 {
		t.Errorf("UnattributedRecords sum to %v, want Unattributed %v", got, u.rat)
	}
	if got := sumNamed(t, cur.Accounts); got.Cmp(total.rat) != 0 {
		t.Errorf("Accounts sum to %v, want Total %v", got, total.rat)
	}
}

func assertSorted(t *testing.T, field string, n []model.NamedAmount) {
	t.Helper()
	for i := 1; i < len(n); i++ {
		if n[i-1].Name > n[i].Name {
			t.Errorf("%s is not sorted: %q before %q", field, n[i-1].Name, n[i].Name)
		}
	}
}

func toMap(n []model.NamedAmount) map[string]string {
	out := map[string]string{}
	for _, a := range n {
		out[a.Name] = a.Amount
	}
	return out
}

// sameMap compares by numeric value, so a test does not fail merely because
// a figure came back as "60.00" instead of "60".
func sameMap(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			return false
		}
		gr, ok1 := new(big.Rat).SetString(g)
		wr, ok2 := new(big.Rat).SetString(w)
		if !ok1 || !ok2 || gr.Cmp(wr) != 0 {
			return false
		}
	}
	return true
}

func sumNamed(t *testing.T, n []model.NamedAmount) *big.Rat {
	t.Helper()
	total := new(big.Rat)
	for _, a := range n {
		parsed, err := parseAmount(a.Amount)
		if err != nil {
			t.Fatalf("%s = %q: %v", a.Name, a.Amount, err)
		}
		total.Add(total, parsed.rat)
	}
	return total
}

// Snapshot.GeneratedAt is stamped when the run starts, so it bounds a cost
// failure only from below. These calls are billed and rate-limited, and
// "sometime after the scan began" does not line up against a CloudTrail entry
// or a throttling window — so each entry carries the instant it was recorded.
func TestLedgerEntriesAreTimestamped(t *testing.T) {
	before := time.Now().UTC()
	f := &fakeCE{err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "no"}}

	_, failures := Collect(context.Background(), f, testOptions())
	if len(failures) == 0 {
		t.Fatal("a failed lookup produced no ledger entry")
	}
	after := time.Now().UTC()

	for _, fail := range failures {
		if fail.Time.IsZero() {
			t.Errorf("ledger entry %q carries no time", fail.Error)
			continue
		}
		if fail.Time.Before(before) || fail.Time.After(after) {
			t.Errorf("ledger entry timed %v, outside the run's own window [%v, %v]",
				fail.Time, before, after)
		}
		// UTC throughout the artifact, like GeneratedAt: a ledger a reader has
		// to timezone-correct before comparing to CloudTrail is a trap.
		if name, _ := fail.Time.Zone(); name != "UTC" {
			t.Errorf("ledger entry timed in %s, want UTC", name)
		}
	}
}

// jsonish renders a report the way the artifact would, so a determinism test
// compares what actually gets written rather than Go's struct printing.
func jsonish(r *model.CostReport) string {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "marshal error: " + err.Error()
	}
	return string(b)
}
