package dataset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shardDir writes n empty shard files so the glob in buildDatabase has something
// to find.
func shardDir(t *testing.T, prefix string, n int) string {
	t.Helper()
	dir := t.TempDir()
	for i := range n {
		name := filepath.Join(dir, prefix+"-"+string(rune('a'+i))+".parquet")
		if err := os.WriteFile(name, []byte("PAR1"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildDatabaseCreatesTableAndIndex(t *testing.T) {
	runner := &fakeRunner{}
	parquetDir := shardDir(t, "npm-edges", 3)
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")

	if err := buildDatabase(context.Background(), runner, parquetDir, dbPath, "npm-edges", newReporter(&strings.Builder{}, 1)); err != nil {
		t.Fatalf("buildDatabase: %v", err)
	}

	args := runner.allArgs()
	if !strings.Contains(args, "CREATE TABLE edges") {
		t.Errorf("duckdb was not asked to create the table:\n%s", args)
	}
	// Without this index a reverse dependency lookup scans the whole table.
	if !strings.Contains(args, "CREATE INDEX idx_depname ON edges(DepName)") {
		t.Errorf("duckdb was not asked to create the DepName index:\n%s", args)
	}
	if !strings.Contains(args, filepath.Join(parquetDir, "npm-edges-*.parquet")) {
		t.Errorf("the parquet glob is missing from the query:\n%s", args)
	}
}

// duckdb draws an ANSI progress bar when its output is a terminal, which redraws
// over the lines already on screen. Handing it something that is not an *os.File
// makes os/exec give it a pipe, and duckdb stays quiet.
func TestBuildDatabaseDoesNotGiveDuckDBTheTerminal(t *testing.T) {
	runner := &fakeRunner{}
	parquetDir := shardDir(t, "npm-edges", 1)
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")

	// os.Stderr is exactly what the command passes in real use.
	if err := buildDatabase(context.Background(), runner, parquetDir, dbPath, "npm-edges", newReporter(os.Stderr, 1)); err != nil {
		t.Fatalf("buildDatabase: %v", err)
	}

	if _, isFile := runner.runProgress.(*os.File); isFile {
		t.Error("duckdb was handed the terminal directly, so it will draw its progress bar")
	}
}

// Hiding the terminal must not hide duckdb's output, or a failing build would say
// nothing about why.
func TestPipedWriterForwardsEverything(t *testing.T) {
	sink := &strings.Builder{}

	piped := pipedWriter{sink}
	if _, err := piped.Write([]byte("Error: out of memory\n")); err != nil {
		t.Fatal(err)
	}
	if sink.String() != "Error: out of memory\n" {
		t.Errorf("pipedWriter dropped output: %q", sink.String())
	}
}

func TestHeartbeatReportsElapsedTime(t *testing.T) {
	out := &syncBuffer{}
	stop := newReporter(out, 1).heartbeat("still building", time.Millisecond)
	// Long enough for several ticks; stop() then waits for the goroutine.
	time.Sleep(50 * time.Millisecond)
	stop()

	if !strings.Contains(out.String(), "still building") {
		t.Errorf("heartbeat printed nothing:\n%s", out.String())
	}
	// Nothing may be written after stop returns, or a line lands mid-table later.
	before := out.String()
	time.Sleep(20 * time.Millisecond)
	if out.String() != before {
		t.Error("the heartbeat kept printing after stop returned")
	}
}

func TestBuildDatabaseRefusesAnEmptyDirectory(t *testing.T) {
	runner := &fakeRunner{}
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")

	err := buildDatabase(context.Background(), runner, t.TempDir(), dbPath, "npm-edges", newReporter(&strings.Builder{}, 1))
	if err == nil {
		t.Fatal("buildDatabase succeeded with no shards")
	}
	if len(runner.calls) != 0 {
		t.Errorf("duckdb was invoked anyway: %v", runner.calls)
	}
}

// The prefix is what keeps two ecosystems' shards from being merged into one
// database when they share a directory.
func TestBuildDatabaseIgnoresOtherEcosystemsShards(t *testing.T) {
	runner := &fakeRunner{}
	parquetDir := shardDir(t, "pypi-edges", 2)
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")

	if err := buildDatabase(context.Background(), runner, parquetDir, dbPath, "npm-edges", newReporter(&strings.Builder{}, 1)); err == nil {
		t.Fatal("buildDatabase used pypi shards to build an npm database")
	}
}

func TestBuildDatabaseRemovesTheDatabaseWhenDuckDBFails(t *testing.T) {
	runner := &fakeRunner{runErr: errors.New("out of memory")}
	parquetDir := shardDir(t, "npm-edges", 1)
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")
	// Stand in for the file duckdb would have started writing.
	if err := os.WriteFile(dbPath, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := buildDatabase(context.Background(), runner, parquetDir, dbPath, "npm-edges", newReporter(&strings.Builder{}, 1)); err == nil {
		t.Fatal("buildDatabase reported success after duckdb failed")
	}
	// A leftover database would let a later analyze run query an incomplete graph.
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("a partial database was left at %s", dbPath)
	}
}

func TestBuildDatabaseReplacesAnExistingDatabase(t *testing.T) {
	runner := &fakeRunner{}
	parquetDir := shardDir(t, "npm-edges", 1)
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")
	if err := os.WriteFile(dbPath, []byte("old database"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := buildDatabase(context.Background(), runner, parquetDir, dbPath, "npm-edges", newReporter(&strings.Builder{}, 1)); err != nil {
		t.Fatalf("buildDatabase: %v", err)
	}
	// CREATE TABLE would fail against a database that still has one.
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Error("the previous database was not removed before the build")
	}
}

func TestCountRows(t *testing.T) {
	runner := &fakeRunner{output: "  549213\n"}

	got, err := countRows(context.Background(), runner, "db.duckdb")
	if err != nil {
		t.Fatalf("countRows: %v", err)
	}
	if got != 549213 {
		t.Errorf("countRows = %d, want 549213", got)
	}
}

func TestCountRowsRejectsNonNumericOutput(t *testing.T) {
	runner := &fakeRunner{output: "Error: no such table: edges\n"}

	if _, err := countRows(context.Background(), runner, "db.duckdb"); err == nil {
		t.Fatal("countRows succeeded on a duckdb error message")
	}
}

func TestEscapeSingleQuotes(t *testing.T) {
	got := escapeSingleQuotes("/tmp/it's/npm-edges-*.parquet")
	if got != "/tmp/it''s/npm-edges-*.parquet" {
		t.Errorf("escapeSingleQuotes = %q", got)
	}
}

// syncBuffer is a buffer safe to read while the heartbeat goroutine writes.
type syncBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}
