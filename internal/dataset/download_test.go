package dataset

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

// downloadFixture wires a Download call against the fake APIs and a real duckdb
// database built on disk under dataDir.
type downloadFixture struct {
	gcp      *fakeGCP
	progress *bytes.Buffer
	dataDir  string
}

func newDownloadFixture(t *testing.T) *downloadFixture {
	t.Helper()
	return &downloadFixture{
		gcp:      newFakeGCP(t),
		progress: &bytes.Buffer{},
		dataDir:  t.TempDir(),
	}
}

// dbPath is where Download builds the database for this fixture's ecosystem.
func (f *downloadFixture) dbPath() string {
	return filepath.Join(f.dataDir, blast.NPM.DBName())
}

// databaseBuilt reports whether a database was built at dbPath.
func (f *downloadFixture) databaseBuilt(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(f.dbPath())
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", f.dbPath(), err)
	return false
}

// run drives Download with the given answers piped to every prompt.
func (f *downloadFixture) run(t *testing.T, answers string, mutate func(*Options)) error {
	t.Helper()
	opts := f.gcp.options(Options{
		System:           blast.NPM,
		ProjectID:        "my-project",
		Bucket:           "my-bucket",
		SnapshotDate:     "2026-03-23",
		DataDir:          f.dataDir,
		Stdin:            strings.NewReader(answers),
		Progress:         f.progress,
		forceInteractive: true,
	})
	if mutate != nil {
		mutate(&opts)
	}
	return Download(context.Background(), opts)
}

// shardsInBucket makes the fake serve shards that are already there before the
// run starts, which is what a stale earlier export looks like.
func (f *downloadFixture) shardsInBucket(prefix string, n int) {
	f.gcp.objects = nil
	for i := range n {
		f.gcp.objects = append(f.gcp.objects,
			gcsObject{Name: prefix + "npm-edges-" + string(rune('a'+i)) + ".parquet"})
	}
}

// shardsAfterExport makes the shards appear only on the second listing, so the
// pre-export "is the prefix clear?" check sees an empty bucket.
func (f *downloadFixture) shardsAfterExport(prefix string, n int) {
	f.shardsInBucket(prefix, n)
	f.gcp.hideObjectsUntilList = 1
}

// The user's requirement: nothing billable runs before an interactive yes.
func TestDownloadRunsNothingBillableWhenTheUserDeclines(t *testing.T) {
	f := newDownloadFixture(t)

	err := f.run(t, "n\n", nil)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("Download = %v, want ErrAborted", err)
	}
	if jobs := f.gcp.billableJobs(); len(jobs) != 0 {
		t.Errorf("declining still issued %d billable BigQuery requests: %+v", len(jobs), jobs)
	}
	if f.databaseBuilt(t) {
		t.Error("declining still built a database")
	}
}

// A dry run is free, so it must happen before the prompt: that is where the
// dollar figure in the prompt comes from.
func TestDownloadPricesBeforeAsking(t *testing.T) {
	f := newDownloadFixture(t)

	f.run(t, "n\n", nil)

	if f.gcp.countRequests("POST", "/jobs") == 0 {
		t.Fatal("no dry run was issued, so the prompt could not have shown a cost")
	}
	out := f.progress.String()
	if !strings.Contains(out, "1.31 TiB") || !strings.Contains(out, "8.19") {
		t.Errorf("the prompt did not show the estimated cost:\n%s", out)
	}
}

func TestDownloadDoesNotCreateABucketWithoutConsent(t *testing.T) {
	f := newDownloadFixture(t)
	f.gcp.bucketStatus = 404

	// The bucket prompt comes first, before anything is exported.
	err := f.run(t, "n\n", nil)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("Download = %v, want ErrAborted", err)
	}
	if got := f.gcp.countRequests("POST", "/storage/v1/b"); got != 0 {
		t.Errorf("issued %d bucket creations after a refusal", got)
	}
}

