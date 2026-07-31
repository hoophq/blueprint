package enrich

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costoptimizationhub"
	cohtypes "github.com/aws/aws-sdk-go-v2/service/costoptimizationhub/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

const (
	dbARN    = "arn:aws:rds:us-east-1:111122223333:db:orders"
	cacheARN = "arn:aws:elasticache:us-east-1:111122223333:cluster:sessions"
)

type fakeCOH struct {
	enrollFn func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error)
	listFn   func(*costoptimizationhub.ListRecommendationsInput) (*costoptimizationhub.ListRecommendationsOutput, error)
	enrolls  []*costoptimizationhub.ListEnrollmentStatusesInput
	lists    []*costoptimizationhub.ListRecommendationsInput
}

func (f *fakeCOH) ListEnrollmentStatuses(_ context.Context, in *costoptimizationhub.ListEnrollmentStatusesInput, _ ...func(*costoptimizationhub.Options)) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
	f.enrolls = append(f.enrolls, in)
	if f.enrollFn != nil {
		return f.enrollFn(in)
	}
	return active(nil), nil
}

func (f *fakeCOH) ListRecommendations(_ context.Context, in *costoptimizationhub.ListRecommendationsInput, _ ...func(*costoptimizationhub.Options)) (*costoptimizationhub.ListRecommendationsOutput, error) {
	f.lists = append(f.lists, in)
	if f.listFn != nil {
		return f.listFn(in)
	}
	return &costoptimizationhub.ListRecommendationsOutput{}, nil
}

// active builds an enrollment response saying the hub is on.
func active(includeMembers *bool) *costoptimizationhub.ListEnrollmentStatusesOutput {
	return &costoptimizationhub.ListEnrollmentStatusesOutput{
		IncludeMemberAccounts: includeMembers,
		Items: []cohtypes.AccountEnrollmentStatus{{
			AccountId: aws.String("111122223333"),
			Status:    cohtypes.EnrollmentStatusActive,
		}},
	}
}

// recs builds a one-page ListRecommendations response.
func recs(items ...cohtypes.Recommendation) func(*costoptimizationhub.ListRecommendationsInput) (*costoptimizationhub.ListRecommendationsOutput, error) {
	return func(*costoptimizationhub.ListRecommendationsInput) (*costoptimizationhub.ListRecommendationsOutput, error) {
		return &costoptimizationhub.ListRecommendationsOutput{Items: items}, nil
	}
}

// priced is a recommendation for one ARN at one cost, refreshed at scanTime
// over a 14-day lookback.
func priced(arn string, monthly float64) cohtypes.Recommendation {
	return cohtypes.Recommendation{
		AccountId:                          aws.String("111122223333"),
		Region:                             aws.String("us-east-1"),
		ResourceArn:                        aws.String(arn),
		CurrentResourceType:                aws.String("RdsDbInstance"),
		CurrencyCode:                       aws.String("USD"),
		EstimatedMonthlyCost:               aws.Float64(monthly),
		LastRefreshTimestamp:               aws.Time(scanTime),
		RecommendationLookbackPeriodInDays: aws.Int32(14),
	}
}

// census is a two-resource snapshot to attach costs to.
func census() []model.Resource {
	return []model.Resource{
		{ARN: dbARN, AccountID: "111122223333", Region: "us-east-1", Type: "AWS::RDS::DBInstance", Name: "orders"},
		{ARN: cacheARN, AccountID: "111122223333", Region: "us-east-1", Type: "AWS::ElastiCache::CacheCluster", Name: "sessions"},
	}
}

func newHub(f *fakeCOH) *CostHub {
	return &CostHub{
		Account:   "111122223333",
		NewClient: func(aws.Config) CostHubAPI { return f },
		Now:       func() time.Time { return scanTime },
	}
}

// runHub drives one enrichment over the given census and returns it enriched.
func runHub(t *testing.T, h *CostHub, resources []model.Resource) ([]model.Resource, []model.Failure) {
	t.Helper()
	failures := h.Enrich(context.Background(), scan.Enrichment{Resources: resources, Concurrency: 4})
	return resources, failures
}

