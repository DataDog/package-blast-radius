package viewer

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// idOf reverses the sealed interner. Production code never needs this (reads
// only ever go id -> string), but tests address nodes by name.
func idOf(t *testing.T, s *store, name string) int32 {
	t.Helper()
	for i, str := range s.strings.strs {
		if str == name {
			return int32(i)
		}
	}
	t.Fatalf("%q was never interned", name)
	return 0
}

func dependentsOf(t *testing.T, s *store, name string) []string {
	t.Helper()
	return s.strings.strs32(s.dependents[idOf(t, s, name)])
}

func routeHops(s *store, r *route) string {
	hops := s.strings.strs32(r.HopIDs)
	if len(hops) == 0 {
		return "(direct)"
	}
	return strings.Join(hops, " -> ")
}

func mustPkg(t *testing.T, s *store, name string) *packageEntry {
	t.Helper()
	p := s.pkg(name)
	if p == nil {
		t.Fatalf("no package %q in the store", name)
	}
	return p
}

func TestStoreReadsSummaryFields(t *testing.T) {
	s := loadFixture(t)

	if s.meta.System != "NPM" {
		t.Errorf("system = %q", s.meta.System)
	}
	if s.meta.TotalEdges != 2915977 {
		t.Errorf("total_edges = %d", s.meta.TotalEdges)
	}
	if s.meta.TotalAffected != 8 {
		t.Errorf("total_affected = %d, want 8", s.meta.TotalAffected)
	}
	if s.meta.MaxDepth != 2 {
		t.Errorf("max_depth = %d", s.meta.MaxDepth)
	}
	if s.meta.Elapsed != "1.5s" {
		t.Errorf("elapsed = %q", s.meta.Elapsed)
	}
	if len(s.meta.Targets) != 3 {
		t.Errorf("targets = %v, want all three declared", s.meta.Targets)
	}
	// With several targets the analyzer writes a label here, not a package
	// reference. Nothing may render it as one.
	if s.meta.Target != "axios@1.14.1, evil@9.9.9, ghost@0.1.0" {
		t.Errorf("target = %q, want the multi-target label", s.meta.Target)
	}
}

// Versions whose paths traverse the same intermediate *names* are one route,
// even when the intermediate versions differ.
func TestStoreGroupsVersionsIntoRoutes(t *testing.T) {
	s := loadFixture(t)

	deep := mustPkg(t, s, "deep-dep")
	if len(deep.Routes) != 2 {
		t.Fatalf("deep-dep has %d routes, want 2", len(deep.Routes))
	}

	byHops := map[string][]string{}
	for i := range deep.Routes {
		r := &deep.Routes[i]
		versions := make([]string, 0, len(r.Versions))
		for _, v := range r.Versions {
			versions = append(versions, v.Version)
		}
		byHops[routeHops(s, r)] = versions
	}

	// Descending semver within a route.
	want := map[string][]string{
		"tremendous": {"2.0.0", "1.5.0"},
		"@scope/pkg": {"1.0.0"},
	}
	for hops, versions := range want {
		if got := byHops[hops]; !slices.Equal(got, versions) {
			t.Errorf("route via %s has versions %v, want %v", hops, got, versions)
		}
	}

	if deep.VersionCount != 3 {
		t.Errorf("deep-dep version_count = %d, want 3", deep.VersionCount)
	}
}

func TestStoreRecordsDepthPerPackageAndVersion(t *testing.T) {
	s := loadFixture(t)

	deep := mustPkg(t, s, "deep-dep")
	if deep.MinDepth != 2 || deep.MaxDepth != 2 {
		t.Errorf("deep-dep depth = %d..%d, want 2..2", deep.MinDepth, deep.MaxDepth)
	}

	wantPackages := map[int]int{1: 3, 2: 1}
	for depth, count := range wantPackages {
		if s.depthCounts[depth] != count {
			t.Errorf("depth_counts[%d] = %d, want %d", depth, s.depthCounts[depth], count)
		}
	}

	wantVersions := map[int]int{1: 5, 2: 3}
	for depth, count := range wantVersions {
		if s.versionDepthCounts[depth] != count {
			t.Errorf("version_depth_counts[%d] = %d, want %d", depth, s.versionDepthCounts[depth], count)
		}
	}
}

// A prerelease must not outrank its own release, which the old numeric-prefix
// comparison got wrong.
func TestStoreVersionBoundsUseSemverOrdering(t *testing.T) {
	s := loadFixture(t)

	scoped := mustPkg(t, s, "@scope/pkg")
	if scoped.LatestVersion != "1.0.0" {
		t.Errorf("latest = %q, want 1.0.0", scoped.LatestVersion)
	}
	if scoped.OldestVersion != "1.0.0-beta.1" {
		t.Errorf("oldest = %q, want 1.0.0-beta.1", scoped.OldestVersion)
	}
}