func TestDownloadCreatesTheBucketAfterConsent(t *testing.T) {
	f := newDownloadFixture(t)
	f.gcp.bucketStatus = 404
	f.shardsAfterExport("blast-radius/2026-03-23/", 2)

	// Create the bucket, then approve both export costs (edges, then publish dates).
	if err := f.run(t, "y\ny\ny\n", nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := f.gcp.countRequests("POST", "/storage/v1/b"); got != 1 {
		t.Errorf("issued %d bucket creations, want 1", got)
	}
}

// Consent is not permission: a rejected creation has to stop the run rather than
// let the export write into a bucket that does not exist.
func TestDownloadStopsWhenBucketCreationIsRejected(t *testing.T) {
	f := newDownloadFixture(t)
	f.gcp.bucketStatus = 404
	f.gcp.createBucketFail = true

	err := f.run(t, "y\ny\n", nil)
	if err == nil {
		t.Fatal("Download continued after the bucket could not be created")
	}
	if !strings.Contains(err.Error(), "insufficient permission") {
		t.Errorf("error %q should carry Google's reason", err)
	}
	if jobs := f.gcp.billableJobs(); len(jobs) != 0 {
		t.Errorf("ran %d billable jobs with no bucket to extract into", len(jobs))
	}
}

// A 403 means "you cannot see this bucket", and creating one instead would report
// the wrong cause.
func TestDownloadDistinguishesForbiddenFromMissingBucket(t *testing.T) {
	f := newDownloadFixture(t)
	f.gcp.bucketStatus = 403

	err := f.run(t, "y\ny\ny\n", nil)
	if err == nil {
		t.Fatal("Download succeeded despite a 403 on the bucket")
	}
	if errors.Is(err, ErrAborted) {
		t.Fatalf("a 403 was reported as an abort: %v", err)
	}
	if got := f.gcp.countRequests("POST", "/storage/v1/b"); got != 0 {
		t.Errorf("tried to create a bucket it could not read: %d attempts", got)
	}
}

// An extract job requires the bucket to share the dataset's region, and the error
// BigQuery returns otherwise names neither side.
func TestDownloadRejectsABucketOutsideTheDatasetRegion(t *testing.T) {
	f := newDownloadFixture(t)
	f.gcp.bucketLocation = "EUROPE-WEST1"

	err := f.run(t, "y\ny\n", nil)
	if err == nil {
		t.Fatal("Download accepted a bucket outside the dataset region")
	}
	if !strings.Contains(err.Error(), "EUROPE-WEST1") || !strings.Contains(err.Error(), "US") {
		t.Errorf("error %q should name both regions", err)
	}
	if jobs := f.gcp.billableJobs(); len(jobs) != 0 {
		t.Errorf("ran %d billable jobs against an unusable bucket", len(jobs))
	}
}

// This is the corrupt-merge bug from the bash version: a second export into a
// prefix that still holds an older snapshot's shards.
func TestDownloadRefusesToExportOverExistingShards(t *testing.T) {
	f := newDownloadFixture(t)
	f.shardsInBucket("blast-radius/2026-03-23/", 3)

	err := f.run(t, "y\ny\n", nil)
	if err == nil {
		t.Fatal("Download exported on top of existing shards")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error %q should point at --force", err)
	}
	if jobs := f.gcp.billableJobs(); len(jobs) != 0 {
		t.Errorf("ran %d billable jobs before hitting the stale prefix", len(jobs))
	}
}

func TestDownloadForceDeletesStaleShardsAfterConfirming(t *testing.T) {
	f := newDownloadFixture(t)
	f.shardsInBucket("blast-radius/2026-03-23/", 3)

	// Approve the deletion, then both export costs (edges, then publish dates).
	if err := f.run(t, "y\ny\ny\n", func(o *Options) { o.Force = true }); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := f.gcp.countRequests("DELETE", ".parquet"); got != 3 {
		t.Errorf("deleted %d stale objects, want 3", got)
	}
}

func TestDownloadForceStillNeedsConsentToDelete(t *testing.T) {
	f := newDownloadFixture(t)
	f.shardsInBucket("blast-radius/2026-03-23/", 3)

	// Refuse the deletion prompt, which comes before the export cost.
	err := f.run(t, "n\n", func(o *Options) { o.Force = true })
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("Download = %v, want ErrAborted", err)
	}
	if got := f.gcp.countRequests("DELETE", ".parquet"); got != 0 {
		t.Errorf("deleted %d objects after a refusal", got)
	}
}

