package cost

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"

	"github.com/hoophq/blueprint/internal/model"
)

// Cost Explorer service names used by more than one test. Spelled out rather
// than referenced from censusServices, because that map is a spend gate whose
// keys may legitimately change; these are the strings AWS sends.
const (
	ceCompute = "Amazon Elastic Compute Cloud - Compute"
	ceOther   = "EC2 - Other"
	ceEBS     = "Amazon Elastic Block Store"
	ceS3      = "Amazon Simple Storage Service"
	ceRoute53 = "Amazon Route 53"
)

// fakeResourceCE answers per service, because that is the axis this pass
// varies: one request per service, each with its own outcome.
type fakeResourceCE struct {
	// pages are consumed in order per service, so a service can paginate.
	pages map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput
	// errs fail a service outright, before any of its pages are served.
	errs map[string]error
	// calls records the SERVICE filter value of every request, in order, so a
	// test can assert both what was billed and in what order.
	calls []string
}

func (f *fakeResourceCE) GetCostAndUsageWithResources(_ context.Context, in *costexplorer.GetCostAndUsageWithResourcesInput, _ ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageWithResourcesOutput, error) {
	svc := serviceOf(in)
	f.calls = append(f.calls, svc)
	if err, ok := f.errs[svc]; ok {
		return nil, err
	}
	pages := f.pages[svc]
	if len(pages) == 0 {
		return &costexplorer.GetCostAndUsageWithResourcesOutput{}, nil
	}
	f.pages[svc] = pages[1:]
	return pages[0], nil
}

// serviceOf digs the SERVICE value out of the request filter, which is also a
// check that the filter is shaped the way resourceFilter builds it.
func serviceOf(in *costexplorer.GetCostAndUsageWithResourcesInput) string {
	if in == nil || in.Filter == nil {
		return ""
	}
	for _, e := range in.Filter.And {
		if e.Dimensions != nil && e.Dimensions.Key == cetypes.DimensionService && len(e.Dimensions.Values) == 1 {
			return e.Dimensions.Values[0]
		}
	}
	return ""
}

// resGroup builds one resource-level group: RESOURCE_ID and an amount.
func resGroup(id, amt string) cetypes.Group {
	return cetypes.Group{
		Keys: []string{id},
		Metrics: map[string]cetypes.MetricValue{
			"AmortizedCost": {Amount: aws.String(amt), Unit: aws.String("USD")},
		},
	}
}

// inCurrency restates a group in another currency, for the mismatch cases.
func inCurrency(g cetypes.Group, unit string) cetypes.Group {
	m := g.Metrics["AmortizedCost"]
	m.Unit = aws.String(unit)
	g.Metrics["AmortizedCost"] = m
	return g
}

// resPage is one response covering one billing period.
func resPage(groups ...cetypes.Group) *costexplorer.GetCostAndUsageWithResourcesOutput {
	return &costexplorer.GetCostAndUsageWithResourcesOutput{
		ResultsByTime: []cetypes.ResultByTime{{Groups: groups}},
	}
}

// withToken marks a page as having a successor, so the pass paginates.
func withToken(p *costexplorer.GetCostAndUsageWithResourcesOutput) *costexplorer.GetCostAndUsageWithResourcesOutput {
	p.NextPageToken = aws.String("next")
	return p
}

// rollupWith builds the minimal account rollup the pass needs, naming services
// and their spend so planProbes has something to order by.
func rollupWith(services ...model.NamedAmount) *model.CostReport {
	return &model.CostReport{
		Window: model.CostWindow{Start: "2026-06-01", End: "2026-07-01", Label: "2026-06"},
		Metric: "AmortizedCost",
		Currencies: []model.CostByCurrency{{
			Currency: "USD", Total: "100.00", Attributed: "100.00", Unattributed: "0.00",
			Services: services,
		}},
	}
}

