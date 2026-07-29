package enrich

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costoptimizationhub"
	cohtypes "github.com/aws/aws-sdk-go-v2/service/costoptimizationhub/types"
	"github.com/aws/smithy-go"

	"github.com/hoophq/blueprint/internal/cost"
	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// CostHubService is the failure-ledger service name for this stage.
//
// Like MetricsService it is a ledger name only. Registering the stage as a
// Scanner would put it in Snapshot.Services, which is part of the history
// scope key, so turning --costs on would re-bucket and orphan every existing
// user's diff history.
//
// It reads as words because it appears in a user-facing ledger next to "cost"
// and "metrics"; the machine-readable short form is model.CostMethodCOH, which
// is what lands in every priced resource's JSON.
const CostHubService = "cost-hub"

// cohRegion is where Cost Optimization Hub lives. It is a global service with
// a single endpoint homed in us-east-1, so the stage pins the region rather
// than inheriting whichever one the caller's profile happens to name.
const cohRegion = "us-east-1"

// maxRecommendationPages bounds pagination.
//
// ListRecommendations is not billed, so this is a runaway guard rather than a
// budget. Unlike the Cost Explorer rollup — where a truncated read is
// discarded, because a total that is missing pages is a wrong total — hitting
// the cap here keeps what was already read: each figure is independently
// reported for one resource, so a page that never arrived means fewer
// resources carry a cost, not that any carried cost is wrong. The gap goes in
// the ledger so "fewer" is never mistaken for "none exist".
const maxRecommendationPages = 1000

// maxEnrollmentPages bounds the enrollment check.
//
// From a management account the enrollment list is one row per member account,
// and the check stops at the first active one — so reaching this cap means an
// organization with hundreds of accounts, none of them enrolled so far. That is
// far enough to stop asking, but not far enough to conclude anything, which is
// why hitting it lets the run continue rather than declaring the hub off.
const maxEnrollmentPages = 20

// CostHubAPI is the slice of Cost Optimization Hub this package uses, named so
// tests can substitute a fake without reaching for HTTP.
type CostHubAPI interface {
	ListEnrollmentStatuses(context.Context, *costoptimizationhub.ListEnrollmentStatusesInput, ...func(*costoptimizationhub.Options)) (*costoptimizationhub.ListEnrollmentStatusesOutput, error)
	ListRecommendations(context.Context, *costoptimizationhub.ListRecommendationsInput, ...func(*costoptimizationhub.Options)) (*costoptimizationhub.ListRecommendationsOutput, error)
}

// CostHubClient builds the Cost Optimization Hub client, pinned to the one
// region the service is reachable in.
//
// The shared retryer is kept, unlike cost.Client's. Cost Explorer disables
// retries because each request is billed and a retry silently doubles the
// charge; Cost Optimization Hub's read APIs are free, so a retried throttle
// costs nothing but time and is the difference between a large organization's
// recommendations arriving and not.
func CostHubClient(cfg aws.Config) CostHubAPI {
	c := cfg.Copy()
	c.Region = cohRegion
	return costoptimizationhub.NewFromConfig(c)
}

// CostHub attaches per-resource cost estimates from Cost Optimization Hub to
// the census.
//
// # What this figure is, and what it is not
//
// ListRecommendations reports estimatedMonthlyCost for the resource's *current*
// configuration — the thing a recommendation proposes changing. That makes it
// the only AWS API that hands back a per-resource dollar figure without the
// caller inventing an allocation, which is why it is the source here. It is
// still a model: a forward-looking monthly rate extrapolated from a usage
// lookback, not an amount anyone was invoiced. It is therefore recorded with
// Method "coh" and Estimated true, and it is never summed with, reconciled
// against, or substituted for the billed Cost Explorer rollup in
// Snapshot.Cost.
//
// # Coverage is partial by construction
//
// Cost Optimization Hub only models resource types it has recommendations for
// (roughly two dozen: EC2, EBS, Lambda, NAT gateways, and most managed
// databases — not S3, not load balancers). A resource it does not cover comes
// away with no cost at all, which is the honest answer; the alternative,
// spreading an account rollup across resources, is the fabricated number the
// guardrails forbid. The meter reports how many resources were priced so the
// gap is visible rather than implied.
//
// # Threading
//
// The stage is deliberately single-goroutine. Cost Optimization Hub is one
// flat, global list rather than a per-region endpoint, so there is nothing to
// fan out over and Enrichment.Concurrency goes unused.
type CostHub struct {
	// Cfg is the caller's own credentials, not a per-account target's.
	//
	// Cost Optimization Hub is org-wide from the management account: one call
	// with the payer's credentials returns recommendations for every enrolled
	// member account, each tagged with its own account ID. Assuming a role per
	// member account would re-read the same list N times, and most member
	// accounts cannot read it at all.
	Cfg aws.Config
	// Account is the caller's account ID, used to attribute ledger entries
	// raised before any recommendation has been read.
	Account string

	// NewClient builds the client. Nil means CostHubClient.
	NewClient func(aws.Config) CostHubAPI

	// Now replaces time.Now for ledger timestamps. Nil means time.Now.
	Now func() time.Time

	meter CostHubMeter
}

