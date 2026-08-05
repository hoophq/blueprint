package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"

	"github.com/hoophq/blueprint/internal/awsx"
	"github.com/hoophq/blueprint/internal/cost"
	"github.com/hoophq/blueprint/internal/demo"
	"github.com/hoophq/blueprint/internal/diff"
	"github.com/hoophq/blueprint/internal/enrich"
	"github.com/hoophq/blueprint/internal/history"
	"github.com/hoophq/blueprint/internal/model"
	"github.com/hoophq/blueprint/internal/orgmode"
	"github.com/hoophq/blueprint/internal/render"
	"github.com/hoophq/blueprint/internal/scan"

	// Scanner implementations self-register via init().
	_ "github.com/hoophq/blueprint/internal/scanners"
)

func scanCmd() *cobra.Command {
	var (
		profile      string
		regions      []string
		org          bool
		roleName     string
		outDir       string
		formats      []string
		concurrency  int
		demoMode     bool
		demoScale    int
		noOpen       bool
		comparePath  string
		failOnChange bool
		noHistory    bool
		costs        costFlags
		metrics      bool
	)

	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Discover AWS resources and their cost, and write the census locally",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			// Validated before anything runs: a typo in --cost-metric should
			// not surface after a multi-minute scan, and certainly not after
			// the billed Cost Explorer calls it would have configured.
			if err := costs.validate(); err != nil {
				return err
			}
			// Same reasoning, and one case worth being blunt about: a
			// --demo-scale passed without --demo would otherwise look like it
			// had padded a real census, which is the one thing this tool must
			// never appear to do.
			if err := validateDemoScale(demoScale, demoMode); err != nil {
				return err
			}

			var snap *model.Snapshot
			if demoMode {
				// Zero is the storyboard, by SnapshotN's own contract, so the
				// unscaled path stays the curated fixture and stays instant.
				snap = demo.SnapshotN(Version, demoScale)
				if costs.enabled {
					// The fixture's meter honestly reports zero requests and
					// zero charge, because --demo makes no AWS calls at all.
					snap.Cost = demo.CostReport()
					demo.AddRecommendations(snap)
					if costs.resources {
						demo.AddResourceCostOverlay(snap)
					}
				}
				if metrics {
					demo.AddMetrics(snap)
				}
			} else {
				var err error
				snap, err = runScan(ctx, cmd, profile, regions, org, roleName, concurrency, costs, metrics)
				if err != nil {
					return err
				}
			}

			if err := writeOutputs(cmd, snap, outDir, formats, !noOpen && isTerminal(os.Stdout)); err != nil {
				return err
			}
			if comparePath != "" {
				// An explicit baseline wins over the automatic one; the scan
				// is still archived so the history stays continuous.
				err := compareAgainst(cmd, snap, comparePath, failOnChange)
				if !noHistory {
					saveHistory(cmd, snap)
				}
				return err
			}
			if noHistory {
				return nil
			}
			return autoDiff(cmd, snap, failOnChange)
		},
	}

	cmd.Flags().StringVar(&profile, "profile", "", "AWS shared config profile to use")
	cmd.Flags().StringSliceVar(&regions, "regions", nil, "regions to scan (default: all enabled regions)")
	cmd.Flags().BoolVar(&org, "org", false, "scan all AWS Organizations member accounts via assume-role")
	cmd.Flags().StringVar(&roleName, "role-name", "OrganizationAccountAccessRole", "role to assume in member accounts (with --org)")
	cmd.Flags().StringVarP(&outDir, "out", "o", ".", "directory for output files")
	cmd.Flags().StringSliceVar(&formats, "formats", []string{"html", "json"}, "outputs to write: html, json, csv")
	cmd.Flags().IntVar(&concurrency, "concurrency", 8, "max concurrent AWS API scan units")
	cmd.Flags().BoolVar(&demoMode, "demo", false, "render outputs from built-in fixture data (no AWS calls)")
	cmd.Flags().IntVar(&demoScale, "demo-scale", 0, "with --demo, grow the fixture to this many resources with deterministically generated extras (default: the curated fixture alone)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "do not open the HTML report in the browser after the scan")
	cmd.Flags().StringVar(&comparePath, "compare", "", "previous census JSON to diff against instead of the automatic history baseline")
	cmd.Flags().BoolVar(&failOnChange, "fail-on-change", false, "exit non-zero when the diff (auto or --compare) finds differences")
	cmd.Flags().BoolVar(&noHistory, "no-history", false, "do not archive this scan in local history or auto-diff against the previous one")
	cmd.Flags().BoolVar(&costs.enabled, "costs", false, "also report account spend for the last full month from AWS Cost Explorer, plus free per-resource savings suggestions from Cost Optimization Hub (AWS BILLS $0.01 per Cost Explorer request; off by default)")
	cmd.Flags().StringVar(&costs.metric, "cost-metric", cost.DefaultMetric, "Cost Explorer metric with --costs: "+strings.Join(cost.Metrics(), ", "))
	cmd.Flags().IntVar(&costs.maxRequests, "cost-max-requests", cost.DefaultMaxRequests, "hard cap on billed Cost Explorer requests per run")
	cmd.Flags().BoolVar(&costs.resources, "cost-resources", false, "with --costs, also ask Cost Explorer what it billed per resource over the last 14 days (AWS BILLS $0.01 per request — at least one per service probed, more if a service's answer paginates — from the same --cost-max-requests budget; requires the account-level \"resource-level data\" preference in the Cost Explorer console, which is not retroactive)")
	cmd.Flags().BoolVar(&metrics, "metrics", false, "also read CloudWatch utilization metrics for scanned resources (AWS BILLS $"+enrich.ChargeUSD(1000)+" per 1,000 metrics requested; off by default)")
	return cmd
}