func resOptions(rollup *model.CostReport) ResourceOptions {
	return ResourceOptions{
		Accounts:      []string{"111111111111"},
		CallerAccount: "111111111111",
		Metric:        DefaultMetric,
		Window:        model.CostWindow{Start: "2026-07-17", End: "2026-07-31", Label: "2026-07-17 → 2026-07-30"},
		Rollup:        rollup,
	}
}

// res builds a census resource. name is the human tag; the ARN carries the
// AWS-assigned identifier Cost Explorer actually reports.
func res(name, arn string) model.Resource {
	return model.Resource{
		ARN: arn, Name: name, Service: model.ServiceEC2, Type: model.TypeEC2Instance,
		Region: "us-east-1", AccountID: "111111111111",
	}
}

// ce returns the resource's Cost Explorer figure, failing if it has none.
func ce(t *testing.T, r model.Resource) *model.ResourceCost {
	t.Helper()
	c := r.CostBy(model.CostMethodCE)
	if c == nil {
		t.Fatalf("%s carries no Cost Explorer figure: %+v", r.Name, r.Costs)
	}
	return c
}

// probeFor returns the report's entry for one service.
func probeFor(t *testing.T, rep *model.ResourceCostReport, service string) model.ServiceProbe {
	t.Helper()
	if rep == nil {
		t.Fatal("report is nil")
	}
	for _, p := range rep.Probes {
		if p.Service == service {
			return p
		}
	}
	t.Fatalf("no probe for %q in %+v", service, rep.Probes)
	return model.ServiceProbe{}
}

// ledgerKinds lists the failure kinds a pass produced, for assertions that care
// which entries exist rather than how they are worded.
func ledgerKinds(failures []model.Failure) []string {
	out := make([]string, 0, len(failures))
	for _, f := range failures {
		kind, _, _ := strings.Cut(f.Error, ":")
		out = append(out, strings.TrimSpace(kind))
	}
	return out
}

func hasKind(failures []model.Failure, kind string) bool {
	return slices.Contains(ledgerKinds(failures), kind)
}

// The two join paths: Cost Explorer reports a full ARN for some services and a
// bare AWS-assigned identifier for others, and both have to land.
func TestCollectResourcesJoinsByARNAndByIdentifier(t *testing.T) {
	bucketARN := "arn:aws:s3:::orders-archive"
	resources := []model.Resource{
		res("api-gateway-1", "arn:aws:ec2:us-east-1:111111111111:instance/i-0abc"),
		{ARN: bucketARN, Name: "orders-archive", Service: model.ServiceS3, Type: model.TypeS3Bucket},
	}
	f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
		ceCompute: {resPage(resGroup("i-0abc", "87.60"))},
		// S3 reports the bucket's full ARN, which the exact index catches.
		ceS3: {resPage(resGroup(bucketARN, "12.34"))},
	}}
	opts := resOptions(rollupWith(
		model.NamedAmount{Name: ceCompute, Amount: "87.60"},
		model.NamedAmount{Name: ceS3, Amount: "12.34"},
	))

	rep, failures := CollectResources(context.Background(), f, resources, opts)
	if len(failures) != 0 {
		t.Fatalf("unexpected ledger entries: %v", ledgerKinds(failures))
	}
	if got := ce(t, resources[0]).Amount; got != "87.60" {
		t.Errorf("instance amount = %q, want 87.60", got)
	}
	// The identifier is not the ARN, so it is recorded — the reader has to be
	// able to see what the join was made on.
	if got := ce(t, resources[0]).MatchKey; got != "i-0abc" {
		t.Errorf("instance match key = %q, want i-0abc", got)
	}
	if got := ce(t, resources[1]).Amount; got != "12.34" {
		t.Errorf("bucket amount = %q, want 12.34", got)
	}
	// Matched on the ARN itself: there is no separate key to disclose, and
	// echoing the ARN would suggest a fallback was used.
	if got := ce(t, resources[1]).MatchKey; got != "" {
		t.Errorf("bucket match key = %q, want empty for an exact ARN match", got)
	}
	if got := probeFor(t, rep, ceCompute).Outcome; got != model.ProbeRows {
		t.Errorf("compute outcome = %q, want rows", got)
	}
	if rep.Meter.Requests != 2 {
		t.Errorf("billed %d requests for 2 services", rep.Meter.Requests)
	}
}

