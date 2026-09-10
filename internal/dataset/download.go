package dataset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

// Options configures a dataset download. Zero values are filled in with the
// conventional defaults, so only System and ProjectID are normally required.
type Options struct {
	System    blast.Ecosystem
	ProjectID string
	// Bucket may be "gs://name" or "name". Empty derives <project>-blast-radius.
	Bucket string
	// SnapshotDate is a deps.dev snapshot as YYYY-MM-DD. Empty discovers the latest.
	SnapshotDate string
	// DataDir holds parquet/ and the built database. Empty means "data".
	DataDir string
	// DBPath overrides where the DuckDB database is built. Empty derives
	// <data-dir>/<ecosystem>-deps.duckdb.
	DBPath string
	// ParquetDir overrides where shards are read from or written to.
	ParquetDir string
	// DatasetID is the BigQuery dataset holding the intermediate table.
	DatasetID string

	AutoApprove bool
	Force       bool
	KeepParquet bool
	// BuildOnly skips every cloud step and builds the database from local shards.
	BuildOnly bool
	// SkipBuild downloads the shards but leaves the database alone.
	SkipBuild bool
	// IncludeDownloadCounts adds package-level weekly download counts to the
	// built database when the ecosystem has a public dataset for them.
	IncludeDownloadCounts bool

	Stdin    io.Reader
	Progress io.Writer

	// HTTPClient is injected by tests; nil means the real thing.
	HTTPClient *http.Client
	// bigQueryURL and storageURL let tests point at an httptest.Server.
	bigQueryURL string
	storageURL  string
	// forceInteractive lets tests drive the prompts from a non-terminal reader.
	forceInteractive bool
	// now fixes the clock that snapshot discovery counts back from.
	now func() time.Time
}

func (o *Options) applyDefaults() {
	if o.DataDir == "" {
		o.DataDir = "data"
	}
	if o.DatasetID == "" {
		o.DatasetID = "blast_radius"
	}
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Progress == nil {
		o.Progress = os.Stderr
	}
	if o.now == nil {
		o.now = time.Now
	}
}

// parquetDir resolves where shards live. Explicit --parquet-dir wins;
// otherwise shards are scoped by snapshot date so two exports never mix, falling
// back to the unscoped dir when no date is known (how --build-only finds shards
// from an older version).
func (o *Options) parquetDir() string {
	if o.ParquetDir != "" {
		return o.ParquetDir
	}
	if o.SnapshotDate != "" {
		return filepath.Join(o.DataDir, "parquet", o.SnapshotDate)
	}
	return filepath.Join(o.DataDir, "parquet")
}

func (o *Options) dbPath() string {
	if o.DBPath != "" {
		return o.DBPath
	}
	return filepath.Join(o.DataDir, o.System.DBName())
}

// Download exports the dependency graph for opts.System and builds a DuckDB
// database. Every billed query and cloud mutation is confirmed first, unless
// AutoApprove is set.
func Download(ctx context.Context, opts Options) error {
	opts.applyDefaults()

	if !opts.System.SupportsDatasetDownload() {
		return fmt.Errorf("no BigQuery dataset export is configured for %s", opts.System)
	}

	r := newReporter(opts.Progress, opts.countSteps())
	ask := newConfirmer(opts.Stdin, opts.Progress, opts.AutoApprove)
	if opts.forceInteractive {
		ask.interactive = true
	}

	if err := preflight(&opts); err != nil {
		return err
	}

	ecosystem := strings.ToLower(string(opts.System))

	if opts.BuildOnly {
		r.header("blast-radius download-data "+ecosystem+" --build-only",
			"shards "+opts.parquetDir())
		return buildAndReport(ctx, &opts, r)
	}

	c, err := newRESTClient(ctx, &opts)
	if err != nil {
		return err
	}

	// The snapshot date belongs in the header, so it must be known before the
	// header prints; discovery is a preamble, not a step.
	snapshotSource := "(given)"
	if opts.SnapshotDate == "" {
		date, err := discoverSnapshotDate(ctx, c, &opts, r)
		if err != nil {
			return err
		}
		opts.SnapshotDate = date
		snapshotSource = "(latest)"
	}

	r.header("blast-radius download-data "+ecosystem,
		"project "+opts.ProjectID,
		"snapshot "+opts.SnapshotDate+" "+snapshotSource)

	bucket, err := resolveBucket(ctx, c, &opts, ask, r)
	if err != nil {
		return err
	}

	prefix := fmt.Sprintf("blast-radius/%s/", opts.SnapshotDate)
	if err := runExport(ctx, c, bucket, prefix, &opts, ask, r); err != nil {
		return err
	}

	if err := fetchShards(ctx, c, bucket, prefix, &opts, r); err != nil {
		return err
	}

	if opts.SkipBuild {
		r.section("Shards are in %s", opts.parquetDir())
		r.line("build the database with: blast-radius download-data %s --build-only --snapshot-date %s",
			ecosystem, opts.SnapshotDate)
		return nil
	}
	return buildAndReport(ctx, &opts, r)
}

