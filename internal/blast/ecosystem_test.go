package blast

import (
	"context"
	"slices"
	"testing"
)

func TestParseEcosystem(t *testing.T) {
	tests := []struct {
		in     string
		want   Ecosystem
		wantOK bool
	}{
		{"npm", NPM, true},
		{"NPM", NPM, true},
		{"  npm  ", NPM, true},
		{"pypi", PyPI, true},
		{"pip", PyPI, true},
		{"cargo", "", false},
		{"", "", false},
	}

	for _, tt := range tests {
		got, ok := ParseEcosystem(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("ParseEcosystem(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestSupportedEcosystems(t *testing.T) {
	got := SupportedEcosystems()

	if !slices.Contains(got, "npm") {
		t.Errorf("got %v, want it to include npm", got)
	}
	if !slices.IsSorted(got) {
		t.Errorf("got %v, want a sorted list for stable help output", got)
	}
	// Every registered ecosystem must be reachable from its CLI name.
	for _, name := range got {
		if _, ok := ParseEcosystem(name); !ok {
			t.Errorf("SupportedEcosystems lists %q but ParseEcosystem rejects it", name)
		}
	}
}

// Every registered ecosystem needs a database name and a matcher, or analyze
// will silently return nothing for it.
func TestEveryEcosystemIsFullyConfigured(t *testing.T) {
	for eco, info := range ecosystems {
		if info.cliName == "" {
			t.Errorf("%s has no CLI name", eco)
		}
		if info.dbName == "" {
			t.Errorf("%s has no database name", eco)
		}
		if info.matches == nil {
			t.Errorf("%s has no version matcher", eco)
		}
		// A BigQuery export is optional, but half of one produces shards the
		// duckdb build glob would never find.
		if (info.bqSystem == "") != (info.parquetPrefix == "") {
			t.Errorf("%s sets only one of bqSystem=%q parquetPrefix=%q; set both or neither",
				eco, info.bqSystem, info.parquetPrefix)
		}
	}
}

func TestUnregisteredEcosystemIsInert(t *testing.T) {
	unknown := Ecosystem("UNKNOWN")
	if got := unknown.DBName(); got != "" {
		t.Errorf("unknown.DBName() = %q, want empty", got)
	}
	if unknown.SupportsEnrichment() {
		t.Error("unknown ecosystem reports enrichment support")
	}
	if MatchesVersion(unknown, ">=1.0", "1.5") {
		t.Error("MatchesVersion matched for an unregistered ecosystem")
	}
	if unknown.SupportsDatasetDownload() {
		t.Error("unknown ecosystem reports dataset download support")
	}
	if err := Enrich(context.Background(), unknown, nil, EnrichOptions{Workers: 1}); err != nil {
		t.Errorf("Enrich on an unregistered ecosystem returned %v, want nil", err)
	}
}

func TestNPMIsConfigured(t *testing.T) {
	if got := NPM.DBName(); got != "npm-deps.duckdb" {
		t.Errorf("NPM.DBName() = %q", got)
	}
	if !NPM.SupportsEnrichment() {
		t.Error("NPM should support download enrichment")
	}
	if !NPM.SupportsDatasetDownload() {
		t.Error("NPM should support dataset download")
	}
	if got := NPM.BigQuerySystem(); got != "NPM" {
		t.Errorf("NPM.BigQuerySystem() = %q, want NPM", got)
	}
	if got := NPM.ParquetPrefix(); got != "npm-edges" {
		t.Errorf("NPM.ParquetPrefix() = %q", got)
	}
}

func TestPyPIIsConfigured(t *testing.T) {
	if got := PyPI.DBName(); got != "pypi-deps.duckdb" {
		t.Errorf("PyPI.DBName() = %q", got)
	}
	if PyPI.SupportsEnrichment() {
		t.Error("PyPI should not use registry download enrichment")
	}
	if !PyPI.SupportsDatasetDownload() {
		t.Error("PyPI should support dataset download")
	}
	if !PyPI.SupportsDownloadCountDataset() {
		t.Error("PyPI should support ingestion-time download counts")
	}
	if got := PyPI.BigQuerySystem(); got != "PYPI" {
		t.Errorf("PyPI.BigQuerySystem() = %q, want PYPI", got)
	}
	if got := PyPI.ParquetPrefix(); got != "pypi-edges" {
		t.Errorf("PyPI.ParquetPrefix() = %q", got)
	}
	if got := PyPI.DownloadsParquetPrefix(); got != "pypi-downloads" {
		t.Errorf("PyPI.DownloadsParquetPrefix() = %q", got)
	}
	if got := NormalizePackageName(PyPI, " My_Package.Name "); got != "my-package-name" {
		t.Errorf("NormalizePackageName(PyPI) = %q", got)
	}
}