// AWS splits one resource's bill across service names: an instance's hours land
// under EC2-Compute and its data transfer under "EC2 - Other". Those are
// components of one bill, so they add. Replacing one with the other would
// publish whichever component happened to be probed last, and since probes run
// most-expensive-first that is systematically the smaller one — an under-count
// with nothing in the artifact to reveal it.
func TestCollectResourcesSumsComponentsAcrossServices(t *testing.T) {
	resources := []model.Resource{res("api-gateway-1", "arn:aws:ec2:us-east-1:111111111111:instance/i-0abc")}
	f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
		ceCompute: {resPage(resGroup("i-0abc", "87.60"))},
		ceOther:   {resPage(resGroup("i-0abc", "0.42"))},
	}}
	opts := resOptions(rollupWith(
		model.NamedAmount{Name: ceCompute, Amount: "87.60"},
		model.NamedAmount{Name: ceOther, Amount: "0.42"},
	))

	_, failures := CollectResources(context.Background(), f, resources, opts)
	if len(failures) != 0 {
		t.Fatalf("unexpected ledger entries: %v", ledgerKinds(failures))
	}
	// The expensive service is probed first, so a replace would leave 0.42.
	if got := f.calls; len(got) != 2 || got[0] != ceCompute {
		t.Fatalf("probe order = %v, want the dearer service first", got)
	}
	c := ce(t, resources[0])
	if c.Amount != "88.02" {
		t.Errorf("amount = %q, want 88.02 (87.60 + 0.42)", c.Amount)
	}
	// A reader comparing this against one service's console view would find it
	// too high, so the figure has to say what it is made of.
	joined := strings.Join(c.Caveats, " | ")
	for _, want := range []string{"2 Cost Explorer services", ceCompute, ceOther} {
		if !strings.Contains(joined, want) {
			t.Errorf("caveats %q do not mention %q", joined, want)
		}
	}
}

// The one case where components cannot be added. The running total stands, and
// both the ledger and the figure say it is short — an under-count the reader can
// see beats a total in a currency that is neither.
func TestCollectResourcesRefusesToAddAcrossCurrencies(t *testing.T) {
	resources := []model.Resource{res("api-gateway-1", "arn:aws:ec2:us-east-1:111111111111:instance/i-0abc")}
	f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
		ceCompute: {resPage(resGroup("i-0abc", "87.60"))},
		ceOther:   {resPage(inCurrency(resGroup("i-0abc", "0.42"), "EUR"))},
	}}
	opts := resOptions(rollupWith(
		model.NamedAmount{Name: ceCompute, Amount: "87.60"},
		model.NamedAmount{Name: ceOther, Amount: "0.42"},
	))

	_, failures := CollectResources(context.Background(), f, resources, opts)
	if !hasKind(failures, "ce_res_merge_currency_conflict") {
		t.Errorf("no ledger entry for the currency mismatch: %v", ledgerKinds(failures))
	}
	c := ce(t, resources[0])
	if c.Amount != "87.60" || c.Currency != "USD" {
		t.Errorf("figure = %q %q, want the USD components alone", c.Amount, c.Currency)
	}
	if !strings.Contains(strings.Join(c.Caveats, " | "), "short by") {
		t.Errorf("caveats %v do not say the figure is short", c.Caveats)
	}
}

