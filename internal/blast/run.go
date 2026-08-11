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
	// EnrichScopedPackages opts into exact npm download-count enrichment for
	// scoped packages even when there are many. Scoped names cannot use npm's
	// bulk downloads endpoint.
	EnrichScopedPackages bool

	// ReportName is an optional title shown as the heading in the viewer
	// instead of the synthesized compromised-package count. Empty keeps the
	// default behavior.
	ReportName string
}

type dependentSource interface {
	QueryDirectDependents(string, func(RawDependent) error) error
	QueryDependentsOfNames([]string, func(RawDependent) error) error
	QueryPublishedAt([]PackageVersion) (map[string]time.Time, error)
}

type weeklyDownloadSource interface {
	QueryWeeklyDownloads([]string) (map[string]int64, error)
}

// parseBundledName splits deps.dev's synthetic bundled-node name, e.g.
// "cloudstructs>0.6.11>@types/keyv", into the bundling root and the version it
// bundled. Real npm names never contain '>', so three '>'-separated parts is
// unambiguously a synthetic node.
func parseBundledName(name string) (rootName, rootVersion string, ok bool) {
	parts := strings.SplitN(name, ">", 3)
	if len(parts) < 3 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ParseCompromisedCSV reads one package per line in either shape:
//
//	package_name;version1,version2,...   (semicolon-separated, no header)
//	package_name,"version1,version2"     (an affected-packages.csv from a prior run)
//
// Blank lines, '#'-comments, and a "package_name,..." header are ignored.
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

		name := NormalizePackageName(system, strings.TrimSpace(parts[0]))
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

// splitCompromisedRecord returns [name, comma-joined-versions], or nil if the
// record is neither shape. Package names contain no ';' or ',', so a
// semicolon in the first field is unambiguous.
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

	targets := normalizeTargetSpecs(system, opts.Targets)
	expanded := expandTargets(system, targets)

	if len(targets) == 1 {
		t := targets[0]
		fmt.Fprintf(progress, "Computing blast radius for %s@%s (%s), depth=%d\n\n",
			t.Name, strings.Join(t.Versions, ","), system, maxDepth)
	} else {
		fmt.Fprintf(progress, "Computing blast radius for %d compromised packages (%d versions total, %s), depth=%d\n\n",
			len(targets), len(expanded), system, maxDepth)
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
	defer source.Close()

	result, err := computeBlastRadius(system, expanded, maxDepth, source, progress, start)
	if err != nil {
		return err
	}

	localDownloadsAvailable, localDownloadsMissing, err := applyLocalDownloadCounts(source, result.Affected)
	if err != nil {
		fmt.Fprintf(progress, "warning: local download counts could not be read: %v\n", err)
	}
	if localDownloadsAvailable {
		resolved := result.UniquePackages - localDownloadsMissing
		fmt.Fprintf(progress, "Applied local download counts for %d/%d unique packages.\n", resolved, result.UniquePackages)
	}

	if opts.EnrichDownloads && len(result.Affected) > 0 {
		if localDownloadsAvailable && localDownloadsMissing == 0 {
			fmt.Fprintf(progress, "Download counts are already available locally; no registry requests needed.\n")
		} else if !system.SupportsEnrichment() {
			if system.SupportsDownloadCountDataset() {
				if localDownloadsAvailable {
					fmt.Fprintf(progress, "warning: local %s download counts are missing for %d unique package(s); no registry fallback is available.\n",
						system, localDownloadsMissing)
				} else {
					fmt.Fprintf(progress, "warning: %s download counts are only available from local ingestion; rebuild the database with download-data %s --include-download-counts.\n",
						system, strings.ToLower(string(system)))
				}
			} else {
				fmt.Fprintf(progress, "warning: download counts are not available for %s\n", system)
			}
		} else {
			if system == NPM && npmAuthToken == "" {
				fmt.Fprintf(progress, "warning: NPM_TOKEN is not set; the npm downloads API will heavily rate-limit unauthenticated requests. Set NPM_TOKEN to lift it.\n")
			}
			toEnrich := packagesMissingDownloadCounts(result.Affected)
			fmt.Fprintf(progress, "Enriching %d unique packages with download counts...\n", len(toEnrich))
			if err := Enrich(ctx, system, toEnrich, EnrichOptions{
				Workers:                system.DefaultEnrichWorkers(),
				Rate:                   system.DefaultEnrichRate(),
				Progress:               progress,
				IncludeScopedPackages:  opts.EnrichScopedPackages,
				ScopedPackageOptInFlag: "--enrich-scoped-packages",
			}); err != nil {
				fmt.Fprintf(progress, "warning: %v\n", err)
			}
			applyResolvedDownloadCounts(result.Affected, toEnrich)
		}
	}
	result.Elapsed = time.Since(start)
	result.ReportName = opts.ReportName

	if err := RenderResults(result, opts.Format, opts.Top, stdout); err != nil {
		return err
	}

	if opts.OutputDir != "" {
		return SaveArtifacts(result, opts.OutputDir, progress)
	}
	return nil
}

func applyLocalDownloadCounts(source dependentSource, affected []AffectedPackage) (available bool, missing int, err error) {
	downloadSource, ok := source.(weeklyDownloadSource)
	if !ok || len(affected) == 0 {
		return false, 0, nil
	}

	names := uniqueAffectedNames(affected)
	counts, err := downloadSource.QueryWeeklyDownloads(names)
	if err != nil {
		return false, 0, err
	}
	if counts == nil {
		return false, 0, nil
	}

	for i := range affected {
		if downloads, ok := counts[affected[i].Name]; ok {
			affected[i].WeeklyDownloads = downloads
		}
	}
	for _, name := range names {
		if _, ok := counts[name]; !ok {
			missing++
		}
	}
	return true, missing, nil
}

func uniqueAffectedNames(affected []AffectedPackage) []string {
	seen := make(map[string]struct{})
	names := make([]string, 0, len(affected))
	for _, a := range affected {
		if _, ok := seen[a.Name]; ok {
			continue
		}
		seen[a.Name] = struct{}{}
		names = append(names, a.Name)
	}
	return names
}

func packagesMissingDownloadCounts(affected []AffectedPackage) []AffectedPackage {
	seen := make(map[string]struct{})
	var out []AffectedPackage
	for _, a := range affected {
		if a.WeeklyDownloads >= 0 {
			continue
		}
		if _, ok := seen[a.Name]; ok {
			continue
		}
		seen[a.Name] = struct{}{}
		out = append(out, AffectedPackage{
			PackageVersion:  PackageVersion{System: a.System, Name: a.Name},
			WeeklyDownloads: -1,
		})
	}
	return out
}

func applyResolvedDownloadCounts(affected, enriched []AffectedPackage) {
	counts := make(map[string]int64)
	for _, a := range enriched {
		if a.WeeklyDownloads >= 0 {
			counts[a.Name] = a.WeeklyDownloads
		}
	}
	for i := range affected {
		if downloads, ok := counts[affected[i].Name]; ok {
			affected[i].WeeklyDownloads = downloads
		}
	}
}

func expandTargets(system Ecosystem, targets []TargetSpec) []PackageVersion {
	var expanded []PackageVersion
	for _, t := range targets {
		for _, v := range t.Versions {
			expanded = append(expanded, PackageVersion{System: system, Name: t.Name, Version: v})
		}
	}
	return expanded
}

func normalizeTargetSpecs(system Ecosystem, targets []TargetSpec) []TargetSpec {
	out := make([]TargetSpec, len(targets))
	for i, t := range targets {
		out[i] = t
		out[i].System = system
		out[i].Name = NormalizePackageName(system, t.Name)
	}
	return out
}

// matchKey identifies a declared dependency on a frontier package: the same
// (name, range) resolves to the same frontier entry within a depth. A
// comparable struct keeps lookups allocation-free.
type matchKey struct {
	name        string
	requirement string
}

// nameVersion is a comparable key for the visited/bundledSeen maps, avoiding
// the string allocation of name + "@" + version on every edge.
type nameVersion struct {
	name    string
	version string
}

// noMatchingParent marks a matchKey no frontier version satisfies, so
// negatives cache as cheaply as positives.
const noMatchingParent = -1

func computeBlastRadius(system Ecosystem, expanded []PackageVersion, maxDepth int, source dependentSource, progress io.Writer, start time.Time) (*BlastResult, error) {
	if progress == nil {
		progress = io.Discard
	}

	totalEdges := 0
	var allAffected []AffectedPackage
	visited := make(map[nameVersion]bool)

	type frontierEntry struct {
		pkg    PackageVersion
		path   []PathStep
		target PackageVersion // the root compromised package this entry traces to
	}

	var frontier []frontierEntry
	for _, pv := range expanded {
		frontier = append(frontier, frontierEntry{pkg: pv, path: nil, target: pv})
		visited[nameVersion{pv.Name, pv.Version}] = true
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

		frontierByName := make(map[string][]frontierEntry)
		for _, f := range frontier {
			frontierByName[f.pkg.Name] = append(frontierByName[f.pkg.Name], f)
		}

		// A (package, requirement) pair resolves to one frontier entry for the whole
		// depth, and popular pairs recur across thousands of edges — caching avoids
		// re-running semver matching each time. Scoped per depth: the indices point
		// into frontierByName's slices, rebuilt every iteration.
		resolvedParent := make(map[matchKey]int)

		var nextFrontier []frontierEntry
		edges := 0

		// Bundled edges (deps.dev's "root>version>local" name, see parseBundledName)
		// can't be committed yet: whether the root shipped the compromised version
		// depends on publish dates known only after the full depth scan.
		type bundledCandidate struct {
			root          PackageVersion
			matchedParent frontierEntry
		}
		var bundledCandidates []bundledCandidate
		bundledSeen := make(map[nameVersion]bool)

		// Filter edges as they arrive rather than collecting: a popular package has
		// millions of direct dependents, and a whole depth's worth is gigabytes.
		keepIfAffected := func(dep RawDependent) error {
			edges++

			parents := frontierByName[dep.TargetName]
			resolveKey := matchKey{name: dep.TargetName, requirement: dep.Requirement}
			matched, cached := resolvedParent[resolveKey]
			if !cached {
				matched = noMatchingParent
				for i := range parents {
					if MatchesVersion(system, dep.Requirement, parents[i].pkg.Version) {
						matched = i
						break
					}
				}
				resolvedParent[resolveKey] = matched
			}
			if matched == noMatchingParent {
				return nil
			}
			matchedParent := &parents[matched]

			if system == NPM {
				rootName, rootVersion, ok := parseBundledName(dep.DependentName)
				if ok {
					root := PackageVersion{System: system, Name: rootName, Version: rootVersion}
					nvKey := nameVersion{rootName, rootVersion}
					if visited[nvKey] || bundledSeen[nvKey] {
						return nil
					}
					bundledSeen[nvKey] = true
					bundledCandidates = append(bundledCandidates, bundledCandidate{
						root: root, matchedParent: *matchedParent,
					})
					return nil
				}
			}

			key := nameVersion{dep.DependentName, dep.DependentVersion}
			if visited[key] {
				return nil
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
			return nil
		}

		if len(names) == 1 {
			err := source.QueryDirectDependents(names[0], keepIfAffected)
			if err != nil {
				return nil, fmt.Errorf("query at depth %d failed: %w", d+1, err)
			}
		} else {
			err := source.QueryDependentsOfNames(names, keepIfAffected)
			if err != nil {
				return nil, fmt.Errorf("query at depth %d failed: %w", d+1, err)
			}
		}
		totalEdges += edges

		// Include a bundled root unless its publish date provably predates the
		// target's: bundling freezes the whole resolved subtree at once, so the
		// target version had to exist by the root's publish date regardless of
		// which intermediate version we matched through. When publish dates are
		// unavailable, include the candidate — hiding a real compromise is worse
		// than showing a safe one. See README "Bundled npm dependencies".
		if len(bundledCandidates) > 0 {
			lookups := make([]PackageVersion, 0, len(bundledCandidates)*2)
			for _, c := range bundledCandidates {
				lookups = append(lookups, c.root, c.matchedParent.target)
			}
			dates, err := source.QueryPublishedAt(lookups)
			if err != nil {
				return nil, fmt.Errorf("looking up publish dates at depth %d: %w", d+1, err)
			}
			for _, c := range bundledCandidates {
				nvKey := nameVersion{c.root.Name, c.root.Version}
				if visited[nvKey] {
					continue
				}
				rootDate, rootOK := dates[c.root.String()]
				targetDate, targetOK := dates[c.matchedParent.target.String()]
				if rootOK && targetOK && rootDate.Before(targetDate) {
					continue // the target version did not exist yet when the bundle was frozen
				}

				visited[nvKey] = true
				path := make([]PathStep, 0, len(c.matchedParent.path)+1)
				path = append(path, PathStep{
					Package:     c.root.Name,
					Version:     c.root.Version,
					Requirement: "bundled",
				})
				path = append(path, c.matchedParent.path...)

				allAffected = append(allAffected, AffectedPackage{
					PackageVersion:  c.root,
					Depth:           d + 1,
					Path:            path,
					Target:          c.matchedParent.target,
					WeeklyDownloads: -1,
				})
				nextFrontier = append(nextFrontier, frontierEntry{
					pkg:    c.root,
					path:   path,
					target: c.matchedParent.target,
				})
			}
		}

		fmt.Fprintf(progress, "Depth %d: scanned %d edges, found %d new affected packages\n",
			d+1, edges, len(nextFrontier))
		frontier = nextFrontier
	}

	uniqueNames := make(map[string]bool)
	for _, a := range allAffected {
		uniqueNames[a.Name] = true
	}

	fmt.Fprintf(progress, "\nTotal: %d affected versions (%d unique packages)\n", len(allAffected), len(uniqueNames))

	result := &BlastResult{
		Targets:        expanded,
		TotalEdges:     totalEdges,
		Affected:       allAffected,
		UniquePackages: len(uniqueNames),
		MaxDepth:       maxDepth,
		Elapsed:        time.Since(start),
	}

	return result, nil
}

// reportArtifact is the JSON report the viewer reads.
const reportArtifact = "blast-radius.json"

// artifacts are written on every run so an investigation keeps its results
// without the caller remembering to redirect stdout.
var artifacts = []struct {
	name   string
	format string
	unit   func(r *BlastResult) string
}{
	{reportArtifact, "json", func(r *BlastResult) string { return "full report" }},
	{"affected-packages.csv", "affected-packages", func(r *BlastResult) string {
		return FormatNumber(int64(r.UniquePackages)) + " packages"
	}},
	{"paths.csv", "csv", func(r *BlastResult) string {
		return FormatNumber(int64(len(r.Affected))) + " rows"
	}},
}

func SaveArtifacts(result *BlastResult, dir string, progress io.Writer) error {
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

	fmt.Fprintf(progress, "\nBrowse the results with:\n  blast-radius visualize %s\n",
		filepath.Join(dir, reportArtifact))
	return nil
}

func writeArtifact(path string, result *BlastResult, format string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	tmpPath := f.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	buf := bufio.NewWriter(f)
	if err := renderTo(result, format, 0, buf); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := buf.Flush(); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	committed = true
	return nil
}
