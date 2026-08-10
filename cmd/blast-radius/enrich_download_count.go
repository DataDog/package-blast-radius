package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

const downloadCountCacheArtifact = "download-count-cache.jsonl"

func newEnrichDownloadCountCmd() *cobra.Command {
	var (
		rate          float64
		workers       int
		npmToken      string
		force         bool
		includeScoped bool
	)

	cmd := &cobra.Command{
		Use:   "enrich-download-count <output-folder>",
		Short: "Add npm weekly download counts to a saved blast-radius report",
		Long: `Fetch weekly download counts from the npm registry and write them back
into a report a previous 'blast-radius analyze' run saved.

Takes the output folder analyze created (the one containing blast-radius.json,
affected-packages.csv, and paths.csv) and enriches all three in place: the JSON
report and paths.csv gain real weekly_downloads values, and affected-packages.csv
is rewritten for consistency. The result is byte-for-byte what
'analyze --enrich-with-download-count' would have produced.

The npm downloads API rate-limits per IP behind Cloudflare, so requests are
paced adaptively: the pace self-tunes down on 429 and back up on success
(--rate is just the starting ceiling), so you don't have to guess the limit.
Set NPM_TOKEN (or pass --npm-token) to lift the unauthenticated rate limit;
without it the run is best-effort and much slower.

By default only packages still missing a download count (weekly_downloads = -1)
are fetched, so re-running completes a partial/failed enrichment. Pass --force
to re-fetch every package and refresh stale counts. npm cannot bulk fetch scoped
package counts, so large scoped tails are skipped unless
--include-scoped-packages is set.

Note: the report is loaded fully into memory. A large run (millions of affected
versions) needs several GB of RAM and a few minutes to rewrite.

Examples:
  NPM_TOKEN=$(cat ~/.npm-token) blast-radius enrich-download-count output/2026-08-10_095042
  blast-radius enrich-download-count output/2026-08-10_095042 --npm-token $TOKEN --rate 1
  blast-radius enrich-download-count output/2026-08-10_095042 --include-scoped-packages
  blast-radius enrich-download-count output/2026-08-10_095042 --force`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			folder := args[0]
			progress := cmd.ErrOrStderr()

			// Token: flag wins, then env. Warn (but continue) if neither is set.
			token := strings.TrimSpace(npmToken)
			if token == "" {
				token = strings.TrimSpace(os.Getenv("NPM_TOKEN"))
			}
			blast.SetNPMAuthToken(token)
			if token == "" {
				fmt.Fprintln(progress, "warning: NPM_TOKEN is not set; the npm downloads API will heavily rate-limit unauthenticated requests. Set NPM_TOKEN (or pass --npm-token) to lift it. Continuing best-effort.")
			}

			jsonPath := filepath.Join(folder, "blast-radius.json")
			if _, err := os.Stat(jsonPath); err != nil {
				return fmt.Errorf("looking for %s: %w\nDid you point this at an 'analyze' output folder?", jsonPath, err)
			}

			fmt.Fprintf(progress, "Loading %s\n", jsonPath)
			start := time.Now()
			result, err := loadJSONResult(jsonPath)
			if err != nil {
				return fmt.Errorf("loading report: %w", err)
			}
			fmt.Fprintf(progress, "Loaded: %d affected entries, %d targets (in %s)\n",
				len(result.Affected), len(result.Targets), time.Since(start).Round(time.Millisecond))

			// Reconstruct the in-memory result the enricher and artifact writer use.
			br, err := blast.BlastResultFromJSON(result)
			if err != nil {
				return fmt.Errorf("reconstructing result: %w", err)
			}

			cachePath := filepath.Join(folder, downloadCountCacheArtifact)
			cache, err := loadDownloadCountCache(cachePath)
			if err != nil {
				return fmt.Errorf("loading %s: %w", cachePath, err)
			}

			// Decide which names to fetch: only unenriched ones, unless --force.
			// A cache hit means a previous enrichment attempt already finished
			// that registry lookup, even if npm had no download data for it.
			need := make(map[string]struct{})
			alreadyEnriched := 0
			cachedCounts := 0
			cachedMissing := 0
			pendingScoped := 0
			skippedScoped := 0
			seenNames := make(map[string]struct{})
			for _, a := range br.Affected {
				if _, ok := seenNames[a.Name]; ok {
					continue
				}
				seenNames[a.Name] = struct{}{}
				if !force && a.WeeklyDownloads >= 0 {
					alreadyEnriched++
					continue
				}
				if !force && a.WeeklyDownloads < 0 {
					if c, ok := cache[a.Name]; ok {
						if c != nil {
							cachedCounts++
						} else {
							cachedMissing++
						}
						continue
					}
				}
				if force || a.WeeklyDownloads < 0 {
					if strings.HasPrefix(a.Name, "@") {
						pendingScoped++
					}
					need[a.Name] = struct{}{}
				}
			}
			if pendingScoped > blast.NPMScopedPackageDefaultThreshold && !includeScoped {
				for name := range need {
					if strings.HasPrefix(name, "@") {
						delete(need, name)
						skippedScoped++
					}
				}
			}
			if !force {
				for i := range br.Affected {
					if br.Affected[i].WeeklyDownloads >= 0 {
						continue
					}
					if c, ok := cache[br.Affected[i].Name]; ok && c != nil {
						br.Affected[i].WeeklyDownloads = *c
					}
				}
			}
			if len(need) == 0 {
				if alreadyEnriched > 0 {
					fmt.Fprintf(progress, "Report already has download counts for %d unique package%s.\n",
						alreadyEnriched, pluralS(alreadyEnriched))
				}
				if skippedScoped > 0 {
					fmt.Fprintf(progress, "Nothing else to fetch: skipped %d scoped package%s because npm rejects scoped names in bulk download lookups. Pass --include-scoped-packages to fetch them one-by-one.\n",
						skippedScoped, pluralS(skippedScoped))
					if cachedCounts == 0 {
						return nil
					}
				}
				if cachedCounts > 0 {
					fmt.Fprintf(progress, "No registry fetches needed; applying %d cached download count%s and rewriting artifacts.\n", cachedCounts, pluralS(cachedCounts))
					if err := blast.SaveArtifacts(br, folder, progress); err != nil {
						return fmt.Errorf("writing artifacts: %w", err)
					}
					fmt.Fprintf(progress, "Done. Enriched %s in %s.\n", folder, time.Since(start).Round(time.Second))
					return nil
				}
				fmt.Fprintf(progress, "Nothing to do: all %d packages already have download counts or cached no-data results (use --force to refresh).\n", countUniqueNames(br.Affected))
				return nil
			}

			// One AffectedPackage per name to fetch; the enricher dedups by name
			// and we apply the resolved counts back onto the full result.
			toFetch := make([]blast.AffectedPackage, 0, len(need))
			for name := range need {
				toFetch = append(toFetch, blast.AffectedPackage{
					PackageVersion:  blast.PackageVersion{System: blast.NPM, Name: name},
					WeeklyDownloads: -1,
				})
			}
			if alreadyEnriched > 0 {
				fmt.Fprintf(progress, "Report already has download counts for %d unique package%s.\n",
					alreadyEnriched, pluralS(alreadyEnriched))
			}
			if cachedCounts+cachedMissing > 0 {
				fmt.Fprintf(progress, "Applied %d cached download count%s and reused %d cached no-data result%s from %s.\n",
					cachedCounts, pluralS(cachedCounts), cachedMissing, pluralS(cachedMissing), cachePath)
			}
			if skippedScoped > 0 {
				fmt.Fprintf(progress, "Skipping %d scoped package%s: npm rejects scoped names in bulk download lookups. Pass --include-scoped-packages to fetch them one-by-one.\n",
					skippedScoped, pluralS(skippedScoped))
			}
			if force {
				fmt.Fprintf(progress, "Refreshing download counts for %d unique package%s (rate %.1f req/s, %d workers)...\n",
					len(toFetch), pluralS(len(toFetch)), rate, workers)
			} else {
				covered := alreadyEnriched + cachedCounts + cachedMissing + skippedScoped
				fmt.Fprintf(progress, "Fetching download counts for %d remaining package%s (%d/%d already handled, rate %.1f req/s, %d workers)...\n",
					len(toFetch), pluralS(len(toFetch)), covered, len(seenNames), rate, workers)
			}

			cacheFile, err := os.OpenFile(cachePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return fmt.Errorf("opening %s: %w", cachePath, err)
			}
			defer cacheFile.Close()
			cacheWriter := newDownloadCountCacheWriter(cacheFile)
			var fetchedHandled atomic.Int64

			baseCtx := cmd.Context()
			if baseCtx == nil {
				baseCtx = context.Background()
			}
			ctx, stopSignals := signal.NotifyContext(baseCtx, os.Interrupt, syscall.SIGTERM)
			defer stopSignals()
			interrupted := false
			if err := blast.Enrich(ctx, blast.NPM, toFetch, blast.EnrichOptions{
				Workers:                workers,
				Rate:                   rate,
				Progress:               progress,
				IncludeScopedPackages:  includeScoped,
				ScopedPackageOptInFlag: "--include-scoped-packages",
				OnPackageResolved: func(name string, downloads *int64) {
					fetchedHandled.Add(1)
					if err := cacheWriter.Write(name, downloads); err != nil {
						fmt.Fprintf(progress, "warning: could not checkpoint download count for %s: %v\n", name, err)
					}
				},
			}); err != nil {
				if errors.Is(err, context.Canceled) {
					interrupted = true
					stopSignals()
					fmt.Fprintln(progress, "\nInterrupted; applying resolved download counts and rewriting artifacts before exit.")
				} else {
					fmt.Fprintf(progress, "warning: %v\n", err)
				}
			}
			if err := cacheWriter.Flush(); err != nil {
				return fmt.Errorf("flushing %s: %w", cachePath, err)
			}

			// Apply resolved counts back onto the full result.
			resolved := 0
			for i := range toFetch {
				if toFetch[i].WeeklyDownloads >= 0 {
					resolved++
				}
			}
			counts := make(map[string]int64, resolved)
			for _, a := range toFetch {
				if a.WeeklyDownloads >= 0 {
					counts[a.Name] = a.WeeklyDownloads
				}
			}
			for i := range br.Affected {
				if c, ok := counts[br.Affected[i].Name]; ok {
					br.Affected[i].WeeklyDownloads = c
				}
			}
			handled := int(fetchedHandled.Load())
			missing := handled - resolved
			unresolved := len(toFetch) - handled
			fmt.Fprintf(progress, "Resolved %d fetched count%s, %d fetched no-data result%s, %d unresolved request%s in %s. Rewriting artifacts...\n",
				resolved, pluralS(resolved), missing, pluralS(missing), unresolved, pluralS(unresolved), time.Since(start).Round(time.Second))

			// Rewrite all three artifacts in place.
			if err := blast.SaveArtifacts(br, folder, progress); err != nil {
				return fmt.Errorf("writing artifacts: %w", err)
			}
			fmt.Fprintf(progress, "Done. Enriched %s in %s.\n", folder, time.Since(start).Round(time.Second))
			if interrupted {
				return errQuiet
			}
			return nil
		},
	}

	cmd.Flags().Float64Var(&rate, "rate", 1.0, "starting request rate (req/s) for the npm downloads API; the pacer self-tunes down on 429 and up on success, so this is just the ceiling")
	cmd.Flags().IntVar(&workers, "workers", 4, "concurrent requests (pipeline depth); the rate limiter bounds the actual rate")
	cmd.Flags().StringVar(&npmToken, "npm-token", "", "npm access token (or set NPM_TOKEN); lifts the unauthenticated rate limit")
	cmd.Flags().BoolVar(&force, "force", false, "re-fetch download counts for every package, even already-enriched ones")
	cmd.Flags().BoolVar(&includeScoped, "include-scoped-packages", false, "include scoped packages even when npm requires slow one-by-one lookups")

	return cmd
}

