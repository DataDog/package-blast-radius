package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/DataDog/package-blast-radius/internal/blast"
	"github.com/DataDog/package-blast-radius/internal/dataset"
)

func newDownloadDataCmd() *cobra.Command {
	var opts dataset.Options

	cmd := &cobra.Command{
		Use:     "download-data <ecosystem>",
		Aliases: []string{"fetch-data"},
		Short:   "Export a dependency graph from BigQuery and build the local database",
		Long: `Export an ecosystem's dependency graph from the deps.dev BigQuery public
dataset and build the indexed DuckDB database that 'analyze' queries.

Runs the whole pipeline: price the query, run it into a temporary BigQuery
table, extract that table to GCS as parquet, download the shards, and build
data/<ecosystem>-deps.duckdb from them, unless --db names another path.

For PyPI, pass --include-download-counts to add package-level weekly download
counts from bigquery-public-data.pypi.file_downloads to the local database.

Every billed query is priced with a free dry run and confirmed before it runs,
and creating a bucket or deleting objects is confirmed too. Nothing costs money
or changes cloud state without an explicit yes. Pass --yes to skip the prompts
in a script.

Requires the duckdb CLI, and GCP credentials from 'gcloud auth login --update-adc'
or from GOOGLE_APPLICATION_CREDENTIALS pointing at a service account key.

Examples:
  blast-radius download-data npm --project my-gcp-project
  blast-radius download-data pypi --project my-gcp-project --include-download-counts
  blast-radius download-data npm --project my-gcp-project --snapshot-date 2026-03-23
  blast-radius download-data npm --project my-gcp-project --bucket gs://my-bucket -y
  blast-radius download-data npm --project my-gcp-project --db /tmp/npm-deps.duckdb

  # Rebuild the database from shards already on disk, with no cloud calls
  blast-radius download-data pypi --build-only --parquet-dir data/parquet --db /tmp/pypi-deps.duckdb`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ecosystem, ok := blast.ParseEcosystem(args[0])
			if !ok {
				return fmt.Errorf("unsupported ecosystem %q (supported: %s)",
					args[0], strings.Join(blast.SupportedEcosystems(), ", "))
			}
			if opts.BuildOnly && opts.SkipBuild {
				return errors.New("cannot combine --build-only with --skip-build")
			}

			opts.System = ecosystem
			opts.Stdin = os.Stdin
			opts.Progress = cmd.ErrOrStderr()

			err := dataset.Download(cmd.Context(), opts)
			// A declined prompt is a clean stop, so it should not print like a crash.
			if errors.Is(err, dataset.ErrAborted) {
				fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
				return errQuiet
			}
			return err
		},
	}

	cmd.Flags().StringVar(&opts.ProjectID, "project", "", "GCP project to bill and run the query in (required unless --build-only)")
	cmd.Flags().StringVar(&opts.Bucket, "bucket", "", "GCS bucket for the export (default: gs://<project>-blast-radius, created if missing)")
	cmd.Flags().StringVar(&opts.SnapshotDate, "snapshot-date", "", "deps.dev snapshot as YYYY-MM-DD (default: discover the latest)")
	cmd.Flags().StringVar(&opts.DataDir, "data-dir", "data", "directory holding parquet/ and the built database")
	cmd.Flags().StringVar(&opts.DBPath, "db", "", "where to write the DuckDB database (default: <data-dir>/<ecosystem>-deps.duckdb)")
	cmd.Flags().StringVar(&opts.ParquetDir, "parquet-dir", "", "where to read or write parquet shards (default: <data-dir>/parquet/<snapshot-date>/)")
	cmd.Flags().StringVar(&opts.DatasetID, "bq-dataset", "blast_radius", "BigQuery dataset holding the intermediate table")
	cmd.Flags().BoolVarP(&opts.AutoApprove, "yes", "y", false, "skip the cost and creation confirmations")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "delete a previous export of the same snapshot date first")
	cmd.Flags().BoolVar(&opts.KeepParquet, "keep-parquet", false, "keep the parquet shards after building the database")
	cmd.Flags().BoolVar(&opts.BuildOnly, "build-only", false, "build the database from local parquet shards, making no cloud calls")
	cmd.Flags().BoolVar(&opts.SkipBuild, "skip-build", false, "download the shards but do not build the database")
	cmd.Flags().BoolVar(&opts.IncludeDownloadCounts, "include-download-counts", false, "for PyPI, include weekly download counts in the built database")

	return cmd
}