func TestStoreAttributesPackagesToTargets(t *testing.T) {
	s := loadFixture(t)

	got := map[string]targetSummary{}
	for _, target := range s.targets {
		got[target.Ref] = target
	}

	axios := got["axios@1.14.1"]
	if axios.Name != "axios" || axios.Version != "1.14.1" {
		t.Errorf("axios split into %q / %q", axios.Name, axios.Version)
	}
	if axios.AttributedPackages != 4 || axios.AttributedVersions != 7 {
		t.Errorf("axios attributed %d packages / %d versions, want 4 / 7",
			axios.AttributedPackages, axios.AttributedVersions)
	}

	evil := got["evil@9.9.9"]
	if evil.AttributedPackages != 1 || evil.AttributedVersions != 1 {
		t.Errorf("evil attributed %d packages / %d versions, want 1 / 1",
			evil.AttributedPackages, evil.AttributedVersions)
	}

	// A target nothing depends on never appears on an affected entry, so the
	// store learns about it only from the report header.
	if _, ok := got["ghost@0.1.0"]; ok {
		t.Error("ghost was attributed something, but nothing depends on it")
	}

	scoped := mustPkg(t, s, "@scope/pkg")
	if targets := s.strings.strs32(scoped.TargetIDs); len(targets) != 2 {
		t.Errorf("@scope/pkg targets = %v, want both", targets)
	}
}

// The graph expands target -> outward, so adjacency is stored as
// dependents[X] = packages that depend on X.
func TestStoreBuildsReverseAdjacency(t *testing.T) {
	s := loadFixture(t)

	tests := map[string][]string{
		"axios":      {"tremendous", "@scope/pkg", "unenriched"},
		"evil":       {"@scope/pkg"},
		"tremendous": {"deep-dep"},
		"@scope/pkg": {"deep-dep"},
	}
	for node, want := range tests {
		got := dependentsOf(t, s, node)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("dependents[%s] = %v, want %v", node, got, want)
		}
	}

	// The affected packages themselves are leaves in this direction.
	if got := dependentsOf(t, s, "deep-dep"); len(got) != 0 {
		t.Errorf("dependents[deep-dep] = %v, want nothing", got)
	}
}

// TestRankedTargetsFoldsInUnattributed ensures rankedTargets includes targets
// the report header declares but nothing depended on (ghost), and that
// targetByRef resolves both attributed and unattributed refs.
func TestRankedTargetsFoldsInUnattributed(t *testing.T) {
	s := loadFixture(t)

	ranked := s.rankedTargets()
	have := make(map[string]targetSummary, len(ranked))
	for _, r := range ranked {
		have[r.Ref] = r
	}

	// ghost was compromised but nothing depended on it, so it's absent from
	// s.targets and must be folded back in from the header with zero attribution.
	ghost, ok := have["ghost@0.1.0"]
	if !ok {
		t.Fatalf("ghost@0.1.0 missing from ranked targets: %v", have)
	}
	if ghost.AttributedPackages != 0 || ghost.AttributedVersions != 0 {
		t.Errorf("ghost attributed %d/%d, want 0/0", ghost.AttributedPackages, ghost.AttributedVersions)
	}

	// targetByRef must resolve an attributed target.
	if got := s.targetByRef("axios@1.14.1"); got == nil || got.Name != "axios" {
		t.Errorf("targetByRef(axios@1.14.1) = %v, want axios", got)
	}
	// And return nil for an unknown ref.
	if got := s.targetByRef("nope@9.9.9"); got != nil {
		t.Errorf("targetByRef(nope@9.9.9) = %v, want nil", got)
	}
}

// Route IDs end up in bookmarks once view state is URL-synced, so two loads of
// the same report must produce the same ones.
func TestRouteIDsAreStableAcrossLoads(t *testing.T) {
	path := writeReport(t, reportFixture())

	ids := func() []string {
		s, err := loadStore(path)
		if err != nil {
			t.Fatalf("loadStore: %v", err)
		}
		var out []string
		for i := range s.packages {
			for j := range s.packages[i].Routes {
				out = append(out, s.strings.str(s.packages[i].NameID)+"/"+s.packages[i].Routes[j].ID)
			}
		}
		slices.Sort(out)
		return out
	}

	first, second := ids(), ids()
	if !slices.Equal(first, second) {
		t.Errorf("route IDs changed between loads:\n%v\n%v", first, second)
	}
	// The target is part of a route's identity, so @scope/pkg contributes two:
	// one to axios and one to evil.
	if len(first) != 6 {
		t.Errorf("got %d routes across the report, want 6", len(first))
	}
}

func TestDisambiguateRouteIDsBreaksCollisions(t *testing.T) {
	routes := []route{{ID: "abc"}, {ID: "abc"}, {ID: "abc"}, {ID: "def"}}
	disambiguateRouteIDs(routes)

	seen := map[string]bool{}
	for _, r := range routes {
		if seen[r.ID] {
			t.Fatalf("duplicate id %q after disambiguation: %v", r.ID, routes)
		}
		seen[r.ID] = true
	}
}