// loadJSONResult reads a blast-radius.json report, transparently handling a
// gzip wrapper (the demo report is gzipped; analyze writes plain JSON).
func loadJSONResult(path string) (*blast.JSONResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	// Sniff the gzip magic. A plain JSON file starts with '{'; a gzipped one
	// with 0x1f 0x8b.
	lead := make([]byte, 2)
	if _, err := io.ReadFull(f, lead); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if lead[0] == 0x1f && lead[1] == 0x8b {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}

	var result blast.JSONResult
	if err := json.NewDecoder(r).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func countUniqueNames(affected []blast.AffectedPackage) int {
	seen := make(map[string]struct{}, len(affected))
	for _, a := range affected {
		seen[a.Name] = struct{}{}
	}
	return len(seen)
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

type downloadCountCacheRecord struct {
	Name            string `json:"name"`
	WeeklyDownloads *int64 `json:"weekly_downloads"`
}

func loadDownloadCountCache(path string) (map[string]*int64, error) {
	out := make(map[string]*int64)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec downloadCountCacheRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Name == "" {
			// A process interrupted during append can leave a partial last line.
			// The cache is opportunistic, so skip malformed records.
			continue
		}
		if rec.WeeklyDownloads == nil {
			out[rec.Name] = nil
			continue
		}
		v := *rec.WeeklyDownloads
		out[rec.Name] = &v
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type downloadCountCacheWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func newDownloadCountCacheWriter(w io.Writer) *downloadCountCacheWriter {
	return &downloadCountCacheWriter{w: bufio.NewWriter(w)}
}

func (w *downloadCountCacheWriter) Write(name string, downloads *int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	rec := downloadCountCacheRecord{Name: name}
	if downloads != nil {
		v := *downloads
		rec.WeeklyDownloads = &v
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := w.w.Write(raw); err != nil {
		return err
	}
	if err := w.w.WriteByte('\n'); err != nil {
		return err
	}
	return w.w.Flush()
}

func (w *downloadCountCacheWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Flush()
}
