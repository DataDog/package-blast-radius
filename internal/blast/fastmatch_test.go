package blast

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
)

const npmTestDBEnv = "BLAST_RADIUS_NPM_DB"

func npmTestDBPath(tb testing.TB) string {
	tb.Helper()

	if dbPath := os.Getenv(npmTestDBEnv); dbPath != "" {
		if _, err := os.Stat(dbPath); err != nil {
			tb.Skipf("%s=%s is not usable: %v", npmTestDBEnv, dbPath, err)
		}
		return dbPath
	}

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		tb.Skipf("could not locate test source; set %s to run database-backed tests", npmTestDBEnv)
	}

	dbPath := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "data", "npm-deps.duckdb"))
	if _, err := os.Stat(dbPath); err != nil {
		tb.Skipf("no database at %s; set %s to run database-backed tests", dbPath, npmTestDBEnv)
	}
	return dbPath
}

// TestFastMatcherMatchesLibrary is a differential test: it pulls real
// (Requirement, Version) pairs from the local DuckDB database and checks that
// the fast matcher returns the same result as the library's Constraints.Check
// for every pair. If this passes, the fast matcher is a faithful replacement
// and analyze output is unchanged.
//
// Run with:
//
//	go test ./internal/blast -run TestFastMatcherMatchesLibrary -v -timeout=10m
func TestFastMatcherMatchesLibrary(t *testing.T) {
	dbPath := npmTestDBPath(t)
	db, err := sql.Open("duckdb", dbPath+"?access_mode=read_only")
	if err != nil {
		t.Skipf("no database at %s: %v", dbPath, err)
	}
	defer db.Close()

	// Sample a wide variety of real constraint/version pairs. We sample by
	// querying distinct (Requirement, Version) tuples — the two columns that
	// MatchesVersion receives. Sampling 200k pairs gives broad coverage of
	// real-world npm constraint strings.
	rows, err := db.Query(`
		SELECT Requirement, Version FROM edges USING SAMPLE 200000
	`)
	if err != nil {
		t.Skipf("could not query edges: %v", err)
	}
	defer rows.Close()

	var checked, mismatches int
	for rows.Next() {
		var req, ver string
		if err := rows.Scan(&req, &ver); err != nil {
			t.Fatalf("scan: %v", err)
		}
		checked++

		// Compute the library result (the old code path).
		constraintStr := convertHyphenRange(req)
		constraintStr = trimAndDefault(constraintStr)
		libResult := libraryCheck(constraintStr, ver)
		fastResult := MatchesVersion(NPM, req, ver)

		if libResult != fastResult {
			mismatches++
			if mismatches <= 20 {
				t.Errorf("MISMATCH req=%q ver=%q: library=%v fast=%v", req, ver, libResult, fastResult)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	t.Logf("checked %d pairs, %d mismatches", checked, mismatches)
	if mismatches > 0 {
		t.Fatalf("%d mismatches out of %d pairs", mismatches, checked)
	}
}

// TestFastMatcherMatchesLibrarySynthetic tests the fast matcher against the
// library with a curated set of edge cases, without needing the database.
func TestFastMatcherMatchesLibrarySynthetic(t *testing.T) {
	pairs := []struct{ constraint, version string }{
		// Caret
		{"^1.2.3", "1.2.3"}, {"^1.2.3", "1.5.0"}, {"^1.2.3", "2.0.0"},
		{"^1.2.3", "1.2.2"}, {"^1.2.3", "1.2.3-beta"}, {"^1.2.3", "1.2.3-beta.1"},
		{"^0.2.3", "0.2.3"}, {"^0.2.3", "0.2.4"}, {"^0.2.3", "0.3.0"},
		{"^0.0.3", "0.0.3"}, {"^0.0.3", "0.0.4"}, {"^0.0.3", "0.1.0"},
		{"^1", "1.5.0"}, {"^1", "2.0.0"}, {"^0", "0.5.0"}, {"^0", "1.0.0"},
		{"^1.2", "1.2.0"}, {"^1.2", "1.3.0"}, {"^1.2", "2.0.0"},
		// Tilde
		{"~1.2.3", "1.2.3"}, {"~1.2.3", "1.2.9"}, {"~1.2.3", "1.3.0"},
		{"~1.2", "1.2.0"}, {"~1.2", "1.3.0"}, {"~1", "1.0.0"}, {"~1", "2.0.0"},
		{"~0.0.0", "0.0.0"}, {"~0.0.0", "1.0.0"},
		// Comparison operators
		{">=1.0.0", "1.0.0"}, {">=1.0.0", "0.9.0"}, {">=1.0.0", "2.0.0"},
		{">1.0.0", "1.0.0"}, {">1.0.0", "1.0.1"}, {">1.0.0", "0.9.0"},
		{"<2.0.0", "1.0.0"}, {"<2.0.0", "2.0.0"}, {"<2.0.0", "2.0.1"},
		{"<=2.0.0", "2.0.0"}, {"<=2.0.0", "2.0.1"},
		// Exact / equality
		{"1.2.3", "1.2.3"}, {"1.2.3", "1.2.4"}, {"=1.2.3", "1.2.3"},
		// Not equal
		{"!=1.2.3", "1.2.3"}, {"!=1.2.3", "1.2.4"},
		{"!=1.x", "1.0.0"}, {"!=1.x", "2.0.0"}, {"!=1.2.x", "1.2.0"}, {"!=1.2.x", "1.3.0"},
		// X-ranges
		{"1.x", "1.0.0"}, {"1.x", "2.0.0"}, {"1.2.x", "1.2.0"}, {"1.2.x", "1.3.0"},
		{"*", "1.0.0"}, {"*", "1.0.0-beta"}, {"", "1.0.0"}, {"", "1.0.0-beta"},
		// AND / OR groups
		{">=1.0.0 <2.0.0", "1.5.0"}, {">=1.0.0 <2.0.0", "2.0.0"},
		{"^1.0.0 || ^2.0.0", "1.5.0"}, {"^1.0.0 || ^2.0.0", "2.5.0"},
		{"^1.0.0 || ^2.0.0", "3.0.0"},
		// Prereleases
		{"1.2.3-beta.1", "1.2.3-beta.1"}, {"1.2.3-beta.1", "1.2.3"},
		{"^1.0.0", "1.5.0-beta.1"}, {">=1.0.0", "1.5.0-beta.1"},
		// Hyphen ranges (converted by convertHyphenRange)
		{"1.0.0 - 2.0.0", "1.5.0"}, {"1.0.0 - 2.0.0", "2.0.0"}, {"1.0.0 - 2.0.0", "2.0.1"},
		// Operator aliases
		{"~>1.2.3", "1.2.4"}, {"~>1.2.3", "1.3.0"},
		{"=>1.0.0", "1.0.0"}, {"=>1.0.0", "0.9.0"},
		{"=<2.0.0", "2.0.0"}, {"=<2.0.0", "2.0.1"},
		// Malformed input the library rejects (must return false)
		{"^1 ||", "1.0.0"}, {">=1.0.0<2.0.0", "1.5.0"}, {">=1.0.0 ,", "1.0.0"},
		// Build metadata
		{"1.2.3", "1.2.3+build.5"},
		// v-prefix
		{"v1.2.3", "1.2.3"}, {"^v1.0.0", "1.5.0"},
	}

	var mismatches int
	for _, p := range pairs {
		constraintStr := convertHyphenRange(p.constraint)
		constraintStr = trimAndDefault(constraintStr)
		libResult := libraryCheck(constraintStr, p.version)
		fastResult := MatchesVersion(NPM, p.constraint, p.version)
		if libResult != fastResult {
			mismatches++
			t.Errorf("MISMATCH constraint=%q version=%q: library=%v fast=%v",
				p.constraint, p.version, libResult, fastResult)
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d mismatches in synthetic test", mismatches)
	}
}

// trimAndDefault mirrors the pre-processing in npmMatches that happens before
// the constraint is cached.
func trimAndDefault(s string) string {
	s = trimSpace(s)
	if s == "latest" {
		return "latest"
	}
	if s == "" {
		return "*"
	}
	return s
}

func trimSpace(s string) string {
	// strings.TrimSpace without importing strings in this helper
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// libraryCheck replicates the old npmMatches code path: parse with the library,
// check with Constraints.Check.
func libraryCheck(constraintStr, version string) bool {
	if constraintStr == "latest" {
		return true
	}
	if hasGitOrURLPrefix(constraintStr) {
		return false
	}
	c, ok := getCachedConstraint(constraintStr)
	if !ok {
		return false
	}
	v, ok := getCachedVersion(version)
	if !ok {
		return false
	}
	return c.Check(v)
}

func hasGitOrURLPrefix(s string) bool {
	return startsWith(s, "git") || startsWith(s, "http") ||
		startsWith(s, "file:") || startsWith(s, "/")
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// Ensure the test binary can find the database for the sampling test.
