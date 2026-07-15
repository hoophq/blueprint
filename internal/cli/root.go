// Package cli wires the cobra command tree.
package cli

import "github.com/spf13/cobra"

// Version is stamped by goreleaser via -ldflags.
var Version = "dev"

// Root builds the blueprint command tree.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "blueprint",
		Short: "A census of every managed database reachable from your AWS credentials",
		Long: "blueprint takes a read-only census of the managed databases (RDS, Aurora,\n" +
			"DocumentDB, Neptune, DynamoDB, ElastiCache, Redshift) reachable from the\n" +
			"AWS credentials you give it. Runs locally; output stays local; zero telemetry.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(scanCmd())
	return root
}
