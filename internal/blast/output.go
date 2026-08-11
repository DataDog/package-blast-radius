package blast

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rodaine/table"
)

func RenderResults(result *BlastResult, format string, top int, w io.Writer) error {
	sortByImpact(result.Affected)
	return renderTo(result, format, top, w)
}

func sortByImpact(affected []AffectedPackage) {
	sort.Slice(affected, func(i, j int) bool {
		a, b := affected[i], affected[j]
		if a.WeeklyDownloads != b.WeeklyDownloads {
			return a.WeeklyDownloads > b.WeeklyDownloads
		}
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		return a.Name < b.Name
	})
}

// renderTo assumes result.Affected is already sorted, so the same result can be
// written to several destinations without re-sorting for each one.
func renderTo(result *BlastResult, format string, top int, w io.Writer) error {
	switch format {
	case "json":
		return renderJSON(result, w)
	case "csv":
		return renderCSV(result, w)
	case "affected-packages":
		return renderAffectedPackagesCSV(result, w)
	default:
		deduped := deduplicateByName(result.Affected)
		return renderTable(result, deduped, top, w)
	}
}

func deduplicateByName(affected []AffectedPackage) []AffectedPackage {
	seen := make(map[string]bool)
	var out []AffectedPackage
	for _, a := range affected {
		if seen[a.Name] {
			continue
		}
		seen[a.Name] = true
		out = append(out, a)
	}
	return out
}

func formatPath(path []PathStep, target PackageVersion) string {
	if len(path) == 0 {
		return target.String()
	}
	var parts []string
	for _, step := range path {
		parts = append(parts, fmt.Sprintf("%s@%s ──(%s)──▶ ", step.Package, step.Version, step.Requirement))
	}
	return strings.Join(parts, "") + target.String()
}

func targetsLabel(targets []PackageVersion) string {
	if len(targets) == 0 {
		return "(none)"
	}
	if len(targets) <= 3 {
		parts := make([]string, len(targets))
		for i, t := range targets {
			parts[i] = t.String()
		}
		return strings.Join(parts, ", ")
	}
	names := make(map[string]bool)
	for _, t := range targets {
		names[t.Name] = true
	}
	return fmt.Sprintf("%d versions across %d packages", len(targets), len(names))
}

func renderTable(result *BlastResult, deduped []AffectedPackage, top int, w io.Writer) error {
	system := ""
	if len(result.Targets) > 0 {
		system = string(result.Targets[0].System)
	}
	fmt.Fprintf(w, "\nBlast radius for %s (%s)\n", targetsLabel(result.Targets), system)
	fmt.Fprintf(w, "Total dependency edges scanned: %s\n", FormatNumber(int64(result.TotalEdges)))
	fmt.Fprintf(w, "Affected: %s versions (%s unique packages)\n",
		FormatNumber(int64(len(result.Affected))),
		FormatNumber(int64(result.UniquePackages)))
	fmt.Fprintf(w, "Max depth: %d | Took %s\n\n", result.MaxDepth, result.Elapsed.Round(100*time.Millisecond))

	if len(deduped) == 0 {
		fmt.Fprintln(w, "No affected packages found.")
		return nil
	}

	display := deduped
	if top > 0 && len(display) > top {
		display = display[:top]
	}

	multiTarget := len(result.Targets) > 1
	headers := []string{"PACKAGE", "VERSION", "DEPTH", "DOWNLOADS/wk"}
	if multiTarget {
		headers = append(headers, "TARGET")
	}

	rows := make([][]string, len(display))
	for i, a := range display {
		downloads := "-"
		if a.WeeklyDownloads >= 0 {
			downloads = FormatNumber(a.WeeklyDownloads)
		}
		rows[i] = []string{a.Name, a.Version, strconv.Itoa(a.Depth), downloads}
		if multiTarget {
			rows[i] = append(rows[i], a.Target.String())
		}
	}

	pathBudget, showPath := pathColumnBudget(displayWidth(w), headers, rows)
	if showPath {
		headers = append(headers, "PATH")
		for i, a := range display {
			rows[i] = append(rows[i], elidePath(a.Path, a.Target, pathBudget))
		}
	}

	headerCells := make([]any, len(headers))
	for i, h := range headers {
		headerCells[i] = h
	}
	tbl := table.New(headerCells...)
	tbl.WithWriter(w)
	tbl.SetRows(rows)
	tbl.Print()

	if !showPath {
		fmt.Fprintln(w, "\nDependency paths omitted: the terminal is too narrow. Widen it, or read paths.csv / --output csv.")
	}

	if top > 0 && len(deduped) > top {
		fmt.Fprintf(w, "\n... and %s more unique packages (use --top 0 for all, or --output csv)\n",
			FormatNumber(int64(len(deduped)-top)))
	}

	return nil
}

