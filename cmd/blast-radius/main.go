package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "blast-radius",
		Short: "Find all packages affected by a compromised dependency",
		Long: `Compute and explore the blast radius of compromised package versions.

  analyze     compute the blast radius of one or more compromised versions
  visualize   browse a previously generated JSON report in a local web UI

Examples:
  blast-radius analyze npm axios 1.14.1
  blast-radius analyze npm axios 1.14.1 --depth 2 --output json > results.json
  blast-radius visualize results.json`,
		SilenceUsage: true,
	}

	root.AddCommand(newAnalyzeCmd(), newVisualizeCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
