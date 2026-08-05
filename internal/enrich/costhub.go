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
// and "metrics".
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
// the cap here keeps what was already read: each recommendation stands on its
// own for one resource, so a page that never arrived means fewer resources
// carry advice, not that any carried advice is wrong. The gap goes in the
// ledger so "fewer" is never mistaken for "none exist".
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

// CostHub attaches Cost Optimization Hub's recommendations to the census: what
// AWS says could be changed about a resource to spend less on it.
//
// # Why this is advice and not a price
//
// ListRecommendations is a recommendations API. Per resource it returns an
// action, a recommended replacement, an estimated monthly saving, an
// implementation effort, and whether the change needs a restart or can be
// rolled back. It also returns estimatedMonthlyCost — and this stage used to
// read that field, and only that field, and record it as a per-resource price
// with Method "coh".
//
// That was the wrong reading. estimatedMonthlyCost is the *"before" baseline of
// a recommendation*: what the resource is modelled to cost per month as
// configured today, extrapolated forward from a usage lookback. Nobody was ever
// invoiced it. Published beside Cost Explorer's billed figure it did not read as
// a different question — it read as two AWS services disagreeing about one
// number, and the reader was left to reconcile a modelled month against a
// billed fortnight.
//
// So the census now takes from this API the field that is actually actionable
// and throws away the one that looked like money. Cost Explorer answers what you
// paid. Cost Optimization Hub answers what you could stop paying. The two are
// never added, subtracted, or reconciled: a saving is not spend, and a saving
// that has not been acted on is not even a saving yet.
//
// # Coverage is partial by construction
//
// Cost Optimization Hub only models resource types it has advice for (roughly
// two dozen: EC2, EBS, Lambda, NAT gateways, and most managed databases — not
// S3, not load balancers). A resource it does not cover comes away with no
// recommendation, and under the old reading that was a reportable absence,
// because a missing price invites being read as zero. It is not one now: having
// no advice is the ordinary state of a well-configured resource, and annotating
// most of an estate with "no recommendation" would be noise dressed as a
// finding. The meter reports how many resources were tipped so the coverage is
// visible rather than implied.
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
	// Tipped is how many census resources came away with at least one
	// recommendation. Read against Recommendations it is the coverage figure: a
	// run where thousands of recommendations tipped nothing is an ARN mismatch,
	// not a well-configured estate.
	//
	// It counts resources rather than recommendations because one resource can
	// carry several, and "20 of 98 resources have advice" is the coverage
	// question a reader is asking.
	Tipped int
	// Unattached is how many recommendations named a resource this census does
	// not hold — a scanner gap or an account-level suggestion, not an error.
	// Kept separate from Recommendations so the two halves of a mismatch stay
	// distinguishable: advice that arrived and found nothing to land on is a
	// different problem from advice that never arrived.
	Unattached int
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