// cohCost returns the hub's figure for a resource, or nil if it did not price
// it. Every assertion in this file is about the hub, so it reads the one method
// this enricher writes rather than whatever happens to be first in the list —
// a later pass adding a Cost Explorer figure must not make these tests pass or
// fail for a reason that has nothing to do with the hub.
func cohCost(r model.Resource) *model.ResourceCost {
	return r.CostBy(model.CostMethodCOH)
}

// ledgered returns the first failure whose text carries the given kind prefix.
func ledgered(t *testing.T, failures []model.Failure, kind string) model.Failure {
	t.Helper()
	for _, f := range failures {
		if strings.HasPrefix(f.Error, kind+":") {
			return f
		}
	}
	t.Fatalf("no %s entry in ledger: %+v", kind, failures)
	return model.Failure{}
}

func TestRecommendationBecomesAResourceCost(t *testing.T) {
	f := &fakeCOH{listFn: recs(priced(dbARN, 412.5))}
	got, failures := runHub(t, newHub(f), census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	c := cohCost(got[0])
	if c == nil {
		t.Fatal("the priced resource came away with no cost")
	}
	if c.Amount != "412.50" {
		t.Errorf("amount = %q, want 412.50", c.Amount)
	}
	if c.Currency != "USD" {
		t.Errorf("currency = %q, want USD", c.Currency)
	}
	if c.Method != model.CostMethodCOH {
		t.Errorf("method = %q, want %q", c.Method, model.CostMethodCOH)
	}
	if !c.Estimated {
		t.Error("a modelled monthly rate must be marked estimated")
	}
	if cohCost(got[1]) != nil {
		t.Errorf("the unpriced resource must stay unpriced, got %+v", cohCost(got[1]))
	}
}

func TestUnenrolledAccountIsLedgeredRatherThanSilentlyEmpty(t *testing.T) {
	f := &fakeCOH{
		enrollFn: func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
			return &costoptimizationhub.ListEnrollmentStatusesOutput{Items: []cohtypes.AccountEnrollmentStatus{{
				AccountId: aws.String("111122223333"),
				Status:    cohtypes.EnrollmentStatusInactive,
			}}}, nil
		},
		listFn: recs(priced(dbARN, 10)),
	}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_not_enrolled")
	if len(f.lists) != 0 {
		t.Errorf("recommendations were read despite the account not being enrolled: %d calls", len(f.lists))
	}
	if cohCost(got[0]) != nil {
		t.Error("an unenrolled account must produce no costs")
	}
}

func TestEnrollmentCheckFollowsPagination(t *testing.T) {
	page := 0
	f := &fakeCOH{
		enrollFn: func(in *costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
			page++
			if page == 1 {
				if in.NextToken != nil {
					t.Error("the first enrollment page asked for a continuation")
				}
				return &costoptimizationhub.ListEnrollmentStatusesOutput{
					Items:     []cohtypes.AccountEnrollmentStatus{{Status: cohtypes.EnrollmentStatusInactive}},
					NextToken: aws.String("more"),
				}, nil
			}
			if aws.ToString(in.NextToken) != "more" {
				t.Errorf("continuation token = %q, want more", aws.ToString(in.NextToken))
			}
			return active(nil), nil
		},
		listFn: recs(priced(dbARN, 10)),
	}
	got, failures := runHub(t, newHub(f), census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if cohCost(got[0]) == nil {
		t.Error("an account enrolled on the second page must still be priced")
	}
}

func TestMemberAccountsExcludedIsReportedForAMultiAccountCensus(t *testing.T) {
	f := &fakeCOH{
		enrollFn: func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
			return active(aws.Bool(false)), nil
		},
		listFn: recs(priced(dbARN, 10)),
	}
	resources := census()
	resources[1].AccountID = "444455556666"
	got, failures := runHub(t, newHub(f), resources)
	ledgered(t, failures, "coh_member_accounts_excluded")
	// The run continues: the caller's own resources are still priced.
	if cohCost(got[0]) == nil {
		t.Error("the enrolled account's resource must still be priced")
	}
}

