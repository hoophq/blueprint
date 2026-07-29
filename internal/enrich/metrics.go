package enrich

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/scan"
)

// MetricsService is the failure-ledger service name for this stage.
//
// It is a ledger name only. Registering the stage as a Scanner would put it in
// Snapshot.Services, which is part of the history scope key, so merely turning
// --metrics on would re-bucket and orphan every existing user's diff history —
// the same reason internal/cost is not a scanner either.
const MetricsService = "metrics"

const (
	// maxQueriesPerCall is GetMetricData's hard limit on MetricDataQueries.
	// Batching to it is the whole reason this runs as an enricher: one call
	// covers 500 resources that the scan saw one unit at a time.
	maxQueriesPerCall = 500

	// defaultPeriod is one day, the resolution the storage metrics this stage
	// reads are actually published at.
	defaultPeriod = 86400

	// defaultLookback is how far back to look for the newest datapoint. Three
	// daily buckets is enough to survive CloudWatch's 24–48h lag on daily
	// statistics while still being narrow enough that a resource which stopped
	// publishing a week ago reports nothing rather than something stale.
	defaultLookback = 72 * time.Hour

	// maxListPages bounds discovery. ListMetrics is free, so the cap is a
	// runaway guard rather than a budget; hitting it is reported and turns
	// filtering off for that call instead of silently narrowing the census.
	maxListPages = 100

	// maxDatapointsPerCall is GetMetricData's per-response datapoint ceiling.
	// Only used to justify not paginating — see fetch.
	maxDatapointsPerCall = 100_800
)

// API is the slice of CloudWatch this package uses, named so tests can
// substitute a fake without reaching for HTTP.
type API interface {
	ListMetrics(context.Context, *cloudwatch.ListMetricsInput, ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error)
	GetMetricData(context.Context, *cloudwatch.GetMetricDataInput, ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// Client builds the CloudWatch client for one account and region.
//
// Unlike cost.Client this keeps the shared adaptive retryer. GetMetricData is
// billed at $0.01 per 1,000 metrics requested — a thousandth of a Cost Explorer
// request — while its throttling limits are low enough that retries are what
// makes a large estate finish at all. Disabling them here would trade a real
// coverage gap for a rounding error.
func Client(cfg aws.Config) API { return cloudwatch.NewFromConfig(cfg) }

// Metrics attaches CloudWatch time-series readings to the census.
//
// # Two traps this deliberately avoids
//
// CloudWatch Metrics Insights (GetMetricData with a SQL "SELECT … FROM SCHEMA"
// expression) looks like the natural way to ask for one statistic across every
// instance in a namespace, and it is a dead end here: Metrics Insights only
// queries the last three hours. For a daily metric — S3 BucketSizeBytes, RDS
// FreeStorageSpace when an instance is idle — it returns an empty result set
// and no error, which is indistinguishable from "this resource has no data".
// Plain MetricStat queries over a multi-day window are used instead.
//
// ListMetrics has the mirror-image trap: passing RecentlyActive: "PT3H" narrows
// discovery to the same three hours. It is left unset, so discovery uses the
// default two-week horizon.
type Metrics struct {
	// NewClient builds the client for one (account, region). Nil means Client.
	NewClient func(aws.Config) API

	// Now replaces time.Now for the query window and ledger timestamps. Nil
	// means time.Now.
	Now func() time.Time

	// Lookback overrides defaultLookback.
	Lookback time.Duration

	mu    sync.Mutex
	meter Meter
}

// Meter records what one run asked CloudWatch for.
//
// GetMetricData is billed per metric requested — $0.01 per 1,000 — so Series
// is the number with a price attached, and the CLI quotes it back. Discovery
// is free, and counted separately for exactly that reason.
//
// Only calls AWS answered are counted. A call that errored is not billed, and
// it is already in the failure ledger, so counting it would inflate the one
// figure the user checks against their bill.
type Meter struct {
	Series        int // metrics requested via GetMetricData, the billed unit
	GetCalls      int
	DiscoverCalls int
}

// Meter returns the tally so far. Safe to call once Enrich has returned.
func (m *Metrics) Meter() Meter {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.meter
}

func (m *Metrics) countDiscovery() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.meter.DiscoverCalls++
}

func (m *Metrics) countFetch(series int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.meter.GetCalls++
	m.meter.Series += series
}

// ChargeUSD renders what AWS charges for requesting n metrics through
// GetMetricData, at $0.01 per 1,000.
//
// The arithmetic is exact integer arithmetic in hundred-thousandths of a
// dollar — n metrics cost exactly n of them — because at this rate a float
// would turn a real charge into an approximate one, and because the figure is
// quoted before the calls and reported after them. Trailing zeros are trimmed
// but never past cents, so a charge reads like money rather than a fraction.
func ChargeUSD(n int) string {
	neg := ""
	if n < 0 {
		// Not reachable from a counter, but "-0.-3" would be worse than
		// handling it.
		neg, n = "-", -n
	}
	frac := fmt.Sprintf("%05d", n%100_000)
	for len(frac) > 2 && frac[len(frac)-1] == '0' {
		frac = frac[:len(frac)-1]
	}
	return fmt.Sprintf("%s%d.%s", neg, n/100_000, frac)
}

// NewMetrics returns the stage wired to the real CloudWatch client.
func NewMetrics() *Metrics { return &Metrics{NewClient: Client} }

// Name implements scan.Enricher.
func (m *Metrics) Name() string { return MetricsService }

var _ scan.Enricher = (*Metrics)(nil)

func (m *Metrics) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Metrics) lookback() time.Duration {
	if m.Lookback > 0 {
		return m.Lookback
	}
	return defaultLookback
}

