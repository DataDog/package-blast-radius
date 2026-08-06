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

// DuckDBSource queries a DuckDB database over a single native connection kept
// open for the lifetime of a run. Reusing one connection (rather than opening
// a fresh one per query) is what lets the frontier temp tables below survive
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

// QueryDependentsOfNames hands yield every edge whose DepName is in the given
// set of names. The names are bulk-loaded into a temp table via the appender
// API rather than a large IN/CSV clause, so DuckDB can plan a hash join
// instead of parsing a giant query or CSV file.
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

// QueryPublishedAt looks up when each of pkgs was published, keyed by
// "name@version". A missing "versions" table (built only when download-data
// exported PackageVersions) is not an error: the caller gets an empty map and
// falls back to treating those packages as having an unknown publish date.
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

// scanDependentRows reads rows as they arrive rather than collecting them
// first: a popular package has millions of direct dependents, and holding a
// whole depth's worth of them costs gigabytes.
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

// appendStrings bulk-loads a single VARCHAR column into table, which must be
// a temp table created by the caller with a name we control (never user
// input), since it is interpolated directly into the INSERT statement.
//
// The DuckDB appender API is the usual fast path for bulk loads, but it
// refuses to attach to a connection opened in read-only mode even when the
// target table is a session-local temp table, so a prepared multi-row
// INSERT is used instead.
func (s *DuckDBSource) appendStrings(table string, values []string) error {
	ctx := context.Background()
	stmt, err := s.conn.PrepareContext(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (?)`, table))
	if err != nil {
		return fmt.Errorf("preparing insert: %w", err)
	}
	defer stmt.Close()
	for _, v := range values {
		if _, err := stmt.ExecContext(ctx, v); err != nil {
			return fmt.Errorf("inserting row: %w", err)
		}
	}
	return nil
}

func (s *DuckDBSource) appendPackageVersions(table string, pkgs []PackageVersion) error {
	ctx := context.Background()
	stmt, err := s.conn.PrepareContext(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (?, ?)`, table))
	if err != nil {
		return fmt.Errorf("preparing insert: %w", err)
	}
	defer stmt.Close()
	for _, pv := range pkgs {
		if _, err := stmt.ExecContext(ctx, pv.Name, pv.Version); err != nil {
			return fmt.Errorf("inserting row: %w", err)
		}
	}
	return nil
}
