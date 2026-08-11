package blast

import (
	"context"
	"io"
	"regexp"
	"sort"
	"strings"
)

type Ecosystem string

const (
	NPM  Ecosystem = "NPM"
	PyPI Ecosystem = "PYPI"
)

// ecosystemInfo gathers everything that differs between ecosystems. Adding a
// new one means an entry here plus its matcher and, optionally, an enricher.
type ecosystemInfo struct {
	cliName string
	aliases []string
	dbName  string
	matches func(constraint, version string) bool
	// normalizeName canonicalizes user-provided package names before they are
	// used for graph lookup. nil means package names are already exact.
	normalizeName func(string) string
	enrich        func(ctx context.Context, affected []AffectedPackage, opts EnrichOptions) error // nil if unsupported
	enrichRate    float64
	enrichWorkers int
	// bqSystem is the System value in the deps.dev BigQuery dataset. Empty means
	// no dataset export exists, so 'download-data' rejects the ecosystem.
	bqSystem string
	// parquetPrefix names the exported shards: "npm-edges" -> npm-edges-*.parquet.
	parquetPrefix string
	// versionsParquetPrefix names the publish-date shards, used to decide whether
	// a bundled edge predates/postdates a compromise. Empty = no export.
	versionsParquetPrefix string
	// downloadsParquetPrefix names package-level weekly download-count shards.
	// Empty means this ecosystem has no ingestion-time download-count export.
	downloadsParquetPrefix string
}

var ecosystems = map[Ecosystem]ecosystemInfo{
	NPM: {
		cliName:               "npm",
		dbName:                "npm-deps.duckdb",
		matches:               npmMatches,
		enrich:                enrichNPMDownloads,
		enrichRate:            npmEnrichDefaultRate,
		enrichWorkers:         npmEnrichDefaultWorkers,
		bqSystem:              "NPM",
		parquetPrefix:         "npm-edges",
		versionsParquetPrefix: "npm-versions",
	},
	PyPI: {
		cliName:                "pypi",
		aliases:                []string{"pip"},
		dbName:                 "pypi-deps.duckdb",
		matches:                pypiMatches,
		normalizeName:          normalizePyPIName,
		bqSystem:               "PYPI",
		parquetPrefix:          "pypi-edges",
		versionsParquetPrefix:  "pypi-versions",
		downloadsParquetPrefix: "pypi-downloads",
	},
}

// ParseEcosystem resolves a CLI ecosystem argument such as "npm".
func ParseEcosystem(s string) (Ecosystem, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for eco, info := range ecosystems {
		if info.cliName == s {
			return eco, true
		}
		for _, alias := range info.aliases {
			if alias == s {
				return eco, true
			}
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

// BigQuerySystem is this ecosystem's System value in the deps.dev dataset.
func (e Ecosystem) BigQuerySystem() string {
	return ecosystems[e].bqSystem
}

// ParquetPrefix is the basename of this ecosystem's exported parquet shards.
func (e Ecosystem) ParquetPrefix() string {
	return ecosystems[e].parquetPrefix
}

// VersionsParquetPrefix is the basename of this ecosystem's exported
// publish-date parquet shards.
func (e Ecosystem) VersionsParquetPrefix() string {
	return ecosystems[e].versionsParquetPrefix
}

// DownloadsParquetPrefix is the basename of package-level weekly download-count
// shards. Empty means no ingestion-time download-count export is available.
func (e Ecosystem) DownloadsParquetPrefix() string {
	return ecosystems[e].downloadsParquetPrefix
}

func (e Ecosystem) SupportsDownloadCountDataset() bool {
	return ecosystems[e].downloadsParquetPrefix != ""
}

// SupportsDatasetDownload reports whether the dependency graph for this
// ecosystem can be exported from the deps.dev BigQuery dataset.
func (e Ecosystem) SupportsDatasetDownload() bool {
	return ecosystems[e].bqSystem != ""
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

func NormalizePackageName(system Ecosystem, name string) string {
	info, ok := ecosystems[system]
	if !ok || info.normalizeName == nil {
		return name
	}
	return info.normalizeName(name)
}

// SupportsEnrichment reports whether download counts can be fetched for this ecosystem.
func (e Ecosystem) SupportsEnrichment() bool {
	return ecosystems[e].enrich != nil
}

// EnrichOptions configures download-count enrichment.
type EnrichOptions struct {
	// Workers is the pipeline depth (concurrent in-flight requests). The npm
	// downloads API rate-limits per IP, so this only needs to cover network
	// latency; the Rate limiter bounds the actual request rate.
	Workers int
	// Rate is the target request rate in req/s. 0 means unlimited (burst). For
	// the npm downloads API a small positive rate (e.g. 2) avoids the Cloudflare
	// 429-avalanche that bursting causes.
	Rate float64
	// Progress, if non-nil, receives a periodic one-line status (resolved/total,
	// rate, ETA) while enrichment runs.
	Progress io.Writer
	// IncludeScopedPackages fetches npm scoped packages even when their count is
	// high enough to be slow. npm's bulk downloads endpoint rejects scoped names,
	// so these are one request per package.
	IncludeScopedPackages bool
	// ScopedPackageThreshold is the number of scoped npm packages allowed before
	// they are skipped unless IncludeScopedPackages is true. The package default
	// is used when this is zero or negative.
	ScopedPackageThreshold int
	// ScopedPackageOptInFlag names the caller's flag for enabling slow scoped
	// package enrichment, used only in progress messages.
	ScopedPackageOptInFlag string
	// OnPackageResolved, if non-nil, is called once for each package name after
	// the registry returned either a download count or "no data". A nil
	// downloads pointer means the package has no download data.
	OnPackageResolved func(name string, downloads *int64)
}

// Enrich populates WeeklyDownloads in place, best effort: a returned error
// reports missing data, not that the run should be abandoned. No-op without a
// registry source.
func Enrich(ctx context.Context, system Ecosystem, affected []AffectedPackage, opts EnrichOptions) error {
	info := ecosystems[system]
	if info.enrich == nil {
		return nil
	}
	return info.enrich(ctx, affected, opts)
}

func (e Ecosystem) DefaultEnrichRate() float64 {
	return ecosystems[e].enrichRate
}

func (e Ecosystem) DefaultEnrichWorkers() int {
	return ecosystems[e].enrichWorkers
}

var pypiNameNormalizer = regexp.MustCompile(`[-_.]+`)

func normalizePyPIName(name string) string {
	return pypiNameNormalizer.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
}
