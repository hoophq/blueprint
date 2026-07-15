package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hoophq/dbcensus/internal/awsx"
	"github.com/hoophq/dbcensus/internal/demo"
	"github.com/hoophq/dbcensus/internal/model"
	"github.com/hoophq/dbcensus/internal/orgmode"
	"github.com/hoophq/dbcensus/internal/render"
	"github.com/hoophq/dbcensus/internal/scan"

	// Scanner implementations self-register via init().
	_ "github.com/hoophq/dbcensus/internal/scanners"
)

func scanCmd() *cobra.Command {
	var (
		profile     string
		regions     []string
		org         bool
		roleName    string
		outDir      string
		formats     []string
		concurrency int
		demoMode    bool
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

			return writeOutputs(cmd, snap, outDir, formats)
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
	fmt.Fprintf(cmd.OutOrStdout(), "dbcensus %s — account %s, %d region(s), read-only scan\n",
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

func writeOutputs(cmd *cobra.Command, snap *model.Snapshot, outDir string, formats []string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	// GeneratedAt is UTC; stamp filenames in local time so an evening scan
	// does not get tomorrow's date.
	stamp := snap.GeneratedAt.Local().Format("2006-01-02")
	written := []string{}
	var errs []error
	for _, f := range formats {
		var (
			path string
			err  error
		)
		switch strings.ToLower(strings.TrimSpace(f)) {
		case "json":
			path = filepath.Join(outDir, "dbcensus-"+stamp+".json")
			err = render.JSON(snap, path)
		case "csv":
			path = filepath.Join(outDir, "dbcensus-"+stamp+".csv")
			err = render.CSV(snap, path)
		case "html":
			path = filepath.Join(outDir, "dbcensus-"+stamp+".html")
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
	}

	// The terminal summary always renders, even when some outputs failed;
	// the joined error still forces a non-zero exit.
	render.Terminal(cmd.OutOrStdout(), snap, written)
	return errors.Join(errs...)
}