// costFlags carries the --costs family through to the scan.
type costFlags struct {
	enabled     bool
	metric      string
	maxRequests int
	// resources turns on the per-resource Cost Explorer overlay. It is separate
	// from enabled, unlike the free Cost Optimization Hub stage that rides along
	// with it, because it spends money per service on data AWS's own docs cannot
	// agree that it will return. A user who asked what their account cost has
	// not thereby asked to pay for that.
	resources bool
}

// validate rejects bad cost flags before the scan starts. --cost-max-requests
// is checked even when --costs is off, so a scripted zero is caught at the
// flag rather than silently doing nothing on the day --costs is added.
func (c costFlags) validate() error {
	if !cost.ValidMetric(c.metric) {
		return fmt.Errorf("--cost-metric %q is not valid; choose one of: %s", c.metric, strings.Join(cost.Metrics(), ", "))
	}
	if c.maxRequests < 1 {
		return fmt.Errorf("--cost-max-requests must be at least 1, got %d", c.maxRequests)
	}
	// Rejected rather than quietly ignored: --cost-resources on its own reads
	// like a request for per-resource cost, and a run that answered it with
	// silence would look like an account where nothing has a cost.
	if c.resources && !c.enabled {
		return fmt.Errorf("--cost-resources needs --costs: the per-resource pass takes its service list from the account rollup")
	}
	return nil
}

// validateDemoScale rejects a --demo-scale that cannot mean what it says.
//
// Outside --demo it is refused rather than ignored. The flag generates
// resources, and the one guarantee this tool makes about a real scan is that
// every row in it came from an AWS API response; a flag that looks like it
// could add rows to that has to fail loudly the moment it is combined with
// one, not quietly do nothing and leave the reader to wonder which run they
// are holding.
func validateDemoScale(n int, demoMode bool) error {
	if n == 0 {
		return nil
	}
	if !demoMode {
		return fmt.Errorf("--demo-scale %d needs --demo: it generates fixture resources, "+
			"and a real scan reports only what AWS returned", n)
	}
	if n < 0 {
		return fmt.Errorf("--demo-scale must be a resource count of at least 1, got %d", n)
	}
	// The generator clamps here too, so this is not what protects it. It is
	// what turns the clamp into a sentence: a person who typed a number is owed
	// the news that they are not getting it, rather than a report that quietly
	// holds half a million rows and never says why it stopped there.
	if n > demo.MaxScale {
		return fmt.Errorf("--demo-scale is capped at %d, got %d: past that the single-file "+
			"report stops being a file anyone can open", demo.MaxScale, n)
	}
	return nil
}