// tip is one recommendation reduced to the fields the census keeps, before it
// is attached to a resource. It is model.Recommendation minus the caveats,
// which are derived at attach time from what the census knows about the
// resource the tip lands on.
type tip struct {
	rec model.Recommendation
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

	h.attach(req.Resources, h.recommendations(ctx, api, accounts, report))
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
// The enrollment list is read against the accounts the census actually
// covers, not merely for "is anything here active". An organization can have
// the hub switched on for accounts this scan never touched, and answering
// "enrolled" off one of those rows would send the stage on to a filtered
// ListRecommendations that comes back empty with nothing in the ledger to
// explain it — the silent blank this check exists to prevent, just moved one
// step later.
//
// The converse mistake is worse, so absence is never read as a denial: an
// account missing from the list has not been reported unenrolled, it has not
// been reported at all, and suppressing its costs on that basis would drop
// real figures. Only a scanned account the list positively names as inactive
// counts as evidence against it.
func (h *CostHub) enrolled(ctx context.Context, api CostHubAPI, accounts []string, report reporter) bool {
	scanned := make(map[string]bool, len(accounts))
	for _, id := range accounts {
		scanned[id] = true
	}

	var (
		token *string
		// activeScanned and inactiveScanned hold scanned accounts the list
		// named, by what it said about them. An account in neither was not
		// mentioned, which is not the same as being switched off.
		activeScanned   = map[string]bool{}
		inactiveScanned = map[string]bool{}
		// unattributedActive covers a row that reports an active hub without
		// naming an account — how a plain account hears about its own
		// enrollment. There is nothing to match it against, so it is taken at
		// its word.
		unattributedActive bool
		// otherActive means the hub is on somewhere outside the census. It
		// cannot price a scanned resource, but it does rule out "the whole
		// list is switched off" as the explanation for an empty result.
		otherActive    bool
		includeMembers *bool
		// settled distinguishes a list that ended from one the page cap cut
		// short. A truncated list is not evidence of anything.
		settled bool
	)

	for page := 0; page < maxEnrollmentPages && !settled; page++ {
		if ctx.Err() != nil {
			return false
		}
		// AccountId is deliberately unset on the request. From a plain account
		// that asks about the caller's own enrollment; from a management
		// account it also lists the members, and both answers are read here.
		out, err := api.ListEnrollmentStatuses(ctx, &costoptimizationhub.ListEnrollmentStatusesInput{NextToken: token})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return false
			}
			report(h.Account, "", "%s", h.classify("checking Cost Optimization Hub enrollment", err))
			return false
		}
		h.meter.Requests++
		if out.IncludeMemberAccounts != nil {
			includeMembers = out.IncludeMemberAccounts
		}

		for _, item := range out.Items {
			id := aws.ToString(item.AccountId)
			switch {
			case item.Status == cohtypes.EnrollmentStatusActive && id == "":
				unattributedActive = true
			case item.Status == cohtypes.EnrollmentStatusActive && scanned[id]:
				activeScanned[id] = true
			case item.Status == cohtypes.EnrollmentStatusActive:
				otherActive = true
			case scanned[id]:
				// Named, scanned, and not active — the only shape that is
				// evidence a scanned account is switched off.
				inactiveScanned[id] = true
			}
		}

		switch {
		case len(activeScanned) == len(accounts):
			// Every scanned account is confirmed on; nothing further in the
			// list can change the answer, so the remaining pages of an
			// organization's members go unread.
			settled = true
		case out.NextToken == nil || *out.NextToken == "":
			settled = true
		default:
			token = out.NextToken
		}
	}

	if len(activeScanned) == 0 && !unattributedActive {
		switch {
		case !settled:
			// The list ran past the cap without confirming anything. That is
			// not proof either way: the run continues, because refusing to
			// read recommendations here would suppress real costs on the
			// strength of a check that did not finish.
			report(h.Account, "", "coh_enrollment_unknown: gave up checking Cost Optimization Hub enrollment after %d "+
				"pages of member accounts without reaching the scanned one(s); recommendations were read anyway, so "+
				"an empty result below may mean the service is not enabled rather than that there is nothing to act on",
				maxEnrollmentPages)
			return true
		case len(inactiveScanned) == len(accounts), !otherActive:
			// Either every scanned account was named and none is on, or the
			// whole list was read and nothing anywhere is on. Both are the
			// case where an empty result can be explained rather than merely
			// observed.
			report(h.Account, "", "coh_not_enrolled: Cost Optimization Hub is not enabled for the %d scanned "+
				"account(s), so no cost-cutting recommendations were collected — opt in from the Cost Optimization "+
				"Hub console (it is free, and AWS takes about 24 hours to produce the first recommendations); "+
				"spend figures are unaffected, they come from Cost Explorer", len(accounts))
			return false
		default:
			// The hub is on for accounts outside the census and the scanned
			// ones were never mentioned. Proceeding is right — they may well
			// be enrolled — but an empty result now has a second possible
			// reading, and the reader is told which.
			report(h.Account, "", "coh_enrollment_unconfirmed: Cost Optimization Hub is enabled in this organization, "+
				"but the enrollment list never named the %d scanned account(s), so their status is unknown; "+
				"recommendations were read anyway, and an empty result may mean those accounts are not enrolled "+
				"rather than that there is nothing to act on", len(accounts))
			return true
		}
	}

	if len(inactiveScanned) > 0 {
		report(h.Account, "", "coh_account_not_enrolled: Cost Optimization Hub is not enabled for %d of the %d "+
			"scanned account(s), so their resources come away with no recommendations while the rest are covered "+
			"— opt those accounts in from the Cost Optimization Hub console", len(inactiveScanned), len(accounts))
	}
	// A payer enrolled for itself alone still answers ListRecommendations, but
	// only about its own resources, so every other scanned account comes away
	// with no advice for a reason that is a settings choice rather than a
	// property of the estate.
	if len(accounts) > 1 && includeMembers != nil && !*includeMembers {
		report(h.Account, "", "coh_member_accounts_excluded: Cost Optimization Hub is enrolled for this "+
			"account only, not for the organization, so resources in the other %d scanned account(s) "+
			"come away with no recommendations — enable member accounts in the Cost Optimization Hub "+
			"console from the management account", len(accounts)-1)
	}
	return true
}

