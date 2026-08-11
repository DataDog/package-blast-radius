package blast

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

func TestNewDuckDBSourceRejectsMissingDatabase(t *testing.T) {
	if _, err := NewDuckDBSource(t.TempDir() + "/missing.duckdb"); err == nil {
		t.Error("got nil error for a missing database file")
	}
}

func TestQueryDependentsOfNamesIsEmptyForNoNames(t *testing.T) {
	s := &DuckDBSource{}

	err := s.QueryDependentsOfNames(nil, func(RawDependent) error {
		t.Error("got a row for an empty name set")
		return nil
	})
	if err != nil {
		t.Errorf("got error %v, want nil", err)
	}
}

// newFixtureDB builds a small on-disk database with the same schema
// buildDatabase creates, then reopens it the way analyze does: read-only,
// through NewDuckDBSource.
func newFixtureDB(t *testing.T) *DuckDBSource {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "fixture.duckdb")
	setup, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatalf("opening fixture db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE edges(Name VARCHAR, Version VARCHAR, DepName VARCHAR, Requirement VARCHAR)`,
		`INSERT INTO edges VALUES
			('a', '1.0.0', 'axios', '^1.0.0'),
			('b', '2.0.0', 'axios', '~1.2.0'),
			('c', '1.0.0', 'lodash', '^4.0.0')`,
		`CREATE INDEX idx_depname ON edges(DepName)`,
		`CREATE TABLE versions(Name VARCHAR, Version VARCHAR, PublishedAt TIMESTAMP)`,
		`INSERT INTO versions VALUES ('a', '1.0.0', '2024-01-02 03:04:05')`,
		`CREATE INDEX idx_version_name ON versions(Name, Version)`,
		`CREATE TABLE downloads(Name VARCHAR, WeeklyDownloads BIGINT)`,
		`INSERT INTO downloads VALUES ('a', 123), ('b', 456)`,
		`CREATE INDEX idx_downloads_name ON downloads(Name)`,
	}
	for _, stmt := range stmts {
		if _, err := setup.Exec(stmt); err != nil {
			t.Fatalf("setting up fixture db: %v", err)
		}
	}
	if err := setup.Close(); err != nil {
		t.Fatalf("closing fixture db: %v", err)
	}

	source, err := NewDuckDBSource(dbPath)
	if err != nil {
		t.Fatalf("NewDuckDBSource: %v", err)
	}
	t.Cleanup(func() { source.Close() })
	return source
}