// query is one planned series read and the census slot its answer belongs in.
type query struct {
	res  int // index into the enrichment's Resources
	spec Spec
	dims map[string]string
}

// scope is one (account, region) CloudWatch endpoint and the queries aimed at
// it. Scopes partition the census by index, so two scopes never write to the
// same resource and their goroutines need no lock.
type scope struct {
	account string
	region  string
	queries []query
}

// Enrich implements scan.Enricher.
func (m *Metrics) Enrich(ctx context.Context, req scan.Enrichment) []model.Failure {
	scopes := planScopes(req.Resources)
	if len(scopes) == 0 {
		return nil
	}

	cfgs := make(map[string]aws.Config, len(req.Targets))
	for _, t := range req.Targets {
		cfgs[t.AccountID] = t.Cfg
	}

	newClient := m.NewClient
	if newClient == nil {
		newClient = Client
	}

	var mu sync.Mutex
	var failures []model.Failure
	report := func(account, region, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, model.Failure{
			AccountID: account,
			Region:    region,
			Service:   MetricsService,
			Error:     fmt.Sprintf(format, args...),
			Time:      m.now(),
		})
	}

	limit := req.Concurrency
	if limit <= 0 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, sc := range scopes {
		cfg, ok := cfgs[sc.account]
		if !ok {
			// The scan produced resources for an account the caller did not
			// hand us credentials for. Nothing to do but say so — inventing a
			// fallback config would query the wrong account.
			report(sc.account, sc.region, "no credentials for this account among the scan targets; %d metric queries skipped", len(sc.queries))
			continue
		}
		cfg = cfg.Copy()
		cfg.Region = sc.region

		wg.Add(1)
		go func(sc scope, cfg aws.Config) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			m.enrichScope(ctx, newClient(cfg), req.Resources, sc, report)
		}(sc, cfg)
	}
	wg.Wait()
	return failures
}

// planScopes turns the census into per-endpoint work, skipping every resource
// whose type has no spec. Scopes are returned in a stable order so a run's
// call sequence does not depend on map iteration.
func planScopes(resources []model.Resource) []scope {
	byScope := map[string]*scope{}
	for i := range resources {
		r := &resources[i]
		specs := metricSpecs[r.Type]
		if len(specs) == 0 {
			continue
		}
		key := r.AccountID + "\x00" + r.Region
		sc, ok := byScope[key]
		if !ok {
			sc = &scope{account: r.AccountID, region: r.Region}
			byScope[key] = sc
		}
		for _, s := range specs {
			dims := s.Dimensions(r)
			if !usableDimensions(dims) {
				continue
			}
			sc.queries = append(sc.queries, query{res: i, spec: s, dims: dims})
		}
	}

	out := make([]scope, 0, len(byScope))
	for _, sc := range byScope {
		if len(sc.queries) > 0 {
			out = append(out, *sc)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].account != out[j].account {
			return out[i].account < out[j].account
		}
		return out[i].region < out[j].region
	})
	return out
}

// usableDimensions rejects a dimension set with a blank name or value: an
// under-specified set does not fail, it matches a broader aggregate series, so
// it would attach another resource's number to this one.
func usableDimensions(dims map[string]string) bool {
	if len(dims) == 0 {
		return false
	}
	for k, v := range dims {
		if k == "" || v == "" {
			return false
		}
	}
	return true
}

