package blast

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options configures a blast radius computation.
type Options struct {
	System          Ecosystem
	Targets         []TargetSpec
	DBPath          string // empty = search the conventional locations
	Format          string // table, json, csv
	EnrichDownloads bool
	Top             int
	MaxDepth        int
	OutputDir       string // empty = don't save artifacts; the caller owns the naming
	Stdout          io.Writer
	Progress        io.Writer
}

// ParseCompromisedCSV reads one package per line, in either of two shapes:
//
//	package_name;version1,version2,...    (semicolon-separated, no header)
//	package_name,"version1,version2"      (the affected-packages.csv an analyze run writes)
//
// Blank lines, lines starting with '#', and a leading "package_name,..." header
// are ignored.
func ParseCompromisedCSV(path string, system Ecosystem) ([]TargetSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var targets []TargetSpec
	reader := csv.NewReader(bufio.NewReader(f))
	// Rows are 1 or 2 fields depending on the shape, and the semicolon form is
	// a single field that we split ourselves.
	reader.FieldsPerRecord = -1
	// Comments are free text and may contain a stray quote.
	reader.LazyQuotes = true

	isFirstRecord := true
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		// The reader skips blank lines, so track the real file line for errors.
		lineNum, _ := reader.FieldPos(0)

		line := strings.TrimSpace(strings.Join(record, ","))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := splitCompromisedRecord(record)
		if parts == nil {
			return nil, fmt.Errorf("line %d: expected 'package;v1,v2,...' or 'package,\"v1,v2\"' got %q", lineNum, line)
		}
		if isFirstRecord && parts[0] == "package_name" {
			isFirstRecord = false
			continue
		}
		isFirstRecord = false

		name := strings.TrimSpace(parts[0])
		if name == "" {
			return nil, fmt.Errorf("line %d: empty package name", lineNum)
		}
		var vers []string
		for _, v := range strings.Split(parts[1], ",") {
			v = strings.TrimSpace(v)
			if v != "" {
				vers = append(vers, v)
			}
		}
		if len(vers) == 0 {
			return nil, fmt.Errorf("line %d: no versions for %q", lineNum, name)
		}
		targets = append(targets, TargetSpec{System: system, Name: name, Versions: vers})
	}
	return targets, nil
}

// splitCompromisedRecord returns the package name and its comma-joined
// versions, or nil if the record is neither supported shape. Package names
// contain no ';' or ',', so a semicolon in the first field is unambiguous.
func splitCompromisedRecord(record []string) []string {
	if strings.Contains(record[0], ";") {
		parts := strings.SplitN(record[0], ";", 2)
		// A semicolon line the CSV reader split on stray commas, e.g.
		// "axios;1.0.0, 2.0.0" -> ["axios;1.0.0", " 2.0.0"].
		parts[1] = strings.Join(append([]string{parts[1]}, record[1:]...), ",")
		return parts
	}
	if len(record) >= 2 {
		return []string{record[0], strings.Join(record[1:], ",")}
	}
	return nil
}

func findDB(explicit string, system Ecosystem) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	dbName := system.DBName()
	candidates := []string{
		dbName,
		filepath.Join("data", dbName),
	}

	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".blast-radius", dbName))
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("database not found; tried %v\nUse --db to specify the path", candidates)
}

