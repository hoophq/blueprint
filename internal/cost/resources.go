package cost

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/hoophq/blueprint/internal/model"
)

// Resource-level cost attribution: what AWS billed for one resource, rather
// than for one account or one service.
//
// This pass exists because AWS's own documentation disagrees with itself about
// whether it is possible. The GetCostAndUsageWithResources API reference states
// that the operation "requires the Expression 'SERVICE = Amazon Elastic Compute
// Cloud - Compute' in the filter" and that resource IDs are available only for
// EC2-Compute; the Cost Explorer console offers a per-service picker for daily
// resource-level data. Both cannot be true. Rather than encode a guess, this
// pass asks each service the census scanned and records what came back, service
// by service, in model.ResourceCostReport. The answer is the artifact.
//
// Three things follow from that framing and are load-bearing:
//
// A service that returns nothing is recorded as returning nothing, never as
// unsupported. Only an outright rejection from AWS is evidence of non-support.
//
// A service never asked about — because the budget ran out, or because no
// scanner covers it — is recorded as unasked. It is not a service without
// spend.
//
// No resource is marked CostUnavailable by this pass. An absent row here has at
// least four explanations and this pass cannot tell them apart, so it declines
// to name one. See model.Resource.CostUnavailable.
const (
	// ResourceService is the failure-ledger service name for this pass. It is
	// distinct from Service so a reader can tell which paid pass a ledger entry
	// came from — they have separate budgets, separate meters, and separate
	// remedies.
	ResourceService = "cost-resources"

	// resourceWindowDays is how far back resource-level data reaches. AWS
	// documents the start date as unable to be earlier than 14 days ago.
	resourceWindowDays = 14

	// maxResourceGroups is Cost Explorer's per-service group ceiling. Reaching
	// it is not an error and carries no marker in the response, so a service
	// with more resources than this silently reports a short list. The count is
	// compared against it so the artifact can say the list may be short.
	maxResourceGroups = 5000

	// noResourceID is the placeholder Cost Explorer returns for spend within a
	// service that does not belong to any one resource. It is real spend and it
	// is in the rollup; it simply has no resource to attach to here.
	noResourceID = "NoResourceId"
)

// ResourceAPI is the slice of the Cost Explorer client this pass uses. Narrow
// for the same reason API is: it is the seam the tests fake, so no test needs
// credentials, a network, or a billing account.
type ResourceAPI interface {
	GetCostAndUsageWithResources(ctx context.Context, in *costexplorer.GetCostAndUsageWithResourcesInput, optFns ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageWithResourcesOutput, error)
}

// ResourceOptions configures one resource-level pass.
type ResourceOptions struct {
	// Accounts restricts the pass to the accounts the census actually scanned,
	// for the same reason Options.Accounts does.
	Accounts []string
	// CallerAccount is the account whose credentials issue the calls; it labels
	// ledger entries.
	CallerAccount string
	// Metric is a friendly name from Metrics(), matching the rollup's so the two
	// sections of the artifact answer the same question.
	Metric string
	// Window is the period to attribute. Use ResourceWindow.
	Window model.CostWindow
	// Budget is the run's shared request allowance. The rollup has already taken
	// its share; this pass spends what is left. Nil means this pass is the only
	// spender and gets DefaultMaxRequests.
	Budget *Budget
	// Rollup is the account-level report from the same run. It is required, and
	// it is where the service names come from: every SERVICE value sent to Cost
	// Explorer is one Cost Explorer itself reported, never one this tool
	// composed. A name this tool invented would come back empty and be
	// indistinguishable from a service that genuinely reports nothing — which
	// would corrupt the very question this pass exists to answer.
	Rollup *model.CostReport
}

// ResourceWindow returns the most recent 14 complete UTC days, ending
// yesterday.
//
// Complete days only, for the same reason the rollup uses a closed month: a
// window ending today moves every hour, so two runs on one day disagree and the
// diff reports drift that is only the clock advancing. Fourteen is AWS's own
// reach for resource-level data, and holding the duration fixed is what lets
// two runs be compared at all — the dates shift, the span does not, and the
// differ refuses windows whose durations differ rather than subtracting a
// fortnight from a month.
func ResourceWindow(now time.Time) model.CostWindow {
	u := now.UTC()
	today := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	start := today.AddDate(0, 0, -resourceWindowDays)
	return model.CostWindow{
		Start: start.Format("2006-01-02"),
		// Exclusive, so today's partial day is outside the window.
		End:   today.Format("2006-01-02"),
		Label: start.Format("2006-01-02") + "→" + today.AddDate(0, 0, -1).Format("2006-01-02"),
	}
}