func (m *Metrics) enrichScope(ctx context.Context, api API, resources []model.Resource, sc scope, report reporter) {
	queries := sc.queries
	if known, ok := m.discover(ctx, api, sc, report); ok {
		kept := make([]query, 0, len(queries))
		for _, q := range queries {
			if known[signature(q.spec.Namespace, q.spec.MetricName, q.dims)] {
				kept = append(kept, q)
			}
		}
		queries = kept
	}

	for start := 0; start < len(queries); start += maxQueriesPerCall {
		if ctx.Err() != nil {
			return
		}
		end := min(start+maxQueriesPerCall, len(queries))
		m.fetch(ctx, api, resources, sc, queries[start:end], report)
	}
}

// discover lists the series that actually exist, so the batch only asks for
// metrics AWS will answer. ListMetrics is free and GetMetricData is not, so
// this is the cheap half of the stage paying for the expensive half.
//
// The bool reports whether the answer is trustworthy enough to filter on. A
// failed or truncated listing returns false and the caller queries everything:
// filtering on a partial list would drop real measures, and the worst case for
// failing open is paying for queries that come back empty — at $0.01 per 1,000
// metrics, a rounding error against a silent coverage gap.
func (m *Metrics) discover(ctx context.Context, api API, sc scope, report reporter) (map[string]bool, bool) {
	type series struct{ namespace, name string }
	wanted := map[series]bool{}
	for _, q := range sc.queries {
		wanted[series{q.spec.Namespace, q.spec.MetricName}] = true
	}
	names := make([]series, 0, len(wanted))
	for s := range wanted {
		names = append(names, s)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].namespace != names[j].namespace {
			return names[i].namespace < names[j].namespace
		}
		return names[i].name < names[j].name
	})

	known := map[string]bool{}
	for _, s := range names {
		var token *string
		for page := 0; ; page++ {
			if ctx.Err() != nil {
				return nil, false
			}
			if page >= maxListPages {
				report(sc.account, sc.region, "ListMetrics for %s/%s exceeded %d pages; querying without discovery",
					s.namespace, s.name, maxListPages)
				return nil, false
			}
			// RecentlyActive is deliberately unset: its only value, "PT3H",
			// would hide every metric published less often than three-hourly,
			// which is all of them here.
			out, err := api.ListMetrics(ctx, &cloudwatch.ListMetricsInput{
				Namespace:  aws.String(s.namespace),
				MetricName: aws.String(s.name),
				NextToken:  token,
			})
			if err != nil {
				report(sc.account, sc.region, "ListMetrics %s/%s: %v; querying without discovery", s.namespace, s.name, err)
				return nil, false
			}
			m.countDiscovery()
			for _, mt := range out.Metrics {
				known[metricSignature(mt)] = true
			}
			if out.NextToken == nil || *out.NextToken == "" {
				break
			}
			token = out.NextToken
		}
	}
	return known, true
}