// An absent download count means the report was not enriched. It must never
// collapse into a zero, in either direction.
func TestStoreKeepsUnenrichedDistinctFromZero(t *testing.T) {
	s := loadFixture(t)

	if p := mustPkg(t, s, "unenriched"); p.enriched() {
		t.Errorf("unenriched package reports %d downloads", p.WeeklyDownloads)
	}
	if p := mustPkg(t, s, "tremendous"); p.WeeklyDownloads != 24400 {
		t.Errorf("tremendous downloads = %d, want 24400", p.WeeklyDownloads)
	}
	if !s.enriched() || s.combinedDownloads != 29500 {
		t.Errorf("combined downloads = %d (enriched %t), want 29500 summed over enriched packages only",
			s.combinedDownloads, s.enriched())
	}
}

func TestStoreOrdersPackagesByImpact(t *testing.T) {
	s := loadFixture(t)

	var got []string
	for i := range s.packages {
		got = append(got, s.strings.str(s.packages[i].NameID))
	}
	// Unenriched last: an unknown count is not a small one.
	want := []string{"tremendous", "@scope/pkg", "deep-dep", "unenriched"}
	if !slices.Equal(got, want) {
		t.Errorf("package order = %v, want %v", got, want)
	}
}

// An entry with no path has no depth and no place in the graph, so it is
// counted and dropped rather than rendered as a depth-0 row.
func TestStoreSkipsEntriesWithoutAPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pathless.json")
	os.WriteFile(path, []byte(`{
	  "total_affected": 2,
	  "affected": [
	    {"name": "ghosted", "version": "1.0.0", "target": "axios@1.14.1", "path": []},
	    {"name": "real", "version": "1.0.0", "target": "axios@1.14.1",
	     "path": [{"package": "real", "version": "1.0.0", "requirement": "^1.0.0"}]}
	  ]
	}`), 0o600)

	s, err := loadStore(path)
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	if s.skippedPathless != 1 {
		t.Errorf("skipped %d pathless entries, want 1", s.skippedPathless)
	}
	if len(s.packages) != 1 || s.pkg("real") == nil {
		t.Errorf("store kept %d packages, want only the one with a path", len(s.packages))
	}
}

func TestLoadStoreHandlesAnEmptyReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	os.WriteFile(path, []byte(`{"target":"axios@1.14.1","affected":[]}`), 0o600)

	s, err := loadStore(path)
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	if len(s.packages) != 0 {
		t.Errorf("got %d packages from an empty report", len(s.packages))
	}
	if s.enriched() {
		t.Error("an empty report reports itself as enriched")
	}
}

func TestLoadStoreRejectsNonReportFiles(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		if _, err := loadStore(filepath.Join(dir, "absent.json")); err == nil {
			t.Error("got nil error for a missing file")
		}
	})

	t.Run("CSV mistaken for JSON", func(t *testing.T) {
		path := filepath.Join(dir, "report.csv")
		os.WriteFile(path, []byte("package,version\naxios,1.14.1\n"), 0o600)

		_, err := loadStore(path)
		if err == nil {
			t.Fatal("got nil error for a CSV file")
		}
		if !strings.Contains(err.Error(), "--output json") {
			t.Errorf("error = %q, want the --output json hint", err)
		}
	})

	t.Run("truncated report", func(t *testing.T) {
		path := filepath.Join(dir, "truncated.json")
		os.WriteFile(path, []byte(`{"affected":[{"name":"a"},{"nam`), 0o600)

		if _, err := loadStore(path); err == nil {
			t.Error("got nil error for a truncated file")
		}
	})
}

func TestSplitTargetRef(t *testing.T) {
	tests := []struct{ ref, name, version string }{
		{"axios@1.14.1", "axios", "1.14.1"},
		{"@scope/pkg@1.2.3", "@scope/pkg", "1.2.3"},
		{"@scope/pkg", "@scope/pkg", ""},
		{"bare", "bare", ""},
	}
	for _, tt := range tests {
		name, version := splitTargetRef(tt.ref)
		if name != tt.name || version != tt.version {
			t.Errorf("splitTargetRef(%q) = %q / %q, want %q / %q", tt.ref, name, version, tt.name, tt.version)
		}
	}
}

func TestInternerRoundTrips(t *testing.T) {
	in := newInterner(4)

	first := in.id("express")
	if in.id("express") != first {
		t.Error("interning the same string twice produced two ids")
	}
	if in.id("axios") == first {
		t.Error("two different strings share an id")
	}
	if got := in.str(first); got != "express" {
		t.Errorf("str(id(%q)) = %q", "express", got)
	}
	if got := in.str(-1); got != "" {
		t.Errorf("str(-1) = %q, want empty", got)
	}
	if got := in.str(int32(in.len())); got != "" {
		t.Errorf("str(out of range) = %q, want empty", got)
	}

	ids := []int32{in.id("axios"), first}
	if got := in.strs32(ids); !slices.Equal(got, []string{"axios", "express"}) {
		t.Errorf("strs32 = %v", got)
	}

	// Sealing drops the lookup map; id -> string reads keep working.
	in.seal()
	if in.str(first) != "express" {
		t.Error("sealing broke id -> string lookup")
	}
}