func TestSingleAccountCensusDoesNotWarnAboutMembers(t *testing.T) {
	f := &fakeCOH{
		enrollFn: func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
			return active(aws.Bool(false)), nil
		},
	}
	_, failures := runHub(t, newHub(f), census())
	for _, fl := range failures {
		if strings.HasPrefix(fl.Error, "coh_member_accounts_excluded:") {
			t.Errorf("warned about member accounts for a one-account census: %s", fl.Error)
		}
	}
}

func TestRecommendationsAreFilteredToTheScannedAccounts(t *testing.T) {
	f := &fakeCOH{listFn: recs()}
	resources := census()
	resources[1].AccountID = "444455556666"
	runHub(t, newHub(f), resources)
	if len(f.lists) != 1 {
		t.Fatalf("ListRecommendations calls = %d, want 1", len(f.lists))
	}
	in := f.lists[0]
	if in.IncludeAllRecommendations {
		t.Error("recommendations must be de-duped to one row per resource")
	}
	if in.Filter == nil {
		t.Fatal("the request carried no account filter, so a payer would page the whole organization")
	}
	want := []string{"111122223333", "444455556666"}
	if len(in.Filter.AccountIds) != len(want) {
		t.Fatalf("account filter = %v, want %v", in.Filter.AccountIds, want)
	}
	for i, id := range want {
		if in.Filter.AccountIds[i] != id {
			t.Errorf("account filter[%d] = %q, want %q", i, in.Filter.AccountIds[i], id)
		}
	}
}