// A window spanning a month boundary comes back as two periods. The figure the
// artifact wants is the resource's total over the window, not one period's
// slice of it.
func TestCollectResourcesSumsPeriodsWithinOneService(t *testing.T) {
	resources := []model.Resource{res("api-gateway-1", "arn:aws:ec2:us-east-1:111111111111:instance/i-0abc")}
	f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
		ceCompute: {{ResultsByTime: []cetypes.ResultByTime{
			{Groups: []cetypes.Group{resGroup("i-0abc", "20.00")}},
			{Groups: []cetypes.Group{resGroup("i-0abc", "5.25")}},
		}}},
	}}
	opts := resOptions(rollupWith(model.NamedAmount{Name: ceCompute, Amount: "25.25"}))

	if _, failures := CollectResources(context.Background(), f, resources, opts); len(failures) != 0 {
		t.Fatalf("unexpected ledger entries: %v", ledgerKinds(failures))
	}
	if got := ce(t, resources[0]).Amount; got != "25.25" {
		t.Errorf("amount = %q, want 25.25 — the window total, not one period", got)
	}
}

// Every outcome means something different to the reader, and the difference is
// what the probe list exists to carry.
func TestCollectResourcesRecordsWhatEachServiceAnswered(t *testing.T) {
	resources := []model.Resource{res("api-gateway-1", "arn:aws:ec2:us-east-1:111111111111:instance/i-0abc")}
	f := &fakeResourceCE{
		pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
			ceCompute: {resPage(resGroup("i-0abc", "87.60"))},
			ceEBS:     {resPage()},
		},
		errs: map[string]error{
			ceOther: apiErr("ValidationException", "Resource level data is not supported for this service"),
			ceS3:    apiErr("AccessDeniedException", "not authorized to perform ce:GetCostAndUsageWithResources"),
		},
	}
	opts := resOptions(rollupWith(
		model.NamedAmount{Name: ceCompute, Amount: "87.60"},
		model.NamedAmount{Name: ceOther, Amount: "50.00"},
		model.NamedAmount{Name: ceS3, Amount: "40.00"},
		model.NamedAmount{Name: ceEBS, Amount: "30.00"},
		// No scanner covers Route 53, so its spend is real and unreachable.
		model.NamedAmount{Name: ceRoute53, Amount: "20.00"},
	))

	rep, _ := CollectResources(context.Background(), f, resources, opts)
	for service, want := range map[string]string{
		ceCompute: model.ProbeRows,
		ceEBS:     model.ProbeEmpty,
		ceOther:   model.ProbeUnsupported,
		ceS3:      model.ProbeDenied,
		ceRoute53: model.ProbeUncensused,
	} {
		if got := probeFor(t, rep, service).Outcome; got != want {
			t.Errorf("%s outcome = %q, want %q", service, got, want)
		}
	}
	// An uncensused service is never asked, so it is never billed.
	for _, c := range f.calls {
		if c == ceRoute53 {
			t.Error("paid $0.01 to ask about a service no scanner covers")
		}
	}
	if rep.Meter.Requests != 4 {
		t.Errorf("billed %d requests for 4 probed services", rep.Meter.Requests)
	}
}

// The budget ran out before this service's turn, so nothing is known about it.
// "Skipped" is not "zero".
func TestCollectResourcesSkipsServicesTheBudgetCannotReach(t *testing.T) {
	resources := []model.Resource{res("api-gateway-1", "arn:aws:ec2:us-east-1:111111111111:instance/i-0abc")}
	f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
		ceCompute: {resPage(resGroup("i-0abc", "87.60"))},
		ceEBS:     {resPage(resGroup("vol-0abc", "1.00"))},
	}}
	opts := resOptions(rollupWith(
		model.NamedAmount{Name: ceCompute, Amount: "87.60"},
		model.NamedAmount{Name: ceEBS, Amount: "30.00"},
	))
	opts.Budget = NewBudget(1)

	rep, failures := CollectResources(context.Background(), f, resources, opts)
	skipped := probeFor(t, rep, ceEBS)
	if skipped.Outcome != model.ProbeSkipped {
		t.Errorf("unreached service outcome = %q, want skipped", skipped.Outcome)
	}
	if skipped.Rows != 0 {
		t.Errorf("a service that was never asked reported %d rows", skipped.Rows)
	}
	if !rep.Meter.Capped {
		t.Error("meter does not report the cap that stopped the pass")
	}
	if !hasKind(failures, "ce_res_budget_exhausted") {
		t.Errorf("budget exhaustion is not in the ledger: %v", ledgerKinds(failures))
	}
	if rep.Meter.Requests != 1 {
		t.Errorf("billed %d requests against a budget of 1", rep.Meter.Requests)
	}
}

