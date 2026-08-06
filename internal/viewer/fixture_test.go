package viewer

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

// writeReport renders a BlastResult exactly as `analyze --output json` would.
func writeReport(t *testing.T, result *blast.BlastResult) string {
	t.Helper()

	var buf bytes.Buffer
	if err := blast.RenderResults(result, "json", 0, &buf); err != nil {
		t.Fatalf("RenderResults: %v", err)
	}

	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing report: %v", err)
	}
	return path
}

var (
	axiosTarget = blast.PackageVersion{System: blast.NPM, Name: "axios", Version: "1.14.1"}
	evilTarget  = blast.PackageVersion{System: blast.NPM, Name: "evil", Version: "9.9.9"}
	// ghostTarget was compromised but nothing depended on it, so no affected
	// entry ever names it. The summary must still list it.
	ghostTarget = blast.PackageVersion{System: blast.NPM, Name: "ghost", Version: "0.1.0"}
)

func step(pkg, version, requirement string) blast.PathStep {
	return blast.PathStep{Package: pkg, Version: version, Requirement: requirement}
}

func entry(name, version string, downloads int64, target blast.PackageVersion, path ...blast.PathStep) blast.AffectedPackage {
	return blast.AffectedPackage{
		PackageVersion:  blast.PackageVersion{System: blast.NPM, Name: name, Version: version},
		Depth:           len(path),
		Path:            path,
		Target:          target,
		WeeklyDownloads: downloads,
	}
}

// reportFixture is a small report that still exercises every case the viewer
// has to get right: a package with several versions on one route, a package
// with two distinct routes, a scoped name attributed to two targets, a
// prerelease that must not sort above its release, an unenriched package, and
// a compromised target nothing depends on.
func reportFixture() *blast.BlastResult {
	return &blast.BlastResult{
		Targets:    []blast.PackageVersion{axiosTarget, evilTarget, ghostTarget},
		TotalEdges: 2915977,
		Affected: []blast.AffectedPackage{
			entry("tremendous", "3.11.0", 24400, axiosTarget,
				step("tremendous", "3.11.0", "^1.6.1")),
			entry("tremendous", "3.9.0", 24400, axiosTarget,
				step("tremendous", "3.9.0", "^1.6.0")),

			// Two versions reaching axios through the same intermediate
			// *name* at different intermediate versions: one route.
			entry("deep-dep", "2.0.0", 100, axiosTarget,
				step("deep-dep", "2.0.0", "^3.11.0"), step("tremendous", "3.11.0", "^1.6.1")),
			entry("deep-dep", "1.5.0", 100, axiosTarget,
				step("deep-dep", "1.5.0", "^3.9.0"), step("tremendous", "3.9.0", "^1.6.0")),
			// A different intermediate: a second route on the same package.
			entry("deep-dep", "1.0.0", 100, axiosTarget,
				step("deep-dep", "1.0.0", "^1.0.0"), step("@scope/pkg", "1.0.0", "^1.6.1")),

			entry("@scope/pkg", "1.0.0-beta.1", 5000, axiosTarget,
				step("@scope/pkg", "1.0.0-beta.1", "^1.6.1")),
			entry("@scope/pkg", "1.0.0", 5000, evilTarget,
				step("@scope/pkg", "1.0.0", "^9.0.0")),

			entry("unenriched", "1.0.0", -1, axiosTarget,
				step("unenriched", "1.0.0", "^1.6.1")),
		},
		UniquePackages: 4,
		MaxDepth:       2,
		Elapsed:        1500 * time.Millisecond,
	}
}

func loadFixture(t *testing.T) *store {
	t.Helper()

	s, err := loadStore(writeReport(t, reportFixture()))
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	return s
}

func serveFixture(t *testing.T) *httptest.Server {
	t.Helper()

	assets, err := assetFS("")
	if err != nil {
		t.Fatalf("assetFS: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, loadFixture(t), "report.json", assets)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
