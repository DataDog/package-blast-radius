package blast

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// QueryDirectDependents hands yield every (package, version, requirement) tuple
// that directly depends on the given target package name.
func (s *DuckDBSource) QueryDirectDependents(targetName string, yield func(RawDependent) error) error {
	query := fmt.Sprintf(
		`SELECT Name, Version, DepName, Requirement FROM edges WHERE DepName = '%s';`,
		escapeSingleQuotes(targetName),
	)
	return s.streamQuery(query, yield)
}

// QueryDependentsOfNames hands yield every edge whose DepName is in the given
// set of names. Uses a temp CSV file to avoid massive IN clauses.
func (s *DuckDBSource) QueryDependentsOfNames(names []string, yield func(RawDependent) error) error {
	if len(names) == 0 {
		return nil
	}

	tmpFile, err := os.CreateTemp("", "blast-radius-frontier-*.csv")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
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

	return s.streamQuery(query, yield)
}

// stderrLimit caps what is kept from a failing duckdb run, which is enough for
// the error message without letting a chatty failure grow unbounded.
const stderrLimit = 8 << 10

// streamQuery runs a query and hands each row to yield as it is decoded, rather
// than collecting the result. A popular package has millions of direct
// dependents, so materialising one costs gigabytes before any filtering starts.
func (s *DuckDBSource) streamQuery(query string, yield func(RawDependent) error) error {
	cmd := exec.Command("duckdb", s.dbPath, "-json", "-c", query)

	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, remaining: stderrLimit}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("duckdb query failed: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("duckdb query failed: %w", err)
	}

	decodeErr := decodeRows(stdout, yield)
	// duckdb blocks on a full pipe if decoding stopped early, so drain the rest
	// before waiting for it to exit.
	io.Copy(io.Discard, stdout)

	if err := cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("duckdb query failed: %s", msg)
		}
		return fmt.Errorf("duckdb query failed: %w", err)
	}
	return decodeErr
}

// decodeRows reads the JSON array duckdb writes one element at a time.
func decodeRows(r io.Reader, yield func(RawDependent) error) error {
	dec := json.NewDecoder(r)

	open, err := dec.Token()
	if err == io.EOF {
		return nil // duckdb prints nothing at all for an empty result
	}
	if err != nil {
		return fmt.Errorf("parsing duckdb output: %w", err)
	}
	if open != json.Delim('[') {
		return fmt.Errorf("parsing duckdb output: got %v, want a JSON array", open)
	}

	for dec.More() {
		var row duckdbRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("parsing duckdb output: %w", err)
		}
		err := yield(RawDependent{
			DependentName:    row.Name,
			DependentVersion: row.Version,
			TargetName:       row.DepName,
			Requirement:      row.Requirement,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type limitedWriter struct {
	w         io.Writer
	remaining int
}

// Write reports every byte as accepted so the writer never stalls its producer,
// while only the first l.remaining of them reach the underlying writer.
func (l *limitedWriter) Write(p []byte) (int, error) {
	kept := p
	if len(kept) > l.remaining {
		kept = kept[:max(l.remaining, 0)]
	}
	n, err := l.w.Write(kept)
	l.remaining -= n
	if err != nil {
		return n, err
	}
	return len(p), nil
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
