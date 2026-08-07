package viewer

import (
	"fmt"
	"slices"
	"testing"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

func storeFrom(t *testing.T, result *blast.BlastResult) *store {
	t.Helper()

	s, err := loadStore(writeReport(t, result))
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	return s
}

func checkSeries(t *testing.T, got paretoSeries, want paretoSeries) {
	t.Helper()

	if got.Total != want.Total {
		t.Errorf("total = %d, want %d", got.Total, want.Total)
	}
	if got.DistinctSources != want.DistinctSources {
		t.Errorf("distinct sources = %d, want %d", got.DistinctSources, want.DistinctSources)
	}
	if len(got.Rows) != len(want.Rows) {
		t.Fatalf("rows = %+v, want %+v", got.Rows, want.Rows)
	}
	for i, row := range got.Rows {
		if row != want.Rows[i] {
			t.Errorf("row %d = %+v, want %+v", i, row, want.Rows[i])
		}
	}
}

// The fixture attributes seven of its eight versions to axios and one to evil,
// and names a third compromised package nothing depends on.
func TestParetoFixture(t *testing.T) {
	got := storeFrom(t, reportFixture()).paretoStats()

	// Versions partition the report: no overlap, and the rows account for all.
	checkSeries(t, got.Versions, paretoSeries{
		Total:           8,
		DistinctSources: 2,
		Rows: []paretoRow{
			{Package: "axios@1.14.1", Count: 7, CumulativeCount: 7},
			{Package: "evil@9.9.9", Count: 1, CumulativeCount: 8},
		},
	})

	// Packages do not: @scope/pkg is attributed to both, so the counts sum to
	// five against a total of four, and the running total stops at four rather
	// than counting that package twice.
	checkSeries(t, got.Packages, paretoSeries{
		Total:           4,
		DistinctSources: 2,
		Rows: []paretoRow{
			{Package: "axios@1.14.1", Count: 4, CumulativeCount: 4},
			{Package: "evil@9.9.9", Count: 1, CumulativeCount: 4},
		},
	})
}

// A compromised package nothing depends on belongs in neither the rows nor the
// tail: it would draw a bar at zero and imply the tail hides something.
func TestParetoIgnoresUnattributedTargets(t *testing.T) {
	got := storeFrom(t, reportFixture()).paretoStats()

	if got.Versions.DistinctSources != 2 {
		t.Errorf("distinct sources = %d, want 2: ghost was attributed nothing", got.Versions.DistinctSources)
	}
}

// The point of this chart is that it adds up, so the last row has to reach the
// total whenever every compromised package is named.
func TestParetoReachesTotal(t *testing.T) {
	got := storeFrom(t, reportFixture()).paretoStats()

	for _, series := range []struct {
		unit string
		s    paretoSeries
	}{{"versions", got.Versions}, {"packages", got.Packages}} {
		last := series.s.Rows[len(series.s.Rows)-1].CumulativeCount
		if last != series.s.Total {
			t.Errorf("%s: last cumulative = %d, want the total %d", series.unit, last, series.s.Total)
		}
	}
}

// Past the row limit the chart names a head and the client draws the rest as one
// tail bar, so the distinct count must still report everything.
func TestParetoTruncatesToRowLimit(t *testing.T) {
	sources := paretoRowLimit + 6
	affected := make([]blast.AffectedPackage, 0, sources)
	targets := make([]blast.PackageVersion, 0, sources)

	for i := range sources {
		target := blast.PackageVersion{System: blast.NPM, Name: fmt.Sprintf("bad-%02d", i), Version: "1.0.0"}
		targets = append(targets, target)
		// Impact descends with i, so the expected ranking is unambiguous.
		for v := range sources - i {
			affected = append(affected, entry(fmt.Sprintf("app-%02d", i), fmt.Sprintf("1.%d.0", v), 10, target,
				step(fmt.Sprintf("app-%02d", i), fmt.Sprintf("1.%d.0", v), "^1.0.0")))
		}
	}

	got := storeFrom(t, &blast.BlastResult{Targets: targets, Affected: affected, MaxDepth: 1}).paretoStats()

	if got.Versions.DistinctSources != sources {
		t.Errorf("distinct sources = %d, want %d", got.Versions.DistinctSources, sources)
	}
	if len(got.Versions.Rows) != paretoRowLimit {
		t.Fatalf("rows = %d, want %d", len(got.Versions.Rows), paretoRowLimit)
	}
	if first := got.Versions.Rows[0]; first.Package != "bad-00@1.0.0" || first.Count != sources {
		t.Errorf("first row = %+v, want bad-00@1.0.0 with count %d", first, sources)
	}

	// No two compromised packages here share an affected version, so the running
	// total is the plain sum of the rows above it.
	sum := 0
	for i, row := range got.Versions.Rows {
		sum += row.Count
		if row.CumulativeCount != sum {
			t.Errorf("row %d (%s) cumulative = %d, want %d", i, row.Package, row.CumulativeCount, sum)
		}
	}
	if got.Versions.Rows[len(got.Versions.Rows)-1].CumulativeCount >= got.Versions.Total {
		t.Error("a truncated chart must leave a tail for the client to draw")
	}
}

// The Overview's scope panel deep-links into Explore with a bare "@scope" as the
// target filter. A scoped npm name always carries a slash, so that value cannot
// collide with a package name, and it must not match packages from other scopes.
func TestTargetFilterAcceptsAScope(t *testing.T) {
	acme := blast.PackageVersion{System: blast.NPM, Name: "@acme/lib", Version: "1.0.0"}
	acmeOther := blast.PackageVersion{System: blast.NPM, Name: "@acme/util", Version: "2.0.0"}
	rival := blast.PackageVersion{System: blast.NPM, Name: "@rival/lib", Version: "1.0.0"}

	s := storeFrom(t, &blast.BlastResult{
		Targets: []blast.PackageVersion{acme, acmeOther, rival},
		Affected: []blast.AffectedPackage{
			entry("from-acme-lib", "1.0.0", 10, acme, step("from-acme-lib", "1.0.0", "^1.0.0")),
			entry("from-acme-util", "1.0.0", 10, acmeOther, step("from-acme-util", "1.0.0", "^2.0.0")),
			entry("from-rival", "1.0.0", 10, rival, step("from-rival", "1.0.0", "^1.0.0")),
		},
		MaxDepth: 1,
	})

	byScope := packageNames(pageOf(s, packageQuery{target: "@acme"}))
	if len(byScope) != 2 || !slices.Contains(byScope, "from-acme-lib") || !slices.Contains(byScope, "from-acme-util") {
		t.Errorf("target=@acme selected %v, want both @acme packages and nothing else", byScope)
	}

	// The narrower forms still mean what they did.
	if byName := packageNames(pageOf(s, packageQuery{target: "@acme/lib"})); len(byName) != 1 {
		t.Errorf("target=@acme/lib selected %v, want just its own dependent", byName)
	}
	if byRef := packageNames(pageOf(s, packageQuery{target: "@acme/lib@1.0.0"})); len(byRef) != 1 {
		t.Errorf("target=@acme/lib@1.0.0 selected %v, want just its own dependent", byRef)
	}
	if none := packageNames(pageOf(s, packageQuery{target: "@nobody"})); len(none) != 0 {
		t.Errorf("target=@nobody selected %v, want nothing", none)
	}
}

func pageOf(s *store, pq packageQuery) []packageResponse {
	out := make([]packageResponse, 0)
	for _, i := range s.selectPackages(pq) {
		out = append(out, s.packageResponse(&s.packages[i], pq.search))
	}
	return out
}

func TestParetoEndpoint(t *testing.T) {
	srv := serveFixture(t)

	var got paretoResponse
	getJSON(t, srv, "/api/pareto", &got)

	if len(got.Versions.Rows) != 2 || got.Versions.Rows[0].Package != "axios@1.14.1" {
		t.Errorf("versions rows = %+v", got.Versions.Rows)
	}
	if got.Packages.Total != 4 {
		t.Errorf("packages total = %d, want 4", got.Packages.Total)
	}
}