var (
	// errNoRollup is returned when the pass is asked to run without the rollup
	// that supplies its service names.
	errNoRollup = errors.New("resource-level cost needs the account rollup for its service names")
	// errTooManyAccounts is returned when the census covers more accounts than
	// Cost Explorer's filter can carry. The rollup copes by restricting the
	// response instead, which it can do because it groups by linked account;
	// this pass groups by resource alone and has nothing to restrict on.
	errTooManyAccounts = errors.New("more accounts than the Cost Explorer filter can carry")
	// errCurrencyConflict is returned for a resource whose periods came back in
	// different currencies, which cannot be summed.
	errCurrencyConflict = errors.New("cost explorer reported one resource in more than one currency")
)

// censusServices maps a Cost Explorer service name to the census service that
// scans it.
//
// It is a spend gate and nothing else. Its only job is to answer "is it worth
// $0.01 to ask about this service", and it never decides where a returned
// figure lands — that is the ARN join's job, downstream, on data. So a wrong or
// missing entry costs coverage, never correctness: an absent name means a
// service is not probed and is recorded as uncensused, and the reader sees the
// name and the gap.
//
// It is deliberately not the source of the SERVICE value sent to AWS. That
// comes from the rollup, verbatim, so a typo here cannot produce a bogus filter
// whose empty response would masquerade as a service that reports nothing.
// Matching is on the key; the value is documentation for the next reader.
var censusServices = map[string]string{
	"Amazon DocumentDB (with MongoDB compatibility)": "documentdb",
	"Amazon DynamoDB":                        "dynamodb",
	"Amazon Elastic Block Store":             "ebs",
	"Amazon Elastic Compute Cloud - Compute": "ec2",
	"Amazon Elastic Load Balancing":          "elb",
	"Amazon ElastiCache":                     "elasticache",
	"Amazon Neptune":                         "neptune",
	"Amazon Redshift":                        "redshift",
	"Amazon Relational Database Service":     "rds",
	"Amazon Simple Storage Service":          "s3",
	"Amazon Virtual Private Cloud":           "natgateway",
	"AWS Lambda":                             "lambda",
	// "EC2 - Other" is where AWS bills EBS volumes and snapshots, NAT gateway
	// data processing, and idle public IPv4 addresses — three census services in
	// one Cost Explorer name. The join sorts out which resource each row belongs
	// to; this entry only says the request is worth making.
	"EC2 - Other": "ebs",
}