func TestPaginationReadsEveryPage(t *testing.T) {
	page := 0
	f := &fakeCOH{listFn: func(in *costoptimizationhub.ListRecommendationsInput) (*costoptimizationhub.ListRecommendationsOutput, error) {
		page++
		if page == 1 {
			return &costoptimizationhub.ListRecommendationsOutput{
				Items:     []cohtypes.Recommendation{priced(dbARN, 1)},
				NextToken: aws.String("more"),
			}, nil
		}
		if aws.ToString(in.NextToken) != "more" {
			t.Errorf("continuation token = %q, want more", aws.ToString(in.NextToken))
		}
		return &costoptimizationhub.ListRecommendationsOutput{Items: []cohtypes.Recommendation{priced(cacheARN, 2)}}, nil
	}}
	h := newHub(f)
	got, failures := runHub(t, h, census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if cohCost(got[0]) == nil || cohCost(got[1]) == nil {
		t.Fatal("a resource priced on the second page was not attached")
	}
	if m := h.Meter(); m.Recommendations != 2 || m.Priced != 2 {
		t.Errorf("meter = %+v, want 2 recommendations and 2 priced", m)
	}
}

func TestAFailedPageKeepsWhatWasAlreadyRead(t *testing.T) {
	page := 0
	f := &fakeCOH{listFn: func(*costoptimizationhub.ListRecommendationsInput) (*costoptimizationhub.ListRecommendationsOutput, error) {
		page++
		if page == 1 {
			return &costoptimizationhub.ListRecommendationsOutput{
				Items:     []cohtypes.Recommendation{priced(dbARN, 7)},
				NextToken: aws.String("more"),
			}, nil
		}
		return nil, errors.New("boom")
	}}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_failed")
	// Unlike a Cost Explorer rollup, a short read here is fewer priced
	// resources rather than a wrong total, so the first page survives.
	if cohCost(got[0]) == nil {
		t.Error("the page that did arrive was discarded")
	}
}

func TestConflictingAmountsForOneARNAreDropped(t *testing.T) {
	a := priced(dbARN, 10)
	b := priced(dbARN, 25)
	b.CurrentResourceType = aws.String("RdsDbInstanceStorage")
	f := &fakeCOH{listFn: recs(a, b)}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_conflicting_amounts")
	if cohCost(got[0]) != nil {
		t.Errorf("one of two disagreeing figures was published anyway: %+v", cohCost(got[0]))
	}
}

func TestConflictLaterInTheListSuppressesTheEarlierFigure(t *testing.T) {
	page := 0
	f := &fakeCOH{listFn: func(*costoptimizationhub.ListRecommendationsInput) (*costoptimizationhub.ListRecommendationsOutput, error) {
		page++
		if page == 1 {
			return &costoptimizationhub.ListRecommendationsOutput{
				Items:     []cohtypes.Recommendation{priced(dbARN, 10)},
				NextToken: aws.String("more"),
			}, nil
		}
		return &costoptimizationhub.ListRecommendationsOutput{Items: []cohtypes.Recommendation{priced(dbARN, 99)}}, nil
	}}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_conflicting_amounts")
	if cohCost(got[0]) != nil {
		t.Errorf("a figure contradicted on a later page was still published: %+v", cohCost(got[0]))
	}
}

func TestIdenticalDuplicateRowsAreNotAConflict(t *testing.T) {
	f := &fakeCOH{listFn: recs(priced(dbARN, 10), priced(dbARN, 10))}
	got, failures := runHub(t, newHub(f), census())
	if len(failures) != 0 {
		t.Fatalf("two rows that agree are not a conflict: %+v", failures)
	}
	if cohCost(got[0]) == nil || cohCost(got[0]).Amount != "10.00" {
		t.Errorf("cost = %+v, want 10.00", cohCost(got[0]))
	}
}

func TestAZeroCostIsARealFigureAndSurvives(t *testing.T) {
	f := &fakeCOH{listFn: recs(priced(dbARN, 0))}
	got, _ := runHub(t, newHub(f), census())
	if cohCost(got[0]) == nil {
		t.Fatal("a resource modelled at zero dollars was dropped as if unreported")
	}
	if cohCost(got[0]).Amount != "0.00" {
		t.Errorf("amount = %q, want 0.00", cohCost(got[0]).Amount)
	}
}

func TestNegativeZeroDoesNotRenderAsMinusZero(t *testing.T) {
	f := &fakeCOH{listFn: recs(priced(dbARN, math.Copysign(0, -1)))}
	got, _ := runHub(t, newHub(f), census())
	if cohCost(got[0]) == nil || cohCost(got[0]).Amount != "0.00" {
		t.Errorf("cost = %+v, want 0.00", cohCost(got[0]))
	}
}

func TestAnAbsentCostIsNotAZero(t *testing.T) {
	rec := priced(dbARN, 0)
	rec.EstimatedMonthlyCost = nil
	f := &fakeCOH{listFn: recs(rec)}
	got, failures := runHub(t, newHub(f), census())
	if cohCost(got[0]) != nil {
		t.Errorf("a recommendation with no cost produced one anyway: %+v", cohCost(got[0]))
	}
	if len(failures) != 0 {
		t.Errorf("a recommendation that simply carries no cost is not a failure: %+v", failures)
	}
}

func TestNonFiniteCostIsDroppedAndLedgered(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		f := &fakeCOH{listFn: recs(priced(dbARN, v))}
		got, failures := runHub(t, newHub(f), census())
		ledgered(t, failures, "coh_unusable_amount")
		if cohCost(got[0]) != nil {
			t.Errorf("%v was published as a price: %+v", v, cohCost(got[0]))
		}
	}
}

func TestAmountKeepsEveryDigitTheFloatActuallyHas(t *testing.T) {
	// 0.1+0.2 is not 0.3 in binary floating point. The census reports what the
	// float is, not what it was probably meant to be: rounding here would be a
	// number this tool invented. The addends are variables so Go's exact
	// constant arithmetic does not do the sum for us and hide the point.
	tenth, fifth := 0.1, 0.2
	f := &fakeCOH{listFn: recs(priced(dbARN, tenth+fifth))}
	got, _ := runHub(t, newHub(f), census())
	if cohCost(got[0]) == nil || cohCost(got[0]).Amount != "0.30000000000000004" {
		t.Errorf("cost = %+v, want 0.30000000000000004", cohCost(got[0]))
	}
}

