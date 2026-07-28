// Package cost collects account-level spend from AWS Cost Explorer.
//
// Cost Explorer is not a scanner and is deliberately not registered as one.
// It is a global, per-account API, so fanning it out over regions the way the
// scan runner does would multiply a billed call by the region count; and a
// Scanner's Service() name feeds Snapshot.Services, which is part of the
// history scope key, so merely turning --costs on would re-bucket and orphan
// every existing user's diff history. Cost runs as its own phase after the
// scan instead.
//
// # This API costs money
//
// AWS charges $0.01 for each paginated Cost Explorer request. Everything here
// is shaped by that:
//
//   - Retries are disabled on the client. The shared config in internal/awsx
//     installs an adaptive retryer with up to 8 attempts, which on a billed
//     API silently multiplies the charge by up to 8×. Client() overrides it,
//     so one logical request is one billed request and the meter's count is
//     exact rather than a lower bound.
//   - Every run has a hard request budget. Hitting it stops the phase and
//     ledgers the truncation instead of quietly spending more.
//   - The number of requests and the resulting charge are written into the
//     artifact, every time.
package cost

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/smithy-go"

	"github.com/hoophq/blueprint/internal/model"
)

// Service is the failure-ledger service name for the cost phase, mirroring
// the "orgmode" precedent for a non-scanner phase.
const Service = "cost"

// DefaultMaxRequests bounds what one run can spend at $0.01 per request.
// A normal run issues two requests plus pagination; the budget exists for the
// pathological case (a very large organization paginating deeply), not the
// normal one.
const DefaultMaxRequests = 20

// ceRegion pins the Cost Explorer endpoint. Cost Explorer is a global service
// homed in us-east-1; the account's own region list is irrelevant to it.
const ceRegion = "us-east-1"

// maxFilterAccounts bounds how many account IDs are pushed into the request
// filter. Cost Explorer caps the number of filter values, so past this point
// the request goes out unfiltered and the account restriction is applied to
// the response instead. Both paths produce the same set of accounts — the
// filter is a payload optimization, the response-side restriction is the
// correctness guarantee, and it always runs.
const maxFilterAccounts = 100

// metricNames maps the flag's friendly values to Cost Explorer metric names.
//
// These answer materially different questions (amortized spreads reserved and
// savings-plan commitments over the term they cover; unblended is what hit the
// invoice that month; the net variants are after discounts), which is why the
// chosen metric is recorded in the report and why reports using different
// metrics must not be compared.
// BlendedCost is deliberately absent. It averages a rate across an
// organization's accounts, so a per-account blended figure is an internal
// chargeback artifact rather than what that account cost — it reconciles to no
// invoice and would quietly make the per-account breakdown here fiction.
var metricNames = map[string]string{
	"amortized":     "AmortizedCost",
	"unblended":     "UnblendedCost",
	"net_amortized": "NetAmortizedCost",
	"net_unblended": "NetUnblendedCost",
}

// DefaultMetric is amortized cost: it attributes commitment spend to the
// period it actually buys, which is the figure that matches what an estate
// costs to run rather than what happened to be invoiced.
const DefaultMetric = "amortized"