// CollectResources attributes billed spend to individual resources and returns
// the report plus any ledger entries. It writes figures into resources in place
// and re-sorts each resource's cost list.
//
// The report is non-nil whenever the pass got far enough to decide what to ask,
// including when it then asked nothing: which services were considered, and why
// each was or was not probed, is the answer this pass exists to produce. A nil
// report means the pass could not start.
func CollectResources(ctx context.Context, api ResourceAPI, resources []model.Resource, opts ResourceOptions) (*model.ResourceCostReport, []model.Failure) {
	if len(opts.Accounts) == 0 {
		return nil, nil
	}
	metric, ok := metricNames[opts.Metric]
	if !ok {
		return nil, []model.Failure{resLedger(opts, "ce_res_invalid_metric",
			fmt.Sprintf("unknown cost metric %q; valid values are %s", opts.Metric, strings.Join(Metrics(), ", ")))}
	}
	if opts.Rollup == nil || len(opts.Rollup.Currencies) == 0 {
		return nil, []model.Failure{resLedger(opts, "ce_res_no_rollup", fmt.Sprintf(
			"no per-resource costs were requested: %v, and this run published none — "+
				"resource-level spend was not collected, which is not the same as no resource having spend", errNoRollup))}
	}
	if len(opts.Accounts) > maxFilterAccounts {
		return nil, []model.Failure{resLedger(opts, "ce_res_too_many_accounts", fmt.Sprintf(
			"no per-resource costs were collected: %v (%d accounts, limit %d) — asking anyway would return spend "+
				"from accounts this census never scanned, with no linked-account grouping to filter it back out",
			errTooManyAccounts, len(opts.Accounts), maxFilterAccounts))}
	}

	m := &meter{budget: opts.budget()}
	report := &model.ResourceCostReport{
		Window:   opts.Window,
		Metric:   metric,
		Accounts: append([]string(nil), opts.Accounts...),
	}
	var failures []model.Failure

	idx := indexResources(resources)
	// Carried across probes, not rebuilt per probe: a resource billed under two
	// Cost Explorer service names must end up with the sum of both, and only
	// something that outlives one probe can hold the running total. See ceAccum.
	ceFigures := map[int]*ceAccum{}
	var anyRows, estimated bool

	for _, svc := range planProbes(opts.Rollup) {
		probe := model.ServiceProbe{Service: svc}
		if _, censused := censusServices[svc]; !censused {
			probe.Outcome = model.ProbeUncensused
			report.Probes = append(report.Probes, probe)
			continue
		}
		// Checked before the call rather than after it fails, so an exhausted
		// budget reads as "not asked" instead of as an error against AWS.
		if m.budget.Remaining() <= 0 {
			m.blocked = true
			probe.Outcome = model.ProbeSkipped
			report.Probes = append(report.Probes, probe)
			continue
		}

		rows, est, err := fetchResources(ctx, api, m, opts, metric, svc)
		estimated = estimated || est
		if err != nil {
			probe.Outcome, probe.Detail = probeOutcome(err)
			failures = append(failures, classifyResource(opts, svc, err))
		}
		// Rows survive an error here, which is the deliberate opposite of the
		// rollup's rule. A rollup is a total, and half a total published as a
		// whole one is a lie the reader cannot detect. These are per-resource
		// figures that were never summed to anything: each row is independently
		// true of the resource it names, a short list is visibly short, and the
		// probe outcome beside it says the list is short and why. Discarding
		// them would throw away paid-for facts to protect a total that does not
		// exist.
		probe.Rows = len(rows)
		// Only claimed when nothing else already explains a short list. A request
		// that failed part-way returned fewer resources for a reason this pass
		// knows, and crediting AWS's ceiling for it would name the wrong cause —
		// the reader would go looking for a 5,000-resource service that is not
		// there. The ceiling is stated as the constant it is rather than as the
		// number observed, so the sentence stays true when the two differ.
		probe.Truncated = err == nil && len(rows) >= maxResourceGroups
		if probe.Truncated {
			failures = append(failures, resLedger(opts, "ce_res_truncated", fmt.Sprintf(
				"%s: Cost Explorer returned %d resources, at or above its per-service ceiling of %d, so this "+
					"service's list may be short — it truncates without saying so, and a service with exactly that "+
					"many real resources looks identical. The figures shown are correct; the set of them may not be "+
					"complete",
				svc, len(rows), maxResourceGroups)))
		}
		if err == nil {
			if len(rows) > 0 {
				probe.Outcome = model.ProbeRows
			} else {
				probe.Outcome = model.ProbeEmpty
			}
		} else if len(rows) > 0 && probe.Outcome == model.ProbeSkipped {
			// The budget ran out between pages, so this service was asked, was
			// billed, and did report — "skipped" would tell the reader it was
			// never touched while its figures sit in the artifact. The list is
			// short, which is what the outcome now says; the ce_res_budget_exhausted
			// entry already names the budget as the reason.
			probe.Outcome = model.ProbeRows
			probe.Detail = "request budget ran out mid-service, so this list is short"
		}
		anyRows = anyRows || len(rows) > 0

		matched, joinFailures := attach(resources, idx, rows, opts, svc, ceFigures)
		probe.Matched = matched
		failures = append(failures, joinFailures...)
		report.Probes = append(report.Probes, probe)
	}

	if anyRows {
		// Only meaningful alongside published figures: with nothing to
		// describe, "estimated: false" would be a claim about data that does not
		// exist.
		report.Estimated = &estimated
		if estimated {
			failures = append(failures, resLedger(opts, "ce_res_estimated_data", fmt.Sprintf(
				"AWS still marks %s as estimated, so these per-resource figures are not final and will move as it "+
					"restates them — this is normal for a window this recent and does not make them modelled",
				opts.Window.Label)))
		}
	}
	report.Meter = m.report()
	if m.blocked {
		failures = append(failures, resLedger(opts, "ce_res_budget_exhausted", fmt.Sprintf(
			"the run's Cost Explorer request budget ran out before every service was probed, so the services "+
				"marked skipped were never asked — that is silence, not zero spend; raise --cost-max-requests "+
				"to cover them (each request costs $0.01)")))
	}
	report.Sort()
	for i := range resources {
		resources[i].SortCosts()
	}
	return report, failures
}

// budget resolves the allowance this pass spends against.
func (o ResourceOptions) budget() *Budget {
	if o.Budget != nil {
		return o.Budget
	}
	return NewBudget(DefaultMaxRequests)
}