func runScan(ctx context.Context, cmd *cobra.Command, profile string, regions []string, org bool, roleName string, concurrency int, costs costFlags, metrics bool) (*model.Snapshot, error) {
	cfg, err := awsx.Load(ctx, profile)
	if err != nil {
		return nil, err
	}
	account, err := awsx.CallerAccount(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("verifying credentials: %w", err)
	}
	// pflag's CSV parsing turns "us-east-1," into ["us-east-1",""]; empty
	// tokens would become scan units with an empty region and spam the
	// failure ledger, so trim and drop them up front.
	regions = cleanRegions(regions)
	if cmd.Flags().Changed("regions") && len(regions) == 0 {
		return nil, errors.New("--regions was set but contains no region names (empty entries are dropped)")
	}
	// explicitRegions is non-empty only when the user passed --regions; org
	// mode then applies it verbatim to every member account instead of each
	// account's own enabled-region list.
	explicitRegions := regions
	if len(regions) == 0 {
		regions, err = awsx.EnabledRegions(ctx, cfg)
		if err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "blueprint %s — account %s, %d region(s), read-only scan\n",
		Version, account, len(regions))

	var (
		targets     []scan.Target
		preFailures []model.Failure
	)
	if org {
		targets, preFailures, err = orgmode.Targets(ctx, cfg, account, roleName, regions, explicitRegions)
		if err != nil {
			return nil, err
		}
	} else {
		targets = []scan.Target{{AccountID: account, Cfg: cfg, Regions: regions}}
	}

	runner := &scan.Runner{
		Scanners:    scan.All(),
		Concurrency: concurrency,
		OnUnit: func(accountID, region, service string, found int, err error) {
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "  ! %s/%s/%s: %v\n", accountID, region, service, err)
			} else if found > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  ✓ %s/%s/%s: %d\n", accountID, region, service, found)
			}
		},
		OnEnrich: func(name string, failed int) {
			if failed > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "  ! %s: %d coverage gap(s), see the failure ledger\n", name, failed)
			}
		},
	}
	var metricsStage *enrich.Metrics
	if metrics {
		// Enrichment runs inside Run, after the scan and before Finalize, so
		// the exact number of series cannot be known here — only the rate.
		// The count follows below, once it has been spent.
		fmt.Fprintf(cmd.OutOrStdout(), "  … metrics: reading CloudWatch for scanned resources — AWS bills $%s per 1,000 metrics requested\n",
			enrich.ChargeUSD(1000))
		metricsStage = enrich.NewMetrics()
		runner.Enrichers = append(runner.Enrichers, metricsStage)
	}
	var costHubStage *enrich.CostHub
	if costs.enabled {
		// Savings advice rides on --costs rather than a flag of its own. It
		// answers a different question from Cost Explorer — what you could
		// stop paying, not what you paid — but it is the question a reader
		// asks immediately after seeing the bill, and a suggestion is only
		// worth acting on next to the spend it would reduce.
		// Unlike Cost Explorer it is free, which is worth saying out loud next
		// to a flag whose help text warns about being billed.
		fmt.Fprintln(cmd.OutOrStdout(), "  … cost-hub: reading savings suggestions from Cost Optimization Hub — not billed by AWS")
		// Constructed with the caller's own credentials, like the Cost
		// Explorer phase and for the same reason: the hub is one org-wide
		// endpoint in the payer account, so there is no member role to assume
		// and nothing to fan out over.
		costHubStage = enrich.NewCostHub(cfg, account)
		runner.Enrichers = append(runner.Enrichers, costHubStage)
	}
	snap := runner.Run(ctx, targets, Version)
	if metricsStage != nil {
		meter := metricsStage.Meter()
		fmt.Fprintf(cmd.OutOrStdout(), "  ✓ metrics: %d series requested in %d call(s), ~$%s charged by AWS\n",
			meter.Series, meter.GetCalls, enrich.ChargeUSD(meter.Series))
	}
	if costHubStage != nil {
		// Tipped is reported against Recommendations, not on its own: the two
		// together are the coverage figure. Cost Optimization Hub does not
		// model every resource type, so a gap is expected — but "2,000 read,
		// 0 attached" is an ARN mismatch, and only the pair shows it. The
		// unattached count is printed only when there is one, because on a
		// healthy run it is zero and a zero here reads as a warning.
		meter := costHubStage.Meter()
		fmt.Fprintf(cmd.OutOrStdout(), "  ✓ cost-hub: %d resource(s) with suggestions from %d recommendation(s) in %d call(s), no charge\n",
			meter.Tipped, meter.Recommendations, meter.Requests)
		if meter.Unattached > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "    %d recommendation(s) named a resource this census does not hold\n", meter.Unattached)
		}
	}
	// Org-mode pre-scan failures (unassumable member roles) belong in the
	// same ledger as per-unit scan failures; re-sort so the artifact stays
	// deterministic after the append.
	snap.Failures = append(snap.Failures, preFailures...)
	if costs.enabled {
		// Cost runs after the scan because it reports on the accounts the
		// census actually covered, and it uses the caller's own credentials
		// even in org mode: billing for the whole organization lives in the
		// payer account, so there is nothing to assume a role for.
		snap.Failures = append(snap.Failures, collectCosts(ctx, cmd, cfg, account, snap, costs)...)
	}
	snap.SortFailures()
	return snap, nil
}

