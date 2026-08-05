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

// saves is the ordinary recommendation this file is built on: rightsize one
// resource, save the given amount a month, modelled at scanTime over a 14-day
// lookback.
//
// The identifier is derived from the ARN so that two calls for one resource
// look to the stage like the same row arriving twice, which is what they are.
// Tests that need two genuinely different suggestions for one resource say so
// by giving the second its own id — see withID.
func saves(arn string, monthly float64) cohtypes.Recommendation {
	return cohtypes.Recommendation{
		RecommendationId:                   aws.String("rec-" + arn),
		AccountId:                          aws.String("111122223333"),
		Region:                             aws.String("us-east-1"),
		ResourceArn:                        aws.String(arn),
		ActionType:                         aws.String("Rightsize"),
		CurrentResourceType:                aws.String("RdsDbInstance"),
		CurrencyCode:                       aws.String("USD"),
		EstimatedMonthlySavings:            aws.Float64(monthly),
		LastRefreshTimestamp:               aws.Time(scanTime),
		RecommendationLookbackPeriodInDays: aws.Int32(14),
	}
}

// withID relabels a recommendation, so a test can build two distinct rows for
// one resource — the compute-and-storage case the hub really does return.
func withID(rec cohtypes.Recommendation, id string) cohtypes.Recommendation {
	rec.RecommendationId = aws.String(id)
	return rec
}

// census is a two-resource snapshot to attach recommendations to.
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