func TestALargeAmountIsNotWrittenInExponentNotation(t *testing.T) {
	f := &fakeCOH{listFn: recs(priced(dbARN, 1.25e7))}
	got, _ := runHub(t, newHub(f), census())
	if cohCost(got[0]) == nil || cohCost(got[0]).Amount != "12500000.00" {
		t.Errorf("cost = %+v, want 12500000.00", cohCost(got[0]))
	}
}

func TestAMissingCurrencyIsNotAssumedToBeDollars(t *testing.T) {
	rec := priced(dbARN, 10)
	rec.CurrencyCode = nil
	f := &fakeCOH{listFn: recs(rec)}
	got, _ := runHub(t, newHub(f), census())
	if cohCost(got[0]) == nil {
		t.Fatal("an amount without a currency is still an amount")
	}
	if cohCost(got[0]).Currency != "" {
		t.Errorf("currency = %q, want empty — never a default", cohCost(got[0]).Currency)
	}
}

func TestObservedWindowComesFromTheRefreshTimeAndLookback(t *testing.T) {
	f := &fakeCOH{listFn: recs(priced(dbARN, 10))}
	got, _ := runHub(t, newHub(f), census())
	c := cohCost(got[0])
	if c.ObservedTo == nil || !c.ObservedTo.Equal(scanTime) {
		t.Errorf("observed_to = %v, want %v", c.ObservedTo, scanTime)
	}
	want := scanTime.Add(-14 * 24 * time.Hour)
	if c.ObservedFrom == nil || !c.ObservedFrom.Equal(want) {
		t.Errorf("observed_from = %v, want %v", c.ObservedFrom, want)
	}
}

func TestAnUnplaceableWindowIsLeftNilRatherThanGuessed(t *testing.T) {
	rec := priced(dbARN, 10)
	rec.RecommendationLookbackPeriodInDays = nil
	f := &fakeCOH{listFn: recs(rec)}
	got, _ := runHub(t, newHub(f), census())
	c := cohCost(got[0])
	if c.ObservedFrom != nil {
		t.Errorf("observed_from = %v, want nil when no lookback was reported", c.ObservedFrom)
	}
	if c.ObservedTo == nil {
		t.Error("the refresh time was reported and must still be kept")
	}

	rec.LastRefreshTimestamp = nil
	f = &fakeCOH{listFn: recs(rec)}
	got, _ = runHub(t, newHub(f), census())
	if cohCost(got[0]).ObservedTo != nil {
		t.Errorf("observed_to = %v, want nil when no refresh time was reported", cohCost(got[0]).ObservedTo)
	}
}

func TestPartialScopeIsDisclosedAsACaveat(t *testing.T) {
	rec := priced(dbARN, 10)
	rec.CurrentResourceType = aws.String("RdsDbInstanceStorage")
	f := &fakeCOH{listFn: recs(rec)}
	got, _ := runHub(t, newHub(f), census())
	c := cohCost(got[0])
	if len(c.Caveats) != 1 || !strings.Contains(c.Caveats[0], "storage") {
		t.Errorf("caveats = %v, want one naming the partial scope", c.Caveats)
	}
}

func TestAWholeResourceFigureCarriesNoCaveats(t *testing.T) {
	f := &fakeCOH{listFn: recs(priced(dbARN, 10))}
	got, _ := runHub(t, newHub(f), census())
	if len(cohCost(got[0]).Caveats) != 0 {
		t.Errorf("caveats = %v, want none — blanket disclosures belong in docs", cohCost(got[0]).Caveats)
	}
}

func TestAResourceYoungerThanTheWindowIsFlaggedAsExtrapolated(t *testing.T) {
	resources := census()
	born := scanTime.Add(-3 * 24 * time.Hour)
	resources[0].CreatedAt = &born
	f := &fakeCOH{listFn: recs(priced(dbARN, 10))}
	got, _ := runHub(t, newHub(f), resources)
	c := cohCost(got[0])
	if len(c.Caveats) != 1 || !strings.Contains(c.Caveats[0], "extrapolates") {
		t.Errorf("caveats = %v, want one naming the extrapolation", c.Caveats)
	}
}