// The budget runs out *between pages*, so this service was asked, was billed,
// and did report. Calling that "skipped" would tell the reader it was never
// touched while its figures sit in the artifact, and it would drop a billed
// request from the "services probed" count.
func TestCollectResourcesReportsAServiceBilledMidPagination(t *testing.T) {
	resources := []model.Resource{res("api-gateway-1", "arn:aws:ec2:us-east-1:111111111111:instance/i-0abc")}
	f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
		ceCompute: {
			withToken(resPage(resGroup("i-0abc", "87.60"))),
			resPage(resGroup("i-0def", "1.00")),
		},
	}}
	opts := resOptions(rollupWith(model.NamedAmount{Name: ceCompute, Amount: "87.60"}))
	opts.Budget = NewBudget(1)

	rep, failures := CollectResources(context.Background(), f, resources, opts)
	p := probeFor(t, rep, ceCompute)
	if p.Outcome != model.ProbeRows {
		t.Errorf("outcome = %q, want rows — the service answered and was billed", p.Outcome)
	}
	if p.Rows != 1 || p.Matched != 1 {
		t.Errorf("rows/matched = %d/%d, want 1/1", p.Rows, p.Matched)
	}
	if !strings.Contains(p.Detail, "budget") {
		t.Errorf("detail %q does not say why the list is short", p.Detail)
	}
	// The page that was fetched was paid for and its figure stands.
	if got := ce(t, resources[0]).Amount; got != "87.60" {
		t.Errorf("amount = %q, want the figure from the page that was billed", got)
	}
	if !hasKind(failures, "ce_res_budget_exhausted") {
		t.Errorf("budget exhaustion is not in the ledger: %v", ledgerKinds(failures))
	}
}

// A row matching two resources is refused. A figure on the wrong resource is
// worse than a missing one: it is wrong in a way the reader cannot see.
func TestCollectResourcesRefusesAnAmbiguousRow(t *testing.T) {
	resources := []model.Resource{
		{ARN: "arn:aws:dynamodb:us-east-1:111111111111:table/orders", Name: "orders",
			Service: model.ServiceDynamoDB, Type: model.TypeDynamoDBTable, Region: "us-east-1"},
		{ARN: "arn:aws:dynamodb:eu-west-1:111111111111:table/orders", Name: "orders",
			Service: model.ServiceDynamoDB, Type: model.TypeDynamoDBTable, Region: "eu-west-1"},
	}
	f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
		"Amazon DynamoDB": {resPage(resGroup("orders", "9.99"))},
	}}
	opts := resOptions(rollupWith(model.NamedAmount{Name: "Amazon DynamoDB", Amount: "9.99"}))

	rep, failures := CollectResources(context.Background(), f, resources, opts)
	if !hasKind(failures, "ce_res_ambiguous_match") {
		t.Errorf("ambiguity is not in the ledger: %v", ledgerKinds(failures))
	}
	for i := range resources {
		if c := resources[i].CostBy(model.CostMethodCE); c != nil {
			t.Errorf("resource %d was given a figure that could belong to either: %+v", i, c)
		}
	}
	p := probeFor(t, rep, "Amazon DynamoDB")
	if p.Rows != 1 || p.Matched != 0 {
		t.Errorf("rows/matched = %d/%d, want 1/0 — the row arrived but was not joined", p.Rows, p.Matched)
	}
}