// countSteps decides the denominator in the [n/m] labels up front, so a step that
// this run skips never leaves a gap in the numbering.
func (o *Options) countSteps() int {
	if o.BuildOnly {
		return 1
	}
	steps := 3 // bucket, export, download
	if !o.SkipBuild {
		steps++
	}
	return steps
}

// preflight fails on missing local tooling before anything billable happens
// — the bash version this replaced found a missing gcloud only after the ~$8
// query had already run.
func preflight(opts *Options) error {
	var problems []string

	if !opts.BuildOnly && opts.ProjectID == "" {
		problems = append(problems, "no GCP project given (pass --project)")
	}

	if opts.SnapshotDate != "" {
		if _, err := time.Parse("2006-01-02", opts.SnapshotDate); err != nil {
			problems = append(problems, fmt.Sprintf("invalid --snapshot-date %q: expected YYYY-MM-DD", opts.SnapshotDate))
		}
	}
	if opts.IncludeDownloadCounts && !opts.System.SupportsDownloadCountDataset() {
		problems = append(problems, fmt.Sprintf("download-count ingestion is not available for %s", opts.System))
	}

	if len(problems) > 0 {
		return fmt.Errorf("cannot start:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

func newRESTClient(ctx context.Context, opts *Options) (*client, error) {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		authed, err := newAuthedClient(ctx)
		if err != nil {
			return nil, err
		}
		httpClient = authed
	}

	c := newClient(httpClient)
	if opts.bigQueryURL != "" {
		c.bigQueryURL = opts.bigQueryURL
	}
	if opts.storageURL != "" {
		c.storageURL = opts.storageURL
	}
	return c, nil
}

// snapshotSearchDays bounds how far back a snapshot is looked for. deps.dev
// publishes daily, so the answer is normally today or yesterday.
const snapshotSearchDays = 14

// discoverSnapshotDate finds the latest snapshot by dry-running the export
// query one day at a time, newest first. A dry run is free and reports zero
// bytes when no partition matches. Asking BigQuery with MAX(SnapshotAt) instead
// scans the timestamp column across every recent partition (~0.25 TiB).
func discoverSnapshotDate(ctx context.Context, c *client, opts *Options, r *reporter) (string, error) {
	r.section("Finding the latest deps.dev snapshot")

	today := opts.now().UTC()
	for daysBack := range snapshotSearchDays {
		date := today.AddDate(0, 0, -daysBack).Format("2006-01-02")
		sql := edgeExportSQL(opts.System.BigQuerySystem(), date)

		bytes, err := c.estimateBytes(ctx, opts.ProjectID, sql)
		if err != nil {
			return "", fmt.Errorf("looking for a snapshot on %s: %w", date, err)
		}
		if bytes > 0 {
			return date, nil
		}
	}

	oldest := today.AddDate(0, 0, -(snapshotSearchDays - 1)).Format("2006-01-02")
	return "", fmt.Errorf("no %s snapshot published between %s and %s; pass --snapshot-date to name an older one",
		opts.System, oldest, today.Format("2006-01-02"))
}

// resolveBucket returns a usable bucket name, creating one only after asking.
func resolveBucket(ctx context.Context, c *client, opts *Options, ask *confirmer, r *reporter) (string, error) {
	r.step("Bucket")

	raw := opts.Bucket
	if raw == "" {
		raw = opts.ProjectID + "-blast-radius"
	}
	bucket, err := parseBucketURI(raw)
	if err != nil {
		return "", err
	}

	location, err := c.bucketLocation(ctx, bucket)
	switch {
	case err == nil:
		r.line("gs://%s exists in %s", bucket, location)
		// An extract job requires the bucket to share the dataset's region, and
		// the failure it produces otherwise names neither side.
		if !strings.EqualFold(location, datasetLocation) {
			return "", fmt.Errorf("gs://%s is in %s, but the deps.dev dataset is in %s and an extract job needs both to match\n"+
				"Pass a %s bucket with --bucket, or omit it to create one",
				bucket, location, datasetLocation, datasetLocation)
		}
		return bucket, nil

	case isNotFound(err):
		r.line("gs://%s does not exist", bucket)
		if err := ask.confirm(fmt.Sprintf("Create it in %s?", datasetLocation)); err != nil {
			return "", err
		}
		if err := c.createBucket(ctx, opts.ProjectID, bucket); err != nil {
			return "", err
		}
		r.line("created")
		return bucket, nil

	default:
		// A 403 is a permissions problem, not an absent bucket, and silently
		// trying to create it would report the wrong cause.
		return "", fmt.Errorf("checking gs://%s: %w", bucket, err)
	}
}

// ensurePrefixIsClear refuses to export on top of an earlier run's shards —
// the wildcard download would otherwise merge them into a corrupt dataset.
func ensurePrefixIsClear(ctx context.Context, c *client, bucket, prefix string, opts *Options, ask *confirmer, r *reporter) error {
	existing, err := c.listPrefix(ctx, bucket, prefix)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		return nil
	}

	r.field("stale", "gs://%s/%s already contains %d objects", bucket, prefix, len(existing))
	for _, obj := range existing[:min(5, len(existing))] {
		r.line("  %s", obj.Name)
	}
	if !opts.Force {
		return fmt.Errorf("refusing to export on top of %d existing objects; re-run with --force to delete them first", len(existing))
	}
	if err := ask.confirm(fmt.Sprintf("Delete all %d objects under gs://%s/%s?", len(existing), bucket, prefix)); err != nil {
		return err
	}
	return c.deleteObjects(ctx, bucket, existing, r)
}

func runExport(ctx context.Context, c *client, bucket, prefix string, opts *Options, ask *confirmer, r *reporter) error {
	r.step("Export query")

	if err := ensurePrefixIsClear(ctx, c, bucket, prefix, opts, ask, r); err != nil {
		return err
	}

	sql := edgeExportSQL(opts.System.BigQuerySystem(), opts.SnapshotDate)
	if err := exportOne(ctx, c, bucket, prefix, opts, ask, r, opts.System.ParquetPrefix(), sql); err != nil {
		return err
	}

	// The publish-date export lets a bundled edge be judged against when its
	// bundling package and the depended-on version were each released, instead
	// of being excluded outright. See README "How bundled dependencies are handled".
	if versionsPrefix := opts.System.VersionsParquetPrefix(); versionsPrefix != "" {
		versionsSQL := versionExportSQL(opts.System.BigQuerySystem(), opts.SnapshotDate)
		if err := exportOne(ctx, c, bucket, prefix, opts, ask, r, versionsPrefix, versionsSQL); err != nil {
			return err
		}
	}

	if opts.IncludeDownloadCounts {
		downloadStart, downloadEnd := downloadCountWindow(opts.now().UTC())
		downloadsSQL := pypiDownloadCountsSQL(downloadStart, downloadEnd)
		if err := exportOne(ctx, c, bucket, prefix, opts, ask, r, opts.System.DownloadsParquetPrefix(), downloadsSQL); err != nil {
			return err
		}
	}

	return nil
}

func downloadCountWindow(now time.Time) (startDate, endDate string) {
	end := now.AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -6)
	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

// exportOne prices, confirms, runs, and extracts a single query into its own
// destination table and parquet shards under the shared prefix.
func exportOne(ctx context.Context, c *client, bucket, prefix string, opts *Options, ask *confirmer, r *reporter, parquetPrefix, sql string) error {
	tableID := strings.ReplaceAll(parquetPrefix, "-", "_")
	dest := &tableRef{ProjectID: opts.ProjectID, DatasetID: opts.DatasetID, TableID: tableID}

	r.field("destination", "%s:%s.%s", dest.ProjectID, dest.DatasetID, dest.TableID)

	if err := c.priceAndConfirm(ctx, opts.ProjectID, sql, ask, r); err != nil {
		if errors.Is(err, errEmptyScan) {
			return fmt.Errorf("no %s snapshot on %s: the export query would read nothing\n"+
				"Omit --snapshot-date to use the latest snapshot", opts.System, opts.SnapshotDate)
		}
		return err
	}

	if err := c.ensureDataset(ctx, opts.ProjectID, opts.DatasetID); err != nil {
		return err
	}
	if err := c.runQueryToTable(ctx, opts.ProjectID, sql, dest, r); err != nil {
		return err
	}

	destURI := fmt.Sprintf("gs://%s/%s%s-*.parquet", bucket, prefix, parquetPrefix)
	if err := c.extractToGCS(ctx, opts.ProjectID, dest, destURI, r); err != nil {
		return err
	}
	r.field("wrote", "%s", destURI)
	return nil
}

func fetchShards(ctx context.Context, c *client, bucket, prefix string, opts *Options, r *reporter) error {
	objects, err := c.listPrefix(ctx, bucket, prefix)
	if err != nil {
		return err
	}
	if len(objects) == 0 {
		return fmt.Errorf("the extract job reported success but gs://%s/%s is empty", bucket, prefix)
	}

	destDir := opts.parquetDir()
	r.step("Download")
	r.line("%d shards to %s", len(objects), destDir)
	return c.downloadObjects(ctx, bucket, objects, destDir, r)
}

func buildAndReport(ctx context.Context, opts *Options, r *reporter) error {
	if opts.SkipBuild {
		return nil
	}

	parquetDir := opts.parquetDir()
	dbPath := opts.dbPath()

	r.step("Build")
	downloadsPrefix := ""
	if opts.IncludeDownloadCounts {
		downloadsPrefix = opts.System.DownloadsParquetPrefix()
	}
	if err := buildDatabase(ctx, parquetDir, dbPath, opts.System.ParquetPrefix(), opts.System.VersionsParquetPrefix(), downloadsPrefix, r); err != nil {
		return err
	}

	rows, err := countRows(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("database built but its row count could not be read: %w", err)
	}

	size := int64(0)
	if info, err := os.Stat(dbPath); err == nil {
		size = info.Size()
	}

	r.section("Done in %s", r.elapsed())
	r.field("database", "%s", dbPath)
	r.field("rows", "%s", blast.FormatNumber(rows))
	r.field("size", "%s", humanBytes(size))

	if !opts.KeepParquet && !opts.BuildOnly {
		prefixes := []string{opts.System.ParquetPrefix()}
		if v := opts.System.VersionsParquetPrefix(); v != "" {
			prefixes = append(prefixes, v)
		}
		if opts.IncludeDownloadCounts {
			prefixes = append(prefixes, opts.System.DownloadsParquetPrefix())
		}
		if err := removeShards(parquetDir, prefixes); err != nil {
			r.field("shards", "kept in %s: %v", parquetDir, err)
		} else {
			r.field("shards", "removed from %s (--keep-parquet keeps them)", parquetDir)
		}
	}

	r.section("Next")
	r.line("blast-radius analyze %s <package> <version>", strings.ToLower(string(opts.System)))
	return nil
}

// removeShards deletes only the shards this tool wrote, leaving anything else in
// the directory alone.
func removeShards(parquetDir string, parquetPrefixes []string) error {
	for _, prefix := range parquetPrefixes {
		shards, err := filepath.Glob(filepath.Join(parquetDir, prefix+"-*.parquet"))
		if err != nil {
			return err
		}
		for _, shard := range shards {
			if err := os.Remove(shard); err != nil {
				return err
			}
		}
	}
	os.Remove(parquetDir) // Only succeeds if it is now empty, which is the intent.
	return nil
}