// planProbes lists the services to consider, most expensive first.
//
// Names come from the rollup verbatim — see ResourceOptions.Rollup. Ordering is
// by spend so that a budget too small for every service is spent on the
// services where per-resource detail is worth the most; ties break on name so
// the request order is reproducible.
//
// Ranking compares amounts across currencies, which is not a thing this tool
// will publish as a figure. It is defensible here and only here: the comparison
// picks an order for spending requests, and the worst a wrong order can do is
// probe a cheaper service first. Nothing derived from it reaches the artifact.
func planProbes(rollup *model.CostReport) []string {
	weight := map[string]*big.Rat{}
	for _, cur := range rollup.Currencies {
		for _, s := range cur.Services {
			amt, err := parseAmount(s.Amount)
			if err != nil {
				continue
			}
			if w, ok := weight[s.Name]; !ok || amt.rat.Cmp(w) > 0 {
				weight[s.Name] = amt.rat
			}
		}
	}
	names := make([]string, 0, len(weight))
	for name := range weight {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if c := weight[names[j]].Cmp(weight[names[i]]); c != 0 {
			return c < 0
		}
		return names[i] < names[j]
	})
	return names
}

// resourceRow is one resource's total for the window, as Cost Explorer reported
// it.
type resourceRow struct {
	// id is Cost Explorer's RESOURCE_ID, verbatim. Its format is
	// service-dependent — an instance ID, a bucket name, a full ARN — which is
	// why it is stored rather than normalized, and why it ends up in
	// ResourceCost.MatchKey.
	id       string
	currency string
	amount   string
	// conflict is set when the periods for this resource disagreed about the
	// currency, so no total could be formed.
	conflict bool
}

// resourceAcc accumulates one resource's periods before they are totalled.
type resourceAcc struct {
	currency string
	parts    []amount
	conflict bool
}

// fetchResources issues one service's resource-level query and paginates it.
//
// Unlike fetch, a partial result is returned alongside the error rather than
// discarded. See the comment at the call site for why the two rules differ.
func fetchResources(ctx context.Context, api ResourceAPI, m *meter, opts ResourceOptions, metric, service string) ([]resourceRow, bool, error) {
	// A monthly granularity over a fortnight is one or two periods rather than
	// fourteen, which is the difference between one billed request and several
	// pages of them. The window crosses a month boundary about half the time, so
	// the accumulator below is not an edge case.
	in := &costexplorer.GetCostAndUsageWithResourcesInput{
		Granularity: cetypes.GranularityMonthly,
		Metrics:     []string{metric},
		TimePeriod: &cetypes.DateInterval{
			Start: aws.String(opts.Window.Start),
			End:   aws.String(opts.Window.End),
		},
		GroupBy: []cetypes.GroupDefinition{
			{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String(string(cetypes.DimensionResourceId))},
		},
		Filter: resourceFilter(service, opts.Accounts),
	}

	// Accumulated by resource because a window spanning a month boundary comes
	// back as two periods, and the figure the artifact wants is the resource's
	// total over the window rather than one period's slice of it.
	totals := map[string]*resourceAcc{}
	var order []string
	var estimated bool

	for {
		if !m.take() {
			return collectRows(totals, order), estimated, errBudget
		}
		page, err := api.GetCostAndUsageWithResources(ctx, in)
		if err != nil {
			return collectRows(totals, order), estimated, err
		}
		if page == nil {
			return collectRows(totals, order), estimated, errors.New("Cost Explorer returned no result")
		}
		for _, result := range page.ResultsByTime {
			estimated = estimated || result.Estimated
			for _, g := range result.Groups {
				if len(g.Keys) < 1 {
					continue
				}
				id := g.Keys[0]
				// Real spend within the service that belongs to no one resource
				// — it is already in the rollup, and spreading it across the
				// resources that do have rows would fabricate per-resource
				// figures nobody reported.
				if id == "" || id == noResourceID {
					continue
				}
				mv, okMetric := g.Metrics[metric]
				if !okMetric || mv.Amount == nil {
					return collectRows(totals, order), estimated,
						fmt.Errorf("%w: %s (resource %q)", errMetricUnavailable, metric, id)
				}
				amt, err := parseAmount(*mv.Amount)
				if err != nil {
					return collectRows(totals, order), estimated,
						fmt.Errorf("cost amount for %q: %w", id, err)
				}
				currency := aws.ToString(mv.Unit)
				a, seen := totals[id]
				if !seen {
					a = &resourceAcc{currency: currency}
					totals[id] = a
					order = append(order, id)
				}
				if a.currency != currency {
					// Two periods in two currencies cannot be added. The
					// resource is kept in the set and marked, so the join
					// reports it rather than dropping it silently.
					a.conflict = true
				}
				a.parts = append(a.parts, amt)
			}
		}
		if aws.ToString(page.NextPageToken) == "" {
			return collectRows(totals, order), estimated, nil
		}
		in.NextPageToken = page.NextPageToken
	}
}