// Spend on something no scanner covers is a coverage gap, not lost money: it is
// still in the account rollup, and the probe's rows-minus-matched gap shows it.
func TestCollectResourcesReportsRowsItCannotPlace(t *testing.T) {
	resources := []model.Resource{res("api-gateway-1", "arn:aws:ec2:us-east-1:111111111111:instance/i-0abc")}
	f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
		ceCompute: {resPage(resGroup("i-0abc", "87.60"), resGroup("i-0nothere", "5.00"))},
	}}
	opts := resOptions(rollupWith(model.NamedAmount{Name: ceCompute, Amount: "92.60"}))

	rep, failures := CollectResources(context.Background(), f, resources, opts)
	if !hasKind(failures, "ce_res_unmatched") {
		t.Errorf("unmatched rows are not in the ledger: %v", ledgerKinds(failures))
	}
	p := probeFor(t, rep, ceCompute)
	if p.Rows != 2 || p.Matched != 1 {
		t.Errorf("rows/matched = %d/%d, want 2/1", p.Rows, p.Matched)
	}
}

// Spend within a service that belongs to no one resource is real, and it is
// already in the rollup. Spreading it over the resources that do have rows
// would fabricate per-resource figures nobody reported.
func TestCollectResourcesDropsTheNoResourcePlaceholder(t *testing.T) {
	resources := []model.Resource{res("api-gateway-1", "arn:aws:ec2:us-east-1:111111111111:instance/i-0abc")}
	f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
		ceCompute: {resPage(resGroup("i-0abc", "87.60"), resGroup(noResourceID, "13.00"))},
	}}
	opts := resOptions(rollupWith(model.NamedAmount{Name: ceCompute, Amount: "100.60"}))

	rep, failures := CollectResources(context.Background(), f, resources, opts)
	if hasKind(failures, "ce_res_unmatched") {
		t.Errorf("the placeholder was reported as a scanner coverage gap: %v", ledgerKinds(failures))
	}
	if got := probeFor(t, rep, ceCompute).Rows; got != 1 {
		t.Errorf("rows = %d, want 1 — the placeholder is not a resource", got)
	}
	if got := ce(t, resources[0]).Amount; got != "87.60" {
		t.Errorf("amount = %q — the unattributable spend was spread onto a resource", got)
	}
}

// A resource AWS billed at exactly zero is a real reading and must survive. The
// recurring bug is a "> 0" filter that reclassifies it as "not reported".
func TestCollectResourcesKeepsAZeroFigure(t *testing.T) {
	resources := []model.Resource{res("idle", "arn:aws:ec2:us-east-1:111111111111:instance/i-0idle")}
	f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
		ceCompute: {resPage(resGroup("i-0idle", "0"))},
	}}
	opts := resOptions(rollupWith(model.NamedAmount{Name: ceCompute, Amount: "0"}))

	if _, failures := CollectResources(context.Background(), f, resources, opts); len(failures) != 0 {
		t.Fatalf("unexpected ledger entries: %v", ledgerKinds(failures))
	}
	c := ce(t, resources[0])
	if c.Amount != "0.00" {
		t.Errorf("amount = %q, want 0.00 — AWS reported zero, which is a finding", c.Amount)
	}
}

// Cost Explorer's RESOURCE_ID is an AWS-assigned identifier, never a Name tag,
// so indexing the tag can only produce a hit that is wrong — and it produces
// them in both directions. A snapshot tagged after the volume it came from must
// not make that volume's spend ambiguous.
func TestJoinIgnoresNameTags(t *testing.T) {
	volume := model.Resource{
		ARN: "arn:aws:ec2:us-east-1:111111111111:volume/vol-0abc", Name: "api-gateway-1-root",
		Service: model.ServiceEBS, Type: model.TypeEBSVolume, Region: "us-east-1",
	}
	// Named after its source volume, which is ordinary practice.
	snapshot := model.Resource{
		ARN: "arn:aws:ec2:us-east-1::snapshot/snap-0abc", Name: "vol-0abc",
		Service: model.ServiceEBS, Type: model.TypeEBSSnapshot, Region: "us-east-1",
	}
	resources := []model.Resource{volume, snapshot}
	f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
		ceEBS: {resPage(resGroup("vol-0abc", "4.20"))},
	}}
	opts := resOptions(rollupWith(model.NamedAmount{Name: ceEBS, Amount: "4.20"}))

	_, failures := CollectResources(context.Background(), f, resources, opts)
	if hasKind(failures, "ce_res_ambiguous_match") {
		t.Fatalf("a Name tag made a real identifier ambiguous: %v", ledgerKinds(failures))
	}
	if got := ce(t, resources[0]).Amount; got != "4.20" {
		t.Errorf("volume amount = %q, want 4.20", got)
	}
	if c := resources[1].CostBy(model.CostMethodCE); c != nil {
		t.Errorf("the snapshot was priced from its Name tag: %+v", c)
	}
}