// CostHubMeter records what one run read.
//
// There is no charge field, and that absence is the point: the neighbouring
// cost and metrics stages both bill the user and both report what they spent,
// so a stage that reports no charge has to be one that provably incurs none.
// Cost Optimization Hub's read APIs are free.
type CostHubMeter struct {
	// Requests is the number of API calls AWS answered.
	Requests int
	// Recommendations is how many rows were read across all pages.
	Recommendations int
	// Priced is how many census resources came away with a cost. Read against
	// Recommendations it is the coverage figure: a run where thousands of
	// recommendations priced nothing is an ARN mismatch, not an empty estate.
	Priced int
}

// Meter returns the tally so far. Safe to call once Enrich has returned.
func (h *CostHub) Meter() CostHubMeter { return h.meter }

// NewCostHub returns the stage wired to the real client, using the caller's
// own credentials and account.
func NewCostHub(cfg aws.Config, account string) *CostHub {
	return &CostHub{Cfg: cfg, Account: account, NewClient: CostHubClient}
}

// Name implements scan.Enricher.
func (h *CostHub) Name() string { return CostHubService }

var _ scan.Enricher = (*CostHub)(nil)

func (h *CostHub) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

// figure is one recommendation reduced to the fields the census keeps.
type figure struct {
	amount   string
	currency string
	from     *time.Time
	to       *time.Time
	// resType is Cost Optimization Hub's own resource type name, kept only to
	// decide whether the amount covers the whole resource (see partialScope).
	resType string
}

// Enrich implements scan.Enricher.
func (h *CostHub) Enrich(ctx context.Context, req scan.Enrichment) []model.Failure {
	if len(req.Resources) == 0 {
		return nil
	}

	var failures []model.Failure
	report := func(account, region, format string, args ...any) {
		failures = append(failures, model.Failure{
			AccountID: account,
			Region:    region,
			Service:   CostHubService,
			Error:     fmt.Sprintf(format, args...),
			Time:      h.now(),
		})
	}

	newClient := h.NewClient
	if newClient == nil {
		newClient = CostHubClient
	}
	api := newClient(h.Cfg)

	accounts := censusAccounts(req.Resources)
	if !h.enrolled(ctx, api, accounts, report) {
		return failures
	}

	figures := h.recommendations(ctx, api, accounts, report)
	h.attach(req.Resources, figures)
	return failures
}