// collectRows renders the accumulator in first-seen order, which keeps the
// request's own ordering rather than imposing one on it.
func collectRows(totals map[string]*resourceAcc, order []string) []resourceRow {
	out := make([]resourceRow, 0, len(order))
	for _, id := range order {
		a := totals[id]
		out = append(out, resourceRow{
			id:       id,
			currency: a.currency,
			amount:   sum(a.parts),
			conflict: a.conflict,
		})
	}
	return out
}

// resourceFilter restricts the request to one service and the census's
// accounts. Both dimensions are always present: Cost Explorer requires a filter
// on this operation, and the account list is known to fit because
// CollectResources refuses to run when it does not.
func resourceFilter(service string, accounts []string) *cetypes.Expression {
	sorted := append([]string(nil), accounts...)
	sort.Strings(sorted)
	return &cetypes.Expression{And: []cetypes.Expression{
		{Dimensions: &cetypes.DimensionValues{
			Key:    cetypes.DimensionService,
			Values: []string{service},
		}},
		{Dimensions: &cetypes.DimensionValues{
			Key:    cetypes.DimensionLinkedAccount,
			Values: sorted,
		}},
	}}
}

// resourceIndex maps the identifiers Cost Explorer might use for a resource
// back to positions in the census.
//
// Two lookups rather than one because RESOURCE_ID has no single format: some
// services report a full ARN, others an instance ID or a bucket name. The ARN
// index is exact. The key index is a fallback over ARN tails — see matchKeys
// for why it stops there — and it is case-folded because AWS is not consistent
// about case across APIs.
//
// Both map to a slice, not to one index, so a key claimed by two resources is
// visible as ambiguous instead of resolving to whichever was scanned last.
type resourceIndex struct {
	byARN map[string][]int
	byKey map[string][]int
}

func indexResources(resources []model.Resource) resourceIndex {
	idx := resourceIndex{
		byARN: make(map[string][]int, len(resources)),
		byKey: make(map[string][]int, len(resources)),
	}
	for i := range resources {
		r := &resources[i]
		if r.ARN != "" {
			idx.byARN[r.ARN] = append(idx.byARN[r.ARN], i)
		}
		for _, k := range matchKeys(r) {
			idx.byKey[k] = append(idx.byKey[k], i)
		}
	}
	return idx
}

// matchKeys returns the case-folded identifiers Cost Explorer may name a
// resource by, other than its full ARN.
//
// The ARN tail and nothing else. Resource.Name is deliberately not a key:
// RESOURCE_ID is an AWS-assigned identifier — i-…, vol-…, a bucket name, a
// table name — and never a Name tag, so indexing the tag can only ever produce
// a hit that is wrong. Worse, it produces them silently in both directions: a
// snapshot tagged Name=vol-0abc after the volume it came from would make the
// real vol-0abc ambiguous and get its spend refused on every run. Where a
// resource genuinely is identified by its name — an S3 bucket, a DynamoDB table
// — that name is the ARN tail and is already indexed.
//
// Returned as a slice because it is one entry today and the join is written to
// take several: services differ enough in how they name things that a second
// derivation is a question of when, not whether.
func matchKeys(r *model.Resource) []string {
	tail := arnTail(r.ARN)
	if tail == "" {
		return nil
	}
	return []string{strings.ToLower(tail)}
}