// recommendations reads the whole list and groups it by ARN.
//
// One ARN can map to several tips. The hub reasons about resources more finely
// than the census does — a database's compute and its storage are separate
// recommendations — and both are true at once, so they are kept as a list
// rather than reconciled into one.
// A short read is not distinguished from an exhausted one in the return value.
// It used to be: while the hub was a price source, a resource missing from the
// answer was stamped "the hub named no price for this", and that sentence is
// only true if the whole list was read. Nothing is stamped now, so the
// distinction has no consumer — a truncated read means fewer tips, which the
// ledger already says out loud.
func (h *CostHub) recommendations(ctx context.Context, api CostHubAPI, accounts []string, report reporter) map[string][]tip {
	tips := map[string][]tip{}
	// unnamed counts recommendations that arrived without an ARN. They are
	// account-level suggestions — buy a Savings Plan, buy Reserved Instances —
	// which are real advice with nothing in a resource census to attach to.
	// Counted rather than dropped in silence: they are often the largest
	// savings AWS is offering, and a reader who sees none of them should be
	// told they existed.
	unnamed := 0
	// seen holds the recommendation ids already stored, so a row that arrives
	// twice is stored once. See the append below for why that is the only
	// de-duplication this stage does.
	seen := map[string]bool{}

	var token *string
	for page := 0; ; page++ {
		if ctx.Err() != nil {
			return nil
		}
		if page >= maxRecommendationPages {
			report(h.Account, "", "coh_pagination_incomplete: stopped reading Cost Optimization Hub after %d pages, "+
				"so resources on the pages not read come away with no recommendations; the ones already "+
				"attached are unaffected", maxRecommendationPages)
			break
		}
		out, err := api.ListRecommendations(ctx, &costoptimizationhub.ListRecommendationsInput{
			// De-duped by resource, which is AWS's own choice of the best
			// suggestion per resource rather than every option it weighed.
			//
			// Asking for all of them would return alternatives — rightsize this
			// instance *or* migrate it to Graviton — that are mutually
			// exclusive, so no total over them could be honest and a reader
			// would see three tips where there is one decision. The
			// de-duplication is by resource id, not by ARN, so the case that
			// actually matters still arrives in full: a database's compute and
			// its storage have separate ids and come back as separate tips.
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
				return tips
			}
			report(h.Account, "", "%s", h.classify(fmt.Sprintf("reading Cost Optimization Hub recommendations (page %d)", page+1), err))
			break
		}
		h.meter.Requests++
		h.meter.Recommendations += len(out.Items)

		for _, rec := range out.Items {
			arn := aws.ToString(rec.ResourceArn)
			if arn == "" {
				unnamed++
				continue
			}
			t, ok := h.reduce(rec, report)
			if !ok {
				continue
			}
			// Appended, never merged. Two recommendations for one ARN are two
			// things to do, not two answers to one question — the case this
			// used to treat as a conflict and drop both halves of.
			//
			// One row arriving twice under the same recommendation id is the
			// exception, and it is not an exception to that rule so much as an
			// application of it: by AWS's own identifier those are not two
			// things to do, they are one thing listed twice. Keeping both would
			// print the suggestion twice and count its saving twice in the
			// per-currency total. A row AWS gave no id is kept as it came —
			// telling two of those apart would need a similarity rule this tool
			// would be inventing, and inventing one risks dropping real advice
			// to avoid printing a duplicate.
			if t.rec.ID != "" {
				if seen[t.rec.ID] {
					continue
				}
				seen[t.rec.ID] = true
			}
			tips[arn] = append(tips[arn], t)
		}

		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		token = out.NextToken
	}

	if unnamed > 0 {
		report(h.Account, "", "coh_account_level_recommendations: Cost Optimization Hub returned %d recommendation(s) "+
			"that name no resource — commitment purchases such as Savings Plans and Reserved Instances apply to an "+
			"account rather than to anything in this census, so they are not listed here; see them in the Cost "+
			"Optimization Hub console", unnamed)
	}
	return tips
}

