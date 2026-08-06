package dataset

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

// buildDatabase creates an indexed DuckDB database from the parquet shards in
// parquetDir. The index on DepName is what makes reverse dependency lookup fast,
// and it is the whole reason for materialising a database instead of querying the
// parquet files directly.
func buildDatabase(ctx context.Context, parquetDir, dbPath, parquetPrefix, versionsPrefix string, r *reporter) error {
	shards, err := filepath.Glob(filepath.Join(parquetDir, parquetPrefix+"-*.parquet"))
	if err != nil {
		return err
	}
	if len(shards) == 0 {
		return fmt.Errorf("no %s-*.parquet files in %s", parquetPrefix, parquetDir)
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dbPath), err)
	}
	// CREATE TABLE would fail against a database that already has one, and a
	// half-replaced database is worse than none.
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing existing %s: %w", dbPath, err)
	}

	r.line("%s from %d shards, this takes several minutes", dbPath, len(shards))

	glob := filepath.Join(parquetDir, parquetPrefix+"-*.parquet")
	query := fmt.Sprintf(
		"CREATE TABLE edges AS SELECT * FROM '%s';\nCREATE INDEX idx_depname ON edges(DepName);",
		escapeSingleQuotes(glob))

	// The versions table is optional: an older parquet directory downloaded
	// before publish dates were exported has no such shards, and the database
	// still builds, just without the data QueryPublishedAt needs.
	if versionsPrefix != "" {
		versionShards, err := filepath.Glob(filepath.Join(parquetDir, versionsPrefix+"-*.parquet"))
		if err != nil {
			return err
		}
		if len(versionShards) > 0 {
			versionsGlob := filepath.Join(parquetDir, versionsPrefix+"-*.parquet")
			query += fmt.Sprintf(
				"\nCREATE TABLE versions AS SELECT * FROM '%s';\nCREATE INDEX idx_version_name ON versions(Name, Version);",
				escapeSingleQuotes(versionsGlob))
		}
	}

	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", dbPath, err)
	}
	defer db.Close()

	start := time.Now()
	stopHeartbeat := r.heartbeat("still building", heartbeatInterval)
	_, err = db.ExecContext(ctx, query)
	stopHeartbeat()

	if err != nil {
		// Leaving a partially built database behind would let a later analyze run
		// silently query an incomplete graph.
		db.Close()
		os.Remove(dbPath)
		return fmt.Errorf("building database: %w", err)
	}
	r.line("done in %s", time.Since(start).Round(time.Second))
	return nil
}

// countRows reports the edge count, which is the one number that tells you the
// build actually ingested the whole snapshot.
func countRows(ctx context.Context, dbPath string) (int64, error) {
	db, err := sql.Open("duckdb", dbPath+"?access_mode=read_only")
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", dbPath, err)
	}
	defer db.Close()

	var count int64
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM edges").Scan(&count)
	return count, err
}

func escapeSingleQuotes(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