func TestDownloadHappyPathBuildsTheDatabase(t *testing.T) {
	f := newDownloadFixture(t)
	f.shardsAfterExport("blast-radius/2026-03-23/", 4)

	// Two export queries now run: edges, then publish dates.
	if err := f.run(t, "y\ny\n", nil); err != nil {
		t.Fatalf("Download: %v", err)
	}

	if jobs := f.gcp.billableJobs(); len(jobs) != 4 {
		t.Errorf("ran %d billable jobs, want 4 (edges query+extract, versions query+extract)", len(jobs))
	}
	if !f.databaseBuilt(t) {
		t.Fatal("the database was not built")
	}
	db := openBuilt(t, f.dbPath())
	if !hasTable(t, db, "edges") {
		t.Error("the edges table was not created")
	}
	out := f.progress.String()
	// One row per downloaded shard.
	if !strings.Contains(out, "rows") || !strings.Contains(out, "4") {
		t.Errorf("the row count was not reported:\n%s", out)
	}
	// Shards are scoped by snapshot date so two exports never mix locally.
	if !strings.Contains(out, filepath.Join("parquet", "2026-03-23")) {
		t.Errorf("shards were not downloaded into a date-scoped directory:\n%s", out)
	}
}

func TestDownloadRemovesShardsUnlessAskedToKeepThem(t *testing.T) {
	for _, keep := range []bool{false, true} {
		f := newDownloadFixture(t)
		f.shardsAfterExport("blast-radius/2026-03-23/", 2)

		if err := f.run(t, "y\ny\n", func(o *Options) { o.KeepParquet = keep }); err != nil {
			t.Fatalf("Download(keep=%v): %v", keep, err)
		}

		shards, _ := filepath.Glob(filepath.Join(f.dataDir, "parquet", "2026-03-23", "npm-edges-*.parquet"))
		if keep && len(shards) != 2 {
			t.Errorf("--keep-parquet kept %d of 2 shards", len(shards))
		}
		if !keep && len(shards) != 0 {
			t.Errorf("left %d shards behind without --keep-parquet", len(shards))
		}
	}
}