// minPathWidth is the narrowest PATH column still worth printing: enough for an
// elided package name plus the target it reaches.
const minPathWidth = 24

// pathColumnBudget returns how many columns the PATH cells may use. A budget of
// 0 with show=true means the width is unbounded; show=false means the fixed
// columns already fill the terminal and PATH should be dropped entirely.
func pathColumnBudget(termWidth int, headers []string, rows [][]string) (int, bool) {
	if termWidth <= 0 {
		return 0, true
	}

	used := 0
	for i, h := range headers {
		cellWidth := utf8.RuneCountInString(h)
		for _, row := range rows {
			if n := utf8.RuneCountInString(row[i]); n > cellWidth {
				cellWidth = n
			}
		}
		used += cellWidth + table.DefaultPadding
	}

	// The table pads every column, including the last, so the PATH cell has to
	// fit inside the terminal with that trailing padding still on the line.
	budget := termWidth - used - table.DefaultPadding - 1
	if budget < minPathWidth {
		return 0, false
	}
	return budget, true
}

// elidePath renders a dependency path within budget columns by dropping leading
// hops, keeping the ones nearest the target. The first hop is the row's own
// package, which the PACKAGE and VERSION columns already show, so the hops worth
// the space are at the other end.
func elidePath(path []PathStep, target PackageVersion, budget int) string {
	full := formatPath(path, target)
	if budget <= 0 || utf8.RuneCountInString(full) <= budget {
		return full
	}

	for dropped := 1; dropped <= len(path); dropped++ {
		candidate := fmt.Sprintf("…(%d more)──▶ %s", dropped, formatPath(path[dropped:], target))
		if utf8.RuneCountInString(candidate) <= budget {
			return candidate
		}
	}
	return truncateRunes(target.String(), budget)
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-1]) + "…"
}

// The JSON* types are the on-disk contract for `--output json`; the viewer
// decodes the same types so the two sides can't drift.

type JSONStep struct {
	Package     string `json:"package"`
	Version     string `json:"version"`
	Requirement string `json:"requirement"`
}

type JSONTarget struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type JSONAffected struct {
	Name            string     `json:"name"`
	Version         string     `json:"version"`
	Depth           int        `json:"depth"`
	WeeklyDownloads int64      `json:"weekly_downloads,omitempty"`
	Target          string     `json:"target"`
	Path            []JSONStep `json:"path"`
}

type JSONResult struct {
	Target         string         `json:"target"`
	Targets        []JSONTarget   `json:"targets"`
	System         string         `json:"system"`
	TotalEdges     int            `json:"total_edges"`
	TotalAffected  int            `json:"total_affected"`
	UniquePackages int            `json:"unique_packages"`
	MaxDepth       int            `json:"max_depth"`
	Elapsed        string         `json:"elapsed"`
	ReportName     string         `json:"report_name,omitempty"`
	Affected       []JSONAffected `json:"affected"`
}

// BlastResultFromJSON reconstructs a BlastResult from a JSONResult — the
// inverse of renderJSON. It lets `enrich-download-count` re-enrich a report a
// previous `analyze` run saved, without re-running the analysis.
func BlastResultFromJSON(r *JSONResult) (*BlastResult, error) {
	system, ok := ParseEcosystem(r.System)
	if !ok {
		return nil, fmt.Errorf("unknown ecosystem %q", r.System)
	}
	targets := make([]PackageVersion, len(r.Targets))
	for i, t := range r.Targets {
		targets[i] = PackageVersion{System: system, Name: t.Name, Version: t.Version}
	}
	affected := make([]AffectedPackage, len(r.Affected))
	for i, a := range r.Affected {
		tName, tVer := splitTargetRef(a.Target)
		steps := make([]PathStep, len(a.Path))
		for j, s := range a.Path {
			steps[j] = PathStep(s)
		}
		affected[i] = AffectedPackage{
			PackageVersion:  PackageVersion{System: system, Name: a.Name, Version: a.Version},
			Depth:           a.Depth,
			Path:            steps,
			Target:          PackageVersion{System: system, Name: tName, Version: tVer},
			WeeklyDownloads: a.WeeklyDownloads,
		}
	}
	elapsed, _ := time.ParseDuration(r.Elapsed)
	return &BlastResult{
		Targets:        targets,
		TotalEdges:     r.TotalEdges,
		Affected:       affected,
		UniquePackages: r.UniquePackages,
		MaxDepth:       r.MaxDepth,
		Elapsed:        elapsed,
		ReportName:     r.ReportName,
	}, nil
}