// tipOf returns the single recommendation attached to a resource, failing the
// test when there is not exactly one. Most cases here are one-tip cases, and a
// test that quietly read the first of two would assert the wrong thing.
func tipOf(t *testing.T, r model.Resource) model.Recommendation {
	t.Helper()
	if len(r.Recommendations) != 1 {
		t.Fatalf("%s: recommendations = %d, want 1: %+v", r.Name, len(r.Recommendations), r.Recommendations)
	}
	return r.Recommendations[0]
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

// notLedgered fails when any entry carries the given kind.
func notLedgered(t *testing.T, failures []model.Failure, kind string) {
	t.Helper()
	for _, f := range failures {
		if strings.HasPrefix(f.Error, kind+":") {
			t.Errorf("unexpected %s entry: %s", kind, f.Error)
		}
	}
}

// The headline of this stage's rewrite, and the one assertion that would catch
// a regression to what it used to do. Cost Optimization Hub models what a
// resource *would* cost after a change and what it costs now as configured;
// neither was ever invoiced. If either reached Resource.Costs it would be in
// front of every consumer that adds figures up — the report's group totals, the
// diff's net drift, the CSV's summable columns — and each would fold a
// hypothetical into a bill.
func TestTheHubProducesAdviceAndNeverAPrice(t *testing.T) {
	rec := saves(dbARN, 412.5)
	// Set on purpose: the field this stage used to read, and the only reason
	// this test can prove it no longer does.
	rec.EstimatedMonthlyCost = aws.Float64(998.00)
	f := &fakeCOH{listFn: recs(rec)}
	got, failures := runHub(t, newHub(f), census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	for _, r := range got {
		if len(r.Costs) != 0 {
			t.Errorf("%s: the hub wrote a per-resource price: %+v", r.Name, r.Costs)
		}
		if r.Priced() {
			t.Errorf("%s: the hub marked a resource priced", r.Name)
		}
	}
	if !got[0].Tipped() {
		t.Error("the tipped resource came away with no recommendation")
	}
}

func TestRecommendationBecomesATip(t *testing.T) {
	f := &fakeCOH{listFn: recs(saves(dbARN, 412.5))}
	got, failures := runHub(t, newHub(f), census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	tip := tipOf(t, got[0])
	if tip.EstimatedMonthlySavings != "412.50" {
		t.Errorf("savings = %q, want 412.50", tip.EstimatedMonthlySavings)
	}
	if tip.Currency != "USD" {
		t.Errorf("currency = %q, want USD", tip.Currency)
	}
	if tip.ActionType != "Rightsize" {
		t.Errorf("action = %q, want Rightsize", tip.ActionType)
	}
	if got[1].Tipped() {
		t.Errorf("the untipped resource must stay untipped, got %+v", got[1].Recommendations)
	}
}

// Everything AWS reports about a recommendation is carried through as AWS wrote
// it. The effort here is deliberately not one of the five values AWS documents:
// ActionType and ImplementationEffort arrive as plain strings rather than
// enums, so a value AWS adds after this SDK pin is still real advice and must
// reach the report rather than be dropped by an allow-list this tool keeps.
func TestEveryReportedFieldReachesTheCensusVerbatim(t *testing.T) {
	rec := saves(dbARN, 340)
	rec.ActionType = aws.String("SomethingAWSAddedLater")
	rec.ImplementationEffort = aws.String("Glacial")
	rec.CurrentResourceSummary = aws.String("db.r5.2xlarge")
	rec.RecommendedResourceSummary = aws.String("db.r5.xlarge")
	rec.RecommendedResourceType = aws.String("RdsDbInstance")
	rec.EstimatedSavingsPercentage = aws.Float64(42.5)
	rec.RestartNeeded = aws.Bool(true)
	rec.RollbackPossible = aws.Bool(false)

	f := &fakeCOH{listFn: recs(rec)}
	got, _ := runHub(t, newHub(f), census())
	tip := tipOf(t, got[0])

	for _, c := range []struct{ name, got, want string }{
		{"id", tip.ID, "rec-" + dbARN},
		{"action_type", tip.ActionType, "SomethingAWSAddedLater"},
		{"implementation_effort", tip.ImplementationEffort, "Glacial"},
		{"current_resource_type", tip.CurrentResourceType, "RdsDbInstance"},
		{"recommended_resource_type", tip.RecommendedResourceType, "RdsDbInstance"},
		{"current_resource_summary", tip.CurrentResourceSummary, "db.r5.2xlarge"},
		{"recommended_resource_summary", tip.RecommendedResourceSummary, "db.r5.xlarge"},
		{"estimated_savings_percentage", tip.EstimatedSavingsPercentage, "42.5"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if tip.RestartNeeded == nil || !*tip.RestartNeeded {
		t.Errorf("restart_needed = %v, want true", tip.RestartNeeded)
	}
	if tip.RollbackPossible == nil || *tip.RollbackPossible {
		t.Errorf("rollback_possible = %v, want false", tip.RollbackPossible)
	}
}

// Both flags are tri-state and the third state is the useful one: "AWS did not
// say whether this needs a restart" is not "no restart needed", and a reader
// scheduling the change acts differently on each.
func TestRestartAndRollbackKeepTheirThirdState(t *testing.T) {
	rec := saves(dbARN, 10)
	rec.RestartNeeded = aws.Bool(false)
	// RollbackPossible left unset: AWS said nothing.
	f := &fakeCOH{listFn: recs(rec)}
	got, _ := runHub(t, newHub(f), census())
	tip := tipOf(t, got[0])
	if tip.RestartNeeded == nil {
		t.Fatal("a reported false became an unreported nil")
	}
	if *tip.RestartNeeded {
		t.Error("restart_needed = true, want the reported false")
	}
	if tip.RollbackPossible != nil {
		t.Errorf("rollback_possible = %v, want nil — AWS did not say", *tip.RollbackPossible)
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
		listFn: recs(saves(dbARN, 10)),
	}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_not_enrolled")
	if len(f.lists) != 0 {
		t.Errorf("recommendations were read despite the account not being enrolled: %d calls", len(f.lists))
	}
	if got[0].Tipped() {
		t.Error("an unenrolled account must produce no recommendations")
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
		listFn: recs(saves(dbARN, 10)),
	}
	got, failures := runHub(t, newHub(f), census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if !got[0].Tipped() {
		t.Error("an account enrolled on the second page must still be tipped")
	}
}

func TestMemberAccountsExcludedIsReportedForAMultiAccountCensus(t *testing.T) {
	f := &fakeCOH{
		enrollFn: func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
			return active(aws.Bool(false)), nil
		},
		listFn: recs(saves(dbARN, 10)),
	}
	resources := census()
	resources[1].AccountID = "444455556666"
	got, failures := runHub(t, newHub(f), resources)
	ledgered(t, failures, "coh_member_accounts_excluded")
	// The run continues: the caller's own resources still get their advice.
	if !got[0].Tipped() {
		t.Error("the enrolled account's resource must still be tipped")
	}
}

func TestSingleAccountCensusDoesNotWarnAboutMembers(t *testing.T) {
	f := &fakeCOH{
		enrollFn: func(*costoptimizationhub.ListEnrollmentStatusesInput) (*costoptimizationhub.ListEnrollmentStatusesOutput, error) {
			return active(aws.Bool(false)), nil
		},
	}
	_, failures := runHub(t, newHub(f), census())
	notLedgered(t, failures, "coh_member_accounts_excluded")
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
				Items:     []cohtypes.Recommendation{saves(dbARN, 1)},
				NextToken: aws.String("more"),
			}, nil
		}
		if aws.ToString(in.NextToken) != "more" {
			t.Errorf("continuation token = %q, want more", aws.ToString(in.NextToken))
		}
		return &costoptimizationhub.ListRecommendationsOutput{Items: []cohtypes.Recommendation{saves(cacheARN, 2)}}, nil
	}}
	h := newHub(f)
	got, failures := runHub(t, h, census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if !got[0].Tipped() || !got[1].Tipped() {
		t.Fatal("a resource tipped on the second page was not attached")
	}
	if m := h.Meter(); m.Recommendations != 2 || m.Tipped != 2 || m.Unattached != 0 {
		t.Errorf("meter = %+v, want 2 recommendations, 2 tipped, 0 unattached", m)
	}
}

func TestAFailedPageKeepsWhatWasAlreadyRead(t *testing.T) {
	page := 0
	f := &fakeCOH{listFn: func(*costoptimizationhub.ListRecommendationsInput) (*costoptimizationhub.ListRecommendationsOutput, error) {
		page++
		if page == 1 {
			return &costoptimizationhub.ListRecommendationsOutput{
				Items:     []cohtypes.Recommendation{saves(dbARN, 7)},
				NextToken: aws.String("more"),
			}, nil
		}
		return nil, errors.New("boom")
	}}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_failed")
	// Unlike a Cost Explorer rollup, a short read here is fewer tipped
	// resources rather than a wrong total, so the first page survives.
	if !got[0].Tipped() {
		t.Error("the page that did arrive was discarded")
	}
}

// The hub reasons about resources more finely than the census does: a database
// is one row here, but its compute and its storage are separate
// recommendations with separate savings. Both are true at once and both are
// actionable, so they are kept side by side. This is the case the stage used to
// call a conflict and drop both halves of, on the reading that two figures for
// one resource had to be two answers to one question.
func TestTwoSuggestionsForOneResourceAreBothKept(t *testing.T) {
	compute := saves(dbARN, 340)
	storage := withID(saves(dbARN, 25), "rec-storage")
	storage.CurrentResourceType = aws.String("RdsDbInstanceStorage")
	f := &fakeCOH{listFn: recs(compute, storage)}
	got, failures := runHub(t, newHub(f), census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if len(got[0].Recommendations) != 2 {
		t.Fatalf("recommendations = %d, want 2: %+v", len(got[0].Recommendations), got[0].Recommendations)
	}
	types := map[string]string{}
	for _, rec := range got[0].Recommendations {
		types[rec.CurrentResourceType] = rec.EstimatedMonthlySavings
	}
	if types["RdsDbInstance"] != "340.00" || types["RdsDbInstanceStorage"] != "25.00" {
		t.Errorf("savings by scope = %v, want the compute and storage figures kept apart", types)
	}
}

// Two suggestions for one resource that arrive on different pages are still two
// suggestions. The old stage read a figure contradicted on a later page as a
// conflict and suppressed the earlier one, so pagination changed the answer.
func TestASecondSuggestionOnALaterPageDoesNotSuppressTheFirst(t *testing.T) {
	page := 0
	f := &fakeCOH{listFn: func(*costoptimizationhub.ListRecommendationsInput) (*costoptimizationhub.ListRecommendationsOutput, error) {
		page++
		if page == 1 {
			return &costoptimizationhub.ListRecommendationsOutput{
				Items:     []cohtypes.Recommendation{saves(dbARN, 10)},
				NextToken: aws.String("more"),
			}, nil
		}
		return &costoptimizationhub.ListRecommendationsOutput{
			Items: []cohtypes.Recommendation{withID(saves(dbARN, 99), "rec-second")},
		}, nil
	}}
	got, failures := runHub(t, newHub(f), census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if len(got[0].Recommendations) != 2 {
		t.Errorf("recommendations = %d, want 2: a page boundary is not a contradiction", len(got[0].Recommendations))
	}
}

// One row arriving twice is one thing to do, by AWS's own identifier — and
// storing it twice would print the suggestion twice and count its saving twice
// in the per-currency total the report draws.
func TestARowRepeatedUnderOneIDIsStoredOnce(t *testing.T) {
	f := &fakeCOH{listFn: recs(saves(dbARN, 10), saves(dbARN, 10))}
	h := newHub(f)
	got, failures := runHub(t, h, census())
	if len(failures) != 0 {
		t.Fatalf("a repeated row is not a failure: %+v", failures)
	}
	tip := tipOf(t, got[0])
	if tip.EstimatedMonthlySavings != "10.00" {
		t.Errorf("savings = %q, want 10.00", tip.EstimatedMonthlySavings)
	}
	// The meter counts rows read, not rows stored: two arrived.
	if m := h.Meter(); m.Recommendations != 2 {
		t.Errorf("recommendations read = %d, want 2", m.Recommendations)
	}
}

// De-duplication keys on AWS's identifier and on nothing else. Two rows AWS
// gave no id to are genuinely indistinguishable to this tool, and telling them
// apart would need a similarity rule this tool would be inventing — one that
// could drop real advice to avoid printing a duplicate.
func TestRowsWithNoIDAreKeptAsTheyCame(t *testing.T) {
	a := saves(dbARN, 10)
	a.RecommendationId = nil
	b := saves(dbARN, 25)
	b.RecommendationId = nil
	f := &fakeCOH{listFn: recs(a, b)}
	got, _ := runHub(t, newHub(f), census())
	if len(got[0].Recommendations) != 2 {
		t.Errorf("recommendations = %d, want 2: unlabelled rows are not de-duplicated", len(got[0].Recommendations))
	}
}

func TestAZeroSavingIsARealFigureAndSurvives(t *testing.T) {
	f := &fakeCOH{listFn: recs(saves(dbARN, 0))}
	got, _ := runHub(t, newHub(f), census())
	tip := tipOf(t, got[0])
	if tip.EstimatedMonthlySavings != "0.00" {
		t.Errorf("savings = %q, want 0.00 — a change AWS priced at nothing is an answer", tip.EstimatedMonthlySavings)
	}
}

func TestNegativeZeroDoesNotRenderAsMinusZero(t *testing.T) {
	f := &fakeCOH{listFn: recs(saves(dbARN, math.Copysign(0, -1)))}
	got, _ := runHub(t, newHub(f), census())
	if s := tipOf(t, got[0]).EstimatedMonthlySavings; s != "0.00" {
		t.Errorf("savings = %q, want 0.00", s)
	}
}

// The two absences are different statements. AWS reporting no savings figure is
// not AWS reporting a saving of nothing, and an empty string is how the census
// says the first without saying the second.
func TestAnAbsentSavingIsNotAZero(t *testing.T) {
	rec := saves(dbARN, 0)
	rec.EstimatedMonthlySavings = nil
	f := &fakeCOH{listFn: recs(rec)}
	got, failures := runHub(t, newHub(f), census())
	if s := tipOf(t, got[0]).EstimatedMonthlySavings; s != "" {
		t.Errorf("savings = %q, want empty — AWS reported none", s)
	}
	if len(failures) != 0 {
		t.Errorf("a recommendation that simply carries no figure is not a failure: %+v", failures)
	}
}

// A row with an action and no money on it is advice AWS gave and the reader can
// act on. Rejecting it for saying too little would suppress the tip and report
// nothing in its place.
func TestARecommendationWithNoFigureIsStillListed(t *testing.T) {
	rec := cohtypes.Recommendation{
		RecommendationId: aws.String("rec-bare"),
		AccountId:        aws.String("111122223333"),
		ResourceArn:      aws.String(dbARN),
		ActionType:       aws.String("Stop"),
	}
	f := &fakeCOH{listFn: recs(rec)}
	got, failures := runHub(t, newHub(f), census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	tip := tipOf(t, got[0])
	if tip.ActionType != "Stop" {
		t.Errorf("action = %q, want Stop", tip.ActionType)
	}
	if tip.EstimatedMonthlySavings != "" {
		t.Errorf("savings = %q, want empty", tip.EstimatedMonthlySavings)
	}
}

func TestNonFiniteSavingIsDroppedAndLedgeredButTheTipSurvives(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		f := &fakeCOH{listFn: recs(saves(dbARN, v))}
		got, failures := runHub(t, newHub(f), census())
		entry := ledgered(t, failures, "coh_unusable_savings")
		if !strings.Contains(entry.Error, dbARN) {
			t.Errorf("the ledger entry does not name the resource: %s", entry.Error)
		}
		tip := tipOf(t, got[0])
		if tip.EstimatedMonthlySavings != "" {
			t.Errorf("%v was published as a saving: %q", v, tip.EstimatedMonthlySavings)
		}
		if tip.ActionType != "Rightsize" {
			t.Errorf("the recommendation was dropped along with its unusable figure: %+v", tip)
		}
	}
}

func TestNonFinitePercentageIsDroppedAndLedgeredButTheTipSurvives(t *testing.T) {
	rec := saves(dbARN, 10)
	rec.EstimatedSavingsPercentage = aws.Float64(math.Inf(1))
	f := &fakeCOH{listFn: recs(rec)}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_unusable_savings_percentage")
	tip := tipOf(t, got[0])
	if tip.EstimatedSavingsPercentage != "" {
		t.Errorf("percentage = %q, want empty", tip.EstimatedSavingsPercentage)
	}
	if tip.EstimatedMonthlySavings != "10.00" {
		t.Errorf("an unusable percentage took the amount with it: %q", tip.EstimatedMonthlySavings)
	}
}

func TestSavingKeepsEveryDigitTheFloatActuallyHas(t *testing.T) {
	// 0.1+0.2 is not 0.3 in binary floating point. The census reports what the
	// float is, not what it was probably meant to be: rounding here would be a
	// number this tool invented. The addends are variables so Go's exact
	// constant arithmetic does not do the sum for us and hide the point.
	tenth, fifth := 0.1, 0.2
	f := &fakeCOH{listFn: recs(saves(dbARN, tenth+fifth))}
	got, _ := runHub(t, newHub(f), census())
	if s := tipOf(t, got[0]).EstimatedMonthlySavings; s != "0.30000000000000004" {
		t.Errorf("savings = %q, want 0.30000000000000004", s)
	}
}

func TestALargeSavingIsNotWrittenInExponentNotation(t *testing.T) {
	f := &fakeCOH{listFn: recs(saves(dbARN, 1.25e7))}
	got, _ := runHub(t, newHub(f), census())
	if s := tipOf(t, got[0]).EstimatedMonthlySavings; s != "12500000.00" {
		t.Errorf("savings = %q, want 12500000.00", s)
	}
}

func TestAMissingCurrencyIsNotAssumedToBeDollars(t *testing.T) {
	rec := saves(dbARN, 10)
	rec.CurrencyCode = nil
	f := &fakeCOH{listFn: recs(rec)}
	got, _ := runHub(t, newHub(f), census())
	tip := tipOf(t, got[0])
	if tip.EstimatedMonthlySavings != "10.00" {
		t.Fatal("an amount without a currency is still an amount")
	}
	if tip.Currency != "" {
		t.Errorf("currency = %q, want empty — never a default", tip.Currency)
	}
}

func TestObservedWindowComesFromTheRefreshTimeAndLookback(t *testing.T) {
	f := &fakeCOH{listFn: recs(saves(dbARN, 10))}
	got, _ := runHub(t, newHub(f), census())
	tip := tipOf(t, got[0])
	if tip.ObservedTo == nil || !tip.ObservedTo.Equal(scanTime) {
		t.Errorf("observed_to = %v, want %v", tip.ObservedTo, scanTime)
	}
	want := scanTime.Add(-14 * 24 * time.Hour)
	if tip.ObservedFrom == nil || !tip.ObservedFrom.Equal(want) {
		t.Errorf("observed_from = %v, want %v", tip.ObservedFrom, want)
	}
}

func TestAnUnplaceableWindowIsLeftNilRatherThanGuessed(t *testing.T) {
	rec := saves(dbARN, 10)
	rec.RecommendationLookbackPeriodInDays = nil
	f := &fakeCOH{listFn: recs(rec)}
	got, _ := runHub(t, newHub(f), census())
	tip := tipOf(t, got[0])
	if tip.ObservedFrom != nil {
		t.Errorf("observed_from = %v, want nil when no lookback was reported", tip.ObservedFrom)
	}
	if tip.ObservedTo == nil {
		t.Error("the refresh time was reported and must still be kept")
	}

	rec.LastRefreshTimestamp = nil
	f = &fakeCOH{listFn: recs(rec)}
	got, _ = runHub(t, newHub(f), census())
	if to := tipOf(t, got[0]).ObservedTo; to != nil {
		t.Errorf("observed_to = %v, want nil when no refresh time was reported", to)
	}
}

// Scope stopped being a caveat when the hub stopped being a price source. A
// storage *price* attached to a database understates the database and had to be
// disclosed; a storage *saving* attached to a database is simply a saving on its
// storage. CurrentResourceType says which, so a sentence repeating it would be
// explaining the data rather than qualifying it.
func TestAComponentScopeIsCarriedOnTheTipRatherThanDisclosedAsACaveat(t *testing.T) {
	rec := saves(dbARN, 10)
	rec.CurrentResourceType = aws.String("RdsDbInstanceStorage")
	f := &fakeCOH{listFn: recs(rec)}
	got, _ := runHub(t, newHub(f), census())
	tip := tipOf(t, got[0])
	if len(tip.Caveats) != 0 {
		t.Errorf("caveats = %v, want none — the scope is on the tip itself", tip.Caveats)
	}
	if tip.CurrentResourceType != "RdsDbInstanceStorage" {
		t.Errorf("current_resource_type = %q, want RdsDbInstanceStorage", tip.CurrentResourceType)
	}
}

func TestAWholeResourceTipCarriesNoCaveats(t *testing.T) {
	f := &fakeCOH{listFn: recs(saves(dbARN, 10))}
	got, _ := runHub(t, newHub(f), census())
	if c := tipOf(t, got[0]).Caveats; len(c) != 0 {
		t.Errorf("caveats = %v, want none — blanket disclosures belong in docs", c)
	}
}

func TestAResourceYoungerThanTheWindowIsFlaggedAsExtrapolated(t *testing.T) {
	resources := census()
	born := scanTime.Add(-3 * 24 * time.Hour)
	resources[0].CreatedAt = &born
	f := &fakeCOH{listFn: recs(saves(dbARN, 10))}
	got, _ := runHub(t, newHub(f), resources)
	c := tipOf(t, got[0]).Caveats
	if len(c) != 1 || !strings.Contains(c[0], "extrapolates") {
		t.Errorf("caveats = %v, want one naming the extrapolation", c)
	}
}

func TestAResourceOlderThanTheWindowIsNotFlagged(t *testing.T) {
	resources := census()
	born := scanTime.Add(-90 * 24 * time.Hour)
	resources[0].CreatedAt = &born
	f := &fakeCOH{listFn: recs(saves(dbARN, 10))}
	got, _ := runHub(t, newHub(f), resources)
	if c := tipOf(t, got[0]).Caveats; len(c) != 0 {
		t.Errorf("caveats = %v, want none", c)
	}
}

func TestARNMatchingIsExact(t *testing.T) {
	f := &fakeCOH{listFn: recs(saves(strings.ToUpper(dbARN), 10))}
	got, _ := runHub(t, newHub(f), census())
	if got[0].Tipped() {
		t.Error("a case-folded ARN matched; advice on the wrong resource is worse than none")
	}
}

// Commitment purchases — Savings Plans, Reserved Instances — apply to an
// account rather than to anything in a resource census, so they arrive with no
// ARN and have nothing to attach to. They are often the largest savings AWS is
// offering, so a reader who sees none of them is told they existed rather than
// left to assume the hub had nothing to say.
func TestAnAccountLevelRecommendationIsLedgeredRatherThanDroppedInSilence(t *testing.T) {
	rec := saves(dbARN, 900)
	rec.ResourceArn = nil
	f := &fakeCOH{listFn: recs(rec, saves(cacheARN, 5))}
	got, failures := runHub(t, newHub(f), census())
	entry := ledgered(t, failures, "coh_account_level_recommendations")
	if !strings.Contains(entry.Error, "Savings Plans") {
		t.Errorf("the ledger entry does not say what kind of advice went unlisted: %s", entry.Error)
	}
	if got[0].Tipped() {
		t.Error("an account-level recommendation landed on a resource")
	}
	if !got[1].Tipped() {
		t.Error("an ARN-less row took the rest of the page down with it")
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

// Read against each other the three counters answer the coverage question. A
// run where thousands of recommendations arrived and nothing was tipped is an
// ARN mismatch rather than a well-configured estate, and Unattached is what
// makes the difference visible.
func TestMeterCountsRequestsAndCoverage(t *testing.T) {
	f := &fakeCOH{listFn: recs(
		saves(dbARN, 1),
		saves("arn:aws:rds:us-east-1:111122223333:db:absent", 2),
	)}
	h := newHub(f)
	runHub(t, h, census())
	m := h.Meter()
	if m.Requests != 2 {
		t.Errorf("requests = %d, want 2 (one enrollment, one list)", m.Requests)
	}
	if m.Recommendations != 2 {
		t.Errorf("recommendations = %d, want 2", m.Recommendations)
	}
	if m.Tipped != 1 {
		t.Errorf("tipped = %d, want 1", m.Tipped)
	}
	if m.Unattached != 1 {
		t.Errorf("unattached = %d, want 1: one row named a resource this census never saw", m.Unattached)
	}
}

// Tipped counts resources, not recommendations, because "20 of 98 resources
// have advice" is the coverage question a reader is asking. One resource
// carrying two suggestions is one covered resource.
func TestTippedCountsResourcesRatherThanSuggestions(t *testing.T) {
	f := &fakeCOH{listFn: recs(saves(dbARN, 340), withID(saves(dbARN, 25), "rec-storage"))}
	h := newHub(f)
	runHub(t, h, census())
	m := h.Meter()
	if m.Tipped != 1 {
		t.Errorf("tipped = %d, want 1 resource", m.Tipped)
	}
	if m.Recommendations != 2 {
		t.Errorf("recommendations = %d, want 2", m.Recommendations)
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

// A percentage is not money and is not padded like it. "12.50%" claims a
// precision AWS did not report and dresses a proportion up as a currency.
func TestFormatPercentDoesNotPadToCents(t *testing.T) {
	cases := map[float64]string{
		12.5:                 "12.5",
		30:                   "30",
		0:                    "0",
		math.Copysign(0, -1): "0",
		0.5:                  "0.5",
		33.333333333333336:   "33.333333333333336",
	}
	for in, want := range cases {
		got, ok := formatPercent(in)
		if !ok {
			t.Errorf("formatPercent(%v) was rejected", in)
			continue
		}
		if got != want {
			t.Errorf("formatPercent(%v) = %q, want %q", in, got, want)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got, ok := formatPercent(v); ok {
			t.Errorf("formatPercent(%v) = %q, want rejected", v, got)
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
		listFn: recs(saves(dbARN, 10)),
	}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_not_enrolled")
	if len(f.lists) != 0 {
		t.Errorf("recommendations were read for an account the list said is not enrolled: %d calls", len(f.lists))
	}
	if got[0].Tipped() {
		t.Error("an account the list reported inactive must produce no recommendations")
	}
}

// The converse mistake is worse. An account missing from the enrollment list
// has not been reported unenrolled — it has not been reported at all — and a
// management account that answers for the organization need not enumerate
// every member. Suppressing here would drop real advice, so the stage reads on
// and says in the ledger that the status is unknown.
func TestAnUnmentionedScannedAccountIsNotTreatedAsUnenrolled(t *testing.T) {
	f := &fakeCOH{
		enrollFn: enrollment(enrolledRow(unscannedAccount, cohtypes.EnrollmentStatusActive)),
		listFn:   recs(saves(dbARN, 10)),
	}
	got, failures := runHub(t, newHub(f), census())
	ledgered(t, failures, "coh_enrollment_unconfirmed")
	if len(f.lists) == 0 {
		t.Error("an unmentioned account must still be read for recommendations, not written off")
	}
	if !got[0].Tipped() {
		t.Error("advice AWS actually returned was dropped on an unproven enrollment guess")
	}
}

// A mixed organization reports what it can and names what it could not, rather
// than failing whole or reporting a partial answer as a complete one.
func TestAnUnenrolledAccountAlongsideAnEnrolledOneIsNamedNotSuppressed(t *testing.T) {
	f := &fakeCOH{
		enrollFn: enrollment(
			enrolledRow(scannedAccount(), cohtypes.EnrollmentStatusActive),
			enrolledRow(unscannedAccount, cohtypes.EnrollmentStatusInactive),
		),
		listFn: recs(saves(dbARN, 10)),
	}
	resources := census()
	resources[1].AccountID = unscannedAccount
	got, failures := runHub(t, newHub(f), resources)
	ledgered(t, failures, "coh_account_not_enrolled")
	if !got[0].Tipped() {
		t.Error("the enrolled account's resource must still be tipped")
	}
	if got[1].Tipped() {
		t.Error("the unenrolled account's resource must not be tipped")
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
		listFn: recs(saves(dbARN, 10)),
	}
	_, failures := runHub(t, newHub(f), census())
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}
	if len(f.enrolls) != 1 {
		t.Errorf("enrollment pages read = %d, want 1: the scanned account was confirmed on the first one", len(f.enrolls))
	}
}

// CostUnavailable is the census's way of saying a cost source looked and found
// nothing, which stops a blank cost cell being read as zero spend. This stage
// no longer writes it under any exit, and the reason is not tidiness: it is not
// a cost source. A resource it has no advice about is the ordinary state of a
// well-configured resource, and stamping "the hub named nothing for this" on
// most of an estate would turn silence into a finding — while carrying it in
// the field that means "no spend figure" would say something about money that
// the hub never looked at.
func TestNoExitPathClaimsAnAbsenceOfCost(t *testing.T) {
	page := 0
	cases := map[string]*fakeCOH{
		// The hub answered in full and named only the database.
		"complete read": {listFn: recs(saves(dbARN, 10))},
		// The read stopped early, so nothing is known about the rest.
		"truncated read": {listFn: func(*costoptimizationhub.ListRecommendationsInput) (*costoptimizationhub.ListRecommendationsOutput, error) {
			page++
			if page == 1 {
				return &costoptimizationhub.ListRecommendationsOutput{
					Items:     []cohtypes.Recommendation{saves(dbARN, 7)},
					NextToken: aws.String("more"),
				}, nil
			}
			return nil, errors.New("boom")
		}},
		// The list was never reached at all.
		"unenrolled account": {enrollFn: enrollment(enrolledRow(scannedAccount(), cohtypes.EnrollmentStatusInactive))},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			page = 0
			got, _ := runHub(t, newHub(f), census())
			for _, r := range got {
				if r.CostUnavailable != "" {
					t.Errorf("%s: a tips source explained an absence of cost: %q", r.Name, r.CostUnavailable)
				}
			}
		})
	}
}