func TestAResourceOlderThanTheWindowIsNotFlagged(t *testing.T) {
	resources := census()
	born := scanTime.Add(-90 * 24 * time.Hour)
	resources[0].CreatedAt = &born
	f := &fakeCOH{listFn: recs(priced(dbARN, 10))}
	got, _ := runHub(t, newHub(f), resources)
	if len(cohCost(got[0]).Caveats) != 0 {
		t.Errorf("caveats = %v, want none", cohCost(got[0]).Caveats)
	}
}

func TestARNMatchingIsExact(t *testing.T) {
	rec := priced(strings.ToUpper(dbARN), 10)
	f := &fakeCOH{listFn: recs(rec)}
	got, _ := runHub(t, newHub(f), census())
	if cohCost(got[0]) != nil {
		t.Error("a case-folded ARN matched; a figure on the wrong resource is worse than none")
	}
}

func TestAnEmptyCensusMakesNoCalls(t *testing.T) {
	f := &fakeCOH{}
	h := newHub(f)
	if failures := h.Enrich(context.Background(), scan.Enrichment{}); len(failures) != 0 {
		t.Errorf("unexpected failures: %+v", failures)
	}
	if len(f.enrolls) != 0 || len(f.lists) != 0 {
		t.Errorf("called AWS for an empty census: %d enroll, %d list", len(f.enrolls), len(f.lists))
	}
}

func TestAccessDeniedNamesTheIAMStatement(t *testing.T) {
	f := &fakeCOH{enrollFn: func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
		return nil, errors.New("AccessDeniedException: User is not authorized to perform: cost-optimization-hub:ListEnrollmentStatuses")
	}}
	_, failures := runHub(t, newHub(f), census())
	entry := ledgered(t, failures, "coh_access_denied")
	if !strings.Contains(entry.Error, "docs/iam-policy.json") {
		t.Errorf("the ledger entry does not name the fix: %s", entry.Error)
	}
}

func TestCanceledContextStopsTheStage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeCOH{}
	h := newHub(f)
	failures := h.Enrich(ctx, scan.Enrichment{Resources: census()})
	if len(f.enrolls) != 0 || len(f.lists) != 0 {
		t.Errorf("a canceled run still called AWS: %d enroll, %d list", len(f.enrolls), len(f.lists))
	}
	if len(failures) != 0 {
		t.Errorf("cancellation is not a coverage gap: %+v", failures)
	}
}

func TestLedgerEntriesUseTheStageName(t *testing.T) {
	f := &fakeCOH{enrollFn: func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
		return &costoptimizationhub.ListEnrollmentStatusesOutput{}, nil
	}}
	_, failures := runHub(t, newHub(f), census())
	entry := ledgered(t, failures, "coh_not_enrolled")
	if entry.Service != CostHubService {
		t.Errorf("service = %q, want %q", entry.Service, CostHubService)
	}
	if entry.AccountID != "111122223333" {
		t.Errorf("account = %q, want the caller's", entry.AccountID)
	}
	if entry.Region != "" {
		t.Errorf("region = %q, want empty — Cost Optimization Hub is global", entry.Region)
	}
	if !entry.Time.Equal(scanTime) {
		t.Errorf("time = %v, want %v", entry.Time, scanTime)
	}
}

func TestMeterCountsRequestsAndCoverage(t *testing.T) {
	f := &fakeCOH{listFn: recs(priced(dbARN, 1), priced("arn:aws:rds:us-east-1:111122223333:db:absent", 2))}
	h := newHub(f)
	runHub(t, h, census())
	m := h.Meter()
	if m.Requests != 2 {
		t.Errorf("requests = %d, want 2 (one enrollment, one list)", m.Requests)
	}
	if m.Recommendations != 2 {
		t.Errorf("recommendations = %d, want 2", m.Recommendations)
	}
	// One recommendation named a resource this census never saw, so coverage
	// is below what was read — the number that makes an ARN mismatch visible.
	if m.Priced != 1 {
		t.Errorf("priced = %d, want 1", m.Priced)
	}
}

