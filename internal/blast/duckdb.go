package blast

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type DuckDBSource struct {
	dbPath string
}

func NewDuckDBSource(dbPath string) (*DuckDBSource, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("database not found: %s", dbPath)
	}
	if _, err := exec.LookPath("duckdb"); err != nil {
		return nil, fmt.Errorf("duckdb CLI not found in PATH")
	}
	return &DuckDBSource{dbPath: dbPath}, nil
}

type RawDependent struct {
	DependentName    string
	DependentVersion string
	TargetName       string
	Requirement      string
}

type duckdbRow struct {
	Name        string `json:"Name"`
	Version     string `json:"Version"`
	DepName     string `json:"DepName"`
	Requirement string `json:"Requirement"`
}

// QueryDirectDependents returns all (package, version, requirement) tuples
// that directly depend on the given target package name.
func (s *DuckDBSource) QueryDirectDependents(targetName string) ([]RawDependent, error) {
	query := fmt.Sprintf(
		`SELECT Name, Version, DepName, Requirement FROM edges WHERE DepName = '%s';`,
		escapeSingleQuotes(targetName),
	)
	return s.runQuery(query)
}

// QueryDependentsOfNames returns all edges where DepName is in the given set of names.
// Uses a temp CSV file to avoid massive IN clauses.
func (s *DuckDBSource) QueryDependentsOfNames(names []string) ([]RawDependent, error) {
	if len(names) == 0 {
		return nil, nil
	}

	tmpFile, err := os.CreateTemp("", "blast-radius-frontier-*.csv")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	for _, name := range names {
		fmt.Fprintln(tmpFile, name)
	}
	tmpFile.Close()

	query := fmt.Sprintf(`
		SELECT e.Name, e.Version, e.DepName, e.Requirement
		FROM edges e
		SEMI JOIN read_csv('%s', columns={'name': 'VARCHAR'}, header=false, auto_detect=false) t
		ON e.DepName = t.name;
	`, tmpFile.Name())

	return s.runQuery(query)
}

func (s *DuckDBSource) runQuery(query string) ([]RawDependent, error) {
	cmd := exec.Command("duckdb", s.dbPath, "-json", "-c", query)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("duckdb query failed: %s", string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("duckdb query failed: %w", err)
	}

	if len(out) == 0 || strings.TrimSpace(string(out)) == "[]" {
		return nil, nil
	}

	var rows []duckdbRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("parsing duckdb output: %w", err)
	}

	results := make([]RawDependent, len(rows))
	for i, r := range rows {
		results[i] = RawDependent{
			DependentName:    r.Name,
			DependentVersion: r.Version,
			TargetName:       r.DepName,
			Requirement:      r.Requirement,
		}
	}

	return results, nil
}

func escapeSingleQuotes(s string) string {
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			result = append(result, '\'', '\'')
		} else {
			result = append(result, s[i])
		}
	}
	return string(result)
}