// collectCosts runs the Cost Explorer phases and attaches their reports to snap.
//
// It prints what it is about to spend before spending it and what it actually
// spent afterwards. The flag is opt-in, but "opt-in" is not a licence to spend
// the user's money quietly.
//
// Both paid passes share one budget, created here, and the order they are
// handed it is the policy — see cost.Budget. --cost-max-requests is a ceiling
// on the run, not on each pass, so the printed estimate is the run's worst case
// whether one pass spends it or two.
func collectCosts(ctx context.Context, cmd *cobra.Command, cfg aws.Config, account string, snap *model.Snapshot, flags costFlags) []model.Failure {
	window := cost.LastFullMonth(time.Now())
	fmt.Fprintf(cmd.OutOrStdout(), "  … cost: querying Cost Explorer for %s across %d account(s) — AWS bills $0.01 per request, up to %s\n",
		window.Label, len(snap.Accounts), cost.ChargeUSD(flags.maxRequests))

	budget := cost.NewBudget(flags.maxRequests)
	client := cost.Client(cfg)
	report, failures := cost.Collect(ctx, client, cost.Options{
		Accounts:      snap.Accounts,
		CallerAccount: account,
		Metric:        flags.metric,
		Window:        window,
		Budget:        budget,
	})
	snap.Cost = report
	if report != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "  ✓ cost: %d Cost Explorer request(s), ~$%s charged by AWS\n",
			report.Meter.Requests, report.Meter.EstimatedChargeUSD)
	}
	if flags.resources {
		failures = append(failures, collectResourceCosts(ctx, cmd, client, account, snap, flags, budget)...)
	}
	for _, f := range failures {
		fmt.Fprintf(cmd.ErrOrStderr(), "  ! %s/%s: %s\n", f.AccountID, f.Service, f.Error)
	}
	return failures
}

