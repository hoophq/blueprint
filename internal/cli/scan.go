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

	"github.com/spf13/cobra"

	"github.com/hoophq/blueprint/internal/awsx"
	"github.com/hoophq/blueprint/internal/demo"
	"github.com/hoophq/blueprint/internal/diff"
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
		noOpen       bool
		comparePath  string
		failOnChange bool
		noHistory    bool
	)

	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Discover managed databases and write the census locally",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			var snap *model.Snapshot
			if demoMode {
				snap = demo.Snapshot(Version)
			} else {
				var err error
				snap, err = runScan(ctx, cmd, profile, regions, org, roleName, concurrency)
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
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "do not open the HTML report in the browser after the scan")
	cmd.Flags().StringVar(&comparePath, "compare", "", "previous census JSON to diff against instead of the automatic history baseline")
	cmd.Flags().BoolVar(&failOnChange, "fail-on-change", false, "exit non-zero when the diff (auto or --compare) finds differences")
	cmd.Flags().BoolVar(&noHistory, "no-history", false, "do not archive this scan in local history or auto-diff against the previous one")
	return cmd
}

func runScan(ctx context.Context, cmd *cobra.Command, profile string, regions []string, org bool, roleName string, concurrency int) (*model.Snapshot, error) {
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
	}
	snap := runner.Run(ctx, targets, Version)
	// Org-mode pre-scan failures (unassumable member roles) belong in the
	// same ledger as per-unit scan failures.
	snap.Failures = append(snap.Failures, preFailures...)
	return snap, nil
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
