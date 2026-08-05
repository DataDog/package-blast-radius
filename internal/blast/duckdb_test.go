package blast

import "testing"

// Package names are interpolated into SQL, so quote escaping is the only thing
// standing between a package like "foo'; DROP TABLE edges;--" and the query.
func TestEscapeSingleQuotes(t *testing.T) {
	tests := map[string]string{
		"axios":                  "axios",
		"@scope/pkg":             "@scope/pkg",
		"o'brien":                "o''brien",
		"''":                     "''''",
		"'; DROP TABLE edges;--": "''; DROP TABLE edges;--",
		"":                       "",
	}
	for in, want := range tests {
		if got := escapeSingleQuotes(in); got != want {
			t.Errorf("escapeSingleQuotes(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQueryDependentsOfNamesIsEmptyForNoNames(t *testing.T) {
	s := &DuckDBSource{dbPath: "/nonexistent.duckdb"}

	got, err := s.QueryDependentsOfNames(nil)
	if err != nil {
		t.Errorf("got error %v, want nil", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestNewDuckDBSourceRejectsMissingDatabase(t *testing.T) {
	if _, err := NewDuckDBSource(t.TempDir() + "/missing.duckdb"); err == nil {
		t.Error("got nil error for a missing database file")
	}
}