// splitTargetRef decomposes a "name@version" target reference, handling scoped
// names whose name itself contains an '@' (e.g. "@scope/pkg@1.2.3"). A version
// never contains '@', so the split is the last '@' with a non-empty name before
// it. Mirrors the viewer's splitTargetRef.
func splitTargetRef(ref string) (name, version string) {
	if i := strings.LastIndex(ref, "@"); i > 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

func renderJSON(result *BlastResult, w io.Writer) error {
	system := ""
	if len(result.Targets) > 0 {
		system = string(result.Targets[0].System)
	}
	targets := make([]JSONTarget, len(result.Targets))
	for i, t := range result.Targets {
		targets[i] = JSONTarget{Name: t.Name, Version: t.Version}
	}

	out := JSONResult{
		Target:         targetsLabel(result.Targets),
		Targets:        targets,
		System:         system,
		TotalEdges:     result.TotalEdges,
		TotalAffected:  len(result.Affected),
		UniquePackages: result.UniquePackages,
		MaxDepth:       result.MaxDepth,
		Elapsed:        result.Elapsed.Round(time.Millisecond).String(),
		ReportName:     result.ReportName,
		Affected:       make([]JSONAffected, len(result.Affected)),
	}
	for i, a := range result.Affected {
		steps := make([]JSONStep, len(a.Path))
		for j, s := range a.Path {
			steps[j] = JSONStep(s)
		}
		out.Affected[i] = JSONAffected{
			Name:            a.Name,
			Version:         a.Version,
			Depth:           a.Depth,
			WeeklyDownloads: a.WeeklyDownloads,
			Target:          a.Target.String(),
			Path:            steps,
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func renderCSV(result *BlastResult, w io.Writer) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	cw.Write([]string{"package", "version", "depth", "weekly_downloads", "target", "path"})
	for _, a := range result.Affected {
		downloads := ""
		if a.WeeklyDownloads >= 0 {
			downloads = fmt.Sprintf("%d", a.WeeklyDownloads)
		}
		cw.Write([]string{
			a.Name,
			a.Version,
			fmt.Sprintf("%d", a.Depth),
			downloads,
			a.Target.String(),
			formatPath(a.Path, a.Target),
		})
	}
	return cw.Error()
}

// renderAffectedPackagesCSV writes one row per unique package with all of its
// affected versions collapsed into a single field. This is the same shape
// ParseCompromisedCSV reads, so a run's output can drive the next run.
func renderAffectedPackagesCSV(result *BlastResult, w io.Writer) error {
	versionsByName := make(map[string][]string)
	for _, a := range result.Affected {
		versionsByName[a.Name] = append(versionsByName[a.Name], a.Version)
	}

	names := make([]string, 0, len(versionsByName))
	for name := range versionsByName {
		names = append(names, name)
	}
	sort.Strings(names)

	cw := csv.NewWriter(w)
	defer cw.Flush()

	cw.Write([]string{"package_name", "vulnerable_versions"})
	for _, name := range names {
		versions := dedupeSortedVersions(versionsByName[name])
		cw.Write([]string{name, strings.Join(versions, ",")})
	}
	return cw.Error()
}

func dedupeSortedVersions(versions []string) []string {
	sort.Slice(versions, func(i, j int) bool { return compareVersionStrings(versions[i], versions[j]) < 0 })

	out := versions[:0]
	for i, v := range versions {
		if i == 0 || v != versions[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// compareVersionStrings orders semver-shaped versions numerically so 1.9.0
// sorts before 1.10.0, falling back to plain string order for anything that
// does not parse.
func compareVersionStrings(a, b string) int {
	va, aOK := getCachedVersion(a)
	vb, bOK := getCachedVersion(b)
	if aOK && bOK {
		return va.Compare(vb)
	}
	return strings.Compare(a, b)
}

// FormatNumber groups digits with commas. Row and edge counts here run into the
// hundreds of millions, which are unreadable otherwise.
func FormatNumber(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}

	// Preserve a leading '-' so negative counts format correctly too.
	sign := ""
	if s[0] == '-' {
		sign, s = "-", s[1:]
	}

	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	out := make([]byte, 0, len(s)+len(s)/3)
	out = append(out, s[:first]...)
	for i := first; i < len(s); i += 3 {
		out = append(out, ',')
		out = append(out, s[i:i+3]...)
	}
	return sign + string(out)
}
