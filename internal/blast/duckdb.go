package blast

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

// DuckDBSource queries DuckDB over one native connection kept open for the
// run. Reusing one connection is what lets the frontier temp tables survive
// across depths.
type DuckDBSource struct {
	db   *sql.DB
	conn *sql.Conn
}

func NewDuckDBSource(dbPath string) (*DuckDBSource, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("database not found: %s", dbPath)
	}

	dsn := fmt.Sprintf("%s?access_mode=read_only&threads=%d", dbPath, runtime.NumCPU())
	db, err := sql.Open("duckdb", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	conn, err := db.Conn(context.Background())
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("opening database: %w", err)
	}

	return &DuckDBSource{db: db, conn: conn}, nil
}

func (s *DuckDBSource) Close() error {
	err := s.conn.Close()
	if dbErr := s.db.Close(); err == nil {
		err = dbErr
	}
	return err
}

type RawDependent struct {
	DependentName    string
	DependentVersion string
	TargetName       string
	Requirement      string
}

// QueryDirectDependents hands yield every (package, version, requirement) tuple
// that directly depends on the given target package name.
func (s *DuckDBSource) QueryDirectDependents(targetName string, yield func(RawDependent) error) error {
	rows, err := s.conn.QueryContext(context.Background(),
		`SELECT Name, Version, DepName, Requirement FROM edges WHERE DepName = ?`, targetName)
	if err != nil {
		return fmt.Errorf("duckdb query failed: %w", err)
	}
	return scanDependentRows(rows, yield)
}

// QueryDependentsOfNames hands yield every edge whose DepName is in names. The
// names are bulk-loaded into a temp table rather than a large IN/CSV clause,
// so DuckDB plans a hash join instead of parsing a giant query.
func (s *DuckDBSource) QueryDependentsOfNames(names []string, yield func(RawDependent) error) error {
	if len(names) == 0 {
		return nil
	}

	ctx := context.Background()
	if _, err := s.conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE frontier(name VARCHAR)`); err != nil {
		return fmt.Errorf("creating frontier table: %w", err)
	}
	if err := s.appendStrings("frontier", names); err != nil {
		return fmt.Errorf("loading frontier table: %w", err)
	}

	rows, err := s.conn.QueryContext(ctx, `
		SELECT e.Name, e.Version, e.DepName, e.Requirement
		FROM edges e
		SEMI JOIN frontier f ON e.DepName = f.name
	`)
	if err != nil {
		return fmt.Errorf("duckdb query failed: %w", err)
	}
	return scanDependentRows(rows, yield)
}

// QueryPublishedAt looks up publish dates keyed by "name@version". A missing
// "versions" table (only when download-data exported PackageVersions) isn't an
// error: the caller gets an empty map and treats dates as unknown.
func (s *DuckDBSource) QueryPublishedAt(pkgs []PackageVersion) (map[string]time.Time, error) {
	if len(pkgs) == 0 {
		return nil, nil
	}

	ctx := context.Background()
	if _, err := s.conn.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE pubdate_lookup(name VARCHAR, version VARCHAR)`); err != nil {
		return nil, fmt.Errorf("creating lookup table: %w", err)
	}
	if err := s.appendPackageVersions("pubdate_lookup", pkgs); err != nil {
		return nil, fmt.Errorf("loading lookup table: %w", err)
	}

	rows, err := s.conn.QueryContext(ctx, `
		SELECT v.Name, v.Version, v.PublishedAt
		FROM versions v
		JOIN pubdate_lookup t ON v.Name = t.name AND v.Version = t.version
	`)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "does not exist") || strings.Contains(msg, "Table with name versions") {
			return nil, nil
		}
		return nil, fmt.Errorf("duckdb query failed: %w", err)
	}
	defer rows.Close()

	result := make(map[string]time.Time)
	for rows.Next() {
		var name, version string
		var publishedAt time.Time
		if err := rows.Scan(&name, &version, &publishedAt); err != nil {
			return nil, fmt.Errorf("scanning duckdb row: %w", err)
		}
		result[name+"@"+version] = publishedAt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("duckdb query failed: %w", err)
	}
	return result, nil
}

// scanDependentRows streams rows rather than collecting — a popular package
// has millions of direct dependents, and a whole depth's worth is gigabytes.
func scanDependentRows(rows *sql.Rows, yield func(RawDependent) error) error {
	defer rows.Close()
	for rows.Next() {
		var d RawDependent
		if err := rows.Scan(&d.DependentName, &d.DependentVersion, &d.TargetName, &d.Requirement); err != nil {
			return fmt.Errorf("scanning duckdb row: %w", err)
		}
		if err := yield(d); err != nil {
			return err
		}
	}
	return rows.Err()
}

// insertBatchSize is how many rows one INSERT carries. The appender API is
// the usual fast path but refuses read-only connections even for session-local
// temp tables, so multi-row INSERTs are used instead. Batching keeps a
// multi-thousand-row frontier to a handful of round-trips rather than one per
// row.
const insertBatchSize = 500

// appendStrings bulk-loads one VARCHAR column into a caller-created temp table
// (name we control, never user input, interpolated into the INSERT).
func (s *DuckDBSource) appendStrings(table string, values []string) error {
	ctx := context.Background()
	for start := 0; start < len(values); start += insertBatchSize {
		end := min(start+insertBatchSize, len(values))
		batch := values[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, v := range batch {
			placeholders[i] = "(?)"
			args[i] = v
		}
		q := fmt.Sprintf(`INSERT INTO %s VALUES %s`, table, strings.Join(placeholders, ","))
		if _, err := s.conn.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("inserting rows: %w", err)
		}
	}
	return nil
}

func (s *DuckDBSource) appendPackageVersions(table string, pkgs []PackageVersion) error {
	ctx := context.Background()
	for start := 0; start < len(pkgs); start += insertBatchSize {
		end := min(start+insertBatchSize, len(pkgs))
		batch := pkgs[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch)*2)
		for i, pv := range batch {
			placeholders[i] = "(?, ?)"
			args = append(args, pv.Name, pv.Version)
		}
		q := fmt.Sprintf(`INSERT INTO %s VALUES %s`, table, strings.Join(placeholders, ","))
		if _, err := s.conn.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("inserting rows: %w", err)
		}
	}
	return nil
}
