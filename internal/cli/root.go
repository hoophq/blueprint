// Package cli wires the cobra command tree.
package cli

import "github.com/spf13/cobra"

// Version is stamped by goreleaser via -ldflags.
var Version = "dev"

// Root builds the blueprint command tree.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "blueprint",
		Short: "A census of what you run on AWS, and what it costs",
		Long: "blueprint takes a read-only census of the compute, storage, databases and\n" +
			"networking reachable from the AWS credentials you give it, and attaches what\n" +
			"AWS billed for them. Every figure is one AWS reported: no rate-card estimates,\n" +
			"no totals divided across resources. Runs locally; output stays local; zero\n" +
			"telemetry.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(scanCmd())
	return root
}
