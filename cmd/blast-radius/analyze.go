package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

func newAnalyzeCmd() *cobra.Command {
	var (
		output          string
		dbPath          string
		enrichDownloads bool
		top             int
		depth           int
		versions        string
		csvPath         string
		outputDir       string
		noSave          bool
	)

	cmd := &cobra.Command{
		Use:     "analyze <ecosystem> [package] [version]",
		Aliases: []string{"analyse"},
		Short:   "Find all packages affected by a compromised dependency",
		Long: `Compute the blast radius of one or more compromised package versions.

Finds all public packages that directly or transitively depend on the target
with a version range that COULD resolve to the specified version — even if
that version has been yanked from the registry.

Uses a local DuckDB snapshot of the deps.dev dependency graph for fast,
offline reverse dependency lookup with version range matching.

Runs fully offline by default. Pass --enrich-with-download-count to look up
weekly download counts from the registry, which ranks results by real-world
impact at the cost of several minutes and a lot of API calls.

Every run saves its results to output/<timestamp>/:
  blast-radius.json      full report, the input to 'blast-radius visualize'
  affected-packages.csv  package_name,vulnerable_versions
  paths.csv              one row per affected version, with its path

Examples:
  blast-radius analyze npm axios 1.14.1
  blast-radius analyze npm axios 1.14.1 --enrich-with-download-count
  blast-radius analyze npm axios --versions 1.14.1,0.30.1
  blast-radius analyze npm axios 1.14.1 --depth 2
  blast-radius analyze npm axios 1.14.1 --top 100 --output json
  blast-radius analyze npm --csv compromised.csv --depth 3

--csv format, one package per line:
  axios;1.14.1,0.30.4
  lodash;4.17.20,4.17.21

An affected-packages.csv from a previous run is also accepted, so one run's
results can be the next run's targets.`,
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			ecosystem, ok := blast.ParseEcosystem(args[0])
			if !ok {
				return fmt.Errorf("unsupported ecosystem %q (supported: %s)",
					args[0], strings.Join(blast.SupportedEcosystems(), ", "))
			}

			var targets []blast.TargetSpec
			if csvPath != "" {
				if len(args) > 1 {
					return fmt.Errorf("cannot combine --csv with positional package/version args")
				}
				if versions != "" {
					return fmt.Errorf("cannot combine --csv with --versions")
				}
				parsed, err := blast.ParseCompromisedCSV(csvPath, ecosystem)
				if err != nil {
					return fmt.Errorf("reading %s: %w", csvPath, err)
				}
				if len(parsed) == 0 {
					return fmt.Errorf("no compromised packages found in %s", csvPath)
				}
				targets = parsed
			} else {
				if len(args) < 2 {
					return fmt.Errorf("provide a package name (or use --csv)")
				}
				pkgName := args[1]
				var vers []string
				if versions != "" {
					for _, v := range strings.Split(versions, ",") {
						v = strings.TrimSpace(v)
						if v != "" {
							vers = append(vers, v)
						}
					}
				} else if len(args) == 3 {
					vers = []string{args[2]}
				} else {
					return fmt.Errorf("provide a version as 3rd argument or use --versions")
				}
				targets = []blast.TargetSpec{{System: ecosystem, Name: pkgName, Versions: vers}}
			}

			if noSave && outputDir != "" {
				return fmt.Errorf("cannot combine --no-save with --output-dir")
			}
			runDir := outputDir
			if runDir == "" && !noSave {
				runDir = filepath.Join("output", time.Now().Format("2006-01-02_150405"))
			}

			return blast.Run(cmd.Context(), blast.Options{
				System:          ecosystem,
				Targets:         targets,
				DBPath:          dbPath,
				Format:          output,
				EnrichDownloads: enrichDownloads,
				Top:             top,
				MaxDepth:        depth,
				OutputDir:       runDir,
				Stdout:          cmd.OutOrStdout(),
				Progress:        cmd.ErrOrStderr(),
			})
		},
	}

	cmd.Flags().StringVar(&output, "output", "table", "output format: table, json, csv")
	cmd.Flags().StringVar(&dbPath, "db", "", "path to DuckDB database (default: look for <ecosystem>-deps.duckdb in current dir, ./data/, or ~/.blast-radius/)")
	cmd.Flags().BoolVar(&enrichDownloads, "enrich-with-download-count", false, "look up weekly download counts from the registry and rank results by them (slow, requires network)")
	cmd.Flags().IntVar(&top, "top", 50, "number of results to display in table mode (0 = all)")
	cmd.Flags().IntVar(&depth, "depth", 1, "max dependency depth (1 = direct only, 2+ = transitive)")
	cmd.Flags().StringVar(&versions, "versions", "", "comma-separated list of compromised versions")
	cmd.Flags().StringVar(&csvPath, "csv", "", "path to a CSV of compromised packages ('package;v1,v2' per line, or an affected-packages.csv from a previous run)")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "where to save this run's artifacts (default: output/<timestamp>/)")
	cmd.Flags().BoolVar(&noSave, "no-save", false, "don't save artifacts to disk, only write to stdout")

	return cmd
}
