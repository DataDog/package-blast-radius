package main

import (
	"github.com/spf13/cobra"

	"github.com/DataDog/package-blast-radius/internal/viewer"
)

func newVisualizeCmd() *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:     "visualize <blast-radius-output.json>",
		Aliases: []string{"visualise", "view", "viewer"},
		Short:   "Browse a blast radius JSON report in a local web UI",
		Long: `Serve a local web UI for exploring a blast radius report.

Provides filtering, sorting, pagination, CSV export, and a visual dependency
path graph for each affected package. The report is loaded and deduplicated on
startup (handles files up to ~1 GB), then served through a paginated API.

Example:
  blast-radius analyze npm axios 1.14.1 --depth 2 --output json > results.json
  blast-radius visualize results.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return viewer.Serve(args[0], port)
		},
	}

	cmd.Flags().IntVar(&port, "port", 8080, "port to serve on")

	return cmd
}