// censusAccounts lists the accounts the scan actually covered, sorted.
//
// It is read off the census rather than off Enrichment.Targets so the filter
// matches what was scanned even if a target produced nothing, and it exists to
// stop a payer-credentialed run from paging through an entire organization's
// recommendations for a one-account census.
func censusAccounts(resources []model.Resource) []string {
	set := map[string]bool{}
	for i := range resources {
		if id := resources[i].AccountID; id != "" {
			set[id] = true
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// enrolled reports whether Cost Optimization Hub is switched on, and stops the
// stage when it is not.
//
// The check is not optional politeness. An unenrolled account answers
// ListRecommendations with an empty list and no error, which is
// indistinguishable from "this estate has no recommendations" — so without
// asking first, a census would quietly report no costs for anything and give
// the reader no reason to doubt it. One extra free call converts a silent
// blank into a ledger entry naming the fix.
func (h *CostHub) enrolled(ctx context.Context, api CostHubAPI, accounts []string, report reporter) bool {
	var token *string
	for page := 0; page < maxEnrollmentPages; page++ {
		if ctx.Err() != nil {
			return false
		}
		// AccountId is deliberately unset. From a plain account that asks about
		// the caller's own enrollment; from a management account it also lists
		// the members, and either answer settles the question this check
		// exists to settle — is there anything here to read.
		out, err := api.ListEnrollmentStatuses(ctx, &costoptimizationhub.ListEnrollmentStatusesInput{NextToken: token})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return false
			}
			report(h.Account, "", "%s", h.classify("checking Cost Optimization Hub enrollment", err))
			return false
		}
		h.meter.Requests++

		for _, item := range out.Items {
			if item.Status != cohtypes.EnrollmentStatusActive {
				continue
			}
			// A payer enrolled for itself alone still answers
			// ListRecommendations, but only about its own resources, so every
			// other scanned account comes away unpriced for a reason that is
			// a settings choice rather than a property of the estate.
			if len(accounts) > 1 && out.IncludeMemberAccounts != nil && !*out.IncludeMemberAccounts {
				report(h.Account, "", "coh_member_accounts_excluded: Cost Optimization Hub is enrolled for this "+
					"account only, not for the organization, so resources in the other %d scanned account(s) "+
					"come away with no cost estimate — enable member accounts in the Cost Optimization Hub "+
					"console from the management account", len(accounts)-1)
			}
			return true
		}

		if out.NextToken == nil || *out.NextToken == "" {
			// The whole list was read and nothing in it was active, which is
			// the one case where "no recommendations" can be explained rather
			// than merely observed.
			report(h.Account, "", "coh_not_enrolled: Cost Optimization Hub is not enabled for this account, so no "+
				"per-resource cost estimates were collected — opt in from the Cost Optimization Hub console "+
				"(it is free, and AWS takes about 24 hours to produce the first recommendations); until then "+
				"account-level spend from --costs is the only cost figure available")
			return false
		}
		token = out.NextToken
	}

	// The list ran longer than the cap without an active status. That is not
	// proof of anything: the run continues, because refusing to read
	// recommendations here would suppress real costs on the strength of a
	// check that did not finish, and an empty result is already reported by
	// the meter's coverage figures.
	report(h.Account, "", "coh_enrollment_unknown: gave up checking Cost Optimization Hub enrollment after %d pages "+
		"of member accounts without finding an active one; recommendations were read anyway, so an empty result "+
		"below may mean the service is not enabled rather than that nothing was found", maxEnrollmentPages)
	return true
}

// recommendations reads the whole list and reduces it to one figure per ARN.
func (h *CostHub) recommendations(ctx context.Context, api CostHubAPI, accounts []string, report reporter) map[string]figure {
	figures := map[string]figure{}
	// conflicting holds ARNs two recommendations disagreed about; they are
	// dropped rather than resolved, so nothing is published that cannot be
	// defended. Tracked separately from figures so a conflict discovered on a
	// later page still suppresses the earlier value.
	conflicting := map[string]bool{}

	var token *string
	for page := 0; ; page++ {
		if ctx.Err() != nil {
			return nil
		}
		if page >= maxRecommendationPages {
			report(h.Account, "", "coh_pagination_incomplete: stopped reading Cost Optimization Hub after %d pages, "+
				"so resources on the pages not read come away with no cost estimate; the figures already "+
				"attached are unaffected", maxRecommendationPages)
			break
		}
		out, err := api.ListRecommendations(ctx, &costoptimizationhub.ListRecommendationsInput{
			// De-duped by resource: one row per resource is exactly the shape
			// a per-resource cost wants, and every recommendation for a given
			// resource reports the same current cost anyway.
			IncludeAllRecommendations: false,
			// Restricted to the accounts the census covers, so a management
			// account scanning one member does not page through the whole
			// organization. Nothing is lost: a recommendation for an
			// unscanned account has no resource here to attach to.
			Filter:    &cohtypes.Filter{AccountIds: accounts},
			NextToken: token,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return figures
			}
			report(h.Account, "", "%s", h.classify(fmt.Sprintf("reading Cost Optimization Hub recommendations (page %d)", page+1), err))
			break
		}
		h.meter.Requests++
		h.meter.Recommendations += len(out.Items)

		for _, rec := range out.Items {
			arn := aws.ToString(rec.ResourceArn)
			if arn == "" || conflicting[arn] {
				continue
			}
			f, ok := h.reduce(rec, report)
			if !ok {
				continue
			}
			prev, seen := figures[arn]
			if !seen {
				figures[arn] = f
				continue
			}
			if prev.amount == f.amount && prev.currency == f.currency {
				continue
			}
			// Two rows, two different prices, no way to tell which is the
			// resource's cost — and summing them would assume they cover
			// disjoint parts of it, which nothing in the response says.
			delete(figures, arn)
			conflicting[arn] = true
			report(h.Account, "", "coh_conflicting_amounts: Cost Optimization Hub reported different costs for %s "+
				"(%s %s as %s, %s %s as %s), so neither was attached rather than picking one arbitrarily",
				arn, prev.currency, prev.amount, prev.resType, f.currency, f.amount, f.resType)
		}

		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		token = out.NextToken
	}
	return figures
}

// reduce turns one recommendation into a storable figure, or reports why it
// could not be stored.
func (h *CostHub) reduce(rec cohtypes.Recommendation, report reporter) (figure, bool) {
	// Pointer presence is the test, never the value: a resource genuinely
	// modelled at zero dollars a month is a real finding, and treating it as
	// "not reported" would be the same bug as filtering measures on > 0.
	if rec.EstimatedMonthlyCost == nil {
		return figure{}, false
	}
	amount, ok := formatAmount(*rec.EstimatedMonthlyCost)
	if !ok {
		report(aws.ToString(rec.AccountId), aws.ToString(rec.Region),
			"coh_unusable_amount: Cost Optimization Hub reported a cost for %s that is not a finite number (%v), "+
				"so it was dropped rather than rendered as a figure",
			aws.ToString(rec.ResourceArn), *rec.EstimatedMonthlyCost)
		return figure{}, false
	}
	from, to := observedWindow(rec)
	return figure{
		amount:   amount,
		currency: aws.ToString(rec.CurrencyCode),
		from:     from,
		to:       to,
		resType:  aws.ToString(rec.CurrentResourceType),
	}, true
}

// attach writes the figures onto the census, matching by ARN.
//
// The match is exact. A looser one — case-insensitive, or on the resource ID
// alone — would let a figure land on the wrong resource, and a wrong price is
// worse than none. A systematic mismatch is not hidden by that strictness: the
// meter reports how many resources were priced against how many
// recommendations were read, so "thousands read, none matched" is visible.
func (h *CostHub) attach(resources []model.Resource, figures map[string]figure) {
	if len(figures) == 0 {
		return
	}
	for i := range resources {
		r := &resources[i]
		f, ok := figures[r.ARN]
		if !ok {
			continue
		}
		r.Cost = &model.ResourceCost{
			Amount:       f.amount,
			Currency:     f.currency,
			Method:       model.CostMethodCOH,
			Estimated:    true,
			ObservedFrom: f.from,
			ObservedTo:   f.to,
			Caveats:      caveats(r, f),
		}
		h.meter.Priced++
	}
}

// partialScope names Cost Optimization Hub resource types whose
// estimatedMonthlyCost covers one component of a resource rather than all of
// it — the hub models an RDS instance's compute and its storage as separate
// recommendations. Attaching a storage figure to a database without saying so
// would understate it by whatever the compute costs.
var partialScope = map[string]string{
	"RdsDbInstanceStorage":   "storage",
	"AuroraDbClusterStorage": "storage",
}

// caveats derives the per-resource disclosures that qualify one figure.
//
// Both conditions here are read off what the source said about this specific
// resource. Statements true of every Cost Optimization Hub figure — that it is
// modelled, that it is a forward-looking rate — are carried by Method and
// Estimated instead of repeated on every row.
func caveats(r *model.Resource, f figure) []string {
	var out []string
	if part, ok := partialScope[f.resType]; ok {
		out = append(out, fmt.Sprintf("covers %s only (Cost Optimization Hub resource type %s); "+
			"other charges for this resource are not included", part, f.resType))
	}
	// A resource younger than the window the model ran over has a monthly rate
	// extrapolated from a partial period, which reads as a settled run rate
	// unless it is said out loud.
	if r.CreatedAt != nil && f.from != nil && r.CreatedAt.After(*f.from) {
		out = append(out, fmt.Sprintf("created %s, after this figure's usage period began on %s — "+
			"the monthly rate extrapolates a partial period",
			r.CreatedAt.UTC().Format("2006-01-02"), f.from.Format("2006-01-02")))
	}
	return out
}

// observedWindow places the usage period a figure was modelled over.
//
// A monthly rate with no window attached cannot be judged for staleness — the
// same problem model.AsOfSuffix solves for CloudWatch readings — so the
// recommendation's own refresh time and lookback are carried alongside the
// amount. Either end is left nil when the recommendation did not report enough
// to place it, rather than guessed at.
func observedWindow(rec cohtypes.Recommendation) (from, to *time.Time) {
	if rec.LastRefreshTimestamp == nil {
		return nil, nil
	}
	end := rec.LastRefreshTimestamp.UTC()
	to = &end
	// Pointer presence again: a reported zero-day lookback yields an empty
	// window, which is a strange thing for AWS to say but is what it said. A
	// negative one is not a period at all, so the start is left unplaced.
	if rec.RecommendationLookbackPeriodInDays == nil || *rec.RecommendationLookbackPeriodInDays < 0 {
		return nil, to
	}
	start := end.Add(-time.Duration(*rec.RecommendationLookbackPeriodInDays) * 24 * time.Hour)
	return &start, to
}

// formatAmount renders a Cost Optimization Hub amount as the decimal string
// the census stores.
//
// This is the one place in blueprint where money arrives as a float64, and it
// is not a choice: ListRecommendations models estimatedMonthlyCost as a
// double, so the value has already been through binary floating point before
// this tool sees it. There is no exact decimal here to preserve — only a
// float64 to describe without adding a lie on top of it.
//
// strconv.FormatFloat(v, 'f', -1, 64) is that description. Precision -1 asks
// for the shortest digit string that reads back as exactly this float64: it
// invents no digits the value does not have and rounds none away. Format 'f'
// never emits exponent notation, so the result satisfies the same decimal
// grammar every other amount in the census is held to — which the call to
// cost.ValidDecimal then proves rather than assumes.
//
// NaN and the infinities are rejected outright. There is no decimal string for
// them, and any stand-in would render as a real price.
func formatAmount(v float64) (string, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "", false
	}
	if v == 0 {
		// A resource modelled at exactly zero is a real reading and is kept —
		// but FormatFloat renders negative zero as "-0", and a bill of minus
		// nothing is not a thing anyone should read in an artifact.
		return "0.00", true
	}
	s := atLeastCents(strconv.FormatFloat(v, 'f', -1, 64))
	if !cost.ValidDecimal(s) {
		// Unreachable for a finite float64 formatted with 'f'. Kept because
		// the cost of being wrong is a malformed amount in an artifact that
		// promises every amount is a decimal number.
		return "", false
	}
	return s, true
}

