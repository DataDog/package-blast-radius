package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// errQuiet exits non-zero without printing more, for commands that already
// explained themselves (a declined confirmation).
var errQuiet = errors.New("")

func main() {
	root := &cobra.Command{
		Use:   "blast-radius",
		Short: "Find all packages affected by a compromised dependency",
		Long: `Compute and explore the blast radius of compromised package versions.

  download-data   export a dependency graph from BigQuery and build the local database
  analyze         compute the blast radius of one or more compromised versions
  visualize       browse a previously generated JSON report in a local web UI

Examples:
  blast-radius download-data npm --project my-gcp-project
  blast-radius analyze npm axios 1.14.1
  blast-radius analyze npm axios 1.14.1 --depth 2 --output json > results.json
  blast-radius visualize results.json`,
		SilenceUsage: true,
		// Errors are printed here so errQuiet can be suppressed.
		SilenceErrors: true,
	}

	root.AddCommand(newDownloadDataCmd(), newAnalyzeCmd(), newVisualizeCmd())

	if err := root.Execute(); err != nil {
		if !errors.Is(err, errQuiet) {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
		os.Exit(1)
	}
}