// --build-only is the escape hatch for rebuilding from shards already on disk,
// so it must not touch the network and must not delete them.
func TestBuildOnlyMakesNoCloudCallsAndKeepsShards(t *testing.T) {
	f := newDownloadFixture(t)
	parquetDir := filepath.Join(f.dataDir, "parquet", "2026-03-23")
	if err := os.MkdirAll(parquetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := validParquetFixture(t)
	for _, name := range []string{"npm-edges-a.parquet", "npm-edges-b.parquet"} {
		if err := os.WriteFile(filepath.Join(parquetDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	err := f.run(t, "", func(o *Options) {
		o.BuildOnly = true
		o.ProjectID = "" // --build-only needs no project
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if got := len(f.gcp.recorded()); got != 0 {
		t.Errorf("--build-only issued %d HTTP requests: %+v", got, f.gcp.recorded())
	}
	shards, _ := filepath.Glob(filepath.Join(parquetDir, "npm-edges-*.parquet"))
	if len(shards) != 2 {
		t.Errorf("--build-only deleted the user's shards: %d of 2 left", len(shards))
	}
}

func TestDownloadBuildsDatabaseAtExplicitPath(t *testing.T) {
	f := newDownloadFixture(t)
	f.shardsAfterExport("blast-radius/2026-03-23/", 2)
	dbPath := filepath.Join(t.TempDir(), "custom", "npm.duckdb")

	if err := f.run(t, "y\ny\n", func(o *Options) { o.DBPath = dbPath }); err != nil {
		t.Fatalf("Download: %v", err)
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("custom database was not built at %s: %v", dbPath, err)
	}
	if _, err := os.Stat(f.dbPath()); !os.IsNotExist(err) {
		t.Fatalf("default database path exists after --db override: err=%v", err)
	}
}

func TestSkipBuildNeverInvokesDuckDB(t *testing.T) {
	f := newDownloadFixture(t)
	f.shardsAfterExport("blast-radius/2026-03-23/", 2)

	if err := f.run(t, "y\ny\n", func(o *Options) { o.SkipBuild = true }); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if f.databaseBuilt(t) {
		t.Error("--skip-build built a database")
	}
	// The user has to be told how to finish the job later.
	if !strings.Contains(f.progress.String(), "--build-only") {
		t.Errorf("--skip-build did not explain how to build later:\n%s", f.progress.String())
	}
}

// Discovery walks back a day at a time until a dry run reads something. Only
// 2026-04-11 has a partition here, so the two more recent days must be skipped.
func TestDownloadDiscoversTheSnapshotDateWhenNotGiven(t *testing.T) {
	f := newDownloadFixture(t)
	f.gcp.dryRunBytesFor = func(sql string) string {
		if strings.Contains(sql, "2026-04-11") {
			return "1440360232386"
		}
		return "0"
	}
	f.shardsAfterExport("blast-radius/2026-04-11/", 1)

	err := f.run(t, "y\ny\n", func(o *Options) {
		o.SnapshotDate = ""
		o.now = func() time.Time { return time.Date(2026, 4, 13, 9, 0, 0, 0, time.UTC) }
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	out := f.progress.String()
	if !strings.Contains(out, "2026-04-11") {
		t.Errorf("the discovered snapshot date was not used:\n%s", out)
	}
	// Discovery must cost nothing; a dry run is not a billable job.
	if jobs := f.gcp.billableJobs(); len(jobs) != 4 {
		t.Errorf("ran %d billable jobs, want 4 (edges query+extract, versions query+extract)", len(jobs))
	}
}

func TestDiscoverySearchesBackwardsAndThenGivesUp(t *testing.T) {
	f := newDownloadFixture(t)
	f.gcp.dryRunBytesFor = func(string) string { return "0" }

	err := f.run(t, "y\n", func(o *Options) {
		o.SnapshotDate = ""
		o.now = func() time.Time { return time.Date(2026, 4, 13, 9, 0, 0, 0, time.UTC) }
	})
	if err == nil {
		t.Fatal("Download succeeded with no snapshot published at all")
	}
	// The window it searched has to be in the message, or there is no way to
	// tell a broken query from a genuinely stale dataset.
	if !strings.Contains(err.Error(), "2026-03-31") || !strings.Contains(err.Error(), "2026-04-13") {
		t.Errorf("error %q should name the window it searched", err)
	}
	if jobs := f.gcp.billableJobs(); len(jobs) != 0 {
		t.Errorf("ran %d billable jobs while only probing", len(jobs))
	}
}

func TestDownloadRejectsAnEcosystemWithoutAnExport(t *testing.T) {
	f := newDownloadFixture(t)

	err := f.run(t, "y\n", func(o *Options) { o.System = blast.PyPI })
	if err == nil {
		t.Fatal("Download accepted an ecosystem with no BigQuery export")
	}
	if got := len(f.gcp.recorded()); got != 0 {
		t.Errorf("issued %d requests for an unsupported ecosystem", got)
	}
}

// Without --yes and without a terminal there is nobody to answer, so refusing
// beats blocking forever on a read.
func TestDownloadRefusesNonInteractivelyWithoutYes(t *testing.T) {
	f := newDownloadFixture(t)

	opts := f.gcp.options(Options{
		System:       blast.NPM,
		ProjectID:    "my-project",
		Bucket:       "my-bucket",
		SnapshotDate: "2026-03-23",
		DataDir:      f.dataDir,
		Stdin:        strings.NewReader("y\n"),
		Progress:     f.progress,
	})

	err := Download(context.Background(), opts)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("Download = %v, want ErrAborted", err)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error %q should mention --yes", err)
	}
	if jobs := f.gcp.billableJobs(); len(jobs) != 0 {
		t.Errorf("ran %d billable jobs non-interactively", len(jobs))
	}
}

func TestDownloadAutoApproveRunsEndToEnd(t *testing.T) {
	f := newDownloadFixture(t)
	f.gcp.bucketStatus = 404
	f.shardsAfterExport("blast-radius/2026-03-23/", 2)

	opts := f.gcp.options(Options{
		System:       blast.NPM,
		ProjectID:    "my-project",
		Bucket:       "my-bucket",
		SnapshotDate: "2026-03-23",
		DataDir:      f.dataDir,
		AutoApprove:  true,
		Stdin:        strings.NewReader(""), // nothing to read from
		Progress:     f.progress,
	})

	if err := Download(context.Background(), opts); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := f.gcp.countRequests("POST", "/storage/v1/b"); got != 1 {
		t.Errorf("--yes created %d buckets, want 1", got)
	}
}

// An extract job reporting success with nothing in the bucket must not be turned
// into an empty database.
func TestDownloadFailsWhenTheExportProducedNothing(t *testing.T) {
	f := newDownloadFixture(t)
	f.gcp.objects = nil

	err := f.run(t, "y\ny\n", nil)
	if err == nil {
		t.Fatal("Download succeeded with no shards in the bucket")
	}
	if f.databaseBuilt(t) {
		t.Error("tried to build a database from nothing")
	}
}

func TestPreflightReportsEveryProblemAtOnce(t *testing.T) {
	opts := Options{System: blast.NPM, SkipBuild: false}
	// duckdb may well be installed here, so only assert on the project.
	err := preflight(&opts)
	if err == nil {
		t.Fatal("preflight accepted an empty project")
	}
	if !strings.Contains(err.Error(), "--project") {
		t.Errorf("error %q should name the missing flag", err)
	}
}

func TestPreflightRejectsBadSnapshotDate(t *testing.T) {
	tests := []struct {
		date string
		ok   bool
	}{
		{"2026-03-23", true},
		{"", true}, // empty means discover
		{"2026-3-23", false},
		{"not-a-date", false},
		{"2026-01-01'; DROP TABLE--", false},
	}
	for _, tt := range tests {
		opts := Options{System: blast.NPM, ProjectID: "p", SnapshotDate: tt.date}
		if err := preflight(&opts); (err == nil) != tt.ok {
			t.Errorf("preflight(SnapshotDate=%q) ok=%v, want %v", tt.date, err == nil, tt.ok)
		}
	}
}

func TestParquetDirPrefersTheExplicitOverride(t *testing.T) {
	tests := []struct {
		opts Options
		want string
	}{
		{Options{DataDir: "data", ParquetDir: "/tmp/shards", SnapshotDate: "2026-03-23"}, "/tmp/shards"},
		{Options{DataDir: "data", SnapshotDate: "2026-03-23"}, filepath.Join("data", "parquet", "2026-03-23")},
		{Options{DataDir: "data"}, filepath.Join("data", "parquet")},
	}
	for _, tt := range tests {
		if got := tt.opts.parquetDir(); got != tt.want {
			t.Errorf("parquetDir() = %q, want %q", got, tt.want)
		}
	}
}

func TestDBPathPrefersTheExplicitOverride(t *testing.T) {
	tests := []struct {
		opts Options
		want string
	}{
		{Options{System: blast.NPM, DataDir: "data", DBPath: "/tmp/npm.duckdb"}, "/tmp/npm.duckdb"},
		{Options{System: blast.NPM, DataDir: "data"}, filepath.Join("data", "npm-deps.duckdb")},
	}
	for _, tt := range tests {
		if got := tt.opts.dbPath(); got != tt.want {
			t.Errorf("dbPath() = %q, want %q", got, tt.want)
		}
	}
}

// A snapshot date with no partition behind it must stop the run before anything
// billable, and say why rather than exporting an empty table.
func TestDownloadRejectsASnapshotDateThatHasNoData(t *testing.T) {
	f := newDownloadFixture(t)
	f.gcp.dryRunBytes = "0"

	err := f.run(t, "y\ny\n", func(o *Options) { o.SnapshotDate = "2026-08-04" })
	if err == nil {
		t.Fatal("Download succeeded against a snapshot date with no data")
	}
	if !strings.Contains(err.Error(), "2026-08-04") || !strings.Contains(err.Error(), "read nothing") {
		t.Errorf("error %q should name the date and say the query reads nothing", err)
	}
	if jobs := f.gcp.billableJobs(); len(jobs) != 0 {
		t.Errorf("ran %d billable jobs for an empty snapshot", len(jobs))
	}
}
