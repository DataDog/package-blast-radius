package blast

import (
	"context"
	"sort"
	"strings"
)

type Ecosystem string

const (
	NPM  Ecosystem = "NPM"
	PyPI Ecosystem = "PYPI"
)

// ecosystemInfo gathers everything that differs between package ecosystems.
// Supporting a new one means adding an entry here plus its matcher and,
// optionally, a download enricher.
type ecosystemInfo struct {
	cliName string
	dbName  string
	matches func(constraint, version string) bool
	enrich  func(ctx context.Context, affected []AffectedPackage, workers int) error // nil if unsupported
}

var ecosystems = map[Ecosystem]ecosystemInfo{
	NPM: {
		cliName: "npm",
		dbName:  "npm-deps.duckdb",
		matches: npmMatches,
		enrich:  enrichNPMDownloads,
	},
}

// ParseEcosystem resolves a CLI ecosystem argument such as "npm".
func ParseEcosystem(s string) (Ecosystem, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for eco, info := range ecosystems {
		if info.cliName == s {
			return eco, true
		}
	}
	return "", false
}

// SupportedEcosystems lists the CLI names of every implemented ecosystem.
func SupportedEcosystems() []string {
	names := make([]string, 0, len(ecosystems))
	for _, info := range ecosystems {
		names = append(names, info.cliName)
	}
	sort.Strings(names)
	return names
}

// DBName is the conventional filename of this ecosystem's DuckDB snapshot.
func (e Ecosystem) DBName() string {
	return ecosystems[e].dbName
}

// MatchesVersion reports whether version satisfies the declared dependency
// constraint. Unparseable constraints and non-semver versions (git URLs, file
// paths, ...) return false.
func MatchesVersion(system Ecosystem, constraint, version string) bool {
	info, ok := ecosystems[system]
	if !ok || info.matches == nil {
		return false
	}
	return info.matches(constraint, version)
}

// SupportsEnrichment reports whether download counts can be fetched for this ecosystem.
func (e Ecosystem) SupportsEnrichment() bool {
	return ecosystems[e].enrich != nil
}

// Enrich populates WeeklyDownloads in place. It is best effort: a returned
// error reports how much data is missing, not that the run should be abandoned.
// No-op for ecosystems without a registry source.
func Enrich(ctx context.Context, system Ecosystem, affected []AffectedPackage, workers int) error {
	info := ecosystems[system]
	if info.enrich == nil {
		return nil
	}
	return info.enrich(ctx, affected, workers)
}