// reduce turns one recommendation into a storable tip.
//
// It never rejects a recommendation for saying too little. A row with an action
// and no savings figure is advice AWS gave and the reader can act on; dropping
// it because the money is missing would suppress the tip and report nothing in
// its place. Only an amount that cannot be written down as a decimal is
// dropped, and that goes to the ledger.
func (h *CostHub) reduce(rec cohtypes.Recommendation, report reporter) (tip, bool) {
	out := model.Recommendation{
		ID:                         aws.ToString(rec.RecommendationId),
		ActionType:                 aws.ToString(rec.ActionType),
		Currency:                   aws.ToString(rec.CurrencyCode),
		CurrentResourceType:        aws.ToString(rec.CurrentResourceType),
		RecommendedResourceType:    aws.ToString(rec.RecommendedResourceType),
		CurrentResourceSummary:     aws.ToString(rec.CurrentResourceSummary),
		RecommendedResourceSummary: aws.ToString(rec.RecommendedResourceSummary),
		ImplementationEffort:       aws.ToString(rec.ImplementationEffort),
		RestartNeeded:              rec.RestartNeeded,
		RollbackPossible:           rec.RollbackPossible,
	}
	out.ObservedFrom, out.ObservedTo = observedWindow(rec)

	// Pointer presence is the test, never the value. A recommendation modelled
	// to save exactly zero dollars a month is a real answer — a change worth
	// making that pays for nothing — and filtering on > 0 would reclassify it
	// as "AWS said nothing", which is the recurring bug the measure setters
	// exist to prevent.
	if rec.EstimatedMonthlySavings != nil {
		amount, ok := formatAmount(*rec.EstimatedMonthlySavings)
		if !ok {
			report(aws.ToString(rec.AccountId), aws.ToString(rec.Region),
				"coh_unusable_savings: Cost Optimization Hub reported a saving for %s that is not a finite number (%v), "+
					"so the figure was dropped; the recommendation itself is still listed, without one",
				aws.ToString(rec.ResourceArn), *rec.EstimatedMonthlySavings)
		} else {
			out.EstimatedMonthlySavings = amount
		}
	}
	if rec.EstimatedSavingsPercentage != nil {
		pct, ok := formatPercent(*rec.EstimatedSavingsPercentage)
		if !ok {
			report(aws.ToString(rec.AccountId), aws.ToString(rec.Region),
				"coh_unusable_savings_percentage: Cost Optimization Hub reported a savings percentage for %s that is "+
					"not a finite number (%v), so it was dropped; the recommendation itself is still listed",
				aws.ToString(rec.ResourceArn), *rec.EstimatedSavingsPercentage)
		} else {
			out.EstimatedSavingsPercentage = pct
		}
	}
	return tip{rec: out}, true
}

