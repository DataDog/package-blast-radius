package dataset

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// buildDatabase creates an indexed DuckDB database from the parquet shards in
// parquetDir. The index on DepName is what makes reverse dependency lookup fast,
// and it is the whole reason for materialising a database instead of querying the
// parquet files directly.
func buildDatabase(ctx context.Context, runner Runner, parquetDir, dbPath, parquetPrefix, versionsPrefix string, r *reporter) error {
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

	start := time.Now()
	stopHeartbeat := r.heartbeat("still building", heartbeatInterval)
	err = runner.Run(ctx, pipedWriter{r.writer()}, "duckdb", dbPath, "-c", query)
	stopHeartbeat()

	if err != nil {
		// Leaving a partially built database behind would let a later analyze run
		// silently query an incomplete graph.
		os.Remove(dbPath)
		return fmt.Errorf("building database: %w", err)
	}
	r.line("done in %s", time.Since(start).Round(time.Second))
	return nil
}

// countRows reports the edge count, which is the one number that tells you the
// build actually ingested the whole snapshot.
func countRows(ctx context.Context, runner Runner, dbPath string) (int64, error) {
	out, err := runner.Output(ctx, "duckdb", dbPath, "-csv", "-noheader", "-c", "SELECT COUNT(*) FROM edges;")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

func escapeSingleQuotes(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// pipedWriter hides an *os.File behind a plain io.Writer, which makes os/exec
// give the child a pipe instead of handing it the terminal directly. duckdb draws
// an ANSI progress bar whenever its output is a terminal, and that bar redraws
// over the progress lines already on screen and leaves the terminal scrolled and
// half-cleared. Its output still reaches the user, so real errors are unaffected.
type pipedWriter struct{ w io.Writer }

func (p pipedWriter) Write(b []byte) (int, error) { return p.w.Write(b) }