// atLeastCents pads a decimal string out to two places so an amount reads like
// money. It only ever appends zeros, so it can lose no precision and change no
// value: "1.5" becomes "1.50", and "0.30000000000000004" is left alone.
func atLeastCents(s string) string {
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return s + ".00"
	}
	for len(s)-dot-1 < 2 {
		s += "0"
	}
	return s
}

// classify turns an API error into a ledger line that names the fix.
//
// Same shape and same reasoning as internal/cost's classify: the kind is a
// prefix on the error text rather than a separate field, because the ledger
// sorts on (account, region, service, error) and a field outside that key
// would leave two entries differing only by kind to be ordered by the clock.
func (h *CostHub) classify(what string, err error) string {
	code, msg := cohAPIError(err)
	lower := strings.ToLower(msg)
	switch {
	case code == "AccessDeniedException" || code == "AccessDenied" || strings.Contains(lower, "not authorized"):
		return fmt.Sprintf("coh_access_denied: %s: not authorized — add the BlueprintCostOptimizationHub "+
			"statement from docs/iam-policy.json to the credentials blueprint is using (%v)", what, err)
	case code == "ThrottlingException", code == "TooManyRequestsException", code == "LimitExceededException":
		return fmt.Sprintf("coh_throttled: %s: Cost Optimization Hub throttled the request after retries, so "+
			"some resources come away with no cost estimate — re-run to try again (%v)", what, err)
	case code == "ValidationException" && strings.Contains(lower, "token"):
		return fmt.Sprintf("coh_pagination_incomplete: %s: Cost Optimization Hub rejected the pagination token, "+
			"so only the pages read before it failed were used (%v)", what, err)
	case code == "ResourceNotFoundException":
		return fmt.Sprintf("coh_not_enrolled: %s: Cost Optimization Hub has nothing for this account, which is "+
			"what it returns before the service is opted into — enable it in the Cost Optimization Hub "+
			"console (it is free) (%v)", what, err)
	}
	return fmt.Sprintf("coh_failed: %s: %v", what, err)
}

// cohAPIError extracts the service error code and message when the error came
// from AWS.
func cohAPIError(err error) (code, msg string) {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode(), ae.ErrorMessage()
	}
	return "", err.Error()
}