// attach writes the tips onto the census, matching by ARN.
//
// The match is exact. A looser one — case-insensitive, or on the resource ID
// alone — would let advice land on the wrong resource, and telling someone to
// downsize a database that is not the one AWS meant is worse than telling them
// nothing. A systematic mismatch is not hidden by that strictness: the meter
// reports how many resources were tipped and how many recommendations found no
// resource at all, so "thousands read, none matched" is visible.
//
// A resource with no tip is left untouched. Under the old reading this stage
// stamped an explicit "no figure here" on every unmatched resource, because a
// blank cost cell invites being read as zero. A blank tip does not: the hub
// having no advice about a resource is the ordinary case, and writing it down
// on most of an estate would turn silence into noise.
func (h *CostHub) attach(resources []model.Resource, tips map[string][]tip) {
	matched := map[string]bool{}
	for i := range resources {
		r := &resources[i]
		found, ok := tips[r.ARN]
		if !ok {
			continue
		}
		matched[r.ARN] = true
		for _, t := range found {
			rec := t.rec
			rec.Caveats = caveats(r, &rec)
			r.AddRecommendation(rec)
		}
		h.meter.Tipped++
	}
	for arn, found := range tips {
		if !matched[arn] {
			h.meter.Unattached += len(found)
		}
	}
}

// caveats derives the disclosures that qualify one recommendation.
//
// The condition here is read off what the source said about this specific
// recommendation. Statements true of every Cost Optimization Hub row — that the
// saving is modelled, that it is a forward-looking monthly rate — belong in
// model.Recommendation's documentation and in the report's standing note, not
// repeated on every row of the artifact.
//
// Scope is deliberately not a caveat. The hub models a database's compute and
// its storage separately, and under the old reading that had to be disclosed,
// because a storage price attached to a database understates it. A storage
// saving attached to a database is simply a saving on its storage:
// CurrentResourceType says so on the tip itself, and a sentence repeating it
// would be explaining the data rather than qualifying it.
func caveats(r *model.Resource, rec *model.Recommendation) []string {
	var out []string
	// A resource younger than the window the model ran over has its saving
	// extrapolated from a partial period, which reads as a settled monthly rate
	// unless it is said out loud.
	if r.CreatedAt != nil && rec.ObservedFrom != nil && r.CreatedAt.After(*rec.ObservedFrom) {
		out = append(out, fmt.Sprintf("created %s, after this recommendation's usage period began on %s — "+
			"the monthly saving extrapolates a partial period",
			r.CreatedAt.UTC().Format("2006-01-02"), rec.ObservedFrom.Format("2006-01-02")))
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

// formatPercent renders a savings percentage as a decimal string.
//
// It is formatAmount without the money in it, and the difference is the whole
// reason it exists separately. atLeastCents pads to two places because that is
// what an amount of money looks like; a percentage padded the same way claims a
// precision AWS did not report and dresses a proportion up as a currency. So
// this shares the float64 description (shortest digits that read back exactly,
// no exponent) and the non-finite rejection, and skips the padding.
//
// cost.ValidDecimal is reused as the grammar check rather than re-derived: it
// tests that a string is a plain decimal number and caps no number of places,
// so it fits a percentage exactly as well as it fits an amount.
func formatPercent(v float64) (string, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "", false
	}
	if v == 0 {
		// A recommendation AWS models as saving 0% is a real reading and is
		// kept. FormatFloat renders negative zero as "-0", which is not a
		// proportion anyone should read in an artifact.
		return "0", true
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !cost.ValidDecimal(s) {
		// Unreachable for a finite float64 formatted with 'f', and kept for the
		// same reason as its twin in formatAmount.
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
			"some resources come away with no recommendations — re-run to try again (%v)", what, err)
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