func TestFormatAmountPadsToCentsWithoutChangingTheValue(t *testing.T) {
	cases := map[float64]string{
		1:        "1.00",
		1.5:      "1.50",
		1.25:     "1.25",
		1.234:    "1.234",
		-3.5:     "-3.50",
		0.000001: "0.000001",
	}
	for in, want := range cases {
		got, ok := formatAmount(in)
		if !ok {
			t.Errorf("formatAmount(%v) was rejected", in)
			continue
		}
		if got != want {
			t.Errorf("formatAmount(%v) = %q, want %q", in, got, want)
		}
	}
}

// unscannedAccount is an account the census never covers, used to build
// enrollment rows that must not answer for the scanned one.
const unscannedAccount = "222233334444"

// scannedAccount is the account the fixture census lives in, read off the
// census itself so the literal lives in exactly one place.
func scannedAccount() string { return census()[0].AccountID }

// enrollment builds a single-page response from account/status pairs.
func enrollment(rows ...cohtypes.AccountEnrollmentStatus) func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
	return func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
		return &costoptimizationhub.ListEnrollmentStatusesOutput{Items: rows}, nil
	}
}

func enrolledRow(account string, status cohtypes.EnrollmentStatus) cohtypes.AccountEnrollmentStatus {
	return cohtypes.AccountEnrollmentStatus{AccountId: aws.String(account), Status: status}
}

// An organization can have the hub switched on for accounts this scan never
// touched. Reading "enrolled" off one of those rows would send the stage on to
// a ListRecommendations filtered to the scanned account, which comes back
// empty — the silent blank the enrollment check exists to prevent, just moved
// one step later. Here the scanned account is positively named as inactive, so
// there is evidence and the stage stops.
func TestAnActiveRowForAnotherAccountDoesNotConfirmTheScannedOne(t *testing.T) {
	f := &fakeCOH{
		enrollFn: enrollment(
			enrolledRow(unscannedAccount, cohtypes.EnrollmentStatusActive),
			enrolledRow(scannedAccount(), cohtypes.EnrollmentStatusInactive),
		),
		listFn: recs(priced(dbARN, 10)),
	}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_not_enrolled")
	if len(f.lists) != 0 {
		t.Errorf("recommendations were read for an account the list said is not enrolled: %d calls", len(f.lists))
	}
	if cohCost(got[0]) != nil {
		t.Error("an account the list reported inactive must produce no costs")
	}
}

// The converse mistake is worse. An account missing from the enrollment list
// has not been reported unenrolled — it has not been reported at all — and a
// management account that answers for the organization need not enumerate
// every member. Suppressing here would drop real figures, so the stage reads
// on and says in the ledger that the status is unknown.
func TestAnUnmentionedScannedAccountIsNotTreatedAsUnenrolled(t *testing.T) {
	f := &fakeCOH{
		enrollFn: enrollment(enrolledRow(unscannedAccount, cohtypes.EnrollmentStatusActive)),
		listFn:   recs(priced(dbARN, 10)),
	}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_enrollment_unconfirmed")
	if len(f.lists) == 0 {
		t.Error("an unmentioned account must still be read for recommendations, not written off")
	}
	if cohCost(got[0]) == nil {
		t.Error("a figure AWS actually returned was dropped on an unproven enrollment guess")
	}
}

// A mixed organization prices what it can and names what it could not, rather
// than failing whole or reporting a partial answer as a complete one.
func TestAnUnenrolledAccountAlongsideAnEnrolledOneIsNamedNotSuppressed(t *testing.T) {
	f := &fakeCOH{
		enrollFn: enrollment(
			enrolledRow(scannedAccount(), cohtypes.EnrollmentStatusActive),
			enrolledRow(unscannedAccount, cohtypes.EnrollmentStatusInactive),
		),
		listFn: recs(priced(dbARN, 10)),
	}
	resources := census()
	resources[1].AccountID = unscannedAccount
	got, failures := runHub(t, newHub(f), resources)
	ledgered(t, failures, "coh_account_not_enrolled")
	if cohCost(got[0]) == nil {
		t.Error("the enrolled account's resource must still be priced")
	}
	if cohCost(got[1]) != nil {
		t.Error("the unenrolled account's resource must not be priced")
	}
}