// Truncation is a claim about AWS's ceiling, so it is only made when nothing
// else already explains a short list. A request that failed part-way returned
// fewer rows for a reason this pass knows, and naming the ceiling instead would
// send the reader looking for a service that does not exist.
func TestCollectResourcesDoesNotBlameTruncationForAFailedRequest(t *testing.T) {
	groups := make([]cetypes.Group, maxResourceGroups)
	resources := make([]model.Resource, 0, maxResourceGroups)
	for i := range groups {
		id := "i-" + strings.Repeat("0", 4) + itoa(i)
		groups[i] = resGroup(id, "1.00")
		resources = append(resources, res("host-"+itoa(i), "arn:aws:ec2:us-east-1:111111111111:instance/"+id))
	}

	t.Run("clean request at the ceiling is flagged", func(t *testing.T) {
		f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
			ceCompute: {resPage(groups...)},
		}}
		rs := append([]model.Resource(nil), resources...)
		rep, failures := CollectResources(context.Background(), f, rs,
			resOptions(rollupWith(model.NamedAmount{Name: ceCompute, Amount: "5000.00"})))
		if !probeFor(t, rep, ceCompute).Truncated {
			t.Error("a list at the ceiling was not flagged as possibly short")
		}
		if !hasKind(failures, "ce_res_truncated") {
			t.Errorf("truncation is not in the ledger: %v", ledgerKinds(failures))
		}
	})

	t.Run("throttled request at the ceiling is not", func(t *testing.T) {
		f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
			// The first page fills to the ceiling and points at a second that
			// comes back empty-handed, so the short list has a known cause.
			ceCompute: {withToken(resPage(groups...)), nil},
		}}
		rs := append([]model.Resource(nil), resources...)
		rep, failures := CollectResources(context.Background(), f, rs,
			resOptions(rollupWith(model.NamedAmount{Name: ceCompute, Amount: "5000.00"})))
		if probeFor(t, rep, ceCompute).Truncated {
			t.Error("AWS's ceiling was blamed for a list a failed request cut short")
		}
		if hasKind(failures, "ce_res_truncated") {
			t.Errorf("truncation was ledgered for a failed request: %v", ledgerKinds(failures))
		}
	})
}

// itoa avoids pulling strconv in for one call site in one test.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// The guards that stop the pass before it spends anything. Each returns a
// ledger entry rather than silence, because a pass that ran and found nothing
// and a pass that never ran look identical in the resources.
func TestCollectResourcesRefusesToStartWithoutWhatItNeeds(t *testing.T) {
	base := resOptions(rollupWith(model.NamedAmount{Name: ceCompute, Amount: "1.00"}))
	for _, tc := range []struct {
		name string
		opts func(ResourceOptions) ResourceOptions
		kind string
	}{
		{"unknown metric", func(o ResourceOptions) ResourceOptions {
			o.Metric = "blended"
			return o
		}, "ce_res_invalid_metric"},
		{"no rollup", func(o ResourceOptions) ResourceOptions {
			o.Rollup = nil
			return o
		}, "ce_res_no_rollup"},
		{"too many accounts", func(o ResourceOptions) ResourceOptions {
			for i := 0; i <= maxFilterAccounts; i++ {
				o.Accounts = append(o.Accounts, itoa(i))
			}
			return o
		}, "ce_res_too_many_accounts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeResourceCE{}
			rep, failures := CollectResources(context.Background(), f, nil, tc.opts(base))
			if rep != nil {
				t.Errorf("a pass that could not start published a report: %+v", rep)
			}
			if !hasKind(failures, tc.kind) {
				t.Errorf("ledger = %v, want %s", ledgerKinds(failures), tc.kind)
			}
			if len(f.calls) != 0 {
				t.Errorf("spent %d billed requests before the guard: %v", len(f.calls), f.calls)
			}
		})
	}

	// No accounts is the one case that is not an error: there is nothing to ask
	// about, so there is nothing to report either way.
	t.Run("no accounts", func(t *testing.T) {
		o := base
		o.Accounts = nil
		rep, failures := CollectResources(context.Background(), &fakeResourceCE{}, nil, o)
		if rep != nil || len(failures) != 0 {
			t.Errorf("got report %+v and ledger %v, want silence", rep, ledgerKinds(failures))
		}
	})
}

