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
			"networking reachable from the AWS credentials you give it. Pass --costs and it\n" +
			"attaches two answers, kept apart: what AWS billed, from Cost Explorer, and what\n" +
			"you could stop paying, from Cost Optimization Hub's savings suggestions. Every\n" +
			"figure is one AWS reported: no rate-card estimates, no totals divided across\n" +
			"resources, and no suggestion netted off a bill. Runs locally; output stays\n" +
			"local; zero telemetry.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(scanCmd())
	return root
}