// Run computes the blast radius for opts.Targets and renders the result.
func Run(ctx context.Context, opts Options) error {
	start := time.Now()

	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	progress := opts.Progress
	if progress == nil {
		progress = os.Stderr
	}

	system := opts.System
	maxDepth := opts.MaxDepth

	var expanded []PackageVersion
	for _, t := range opts.Targets {
		for _, v := range t.Versions {
			expanded = append(expanded, PackageVersion{System: system, Name: t.Name, Version: v})
		}
	}

	if len(opts.Targets) == 1 {
		t := opts.Targets[0]
		fmt.Fprintf(progress, "Computing blast radius for %s@%s (%s), depth=%d\n\n",
			t.Name, strings.Join(t.Versions, ","), system, maxDepth)
	} else {
		fmt.Fprintf(progress, "Computing blast radius for %d compromised packages (%d versions total, %s), depth=%d\n\n",
			len(opts.Targets), len(expanded), system, maxDepth)
	}

	resolvedDB, err := findDB(opts.DBPath, system)
	if err != nil {
		return err
	}
	fmt.Fprintf(progress, "Using database: %s\n", resolvedDB)

	source, err := NewDuckDBSource(resolvedDB)
	if err != nil {
		return err
	}

	totalEdges := 0
	var allAffected []AffectedPackage
	visited := make(map[string]bool)

	type frontierEntry struct {
		pkg    PackageVersion
		path   []PathStep
		target PackageVersion // the root compromised package this entry traces to
	}

	var frontier []frontierEntry
	for _, pv := range expanded {
		frontier = append(frontier, frontierEntry{pkg: pv, path: nil, target: pv})
		visited[pv.String()] = true
	}

	for d := 0; d < maxDepth && len(frontier) > 0; d++ {
		nameSet := make(map[string]bool)
		for _, f := range frontier {
			nameSet[f.pkg.Name] = true
		}
		names := make([]string, 0, len(nameSet))
		for n := range nameSet {
			names = append(names, n)
		}

		fmt.Fprintf(progress, "Depth %d: querying dependents of %d package(s)...\n", d+1, len(names))

		var rawDeps []RawDependent
		if len(names) == 1 {
			rawDeps, err = source.QueryDirectDependents(names[0])
		} else {
			rawDeps, err = source.QueryDependentsOfNames(names)
		}
		if err != nil {
			return fmt.Errorf("query at depth %d failed: %w", d+1, err)
		}
		totalEdges += len(rawDeps)

		fmt.Fprintf(progress, "Depth %d: got %d edges, filtering by version range...\n", d+1, len(rawDeps))

		frontierByName := make(map[string][]frontierEntry)
		for _, f := range frontier {
			frontierByName[f.pkg.Name] = append(frontierByName[f.pkg.Name], f)
		}

		var nextFrontier []frontierEntry

		for _, dep := range rawDeps {
			key := dep.DependentName + "@" + dep.DependentVersion
			if visited[key] {
				continue
			}

			parents := frontierByName[dep.TargetName]
			var matchedParent *frontierEntry
			for i := range parents {
				if MatchesVersion(system, dep.Requirement, parents[i].pkg.Version) {
					matchedParent = &parents[i]
					break
				}
			}
			if matchedParent == nil {
				continue
			}

			visited[key] = true

			path := make([]PathStep, 0, len(matchedParent.path)+1)
			path = append(path, PathStep{
				Package:     dep.DependentName,
				Version:     dep.DependentVersion,
				Requirement: dep.Requirement,
			})
			path = append(path, matchedParent.path...)

			depPkg := PackageVersion{
				System:  system,
				Name:    dep.DependentName,
				Version: dep.DependentVersion,
			}

			allAffected = append(allAffected, AffectedPackage{
				PackageVersion:  depPkg,
				Depth:           d + 1,
				Path:            path,
				Target:          matchedParent.target,
				WeeklyDownloads: -1,
			})

			nextFrontier = append(nextFrontier, frontierEntry{
				pkg:    depPkg,
				path:   path,
				target: matchedParent.target,
			})
		}

		fmt.Fprintf(progress, "Depth %d: found %d new affected packages\n", d+1, len(nextFrontier))
		frontier = nextFrontier
	}

	uniqueNames := make(map[string]bool)
	for _, a := range allAffected {
		uniqueNames[a.Name] = true
	}

	fmt.Fprintf(progress, "\nTotal: %d affected versions (%d unique packages)\n", len(allAffected), len(uniqueNames))

	if opts.EnrichDownloads && len(allAffected) > 0 {
		if !system.SupportsEnrichment() {
			fmt.Fprintf(progress, "warning: download counts are not available for %s\n", system)
		} else {
			fmt.Fprintf(progress, "Enriching %d unique packages with download counts...\n", len(uniqueNames))
			if err := Enrich(ctx, system, allAffected, 20); err != nil {
				fmt.Fprintf(progress, "warning: %v\n", err)
			}
		}
	}

	result := &BlastResult{
		Targets:        expanded,
		TotalEdges:     totalEdges,
		Affected:       allAffected,
		UniquePackages: len(uniqueNames),
		MaxDepth:       maxDepth,
		Elapsed:        time.Since(start),
	}

	if err := RenderResults(result, opts.Format, opts.Top, stdout); err != nil {
		return err
	}

	if opts.OutputDir != "" {
		return saveArtifacts(result, opts.OutputDir, progress)
	}
	return nil
}

// artifacts are written on every run so an investigation keeps its results
// without the caller having to remember to redirect stdout.
var artifacts = []struct {
	name   string
	format string
	unit   func(r *BlastResult) string
}{
	{"blast-radius.json", "json", func(r *BlastResult) string { return "feeds `blast-radius visualize`" }},
	{"affected-packages.csv", "affected-packages", func(r *BlastResult) string {
		return FormatNumber(int64(r.UniquePackages)) + " packages"
	}},
	{"paths.csv", "csv", func(r *BlastResult) string {
		return FormatNumber(int64(len(r.Affected))) + " rows"
	}},
}

func saveArtifacts(result *BlastResult, dir string, progress io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	fmt.Fprintf(progress, "\nSaved to %s%c\n", dir, os.PathSeparator)
	for _, a := range artifacts {
		path := filepath.Join(dir, a.name)
		if err := writeArtifact(path, result, a.format); err != nil {
			return err
		}
		fmt.Fprintf(progress, "  %-22s %s\n", a.name, a.unit(result))
	}
	return nil
}

func writeArtifact(path string, result *BlastResult, format string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()

	buf := bufio.NewWriter(f)
	if err := renderTo(result, format, 0, buf); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := buf.Flush(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return f.Close()
}
