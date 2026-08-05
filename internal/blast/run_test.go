package blast

import (
	"bytes"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeCSV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compromised.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

func TestParseCompromisedCSV(t *testing.T) {
	path := writeCSV(t, `# compromised packages, exported 2026-08-05

axios;1.14.1,0.30.4
lodash;4.17.20

  @scope/pkg  ;  1.0.0 , 2.0.0
`)

	got, err := ParseCompromisedCSV(path, NPM)
	if err != nil {
		t.Fatalf("ParseCompromisedCSV: %v", err)
	}

	want := []TargetSpec{
		{System: NPM, Name: "axios", Versions: []string{"1.14.1", "0.30.4"}},
		{System: NPM, Name: "lodash", Versions: []string{"4.17.20"}},
		{System: NPM, Name: "@scope/pkg", Versions: []string{"1.0.0", "2.0.0"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseCompromisedCSVEmptyFile(t *testing.T) {
	got, err := ParseCompromisedCSV(writeCSV(t, "# nothing but a comment\n\n"), NPM)
	if err != nil {
		t.Fatalf("ParseCompromisedCSV: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want no targets", got)
	}
}

func TestParseCompromisedCSVErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"missing separator", "axios 1.14.1\n", "line 1"},
		{"empty package name", ";1.14.1\n", "empty package name"},
		{"no versions", "axios;\n", "no versions"},
		{"only commas for versions", "axios; , ,\n", "no versions"},
		{"error reports the right line", "ok;1.0.0\n\n# c\nbroken\n", "line 4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCompromisedCSV(writeCSV(t, tt.content), NPM)
			if err == nil {
				t.Fatalf("got nil error, want one containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseCompromisedCSVMissingFile(t *testing.T) {
	if _, err := ParseCompromisedCSV(filepath.Join(t.TempDir(), "nope.csv"), NPM); err == nil {
		t.Error("got nil error for a missing file")
	}
}

func TestFindDB(t *testing.T) {
	t.Run("explicit path wins without touching the disk", func(t *testing.T) {
		got, err := findDB("/some/where/custom.duckdb", NPM)
		if err != nil {
			t.Fatalf("findDB: %v", err)
		}
		if got != "/some/where/custom.duckdb" {
			t.Errorf("got %q, want the explicit path", got)
		}
	})

	t.Run("finds the ecosystem database in ./data", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "data"), 0o755); err != nil {
			t.Fatal(err)
		}
		dbPath := filepath.Join(dir, "data", NPM.DBName())
		if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)

		got, err := findDB("", NPM)
		if err != nil {
			t.Fatalf("findDB: %v", err)
		}
		if got != filepath.Join("data", NPM.DBName()) {
			t.Errorf("got %q, want data/%s", got, NPM.DBName())
		}
	})

	t.Run("reports what it tried when nothing is found", func(t *testing.T) {
		t.Chdir(t.TempDir())

		_, err := findDB("", NPM)
		if err == nil {
			t.Fatal("got nil error, want a not-found error")
		}
		if !strings.Contains(err.Error(), NPM.DBName()) || !strings.Contains(err.Error(), "--db") {
			t.Errorf("error = %q, want it to name the database and suggest --db", err)
		}
	})
}

// affected-packages.csv, the shape a previous run writes.
func TestParseCompromisedCSVAcceptsTheOutputShape(t *testing.T) {
	got, err := ParseCompromisedCSV(writeCSV(t, `package_name,vulnerable_versions
axios,"1.14.1,0.30.4"
lodash,4.17.20
@scope/pkg,"1.0.0,2.0.0"
`), NPM)
	if err != nil {
		t.Fatalf("ParseCompromisedCSV: %v", err)
	}

	want := []TargetSpec{
		{System: NPM, Name: "axios", Versions: []string{"1.14.1", "0.30.4"}},
		{System: NPM, Name: "lodash", Versions: []string{"4.17.20"}},
		{System: NPM, Name: "@scope/pkg", Versions: []string{"1.0.0", "2.0.0"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// A package genuinely named package_name on a later line is data, not a header.
func TestParseCompromisedCSVOnlySkipsALeadingHeader(t *testing.T) {
	got, err := ParseCompromisedCSV(writeCSV(t, "axios,1.14.1\npackage_name,1.0.0\n"), NPM)
	if err != nil {
		t.Fatalf("ParseCompromisedCSV: %v", err)
	}
	if len(got) != 2 || got[1].Name != "package_name" {
		t.Errorf("got %+v, want both rows kept", got)
	}
}

func TestParseCompromisedCSVMixesBothShapes(t *testing.T) {
	got, err := ParseCompromisedCSV(writeCSV(t, "axios;1.14.1,0.30.4\nlodash,4.17.20\n"), NPM)
	if err != nil {
		t.Fatalf("ParseCompromisedCSV: %v", err)
	}

	want := []TargetSpec{
		{System: NPM, Name: "axios", Versions: []string{"1.14.1", "0.30.4"}},
		{System: NPM, Name: "lodash", Versions: []string{"4.17.20"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestSaveArtifacts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "output", "2026-08-05_153056")
	result := sampleResult()
	sortByImpact(result.Affected)

	var progress strings.Builder
	if err := saveArtifacts(result, dir, &progress); err != nil {
		t.Fatalf("saveArtifacts: %v", err)
	}

	for _, name := range []string{"blast-radius.json", "affected-packages.csv", "paths.csv"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
		if !strings.Contains(progress.String(), name) {
			t.Errorf("progress output does not mention %s:\n%s", name, progress.String())
		}
	}

	// The command has to be copy-pasteable, so it needs the real path rather
	// than a placeholder.
	wantCommand := "blast-radius visualize " + filepath.Join(dir, "blast-radius.json")
	if !strings.Contains(progress.String(), wantCommand) {
		t.Errorf("progress output does not give the visualize command %q:\n%s", wantCommand, progress.String())
	}

	// The JSON artifact must be byte-identical to what --output json prints,
	// since that is what `visualize` consumes.
	var stdout bytes.Buffer
	if err := renderTo(result, "json", 0, &stdout); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "blast-radius.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != stdout.String() {
		t.Error("blast-radius.json differs from the --output json bytes")
	}

	// paths.csv keeps the full result, not the top-N the table shows.
	paths, err := os.ReadFile(filepath.Join(dir, "paths.csv"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(paths)).ReadAll()
	if err != nil {
		t.Fatalf("paths.csv is not valid CSV: %v", err)
	}
	if len(rows) != len(result.Affected)+1 {
		t.Errorf("paths.csv has %d rows, want header + %d", len(rows), len(result.Affected))
	}
}

func TestSaveArtifactsFailsLoudly(t *testing.T) {
	// A file where the run directory should go.
	blocked := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := saveArtifacts(sampleResult(), filepath.Join(blocked, "run"), io.Discard)
	if err == nil {
		t.Error("got nil error, want a failure rather than silently losing the results")
	}
}