// collectResourceCosts runs the per-resource overlay against whatever is left of
// the run's budget.
//
// It runs even when the rollup published nothing: CollectResources decides for
// itself whether it has what it needs, and says so in the ledger rather than
// being skipped silently here. The one thing this function will not do is spend
// on a budget the rollup already exhausted — that is reported as a skip against
// every service, which is the honest shape of "never asked".
func collectResourceCosts(ctx context.Context, cmd *cobra.Command, api cost.ResourceAPI, account string, snap *model.Snapshot, flags costFlags, budget *cost.Budget) []model.Failure {
	window := cost.ResourceWindow(time.Now())
	// "At least one" rather than "one": fetchResources takes from the budget per
	// page, so a service whose answer paginates costs more than the service count
	// suggests. Stating the request as the billed unit is also what makes the
	// remaining budget below readable — that number counts requests, not services.
	fmt.Fprintf(cmd.OutOrStdout(), "  … cost-resources: asking Cost Explorer what it billed per resource over %s — at least one billed request per service, more if a service's answer paginates, %d left in this run's budget\n",
		window.Label, budget.Remaining())

	report, failures := cost.CollectResources(ctx, api, snap.Resources, cost.ResourceOptions{
		Accounts:      snap.Accounts,
		CallerAccount: account,
		Metric:        flags.metric,
		Window:        window,
		Budget:        budget,
		Rollup:        snap.Cost,
	})
	snap.ResourceCost = report
	if report != nil {
		// Probed and priced are printed together for the same reason the hub
		// stage prints its pair: either number alone is unreadable. "8 services
		// probed, 0 resources priced" is the answer this whole pass exists to
		// surface, and it is invisible if only one of the two is shown.
		fmt.Fprintf(cmd.OutOrStdout(), "  ✓ cost-resources: %d service(s) probed, %d resource(s) priced, %d request(s), ~$%s charged by AWS\n",
			probed(report), pricedBy(snap.Resources, model.CostMethodCE),
			report.Meter.Requests, report.Meter.EstimatedChargeUSD)
	}
	return failures
}

// probed counts the services actually asked, as opposed to the services
// considered — the report lists both, and only the first cost anything.
func probed(report *model.ResourceCostReport) int {
	n := 0
	for _, p := range report.Probes {
		switch p.Outcome {
		case model.ProbeSkipped, model.ProbeUncensused:
		default:
			n++
		}
	}
	return n
}

// pricedBy counts resources carrying a figure from one method.
func pricedBy(resources []model.Resource, method string) int {
	n := 0
	for i := range resources {
		if resources[i].CostBy(method) != nil {
			n++
		}
	}
	return n
}

// compareAgainst diffs the fresh snapshot against a previous census JSON and
// prints the changes. With failOnChange, any difference becomes an error so
// scripts can gate on the exit code.
func compareAgainst(cmd *cobra.Command, snap *model.Snapshot, path string, failOnChange bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading --compare file: %w", err)
	}
	var prev model.Snapshot
	if err := json.Unmarshal(data, &prev); err != nil {
		return fmt.Errorf("parsing --compare file %s (expected a blueprint census JSON): %w", path, err)
	}
	// Refuse cross-schema diffs instead of fabricating drift: field
	// representations differ between schemas (e.g. an omitted bool vs an
	// explicit pointer), which would surface as changes on every resource.
	if prev.Schema != snap.Schema {
		return fmt.Errorf("%s was written with census schema %d and this blueprint writes schema %d — diffing across schemas would report format changes as resource drift; re-scan with this version to create a comparable baseline",
			filepath.Base(path), prev.Schema, snap.Schema)
	}
	d := diff.Compare(&prev, snap)
	d.Write(cmd.OutOrStdout(), filepath.Base(path))
	if failOnChange && !d.Empty() {
		return fmt.Errorf("differences vs %s: %d new, %d removed, %d changed",
			filepath.Base(path), len(d.Added), len(d.Removed), len(d.Changed))
	}
	return nil
}