func TestQueryDirectDependents(t *testing.T) {
	source := newFixtureDB(t)

	var got []RawDependent
	err := source.QueryDirectDependents("axios", func(d RawDependent) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("QueryDirectDependents: %v", err)
	}

	want := []RawDependent{
		{DependentName: "a", DependentVersion: "1.0.0", TargetName: "axios", Requirement: "^1.0.0"},
		{DependentName: "b", DependentVersion: "2.0.0", TargetName: "axios", Requirement: "~1.2.0"},
	}
	sortDependents(got)
	sortDependents(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestQueryDependentsOfNames(t *testing.T) {
	source := newFixtureDB(t)

	var got []RawDependent
	err := source.QueryDependentsOfNames([]string{"axios", "lodash"}, func(d RawDependent) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("QueryDependentsOfNames: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d rows, want 3: %+v", len(got), got)
	}

	// A second call must not fail because of the temp table created by the first.
	got = nil
	err = source.QueryDependentsOfNames([]string{"axios"}, func(d RawDependent) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("QueryDependentsOfNames (second call): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d rows, want 2: %+v", len(got), got)
	}
}

// TestQueryDependentsOfNamesLargeFrontier exercises the batched INSERT path:
// a frontier larger than insertBatchSize must load in multiple chunks without
// dropping or duplicating names.
func TestQueryDependentsOfNamesLargeFrontier(t *testing.T) {
	source := newFixtureDB(t)

	// Build a name set well past insertBatchSize; only 'axios' and 'lodash' have
	// edges in the fixture, the rest are no-ops, but all must load without error.
	names := make([]string, insertBatchSize*2+7)
	for i := range names {
		names[i] = "no-such-pkg-" + strconv.Itoa(i)
	}
	names[0] = "axios"
	names[insertBatchSize+1] = "lodash"

	var got []RawDependent
	err := source.QueryDependentsOfNames(names, func(d RawDependent) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("QueryDependentsOfNames: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d rows, want 3 (axios x2 + lodash x1): %+v", len(got), got)
	}
}

func TestQueryPublishedAt(t *testing.T) {
	source := newFixtureDB(t)

	result, err := source.QueryPublishedAt([]PackageVersion{
		{Name: "a", Version: "1.0.0"},
		{Name: "missing", Version: "9.9.9"},
	})
	if err != nil {
		t.Fatalf("QueryPublishedAt: %v", err)
	}

	got, ok := result["a@1.0.0"]
	if !ok {
		t.Fatalf("missing entry for a@1.0.0 in %+v", result)
	}
	want := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, ok := result["missing@9.9.9"]; ok {
		t.Errorf("got an entry for a package that isn't in the versions table")
	}
}

func TestQueryWeeklyDownloads(t *testing.T) {
	source := newFixtureDB(t)

	result, err := source.QueryWeeklyDownloads([]string{"a", "missing"})
	if err != nil {
		t.Fatalf("QueryWeeklyDownloads: %v", err)
	}
	if result["a"] != 123 {
		t.Errorf("a downloads = %d, want 123", result["a"])
	}
	if _, ok := result["missing"]; ok {
		t.Errorf("got missing package in result: %+v", result)
	}
}

func TestQueryWeeklyDownloadsMissingDownloadsTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "no-downloads.duckdb")
	setup, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatalf("opening fixture db: %v", err)
	}
	if _, err := setup.Exec(`CREATE TABLE edges(Name VARCHAR, Version VARCHAR, DepName VARCHAR, Requirement VARCHAR)`); err != nil {
		t.Fatalf("setting up fixture db: %v", err)
	}
	if err := setup.Close(); err != nil {
		t.Fatalf("closing fixture db: %v", err)
	}

	source, err := NewDuckDBSource(dbPath)
	if err != nil {
		t.Fatalf("NewDuckDBSource: %v", err)
	}
	defer source.Close()

	result, err := source.QueryWeeklyDownloads([]string{"a"})
	if err != nil {
		t.Errorf("got error %v, want nil for a missing downloads table", err)
	}
	if result != nil {
		t.Errorf("got %+v, want nil", result)
	}
}

func TestQueryPublishedAtMissingVersionsTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "no-versions.duckdb")
	setup, err := sql.Open("duckdb", dbPath)
	if err != nil {
		t.Fatalf("opening fixture db: %v", err)
	}
	if _, err := setup.Exec(`CREATE TABLE edges(Name VARCHAR, Version VARCHAR, DepName VARCHAR, Requirement VARCHAR)`); err != nil {
		t.Fatalf("setting up fixture db: %v", err)
	}
	if err := setup.Close(); err != nil {
		t.Fatalf("closing fixture db: %v", err)
	}

	source, err := NewDuckDBSource(dbPath)
	if err != nil {
		t.Fatalf("NewDuckDBSource: %v", err)
	}
	defer source.Close()

	result, err := source.QueryPublishedAt([]PackageVersion{{Name: "a", Version: "1.0.0"}})
	if err != nil {
		t.Errorf("got error %v, want nil for a missing versions table", err)
	}
	if result != nil {
		t.Errorf("got %+v, want nil", result)
	}
}

func sortDependents(deps []RawDependent) {
	sort.Slice(deps, func(i, j int) bool { return deps[i].DependentName < deps[j].DependentName })
}
