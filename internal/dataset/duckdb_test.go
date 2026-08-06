package dataset

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shardDir writes n real, tiny parquet shard files so buildDatabase's glob has
// valid parquet to ingest, not just files matching the naming pattern.
func shardDir(t *testing.T, prefix string, n int) string {
	t.Helper()
	dir := t.TempDir()
	data := validParquetFixture(t)
	for i := range n {
		name := filepath.Join(dir, prefix+"-"+string(rune('a'+i))+".parquet")
		if err := os.WriteFile(name, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// openBuilt reopens a database buildDatabase produced, the way analyze does.
func openBuilt(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("duckdb", dbPath+"?access_mode=read_only")
	if err != nil {
		t.Fatalf("opening built database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func hasIndex(t *testing.T, db *sql.DB, table, index string) bool {
	t.Helper()
	var name string
	err := db.QueryRow(
		`SELECT index_name FROM duckdb_indexes() WHERE table_name = ? AND index_name = ?`,
		table, index,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("querying duckdb_indexes(): %v", err)
	}
	return true
}

func hasTable(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var name string
	err := db.QueryRow(
		`SELECT table_name FROM information_schema.tables WHERE table_name = ?`, table,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("querying information_schema.tables: %v", err)
	}
	return true
}

func TestBuildDatabaseCreatesTableAndIndex(t *testing.T) {
	parquetDir := shardDir(t, "npm-edges", 3)
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")

	if err := buildDatabase(context.Background(), parquetDir, dbPath, "npm-edges", "", newReporter(&strings.Builder{}, 1)); err != nil {
		t.Fatalf("buildDatabase: %v", err)
	}

	db := openBuilt(t, dbPath)
	if !hasTable(t, db, "edges") {
		t.Error("the edges table was not created")
	}
	// Without this index a reverse dependency lookup scans the whole table.
	if !hasIndex(t, db, "edges", "idx_depname") {
		t.Error("the DepName index was not created")
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&count); err != nil {
		t.Fatalf("querying edges: %v", err)
	}
	if count != 3 {
		t.Errorf("edges has %d rows, want 3 (one per shard)", count)
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
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")

	err := buildDatabase(context.Background(), t.TempDir(), dbPath, "npm-edges", "", newReporter(&strings.Builder{}, 1))
	if err == nil {
		t.Fatal("buildDatabase succeeded with no shards")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("a database was created despite no shards existing")
	}
}

// The prefix is what keeps two ecosystems' shards from being merged into one
// database when they share a directory.
func TestBuildDatabaseIgnoresOtherEcosystemsShards(t *testing.T) {
	parquetDir := shardDir(t, "pypi-edges", 2)
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")

	if err := buildDatabase(context.Background(), parquetDir, dbPath, "npm-edges", "", newReporter(&strings.Builder{}, 1)); err == nil {
		t.Fatal("buildDatabase used pypi shards to build an npm database")
	}
}

func TestBuildDatabaseRemovesTheDatabaseWhenDuckDBFails(t *testing.T) {
	parquetDir := t.TempDir()
	// Real parquet magic bytes without the rest of the format, so DuckDB's
	// reader fails partway through instead of at the glob stage.
	if err := os.WriteFile(filepath.Join(parquetDir, "npm-edges-a.parquet"), []byte("PAR1garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")
	// Stand in for the file duckdb would have started writing.
	if err := os.WriteFile(dbPath, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := buildDatabase(context.Background(), parquetDir, dbPath, "npm-edges", "", newReporter(&strings.Builder{}, 1)); err == nil {
		t.Fatal("buildDatabase reported success on unreadable parquet")
	}
	// A leftover database would let a later analyze run query an incomplete graph.
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("a partial database was left at %s", dbPath)
	}
}

func TestBuildDatabaseReplacesAnExistingDatabase(t *testing.T) {
	parquetDir := shardDir(t, "npm-edges", 1)
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")
	if err := os.WriteFile(dbPath, []byte("old database"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := buildDatabase(context.Background(), parquetDir, dbPath, "npm-edges", "", newReporter(&strings.Builder{}, 1)); err != nil {
		t.Fatalf("buildDatabase: %v", err)
	}

	db := openBuilt(t, dbPath)
	if !hasTable(t, db, "edges") {
		t.Error("the old database was not replaced with a freshly built one")
	}
}

func TestBuildDatabaseCreatesVersionsTableWhenShardsPresent(t *testing.T) {
	parquetDir := shardDir(t, "npm-edges", 1)
	// The version shards are written under the same directory as the edge
	// shards, exactly as they land after download-data's two exports.
	data := validParquetFixture(t)
	for i := range 2 {
		name := filepath.Join(parquetDir, "npm-versions-"+string(rune('a'+i))+".parquet")
		if err := os.WriteFile(name, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")

	if err := buildDatabase(context.Background(), parquetDir, dbPath, "npm-edges", "npm-versions", newReporter(&strings.Builder{}, 1)); err != nil {
		t.Fatalf("buildDatabase: %v", err)
	}

	db := openBuilt(t, dbPath)
	if !hasTable(t, db, "versions") {
		t.Error("the versions table was not created")
	}
	if !hasIndex(t, db, "versions", "idx_version_name") {
		t.Error("the versions (Name, Version) index was not created")
	}
}

func TestBuildDatabaseSkipsVersionsTableWhenNoShardsExist(t *testing.T) {
	parquetDir := shardDir(t, "npm-edges", 1)
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")

	if err := buildDatabase(context.Background(), parquetDir, dbPath, "npm-edges", "npm-versions", newReporter(&strings.Builder{}, 1)); err != nil {
		t.Fatalf("buildDatabase: %v", err)
	}

	db := openBuilt(t, dbPath)
	if hasTable(t, db, "versions") {
		t.Error("a versions table was created with no shards on disk")
	}
}

func TestCountRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")
	setup, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	if _, err := setup.Exec(`CREATE TABLE edges(Name VARCHAR); INSERT INTO edges VALUES ('a'), ('b'), ('c')`); err != nil {
		t.Fatalf("seeding db: %v", err)
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := countRows(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("countRows: %v", err)
	}
	if got != 3 {
		t.Errorf("countRows = %d, want 3", got)
	}
}

func TestCountRowsFailsWithoutEdgesTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "npm-deps.duckdb")
	setup, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := countRows(context.Background(), dbPath); err == nil {
		t.Fatal("countRows succeeded against a database with no edges table")
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