// fetch reads one batch and writes the newest datapoint of each series into
// its resource.
func (m *Metrics) fetch(ctx context.Context, api API, resources []model.Resource, sc scope, batch []query, report reporter) {
	end := m.now()
	start := end.Add(-m.window(batch))

	queries := make([]cwtypes.MetricDataQuery, len(batch))
	for i, q := range batch {
		queries[i] = cwtypes.MetricDataQuery{
			Id: aws.String("q" + strconv.Itoa(i)),
			MetricStat: &cwtypes.MetricStat{
				Metric: &cwtypes.Metric{
					Namespace:  aws.String(q.spec.Namespace),
					MetricName: aws.String(q.spec.MetricName),
					Dimensions: dimensionList(q.dims),
				},
				Period: aws.Int32(period(q.spec)),
				Stat:   aws.String(q.spec.Stat),
			},
			ReturnData: aws.Bool(true),
		}
	}

	out, err := api.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		MetricDataQueries: queries,
		StartTime:         aws.Time(start),
		EndTime:           aws.Time(end),
		// Newest first, which is the only datapoint any of these queries
		// needs — and, with the batch sizes above, the reason one response is
		// always enough.
		ScanBy: cwtypes.ScanByTimestampDescending,
	})
	if err != nil {
		report(sc.account, sc.region, "GetMetricData for %d series: %v", len(batch), err)
		return
	}
	m.countFetch(len(batch))

	// The response is not paginated through. A batch asks for at most 500
	// series over three daily buckets — 1,500 datapoints against a ceiling of
	// maxDatapointsPerCall — and TimestampDescending puts the newest datapoint
	// of every series in the first response. A continuation would re-bill the
	// same metrics to fetch older data this stage discards. If one shows up
	// the arithmetic above is wrong somewhere, so say so rather than silently
	// returning a partial answer.
	if out.NextToken != nil && *out.NextToken != "" {
		report(sc.account, sc.region, "GetMetricData returned a continuation token for %d series; readings may be incomplete", len(batch))
	}

	newest := make(map[string]datapoint, len(batch))
	degraded := map[string]int{}
	for _, res := range out.MetricDataResults {
		id := aws.ToString(res.Id)
		if code := string(res.StatusCode); code != "" && res.StatusCode != cwtypes.StatusCodeComplete {
			degraded[code]++
		}
		for i := range res.Timestamps {
			if i >= len(res.Values) {
				break
			}
			// Timestamps and Values are parallel and hold only the datapoints
			// that exist — CloudWatch omits empty buckets rather than padding
			// them — so every pair here is a real reading.
			ts := res.Timestamps[i]
			if cur, ok := newest[id]; ok && !ts.After(cur.at) {
				continue
			}
			newest[id] = datapoint{value: res.Values[i], at: ts.UTC()}
		}
	}
	if len(degraded) > 0 {
		report(sc.account, sc.region, "GetMetricData returned non-complete results: %s", countsByCode(degraded))
	}

	unusable := 0
	for i, q := range batch {
		dp, ok := newest["q"+strconv.Itoa(i)]
		if !ok {
			// No datapoint in the window. That is an absent key, never a zero:
			// a stopped instance and a full volume must not read the same.
			continue
		}
		v, ok := toInt64(dp.value)
		if !ok {
			unusable++
			continue
		}
		// The timestamp is the start of the aggregation bucket, so it errs
		// old — which is the safe direction for a staleness judgement.
		resources[q.res].SetObservedMeasure(q.spec.Measure, v, dp.at)
	}
	if unusable > 0 {
		report(sc.account, sc.region, "%d of %d series returned a value outside the representable range and were dropped", unusable, len(batch))
	}
}

type datapoint struct {
	value float64
	at    time.Time
}

// reporter records a coverage gap in the failure ledger.
type reporter func(account, region, format string, args ...any)

// window is wide enough to hold three of the coarsest buckets in the batch, so
// a metric published once a period still has somewhere to land after
// CloudWatch's lag.
func (m *Metrics) window(batch []query) time.Duration {
	w := m.lookback()
	for _, q := range batch {
		if need := 3 * time.Duration(period(q.spec)) * time.Second; need > w {
			w = need
		}
	}
	return w
}

func period(s Spec) int32 {
	if s.Period > 0 {
		return s.Period
	}
	return defaultPeriod
}

func dimensionList(dims map[string]string) []cwtypes.Dimension {
	names := make([]string, 0, len(dims))
	for k := range dims {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]cwtypes.Dimension, len(names))
	for i, n := range names {
		out[i] = cwtypes.Dimension{Name: aws.String(n), Value: aws.String(dims[n])}
	}
	return out
}

// signature identifies a series by namespace, name, and its exact dimension
// set. The dimension set has to be exact: CloudWatch publishes the same metric
// name against several dimension combinations — per instance, and rolled up by
// engine or class — and a looser match would let a rollup answer for a
// resource.
func signature(namespace, name string, dims map[string]string) string {
	names := make([]string, 0, len(dims))
	for k := range dims {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(namespace)
	b.WriteByte(0)
	b.WriteString(name)
	for _, n := range names {
		b.WriteByte(0)
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(dims[n])
	}
	return b.String()
}

func metricSignature(mt cwtypes.Metric) string {
	dims := make(map[string]string, len(mt.Dimensions))
	for _, d := range mt.Dimensions {
		dims[aws.ToString(d.Name)] = aws.ToString(d.Value)
	}
	return signature(aws.ToString(mt.Namespace), aws.ToString(mt.MetricName), dims)
}

// toInt64 rounds a CloudWatch value to the integer the census stores. A NaN,
// an infinity, or a magnitude past int64 is reported as unusable rather than
// clamped: a saturated number would render as a real reading.
func toInt64(v float64) (int64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	r := math.Round(v)
	if r > math.MaxInt64 || r < math.MinInt64 {
		return 0, false
	}
	return int64(r), true
}

func countsByCode(counts map[string]int) string {
	codes := make([]string, 0, len(counts))
	for c := range counts {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	parts := make([]string, len(codes))
	for i, c := range codes {
		parts[i] = fmt.Sprintf("%s=%d", c, counts[c])
	}
	return strings.Join(parts, ", ")
}