// The window is fourteen complete UTC days ending yesterday. Complete days
// only: a window ending today moves every hour, so two runs on one day would
// disagree and the diff would report drift that is only the clock advancing.
func TestResourceWindowIsFourteenCompleteDays(t *testing.T) {
	now := time.Date(2026, 7, 31, 13, 45, 0, 0, time.UTC)
	w := ResourceWindow(now)
	if w.Start != "2026-07-17" {
		t.Errorf("Start = %q, want 2026-07-17", w.Start)
	}
	// Exclusive, so it names today: the window covers up to but not including it.
	if w.End != "2026-07-31" {
		t.Errorf("End = %q, want 2026-07-31 (exclusive)", w.End)
	}
	// The label reads as inclusive, so it names the last day actually covered.
	if !strings.Contains(w.Label, "2026-07-30") {
		t.Errorf("Label = %q, want it to end on the last covered day", w.Label)
	}

	// The span is what makes two runs comparable; the dates shift, it does not.
	later := ResourceWindow(now.AddDate(0, 0, 9))
	start, _ := time.Parse("2006-01-02", later.Start)
	end, _ := time.Parse("2006-01-02", later.End)
	if got := end.Sub(start).Hours() / 24; got != 14 {
		t.Errorf("span = %v days, want 14", got)
	}
}

// Estimated is a claim about published figures, so it is only made when there
// are some. "estimated: false" beside nothing would describe data that does not
// exist.
func TestCollectResourcesOnlyFlagsEstimatedAlongsideFigures(t *testing.T) {
	opts := resOptions(rollupWith(model.NamedAmount{Name: ceCompute, Amount: "87.60"}))

	t.Run("no rows", func(t *testing.T) {
		f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
			ceCompute: {resPage()},
		}}
		rep, _ := CollectResources(context.Background(), f, nil, opts)
		if rep.Estimated != nil {
			t.Errorf("Estimated = %v beside no figures at all", *rep.Estimated)
		}
	})

	t.Run("rows AWS still calls estimated", func(t *testing.T) {
		resources := []model.Resource{res("api-gateway-1", "arn:aws:ec2:us-east-1:111111111111:instance/i-0abc")}
		f := &fakeResourceCE{pages: map[string][]*costexplorer.GetCostAndUsageWithResourcesOutput{
			ceCompute: {{ResultsByTime: []cetypes.ResultByTime{
				{Estimated: true, Groups: []cetypes.Group{resGroup("i-0abc", "87.60")}},
			}}},
		}}
		rep, failures := CollectResources(context.Background(), f, resources, opts)
		if rep.Estimated == nil || !*rep.Estimated {
			t.Fatalf("Estimated = %v, want true — AWS flagged the data", rep.Estimated)
		}
		if !hasKind(failures, "ce_res_estimated_data") {
			t.Errorf("estimated data is not in the ledger: %v", ledgerKinds(failures))
		}
		// Estimated on the report is AWS restating a bill; the figure is still
		// billed rather than modelled, which is what the flag on it means.
		if ce(t, resources[0]).Estimated {
			t.Error("a billed figure was marked modelled because AWS may restate it")
		}
	})
}