// arnTail returns the last path or colon segment of an ARN — the instance ID,
// table name, or bucket name most services use as their resource ID.
func arnTail(arn string) string {
	if arn == "" {
		return ""
	}
	if i := strings.LastIndexAny(arn, "/:"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// ceAccum is what one resource's Cost Explorer figure has been built from so
// far, across every service probed in this pass.
//
// It exists because a resource can be billed under more than one Cost Explorer
// service name and this pass probes each of them separately. AWS bills an
// instance's hours under "Amazon Elastic Compute Cloud - Compute" and its
// EBS-optimized and data-transfer charges under "EC2 - Other"; a NAT gateway's
// hours under "Amazon Virtual Private Cloud" and its data processing under
// "EC2 - Other". Those are different components of one bill, not two answers to
// one question, so they add.
type ceAccum struct {
	currency string
	parts    []amount
	// services names every Cost Explorer service that contributed, in probe
	// order, so the figure can say what it is made of.
	services []string
	// matchKey is the identifier the first probe joined on. A later probe that
	// names the same resource differently does not overwrite it: the point of
	// the key is to show the reader how the join was made, and the first one
	// made it.
	matchKey string
	// dropped counts components refused because they arrived in a different
	// currency from the running total.
	dropped []string
}

// attach adds each row's figure to the resource it names, returning how many
// rows were joined.
//
// A row that matches more than one resource is refused rather than assigned.
// Guessing would put real money on the wrong resource, and a figure on the
// wrong row is worse than no figure at all: it is wrong in a way the reader has
// no way to see. A row that matches nothing is counted but not laid at any
// resource's door — it is usually spend on something no scanner covers, which
// the probe's Rows-minus-Matched gap already reports.
//
// acc is carried across probes by the caller. A second probe naming a resource
// the first one already priced adds to that resource's total rather than
// replacing it — Resource.AddCost replaces by method, which is right for two
// readings of the same question and wrong for two components of one bill, so
// the sum is formed here and written whole.
func attach(resources []model.Resource, idx resourceIndex, rows []resourceRow, opts ResourceOptions, service string, acc map[int]*ceAccum) (int, []model.Failure) {
	var matched int
	var failures []model.Failure
	var unmatched []string

	for _, row := range rows {
		if row.conflict {
			failures = append(failures, resLedger(opts, "ce_res_currency_conflict", fmt.Sprintf(
				"%q: %v, so no total was formed for it and it carries no figure", row.id, errCurrencyConflict)))
			continue
		}
		hits, ambiguous := idx.lookup(row.id)
		if ambiguous {
			failures = append(failures, resLedger(opts, "ce_res_ambiguous_match", fmt.Sprintf(
				"%q matches %d resources in this census, so its %s was left unattached rather than put on one of "+
					"them at random — a figure on the wrong resource is worse than a missing one",
				row.id, len(hits), strings.TrimSpace(row.amount+" "+row.currency))))
			continue
		}
		if len(hits) == 0 {
			unmatched = append(unmatched, row.id)
			continue
		}
		amt, err := parseAmount(row.amount)
		if err != nil {
			// Unreachable for a row that came through parseAmount already, but
			// a silent zero here would be a fabricated figure.
			failures = append(failures, resLedger(opts, "ce_res_bad_amount", fmt.Sprintf(
				"%q: %v, so it carries no figure", row.id, err)))
			continue
		}

		i := hits[0]
		r := &resources[i]
		a, seen := acc[i]
		if !seen {
			a = &ceAccum{currency: row.currency, matchKey: matchKeyFor(r, row.id)}
			acc[i] = a
		}
		if a.currency != row.currency {
			// Two currencies cannot be added, and picking one would publish a
			// total that is neither. The running total stands, short by this
			// component, and both the ledger and the figure's own caveats say so
			// — an under-count the reader can see beats a wrong number they
			// cannot. The figure is still rewritten below, unchanged in value,
			// because that is the only way the new caveat reaches the artifact:
			// the earlier probe already wrote it without one.
			a.dropped = append(a.dropped, service)
			failures = append(failures, resLedger(opts, "ce_res_merge_currency_conflict", fmt.Sprintf(
				"%q is billed in %s under %s and in %s under %s; the two cannot be added, so its figure covers only "+
					"the %s components and is short by the rest",
				row.id, a.currency, strings.Join(a.services, ", "), row.currency, service, a.currency)))
		} else {
			a.parts = append(a.parts, amt)
			a.services = append(a.services, service)
		}

		r.AddCost(model.ResourceCost{
			Amount:   sum(a.parts),
			Currency: a.currency,
			Method:   model.CostMethodCE,
			// Billed, not modelled. AWS may still restate it — that is what
			// ResourceCostReport.Estimated records — but a bill that has not
			// settled is not a model.
			Estimated:    false,
			ObservedFrom: windowTime(opts.Window.Start),
			ObservedTo:   windowTime(opts.Window.End),
			Caveats:      append(resourceCaveats(r, opts.Window), a.caveats()...),
			MatchKey:     a.matchKey,
		})
		matched++
	}

	if len(unmatched) > 0 {
		sort.Strings(unmatched)
		failures = append(failures, resLedger(opts, "ce_res_unmatched", fmt.Sprintf(
			"%d resources with billed spend are not in this census and were left out of the overlay — their spend "+
				"is still in the account rollup, so this is a scanner coverage gap rather than lost money: %s",
			len(unmatched), strings.Join(truncateList(unmatched, 10), ", "))))
	}
	return matched, failures
}

// caveats states what a multi-service figure is made of, and what it is
// missing.
//
// A single-service figure says nothing: one service is the ordinary case, and a
// caveat on every row would bury the ones that matter. The composition is
// disclosed rather than left implicit because a reader comparing this number
// against one Cost Explorer service view in the console would otherwise find it
// too high and have no way to learn why.
func (a *ceAccum) caveats() []string {
	var out []string
	if len(a.services) > 1 {
		out = append(out, fmt.Sprintf(
			"combines spend billed under %d Cost Explorer services (%s) — AWS splits this resource's bill across "+
				"them, so this total is higher than any one of them shows",
			len(a.services), strings.Join(a.services, ", ")))
	}
	if len(a.dropped) > 0 {
		out = append(out, fmt.Sprintf(
			"short by the spend billed under %s, which arrived in a different currency and could not be added",
			strings.Join(a.dropped, ", ")))
	}
	return out
}

// lookup resolves one Cost Explorer resource ID. The bool reports that the ID
// named more than one census resource, which is a refusal rather than a match.
func (idx resourceIndex) lookup(id string) (hits []int, ambiguous bool) {
	// Exact ARN first: when Cost Explorer reports a full ARN there is nothing to
	// infer, and an ARN that is also some other resource's name must not drag
	// the fallback in.
	if hits = idx.byARN[id]; len(hits) > 0 {
		return hits, len(hits) > 1
	}
	hits = idx.byKey[strings.ToLower(id)]
	return hits, len(hits) > 1
}

// matchKeyFor records what the join matched on, but only when it was not the
// ARN — an ARN-to-ARN match has nothing to disclose.
func matchKeyFor(r *model.Resource, id string) string {
	if id == r.ARN {
		return ""
	}
	return id
}

// resourceCaveats returns the per-resource disclosures for one figure.
//
// Only conditions derived from what is known about *this* resource belong here.
// That the window is a recent fortnight, that Cost Explorer restates it, and
// that the service may have truncated its list are true of every figure from
// this method and are carried by the method name and the report — repeating
// them on every row would bury the one caveat that is actually specific.
func resourceCaveats(r *model.Resource, w model.CostWindow) []string {
	if r.CreatedAt == nil {
		return nil
	}
	start, err := time.Parse("2006-01-02", w.Start)
	if err != nil || !r.CreatedAt.UTC().After(start) {
		return nil
	}
	return []string{fmt.Sprintf(
		"created %s, after the window opened on %s — this is spend for part of the window, not all of it",
		r.CreatedAt.UTC().Format("2006-01-02"), w.Start)}
}

// windowTime parses a window bound into a timestamp, returning nil when it does
// not parse rather than substituting a zero time that would read as year 1.
func windowTime(day string) *time.Time {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// truncateList caps a list for a ledger message, saying how many it dropped.
func truncateList(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	return append(append([]string(nil), items[:max]...), fmt.Sprintf("and %d more", len(items)-max))
}

// resLedger builds a failure-ledger entry for this pass.
func resLedger(opts ResourceOptions, kind, msg string) model.Failure {
	return ledgerEntry(opts.CallerAccount, ResourceService, kind, msg)
}

// probeOutcome maps a failed request to the outcome recorded against the
// service, which is a narrower question than the ledger's: it asks only what
// the reader may now conclude about this service reporting resource-level cost.
func probeOutcome(err error) (outcome, detail string) {
	code, msg := apiError(err)
	if errors.Is(err, errBudget) {
		return model.ProbeSkipped, "request budget exhausted mid-service"
	}
	lower := strings.ToLower(msg)
	switch {
	case code == "AccessDeniedException", code == "AccessDenied",
		code == "DataUnavailableException", strings.Contains(lower, "not authorized"):
		return model.ProbeDenied, msg
	case isServiceRejection(code, lower):
		return model.ProbeUnsupported, msg
	}
	if msg == "" {
		msg = err.Error()
	}
	return model.ProbeFailed, msg
}

// isServiceRejection reports whether AWS refused the query because of the
// service being asked about, as opposed to the window or the granularity.
//
// The ordering matters more than the matching does. A ValidationException over
// a start date 14 days too old, or over an unsupported granularity, is about
// the request — call either of those "unsupported" and this tool publishes a
// false claim about which AWS services report resource-level cost, which is the
// one question the whole pass exists to answer. So the request-shaped
// rejections are ruled out first and only what is left counts as evidence.
func isServiceRejection(code, lowerMsg string) bool {
	if code != "ValidationException" && code != "InvalidParameterValue" {
		return false
	}
	for _, aboutTheRequest := range []string{
		"start date", "end date", "time period", "date range", "14 days",
		"granularity", "hourly", "daily", "monthly",
	} {
		if strings.Contains(lowerMsg, aboutTheRequest) {
			return false
		}
	}
	return true
}

// classifyResource turns a failed probe into a ledger entry whose kind names
// the fix, on the same reasoning as classify. The kinds carry a ce_res_ prefix
// because the remedies differ from the rollup's: this pass needs a second IAM
// action and an account preference the rollup does not.
func classifyResource(opts ResourceOptions, service string, err error) model.Failure {
	what := "reading per-resource cost for " + service
	if errors.Is(err, errBudget) {
		return resLedger(opts, "ce_res_budget_exhausted", fmt.Sprintf(
			"%s: the run's Cost Explorer request budget ran out partway through this service, so its list of "+
				"resources may be short; raise --cost-max-requests to finish it (each request costs $0.01)", what))
	}
	if errors.Is(err, errMetricUnavailable) {
		return resLedger(opts, "ce_res_metric_unavailable", fmt.Sprintf(
			"%s: Cost Explorer returned no figure for this metric, so the resources after that point carry none "+
				"rather than a zero nobody measured — try another --cost-metric from %s (%v)",
			what, strings.Join(Metrics(), ", "), err))
	}

	code, msg := apiError(err)
	lower := strings.ToLower(msg)
	switch {
	case code == "DataUnavailableException",
		strings.Contains(lower, "not enabled"),
		strings.Contains(lower, "resource level"),
		strings.Contains(lower, "resource-level"):
		return resLedger(opts, "ce_res_not_enabled", fmt.Sprintf(
			"%s: resource-level data is not switched on for this account — enable it in the Cost Explorer "+
				"preferences, which is a paid setting and takes about 24 hours; it is not retroactive, so the "+
				"first days after enabling it report nothing (%v)", what, err))

	case code == "AccessDeniedException" || code == "AccessDenied" || strings.Contains(lower, "not authorized"):
		return resLedger(opts, "ce_res_access_denied", fmt.Sprintf(
			"%s: not authorized for ce:GetCostAndUsageWithResources — it is a separate action from "+
				"ce:GetCostAndUsage, so the rollup can succeed while this fails; add the BlueprintCostExplorer "+
				"statement from docs/iam-policy.json (%v)", what, err))

	case code == "ThrottlingException", code == "TooManyRequestsException",
		code == "LimitExceededException", code == "RequestLimitExceeded":
		return resLedger(opts, "ce_res_throttled", fmt.Sprintf(
			"%s: Cost Explorer throttled the request and it was not retried, because retrying a "+
				"per-request-billed API would multiply the charge without asking — re-run to try again (%v)", what, err))

	case code == "ValidationException" || code == "InvalidParameterValue":
		// Split three ways, because the reader's next move differs and because
		// only one of the three says anything about the service.
		switch {
		case containsAny(lower, "start date", "end date", "time period", "date range", "14 days"):
			return resLedger(opts, "ce_res_window_rejected", fmt.Sprintf(
				"%s: Cost Explorer rejected the %s window. This is about the dates asked for, not about the "+
					"service — nothing here says whether this service reports resource-level cost (%v)",
				what, opts.Window.Label, err))
		case containsAny(lower, "granularity", "hourly", "daily", "monthly"):
			return resLedger(opts, "ce_res_granularity_rejected", fmt.Sprintf(
				"%s: Cost Explorer rejected monthly granularity for resource-level data. This is about the "+
					"request, not the service — nothing here says whether this service reports resource-level "+
					"cost (%v)", what, err))
		}
		return resLedger(opts, "ce_res_unsupported_service", fmt.Sprintf(
			"%s: Cost Explorer rejected the request for this service, which is the only positive evidence there "+
				"is that it does not report resource-level cost. Per-resource actuals for it need a CUR or FOCUS "+
				"export instead (%v)", what, err))

	case code == "InvalidNextTokenException":
		return resLedger(opts, "ce_res_pagination_incomplete", fmt.Sprintf(
			"%s: Cost Explorer rejected the pagination token, so this service's list covers only the pages read "+
				"before it failed (%v)", what, err))
	}
	return resLedger(opts, "ce_res_failed", fmt.Sprintf("%s: %v", what, err))
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