// Metrics lists the accepted --cost-metric values, sorted.
func Metrics() []string {
	out := make([]string, 0, len(metricNames))
	for k := range metricNames {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ValidMetric reports whether name is an accepted --cost-metric value.
func ValidMetric(name string) bool {
	_, ok := metricNames[name]
	return ok
}

// attributedRecordTypes are the Cost Explorer RECORD_TYPE values that
// represent consumption of a named service — the spend a later pass could
// plausibly trace down to individual resources.
//
// Everything else (tax, credits, refunds, support, and the recurring and
// upfront fees for reservations and savings plans) belongs to the account
// rather than to any one resource, and is reported separately under its own
// record-type name rather than being spread across services or dropped.
//
// The set is an allowlist, so a record type AWS introduces later falls into
// the unattributed side and is named there. That is the safe direction: an
// unrecognized record type shows up as an unexplained line the reader can
// look up, instead of being silently folded into service usage.
var attributedRecordTypes = map[string]bool{
	"Usage":                   true,
	"DiscountedUsage":         true,
	"SavingsPlanCoveredUsage": true,
}

// API is the slice of the Cost Explorer client this package uses. Narrow on
// purpose: it is the seam the tests fake, so no test needs credentials, a
// network, or a billing account.
type API interface {
	GetCostAndUsage(ctx context.Context, in *costexplorer.GetCostAndUsageInput, optFns ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error)
}

// Client builds a Cost Explorer client from a scan config.
//
// It overrides two things the shared config sets for high-volume scanning.
// The region is pinned because Cost Explorer is global. The retryer is
// reduced to a single attempt because retries on a per-request-billed API
// multiply the user's charge without telling them — a throttled cost lookup
// is reported in the ledger and the user can re-run, which is strictly more
// honest than quietly paying up to 8× for the same answer.
func Client(cfg aws.Config) *costexplorer.Client {
	cfg = cfg.Copy()
	cfg.Region = ceRegion
	cfg.Retryer = func() aws.Retryer {
		return retry.AddWithMaxAttempts(retry.NewStandard(), 1)
	}
	return costexplorer.NewFromConfig(cfg)
}

// Options configures one cost collection.
type Options struct {
	// Accounts restricts the report to the accounts the census actually
	// scanned. This is load-bearing, not cosmetic: Cost Explorer called with
	// a payer account's credentials returns the whole organization's spend,
	// so without this a one-account census would report an entire
	// organization's bill as if it were that account's.
	Accounts []string
	// CallerAccount is the account whose credentials issue the calls. It
	// labels ledger entries, since the failure is the caller's, not the
	// subject accounts'.
	CallerAccount string
	// Metric is a friendly name from Metrics().
	Metric string
	// Window is the billing period to report.
	Window model.CostWindow
	// MaxRequests caps billed requests for this run. Zero means
	// DefaultMaxRequests.
	MaxRequests int
}

// LastFullMonth returns the most recent complete calendar month in UTC.
//
// The window is deliberately a closed month rather than month-to-date.
// Month-to-date moves every hour and is missing late-arriving charges, so two
// runs on the same day disagree and a diff between them reports drift that is
// only the clock advancing. A census wants a figure that is complete and
// stable enough to compare.
func LastFullMonth(now time.Time) model.CostWindow {
	firstOfThisMonth := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	start := firstOfThisMonth.AddDate(0, -1, 0)
	return model.CostWindow{
		Start: start.Format("2006-01-02"),
		// Cost Explorer's end date is exclusive, so the first of this month
		// is the correct end for last month.
		End:   firstOfThisMonth.Format("2006-01-02"),
		Label: start.Format("2006-01"),
	}
}

var (
	// errBudget is returned when the run has spent its request budget.
	errBudget = errors.New("cost explorer request budget exhausted")
	// errMetricUnavailable is returned when a response carries no figure for
	// the metric that was asked for. Substituting another metric, or treating
	// the gap as zero, would both report a number nobody measured.
	errMetricUnavailable = errors.New("cost explorer returned no value for the requested metric")
)

// meter counts billed requests and enforces the budget. Requests are issued
// sequentially, which is what makes the cap exact — a concurrent fan-out
// could overshoot it before the last goroutine checked.
type meter struct {
	n, max int
	// blocked records that the budget actually stopped a request. Spending
	// the last permitted request and finishing is a complete run, so this is
	// deliberately not "n == max": that would label a successful collection
	// truncated and send the reader looking for missing money.
	blocked bool
}

func (m *meter) take() bool {
	if m.n >= m.max {
		m.blocked = true
		return false
	}
	m.n++
	return true
}

// row is one (dimension value, account, currency) amount from a response.
type row struct {
	name     string
	account  string
	currency string
	amt      amount
}

// rowset is the complete result of one grouped query. It only ever describes
// a query that finished: fetch returns the zero value with an error otherwise,
// so there is no way for a caller to reach half a result set by accident.
type rowset struct {
	rows []row
	// estimated is true when AWS flagged any period in the response as still
	// being estimated, i.e. the month has closed but the bill has not settled.
	estimated bool
}

// Collect runs the cost phase and returns the report plus any ledger entries.
//
// The report is non-nil whenever at least one request was issued, even if
// every call failed: the run spent the user's money and the artifact has to
// say so. A nil report means nothing was asked and nothing was charged.
func Collect(ctx context.Context, api API, opts Options) (*model.CostReport, []model.Failure) {
	if len(opts.Accounts) == 0 {
		return nil, nil
	}
	metric, ok := metricNames[opts.Metric]
	if !ok {
		return nil, []model.Failure{ledger(opts, "ce_invalid_metric",
			fmt.Sprintf("unknown cost metric %q; valid values are %s", opts.Metric, strings.Join(Metrics(), ", ")))}
	}
	maxRequests := opts.MaxRequests
	if maxRequests <= 0 {
		maxRequests = DefaultMaxRequests
	}

	m := &meter{max: maxRequests}
	report := &model.CostReport{
		Window:   opts.Window,
		Metric:   metric,
		Accounts: append([]string(nil), opts.Accounts...),
	}
	var failures []model.Failure

	// The record-type call comes first because it is the authoritative one:
	// it covers all spend in the window and yields the total that the
	// attributed/unattributed partition must add up to.
	records, recErr := fetch(ctx, api, m, opts, metric, cetypes.DimensionRecordType, nil)
	if recErr != nil {
		failures = append(failures, classify(opts, "reading cost by record type", recErr))
	}

	// The service call is filtered to attributed record types so its
	// breakdown is exactly the attributed side of the partition. Without that
	// filter it would also carry tax and credits under their own pseudo
	// service names and would sum to the grand total instead.
	//
	// It is skipped when the record-type call failed: the rollup is going to
	// be discarded either way, and there is no reason to bill the user for a
	// second request whose answer cannot be published.
	var services rowset
	var svcErr error
	if recErr == nil {
		services, svcErr = fetch(ctx, api, m, opts, metric, cetypes.DimensionService, attributedRecordTypeValues())
		if svcErr != nil {
			failures = append(failures, classify(opts, "reading cost by service", svcErr))
		}
	}

	// A partial rollup is not published. A truncated sum reads exactly like a
	// complete one — it carries no mark saying which pages are missing — so it
	// is worse than no figure at all: the reader reconciles it against an
	// invoice, finds a gap, and has no way to tell whether the gap is a real
	// finding or this tool giving up halfway. Half a service breakdown is the
	// same problem one level down, since it would no longer sum to Attributed.
	// The meter and the ledger still ship, so the run discloses what it spent
	// and why it has nothing to show for it.
	if recErr == nil && svcErr == nil {
		report.Currencies = assemble(records.rows, services.rows)
		// Only meaningful alongside a published rollup: with nothing to
		// describe, "estimated: false" would be a claim about data that does
		// not exist.
		estimated := records.estimated || services.estimated
		report.Estimated = &estimated
		if estimated {
			failures = append(failures, ledger(opts, "ce_estimated_data", fmt.Sprintf(
				"AWS still marks %s as estimated, so these figures are not final and will change once the "+
					"month is billed — they will not reconcile to the invoice yet", opts.Window.Label)))
		}
	}
	report.Meter = model.CostMeter{
		Requests:           m.n,
		EstimatedChargeUSD: ChargeUSD(m.n),
		Capped:             m.blocked,
	}
	report.Sort()

	if m.n == 0 {
		// Nothing was asked, so there is no spend to disclose and no report
		// to write — only whatever failure explains why.
		return nil, failures
	}
	return report, failures
}

// attributedRecordTypeValues returns the attributed record types, sorted so
// the request body is identical between runs.
func attributedRecordTypeValues() []string {
	out := make([]string, 0, len(attributedRecordTypes))
	for k := range attributedRecordTypes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fetch issues one grouped Cost Explorer query and paginates it.
//
// Results are always grouped by the requested dimension *and* by linked
// account, so the account restriction can be applied to the response whether
// or not it fitted into the request filter.
//
// Every error path returns the zero rowset, discarding the pages that did
// arrive. Those pages were paid for, but a partial sum cannot be told apart
// from a complete one downstream, so handing them back would only offer a
// caller the chance to publish a short total as a real one.
func fetch(ctx context.Context, api API, m *meter, opts Options, metric string, dim cetypes.Dimension, recordTypes []string) (rowset, error) {
	want := make(map[string]bool, len(opts.Accounts))
	for _, a := range opts.Accounts {
		want[a] = true
	}

	in := &costexplorer.GetCostAndUsageInput{
		Granularity: cetypes.GranularityMonthly,
		Metrics:     []string{metric},
		TimePeriod: &cetypes.DateInterval{
			Start: aws.String(opts.Window.Start),
			End:   aws.String(opts.Window.End),
		},
		// Cost Explorer allows at most two groupings, which is why the record
		// type split cannot ride along with the service split and needs its
		// own (billed) request.
		GroupBy: []cetypes.GroupDefinition{
			{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String(string(dim))},
			{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String(string(cetypes.DimensionLinkedAccount))},
		},
		Filter: buildFilter(opts.Accounts, recordTypes),
	}

	var out rowset
	for {
		if !m.take() {
			return rowset{}, errBudget
		}
		page, err := api.GetCostAndUsage(ctx, in)
		if err != nil {
			return rowset{}, err
		}
		if page == nil {
			return rowset{}, errors.New("Cost Explorer returned no result")
		}
		for _, result := range page.ResultsByTime {
			// The SDK models this as a plain bool, so false means "AWS did not
			// flag it" rather than a positive guarantee of finalization — but
			// a true is unambiguous and is the case worth surfacing.
			out.estimated = out.estimated || result.Estimated
			for _, g := range result.Groups {
				r, ok, err := toRow(g, metric)
				if err != nil {
					return rowset{}, err
				}
				// A group outside the census is dropped rather than reported:
				// with payer credentials the response covers accounts this
				// scan never looked at, and counting their spend would claim
				// coverage the census does not have.
				if !ok || !want[r.account] {
					continue
				}
				out.rows = append(out.rows, r)
			}
		}
		if aws.ToString(page.NextPageToken) == "" {
			return out, nil
		}
		in.NextPageToken = page.NextPageToken
	}
}

// toRow converts one response group. The bool is false for a group that is
// not addressable — fewer keys than the two dimensions that were requested —
// which carries no figure to lose.
//
// A group that is addressable but carries no amount for the requested metric
// is an error, not a skip. Cost Explorer returns every metric it was asked
// for, including an explicit "0", so an absent one means the metric itself is
// unavailable for this account or window. Dropping such groups would quietly
// understate the total; filling them with zero would invent a measurement; and
// falling back to a different metric would answer a question nobody asked. The
// run reports the gap and publishes nothing instead.
func toRow(g cetypes.Group, metric string) (row, bool, error) {
	if len(g.Keys) < 2 {
		return row{}, false, nil
	}
	mv, ok := g.Metrics[metric]
	if !ok || mv.Amount == nil {
		return row{}, false, fmt.Errorf("%w: %s (group %q)", errMetricUnavailable, metric, g.Keys[0])
	}
	amt, err := parseAmount(*mv.Amount)
	if err != nil {
		return row{}, false, fmt.Errorf("cost amount for %q: %w", g.Keys[0], err)
	}
	return row{
		name:    g.Keys[0],
		account: g.Keys[1],
		// A metric with no unit is a currency this tool does not know. It is
		// kept separate rather than assumed to be USD, so amounts in an
		// unknown currency are never added to dollars.
		currency: aws.ToString(mv.Unit),
		amt:      amt,
	}, true, nil
}

// buildFilter restricts the request to the given accounts and record types.
// A dimension is omitted when it is empty, or — for accounts — when the list
// is too long for the API's filter limits, in which case fetch's response-side
// restriction is what enforces it.
func buildFilter(accounts, recordTypes []string) *cetypes.Expression {
	var dims []cetypes.Expression
	if n := len(accounts); n > 0 && n <= maxFilterAccounts {
		sorted := append([]string(nil), accounts...)
		sort.Strings(sorted)
		dims = append(dims, cetypes.Expression{Dimensions: &cetypes.DimensionValues{
			Key:    cetypes.DimensionLinkedAccount,
			Values: sorted,
		}})
	}
	if len(recordTypes) > 0 {
		dims = append(dims, cetypes.Expression{Dimensions: &cetypes.DimensionValues{
			Key:    cetypes.DimensionRecordType,
			Values: recordTypes,
		}})
	}
	switch len(dims) {
	case 0:
		return nil
	case 1:
		return &dims[0]
	default:
		return &cetypes.Expression{And: dims}
	}
}

// assemble turns the two result sets into the per-currency report.
//
// The partition is exact by construction rather than by a checked sum: every
// record type is either in the attributed allowlist or it is not, the two
// sides are disjoint and cover everything, and the total is the sum of both.
// There is no arithmetic here that could leave a remainder unaccounted for.
func assemble(records, services []row) []model.CostByCurrency {
	currencies := map[string]bool{}
	for _, r := range records {
		currencies[r.currency] = true
	}
	for _, r := range services {
		currencies[r.currency] = true
	}

	out := make([]model.CostByCurrency, 0, len(currencies))
	for cur := range currencies {
		var all, attributed, unattributed []amount
		byRecord := map[string][]amount{}
		byAccount := map[string][]amount{}
		for _, r := range records {
			if r.currency != cur {
				continue
			}
			all = append(all, r.amt)
			byAccount[r.account] = append(byAccount[r.account], r.amt)
			if attributedRecordTypes[r.name] {
				attributed = append(attributed, r.amt)
				continue
			}
			unattributed = append(unattributed, r.amt)
			byRecord[r.name] = append(byRecord[r.name], r.amt)
		}
		byService := map[string][]amount{}
		for _, r := range services {
			if r.currency == cur {
				byService[r.name] = append(byService[r.name], r.amt)
			}
		}
		out = append(out, model.CostByCurrency{
			Currency:            cur,
			Total:               sum(all),
			Attributed:          sum(attributed),
			Unattributed:        sum(unattributed),
			Services:            named(byService),
			UnattributedRecords: named(byRecord),
			Accounts:            named(byAccount),
		})
	}
	return out
}

// named collapses a name→amounts map into sorted NamedAmounts. A single
// contributing value is passed through verbatim so leaf figures in the
// artifact are exactly the strings Cost Explorer returned.
func named(m map[string][]amount) []model.NamedAmount {
	if len(m) == 0 {
		return nil
	}
	out := make([]model.NamedAmount, 0, len(m))
	for name, amounts := range m {
		value := sum(amounts)
		if len(amounts) == 1 {
			value = amounts[0].raw
		}
		out = append(out, model.NamedAmount{Name: name, Amount: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ledger builds a failure-ledger entry for the cost phase. Region is empty
// because Cost Explorer is global; the terminal roll-up already renders
// region-less units without a dangling separator.
func ledger(opts Options, kind, msg string) model.Failure {
	return model.Failure{
		AccountID: opts.CallerAccount,
		Service:   Service,
		Error:     kind + ": " + msg,
	}
}

// classify turns a Cost Explorer error into a ledger entry whose kind names
// the fix. The kinds are distinct because the remedies are: enabling a
// service, granting a permission, waiting, and re-running are four different
// actions, and one generic "cost lookup failed" line tells the reader none of
// them.
//
// The kind is a prefix on the error text rather than a new Failure field:
// the ledger sorts on (account, region, service, error), so a field outside
// that key would order non-deterministically between two entries that differ
// only by kind.
func classify(opts Options, what string, err error) model.Failure {
	if errors.Is(err, errBudget) {
		return ledger(opts, "ce_pagination_incomplete", fmt.Sprintf(
			"%s: stopped after the per-run Cost Explorer request budget was reached, so the pages already read "+
				"were discarded rather than reported as a total; raise --cost-max-requests to finish it "+
				"(each request costs $0.01)", what))
	}
	if errors.Is(err, errMetricUnavailable) {
		return ledger(opts, "ce_metric_unavailable", fmt.Sprintf(
			"%s: Cost Explorer returned no figure for this metric, so nothing was reported rather than a short "+
				"total or a silently substituted metric — try another --cost-metric from %s (%v)",
			what, strings.Join(Metrics(), ", "), err))
	}

	code, msg := apiError(err)
	lower := strings.ToLower(msg)
	switch {
	case code == "DataUnavailableException",
		strings.Contains(lower, "not enabled"),
		strings.Contains(lower, "enable cost explorer"):
		return ledger(opts, "ce_not_enabled", fmt.Sprintf(
			"%s: Cost Explorer is not enabled for this account — enable it in the Billing console; "+
				"AWS takes about 24 hours to prepare the data (%v)", what, err))

	case code == "AccessDeniedException" || code == "AccessDenied" || strings.Contains(lower, "not authorized"):
		// The linked-account case has no distinct error code — a member
		// account blocked by the payer's Cost Explorer preferences gets a
		// plain access denial — so it is only separated when the message
		// happens to name it. When it does not, it lands under
		// ce_access_denied, whose remedy is checked first anyway.
		if strings.Contains(lower, "linked account") || strings.Contains(lower, "member account") {
			return ledger(opts, "ce_linked_account_access_disabled", fmt.Sprintf(
				"%s: the management account has not granted linked accounts access to billing data — "+
					"enable it in the payer's Cost Explorer preferences, or run with --org from the "+
					"management account (%v)", what, err))
		}
		return ledger(opts, "ce_access_denied", fmt.Sprintf(
			"%s: not authorized for ce:GetCostAndUsage — add the BlueprintCostExplorer statement from "+
				"docs/iam-policy.json to the credentials blueprint is using (%v)", what, err))

	case code == "ThrottlingException", code == "TooManyRequestsException",
		code == "LimitExceededException", code == "RequestLimitExceeded":
		return ledger(opts, "ce_throttled", fmt.Sprintf(
			"%s: Cost Explorer throttled the request and it was not retried, because retrying a "+
				"per-request-billed API would multiply the charge without asking — re-run to try again (%v)", what, err))

	case code == "InvalidNextTokenException":
		return ledger(opts, "ce_pagination_incomplete", fmt.Sprintf(
			"%s: Cost Explorer rejected the pagination token, so this report covers only the pages read "+
				"before it failed (%v)", what, err))
	}
	return ledger(opts, "ce_failed", fmt.Sprintf("%s: %v", what, err))
}

// apiError extracts the service error code and message when the error came
// from AWS. Cost Explorer has no modelled exception for access denial or
// throttling, so those are matched on the wire code rather than a typed error.
func apiError(err error) (code, msg string) {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode(), ae.ErrorMessage()
	}
	return "", err.Error()
}