// Confirming every scanned account settles the question, so the rest of an
// organization's enrollment pages go unread.
func TestEnrollmentPagingStopsOnceEveryScannedAccountIsConfirmed(t *testing.T) {
	f := &fakeCOH{
		enrollFn: func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
			return &costoptimizationhub.ListEnrollmentStatusesOutput{
				Items:     []cohtypes.AccountEnrollmentStatus{enrolledRow(scannedAccount(), cohtypes.EnrollmentStatusActive)},
				NextToken: aws.String("more"),
			}, nil
		},
		listFn: recs(priced(dbARN, 10)),
	}
	_, failures := runHub(t, newHub(f), census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if len(f.enrolls) != 1 {
		t.Errorf("enrollment pages read = %d, want 1: the scanned account was confirmed on the first one", len(f.enrolls))
	}
}

// A hub read that finished is evidence about every resource in the census: the
// list was complete, so a resource missing from it has no recommendation. That
// absence is written down by name rather than left as a nil cost, which a
// reader could take for zero spend.
func TestACompletedReadNamesTheAbsenceOnUnpricedResources(t *testing.T) {
	// Only the database is priced; the cache cluster is not.
	got, failures := runHub(t, newHub(&fakeCOH{listFn: recs(priced(dbARN, 10))}), census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if got[0].CostUnavailable != "" {
		t.Errorf("a priced resource carries an absence reason: %q", got[0].CostUnavailable)
	}
	if cohCost(got[1]) != nil {
		t.Fatal("the unpriced resource was given a cost")
	}
	if got[1].CostUnavailable == "" {
		t.Error("the hub answered in full and named nothing for this resource, but the absence was left blank")
	}
}

// A read that stopped early says nothing about the resources it never reached.
// Naming an absence off a truncated list would turn "the read stopped" into
// "AWS has no figure for this", which is a claim the response does not support.
func TestATruncatedReadDoesNotClaimTheHubHasNothing(t *testing.T) {
	page := 0
	f := &fakeCOH{listFn: func(*costoptimizationhub.ListRecommendationsInput) (*costoptimizationhub.ListRecommendationsOutput, error) {
		page++
		if page == 1 {
			return &costoptimizationhub.ListRecommendationsOutput{
				Items:     []cohtypes.Recommendation{priced(dbARN, 7)},
				NextToken: aws.String("more"),
			}, nil
		}
		return nil, errors.New("boom")
	}}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_failed")

	// The page that arrived still prices what it named.
	if cohCost(got[0]) == nil {
		t.Error("the page that did arrive was discarded")
	}
	// The one it did not reach stays a plain unknown: no cost, and no reason
	// either, because nothing looked all the way for it.
	if cohCost(got[1]) != nil {
		t.Error("a resource on an unread page was given a cost")
	}
	if got[1].CostUnavailable != "" {
		t.Errorf("a truncated read claimed the hub has nothing: %q", got[1].CostUnavailable)
	}
}

// The same rule for the other early exit: an unenrolled account never reached
// the recommendation list at all, so nothing may be said about coverage.
func TestAnUnenrolledAccountLeavesTheAbsenceUnexplained(t *testing.T) {
	f := &fakeCOH{enrollFn: func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
		return &costoptimizationhub.ListEnrollmentStatusesOutput{
			Items: []cohtypes.AccountEnrollmentStatus{enrolledRow(scannedAccount(), cohtypes.EnrollmentStatusInactive)},
		}, nil
	}}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_not_enrolled")
	for _, r := range got {
		if r.CostUnavailable != "" {
			t.Errorf("%s: claimed a coverage answer from a hub that was never read: %q", r.Name, r.CostUnavailable)
		}
	}
}