// autoDiff archives the scan in local history and diffs it against the
// previous census of the same scope (accounts + regions), so "what changed
// since last time" is part of every scan with zero user effort. History
// failures degrade to warnings: an unwritable home directory must never fail
// a successful scan.
func autoDiff(cmd *cobra.Command, snap *model.Snapshot, failOnChange bool) error {
	root, err := history.Dir()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "  ! history disabled: %v\n", err)
		return nil
	}
	prev, err := history.Latest(root, history.ScopeKey(snap))
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "  ! reading history baseline: %v\n", err)
	}
	saveHistory(cmd, snap)
	if prev == nil {
		fmt.Fprintf(cmd.OutOrStdout(),
			"\n  history: first census for this scope — the next scan will show what changed (%s)\n", root)
		return nil
	}
	// Same refusal as --compare: a cross-schema baseline (written by an older
	// blueprint) would fabricate a mass-change event. The fresh scan is
	// already archived, so the next run diffs normally.
	if prev.Schema != snap.Schema {
		fmt.Fprintf(cmd.OutOrStdout(),
			"\n  history: baseline was written with census schema %d (this blueprint writes %d) — skipping the diff; the next scan will diff against this one\n",
			prev.Schema, snap.Schema)
		return nil
	}
	d := diff.Compare(prev, snap)
	d.Write(cmd.OutOrStdout(), "last scan ("+sinceLabel(prev.GeneratedAt)+")")
	if failOnChange && !d.Empty() {
		return fmt.Errorf("differences vs last scan: %d new, %d removed, %d changed",
			len(d.Added), len(d.Removed), len(d.Changed))
	}
	return nil
}

// saveHistory archives the snapshot, downgrading failures to a warning.
func saveHistory(cmd *cobra.Command, snap *model.Snapshot) {
	root, err := history.Dir()
	if err == nil {
		_, err = history.Save(root, snap)
	}
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "  ! saving scan to history: %v\n", err)
	}
}

// sinceLabel renders a baseline timestamp as "Jun 12, 2026 · 33 days ago".
func sinceLabel(t time.Time) string {
	label := t.Local().Format("Jan 2, 2006")
	switch days := int(time.Since(t).Hours() / 24); {
	case days <= 0:
		return label + " · today"
	case days == 1:
		return label + " · yesterday"
	default:
		return fmt.Sprintf("%s · %d days ago", label, days)
	}
}

// cleanRegions trims whitespace and drops empty tokens from a --regions list.
func cleanRegions(in []string) []string {
	out := make([]string, 0, len(in))
	for _, r := range in {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func writeOutputs(cmd *cobra.Command, snap *model.Snapshot, outDir string, formats []string, openHTML bool) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	// GeneratedAt is UTC; stamp filenames in local time so an evening scan
	// does not get tomorrow's date.
	stamp := snap.GeneratedAt.Local().Format("2006-01-02")
	written := []string{}
	htmlPath := ""
	var errs []error
	for _, f := range formats {
		var (
			path string
			err  error
		)
		switch strings.ToLower(strings.TrimSpace(f)) {
		case "json":
			path = filepath.Join(outDir, "blueprint-"+stamp+".json")
			err = render.JSON(snap, path)
		case "csv":
			path = filepath.Join(outDir, "blueprint-"+stamp+".csv")
			err = render.CSV(snap, path)
		case "html":
			path = filepath.Join(outDir, "blueprint-"+stamp+".html")
			err = render.HTML(snap, path)
		default:
			err = fmt.Errorf("unknown format %q", f)
		}
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "  ! %s output: %v\n", f, err)
			errs = append(errs, fmt.Errorf("%s output: %w", f, err))
			continue
		}
		written = append(written, path)
		if strings.EqualFold(strings.TrimSpace(f), "html") {
			htmlPath = path
		}
	}

	// The terminal summary always renders, even when some outputs failed;
	// the joined error still forces a non-zero exit.
	render.Terminal(cmd.OutOrStdout(), snap, written)
	if openHTML && htmlPath != "" {
		openBrowser(htmlPath)
	}
	return errors.Join(errs...)
}
