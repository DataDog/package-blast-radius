package blast

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

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

	err := s.QueryDependentsOfNames(nil, func(RawDependent) error {
		t.Error("got a row for an empty name set")
		return nil
	})
	if err != nil {
		t.Errorf("got error %v, want nil", err)
	}
}

func TestDecodeRows(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []RawDependent
	}{
		{"no output at all", "", nil},
		{"empty array", "[]", nil},
		{
			"two rows",
			`[{"Name":"a","Version":"1.0.0","DepName":"axios","Requirement":"^1.0.0"},
			  {"Name":"b","Version":"2.0.0","DepName":"axios","Requirement":"~1.2.0"}]`,
			[]RawDependent{
				{DependentName: "a", DependentVersion: "1.0.0", TargetName: "axios", Requirement: "^1.0.0"},
				{DependentName: "b", DependentVersion: "2.0.0", TargetName: "axios", Requirement: "~1.2.0"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []RawDependent
			err := decodeRows(strings.NewReader(tt.input), func(d RawDependent) error {
				got = append(got, d)
				return nil
			})
			if err != nil {
				t.Fatalf("decodeRows: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestDecodeRowsStopsOnYieldError(t *testing.T) {
	rows := `[{"Name":"a"},{"Name":"b"},{"Name":"c"}]`
	stop := errors.New("enough")

	seen := 0
	err := decodeRows(strings.NewReader(rows), func(RawDependent) error {
		seen++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Errorf("got error %v, want %v", err, stop)
	}
	if seen != 1 {
		t.Errorf("yield was called %d times, want it to stop after the first error", seen)
	}
}

func TestDecodeRowsRejectsNonArrayOutput(t *testing.T) {
	err := decodeRows(strings.NewReader(`{"error":"boom"}`), func(RawDependent) error { return nil })
	if err == nil {
		t.Error("got nil error for output that is not a JSON array")
	}
}

// A failing duckdb run can write a lot to stderr, and all of it would otherwise
// end up in the error message.
func TestLimitedWriterTruncatesButAcceptsEverything(t *testing.T) {
	var buf bytes.Buffer
	w := &limitedWriter{w: &buf, remaining: 5}

	for range 3 {
		n, err := w.Write([]byte("abcd"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != 4 {
			t.Errorf("Write reported %d bytes, want 4 so the producer never stalls", n)
		}
	}
	if buf.String() != "abcda" {
		t.Errorf("kept %q, want the first 5 bytes", buf.String())
	}
}

func TestNewDuckDBSourceRejectsMissingDatabase(t *testing.T) {
	if _, err := NewDuckDBSource(t.TempDir() + "/missing.duckdb"); err == nil {
		t.Error("got nil error for a missing database file")
	}
}
