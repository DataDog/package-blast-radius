package blast

import (
	"bytes"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeCSV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compromised.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

func TestParseCompromisedCSV(t *testing.T) {
	path := writeCSV(t, `# compromised packages, exported 2026-08-05

axios;1.14.1,0.30.4
lodash;4.17.20

  @scope/pkg  ;  1.0.0 , 2.0.0
`)

	got, err := ParseCompromisedCSV(path, NPM)
	if err != nil {
		t.Fatalf("ParseCompromisedCSV: %v", err)
	}

	want := []TargetSpec{
		{System: NPM, Name: "axios", Versions: []string{"1.14.1", "0.30.4"}},
		{System: NPM, Name: "lodash", Versions: []string{"4.17.20"}},
		{System: NPM, Name: "@scope/pkg", Versions: []string{"1.0.0", "2.0.0"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseCompromisedCSVEmptyFile(t *testing.T) {
	got, err := ParseCompromisedCSV(writeCSV(t, "# nothing but a comment\n\n"), NPM)
	if err != nil {
		t.Fatalf("ParseCompromisedCSV: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want no targets", got)
	}
}

func TestParseCompromisedCSVErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"missing separator", "axios 1.14.1\n", "line 1"},
		{"empty package name", ";1.14.1\n", "empty package name"},
		{"no versions", "axios;\n", "no versions"},
		{"only commas for versions", "axios; , ,\n", "no versions"},
		{"error reports the right line", "ok;1.0.0\n\n# c\nbroken\n", "line 4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCompromisedCSV(writeCSV(t, tt.content), NPM)
			if err == nil {
				t.Fatalf("got nil error, want one containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseCompromisedCSVMissingFile(t *testing.T) {
	if _, err := ParseCompromisedCSV(filepath.Join(t.TempDir(), "nope.csv"), NPM); err == nil {
		t.Error("got nil error for a missing file")
	}
}

func TestFindDB(t *testing.T) {
	t.Run("explicit path wins without touching the disk", func(t *testing.T) {
		got, err := findDB("/some/where/custom.duckdb", NPM)
		if err != nil {
			t.Fatalf("findDB: %v", err)
		}
		if got != "/some/where/custom.duckdb" {
			t.Errorf("got %q, want the explicit path", got)
		}
	})

	t.Run("finds the ecosystem database in ./data", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "data"), 0o755); err != nil {
			t.Fatal(err)
		}
		dbPath := filepath.Join(dir, "data", NPM.DBName())
		if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)

		got, err := findDB("", NPM)
		if err != nil {
			t.Fatalf("findDB: %v", err)
		}
		if got != filepath.Join("data", NPM.DBName()) {
			t.Errorf("got %q, want data/%s", got, NPM.DBName())
		}
	})

	t.Run("reports what it tried when nothing is found", func(t *testing.T) {
		t.Chdir(t.TempDir())

		_, err := findDB("", NPM)
		if err == nil {
			t.Fatal("got nil error, want a not-found error")
		}
		if !strings.Contains(err.Error(), NPM.DBName()) || !strings.Contains(err.Error(), "--db") {
			t.Errorf("error = %q, want it to name the database and suggest --db", err)
		}
	})
}

func TestComputeBlastRadiusTraversesMatchingDependents(t *testing.T) {
	target := PackageVersion{System: NPM, Name: "vulnerable", Version: "1.2.3"}
	source := &fakeDependentSource{edges: map[string][]RawDependent{
		"vulnerable": {
			{DependentName: "direct", DependentVersion: "1.0.0", TargetName: "vulnerable", Requirement: "^1.2.0"},
			{DependentName: "exact", DependentVersion: "1.0.0", TargetName: "vulnerable", Requirement: "1.2.3"},
			{DependentName: "ignored", DependentVersion: "1.0.0", TargetName: "vulnerable", Requirement: "<1.0.0"},
		},
		"direct": {
			{DependentName: "consumer", DependentVersion: "2.0.0", TargetName: "direct", Requirement: "^1.0.0"},
			{DependentName: "stale", DependentVersion: "1.0.0", TargetName: "direct", Requirement: "<1.0.0"},
			{DependentName: "vulnerable", DependentVersion: "1.2.3", TargetName: "direct", Requirement: "^1.0.0"},
		},
		"consumer": {
			{DependentName: "too-deep", DependentVersion: "3.0.0", TargetName: "consumer", Requirement: "^2.0.0"},
		},
	}}

	result, err := computeBlastRadius(NPM, []PackageVersion{target}, 2, source, io.Discard, time.Now())
	if err != nil {
		t.Fatalf("computeBlastRadius: %v", err)
	}

	if result.TotalEdges != 6 {
		t.Errorf("TotalEdges = %d, want 6", result.TotalEdges)
	}
	if result.UniquePackages != 3 {
		t.Errorf("UniquePackages = %d, want 3", result.UniquePackages)
	}
	if !reflect.DeepEqual(source.directQueries, []string{"vulnerable"}) {
		t.Errorf("direct queries = %v, want [vulnerable]", source.directQueries)
	}
	if len(source.multiQueries) != 1 || !sameStringSet(source.multiQueries[0], []string{"direct", "exact"}) {
		t.Errorf("multi queries = %v, want one query for direct and exact", source.multiQueries)
	}

	affected := affectedByKey(result.Affected)
	for _, key := range []string{"direct@1.0.0", "exact@1.0.0", "consumer@2.0.0"} {
		if _, ok := affected[key]; !ok {
			t.Errorf("missing affected package %s in %+v", key, result.Affected)
		}
	}
	for _, key := range []string{"ignored@1.0.0", "stale@1.0.0", "vulnerable@1.2.3", "too-deep@3.0.0"} {
		if _, ok := affected[key]; ok {
			t.Errorf("unexpected affected package %s in %+v", key, result.Affected)
		}
	}

	direct := affected["direct@1.0.0"]
	if direct.Depth != 1 || direct.Target != target {
		t.Errorf("direct = %+v, want depth 1 and target %s", direct, target)
	}
	wantDirectPath := []PathStep{{Package: "direct", Version: "1.0.0", Requirement: "^1.2.0"}}
	if !reflect.DeepEqual(direct.Path, wantDirectPath) {
		t.Errorf("direct path = %+v, want %+v", direct.Path, wantDirectPath)
	}

	consumer := affected["consumer@2.0.0"]
	if consumer.Depth != 2 || consumer.Target != target {
		t.Errorf("consumer = %+v, want depth 2 and target %s", consumer, target)
	}
	wantConsumerPath := []PathStep{
		{Package: "consumer", Version: "2.0.0", Requirement: "^1.0.0"},
		{Package: "direct", Version: "1.0.0", Requirement: "^1.2.0"},
	}
	if !reflect.DeepEqual(consumer.Path, wantConsumerPath) {
		t.Errorf("consumer path = %+v, want %+v", consumer.Path, wantConsumerPath)
	}
}

// When several compromised versions of the same package satisfy one declared
// requirement, the dependent is attributed to the first one in target order.
// Nothing else pins this, and it is the invariant any caching of the
// requirement-to-parent resolution has to preserve.
func TestComputeBlastRadiusAttributesTiesToTheFirstMatchingTarget(t *testing.T) {
	// Both versions satisfy ">=1.0.0". The frontier is seeded straight from this
	// slice, so 1.0.0 is unambiguously the earlier candidate.
	targets := []PackageVersion{
		{System: NPM, Name: "vulnerable", Version: "1.0.0"},
		{System: NPM, Name: "vulnerable", Version: "1.5.0"},
	}
	source := &fakeDependentSource{edges: map[string][]RawDependent{
		"vulnerable": {
			{DependentName: "consumer", DependentVersion: "2.0.0", TargetName: "vulnerable", Requirement: ">=1.0.0"},
		},
	}}

	result, err := computeBlastRadius(NPM, targets, 1, source, io.Discard, time.Now())
	if err != nil {
		t.Fatalf("computeBlastRadius: %v", err)
	}

	if len(result.Affected) != 1 {
		t.Fatalf("Affected = %+v, want exactly consumer@2.0.0", result.Affected)
	}
	got := result.Affected[0]
	if got.Target != targets[0] {
		t.Errorf("Target = %s, want %s (the first matching target)", got.Target, targets[0])
	}
	if got.Depth != 1 {
		t.Errorf("Depth = %d, want 1", got.Depth)
	}
	wantPath := []PathStep{{Package: "consumer", Version: "2.0.0", Requirement: ">=1.0.0"}}
	if !reflect.DeepEqual(got.Path, wantPath) {
		t.Errorf("Path = %+v, want %+v", got.Path, wantPath)
	}
}

func TestComputeBlastRadiusUsesPyPISpecifiers(t *testing.T) {
	target := PackageVersion{System: PyPI, Name: "vulnerable-pkg", Version: "2.1.0"}
	source := &fakeDependentSource{edges: map[string][]RawDependent{
		"vulnerable-pkg": {
			{DependentName: "direct", DependentVersion: "1.0.0", TargetName: "vulnerable-pkg", Requirement: ">=2,<3 ; python_version >= '3.10'"},
			{DependentName: "ignored", DependentVersion: "1.0.0", TargetName: "vulnerable-pkg", Requirement: "<2"},
		},
		"direct": {
			{DependentName: "consumer", DependentVersion: "4.0.0", TargetName: "direct", Requirement: "~=1.0"},
		},
	}}

	result, err := computeBlastRadius(PyPI, []PackageVersion{target}, 2, source, io.Discard, time.Now())
	if err != nil {
		t.Fatalf("computeBlastRadius: %v", err)
	}

	affected := affectedByKey(result.Affected)
	for _, key := range []string{"direct@1.0.0", "consumer@4.0.0"} {
		if _, ok := affected[key]; !ok {
			t.Errorf("missing affected package %s in %+v", key, result.Affected)
		}
	}
	if _, ok := affected["ignored@1.0.0"]; ok {
		t.Errorf("unexpected affected package in %+v", result.Affected)
	}
}

func TestComputeBlastRadiusReportsUnparseablePyPISpecifiers(t *testing.T) {
	target := PackageVersion{System: PyPI, Name: "vulnerable-pkg", Version: "1.0.0"}
	source := &fakeDependentSource{edges: map[string][]RawDependent{
		"vulnerable-pkg": {
			{DependentName: "bad", DependentVersion: "1.0.0", TargetName: "vulnerable-pkg", Requirement: ">=1.0.*"},
			{DependentName: "miss", DependentVersion: "1.0.0", TargetName: "vulnerable-pkg", Requirement: "<1.0"},
			{DependentName: "hit", DependentVersion: "1.0.0", TargetName: "vulnerable-pkg", Requirement: ">=1.0"},
		},
	}}
	var progress strings.Builder

	result, err := computeBlastRadius(PyPI, []PackageVersion{target}, 1, source, &progress, time.Now())
	if err != nil {
		t.Fatalf("computeBlastRadius: %v", err)
	}

	affected := affectedByKey(result.Affected)
	if _, ok := affected["hit@1.0.0"]; !ok {
		t.Fatalf("Affected = %+v, want hit@1.0.0", result.Affected)
	}
	if _, ok := affected["bad@1.0.0"]; ok {
		t.Fatalf("Affected = %+v, want bad requirement skipped", result.Affected)
	}
	if !strings.Contains(progress.String(), "Skipped 1 edge(s) with unparseable PYPI version specifiers.") {
		t.Fatalf("progress did not report skipped unparseable specifiers:\n%s", progress.String())
	}
}

func TestNormalizeTargetSpecsUsesEcosystemRules(t *testing.T) {
	got := normalizeTargetSpecs(PyPI, []TargetSpec{{Name: "My_Pkg.Name", Versions: []string{"1.0"}}})
	want := []TargetSpec{{System: PyPI, Name: "my-pkg-name", Versions: []string{"1.0"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalizeTargetSpecs = %+v, want %+v", got, want)
	}
}

func TestComputeBlastRadiusExcludesABundleThatPredatesTheCompromise(t *testing.T) {
	target := PackageVersion{System: NPM, Name: "vulnerable", Version: "1.2.3"}
	source := &fakeDependentSource{
		edges: map[string][]RawDependent{
			"vulnerable": {
				{DependentName: "bundler>1.0.0>vulnerable", DependentVersion: "1.0.0", TargetName: "vulnerable", Requirement: "^1.0.0"},
			},
		},
		publishedAt: map[string]time.Time{
			"bundler@1.0.0":    time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			"vulnerable@1.2.3": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	result, err := computeBlastRadius(NPM, []PackageVersion{target}, 1, source, io.Discard, time.Now())
	if err != nil {
		t.Fatalf("computeBlastRadius: %v", err)
	}
	if len(result.Affected) != 0 {
		t.Errorf("Affected = %+v, want none: bundler@1.0.0 predates vulnerable@1.2.3", result.Affected)
	}
}

func TestComputeBlastRadiusIncludesABundleThatPostdatesTheCompromise(t *testing.T) {
	target := PackageVersion{System: NPM, Name: "vulnerable", Version: "1.2.3"}
	source := &fakeDependentSource{
		edges: map[string][]RawDependent{
			"vulnerable": {
				{DependentName: "bundler>2.0.0>vulnerable", DependentVersion: "2.0.0", TargetName: "vulnerable", Requirement: "^1.0.0"},
			},
		},
		publishedAt: map[string]time.Time{
			"bundler@2.0.0":    time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			"vulnerable@1.2.3": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	result, err := computeBlastRadius(NPM, []PackageVersion{target}, 1, source, io.Discard, time.Now())
	if err != nil {
		t.Fatalf("computeBlastRadius: %v", err)
	}
	affected := affectedByKey(result.Affected)
	got, ok := affected["bundler@2.0.0"]
	if !ok {
		t.Fatalf("Affected = %+v, want bundler@2.0.0 (published after vulnerable@1.2.3)", result.Affected)
	}
	if got.Target != target {
		t.Errorf("target = %+v, want %+v", got.Target, target)
	}
	if len(got.Path) != 1 || got.Path[0].Requirement != "bundled" {
		t.Errorf("path = %+v, want a single bundled step", got.Path)
	}
}

func TestComputeBlastRadiusExcludesNestedBundleThatPredatesTheCompromise(t *testing.T) {
	target := PackageVersion{System: NPM, Name: "vulnerable", Version: "1.2.3"}
	source := &fakeDependentSource{
		edges: map[string][]RawDependent{
			"vulnerable": {
				{DependentName: "intermediate", DependentVersion: "1.0.0", TargetName: "vulnerable", Requirement: "^1.0.0"},
			},
			"intermediate": {
				{DependentName: "bundler>1.0.0>intermediate", DependentVersion: "1.0.0", TargetName: "intermediate", Requirement: "^1.0.0"},
			},
		},
		publishedAt: map[string]time.Time{
			"intermediate@1.0.0": time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
			"bundler@1.0.0":      time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			"vulnerable@1.2.3":   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	result, err := computeBlastRadius(NPM, []PackageVersion{target}, 2, source, io.Discard, time.Now())
	if err != nil {
		t.Fatalf("computeBlastRadius: %v", err)
	}
	if _, ok := affectedByKey(result.Affected)["bundler@1.0.0"]; ok {
		t.Errorf("Affected = %+v, want bundler excluded: root predates the target even though it postdates the matched intermediate", result.Affected)
	}
}

// A bundled edge found through an intermediate hop must be judged against the
// actual compromised target's publish date, not the specific intermediate
// version our traversal happened to match through. Bundling freezes the
// entire resolved subtree at once, so whichever target version ended up
// frozen inside the root's tarball had to exist by the root's publish date
// regardless of which intermediate satisfied the range - an older
// intermediate could just as well have been the one actually bundled.
// Excluding on the intermediate's date instead would hide a real compromise:
// the exact regression this test guards against.
func TestComputeBlastRadiusKeepsABundleWhenOnlyTheIntermediatePostdatesTheRoot(t *testing.T) {
	target := PackageVersion{System: NPM, Name: "vulnerable", Version: "1.0.0"}
	source := &fakeDependentSource{
		edges: map[string][]RawDependent{
			"vulnerable": {
				{DependentName: "@types/thing", DependentVersion: "2.0.0", TargetName: "vulnerable", Requirement: "^1.0.0"},
			},
			"@types/thing": {
				{DependentName: "bundler>1.0.0>something", DependentVersion: "1.0.0", TargetName: "@types/thing", Requirement: "^2.0.0"},
			},
		},
		publishedAt: map[string]time.Time{
			"vulnerable@1.0.0":   time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC),
			"bundler@1.0.0":      time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			"@types/thing@2.0.0": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), // postdates the root
		},
	}

	result, err := computeBlastRadius(NPM, []PackageVersion{target}, 2, source, io.Discard, time.Now())
	if err != nil {
		t.Fatalf("computeBlastRadius: %v", err)
	}
	affected := affectedByKey(result.Affected)
	got, ok := affected["bundler@1.0.0"]
	if !ok {
		t.Fatalf("Affected = %+v, want bundler@1.0.0 kept: root postdates the target even though the matched intermediate postdates the root", result.Affected)
	}
	if got.Target != target {
		t.Errorf("target = %+v, want %+v", got.Target, target)
	}
}

func TestComputeBlastRadiusIncludesABundleWithNoKnownPublishDate(t *testing.T) {
	target := PackageVersion{System: NPM, Name: "vulnerable", Version: "1.2.3"}
	source := &fakeDependentSource{
		edges: map[string][]RawDependent{
			"vulnerable": {
				{DependentName: "bundler>3.0.0>vulnerable", DependentVersion: "3.0.0", TargetName: "vulnerable", Requirement: "^1.0.0"},
			},
		},
		// No versions table available locally: publishedAt is nil.
	}

	result, err := computeBlastRadius(NPM, []PackageVersion{target}, 1, source, io.Discard, time.Now())
	if err != nil {
		t.Fatalf("computeBlastRadius: %v", err)
	}
	if len(result.Affected) != 1 || result.Affected[0].Name != "bundler" {
		t.Errorf("Affected = %+v, want bundler included when publish dates are unknown", result.Affected)
	}
}

func TestApplyLocalDownloadCounts(t *testing.T) {
	affected := []AffectedPackage{
		{PackageVersion: PackageVersion{System: PyPI, Name: "requests", Version: "1.0.0"}, WeeklyDownloads: -1},
		{PackageVersion: PackageVersion{System: PyPI, Name: "requests", Version: "1.0.1"}, WeeklyDownloads: -1},
		{PackageVersion: PackageVersion{System: PyPI, Name: "missing", Version: "2.0.0"}, WeeklyDownloads: -1},
	}
	source := &fakeDependentSource{weeklyDownloads: map[string]int64{"requests": 1234}}

	available, missing, err := applyLocalDownloadCounts(source, affected)
	if err != nil {
		t.Fatalf("applyLocalDownloadCounts: %v", err)
	}
	if !available {
		t.Fatal("local download counts were reported unavailable")
	}
	if missing != 1 {
		t.Fatalf("missing = %d, want 1", missing)
	}
	for _, a := range affected[:2] {
		if a.WeeklyDownloads != 1234 {
			t.Errorf("%s downloads = %d, want 1234", a.String(), a.WeeklyDownloads)
		}
	}
	if affected[2].WeeklyDownloads != -1 {
		t.Errorf("missing package downloads = %d, want -1", affected[2].WeeklyDownloads)
	}
}

type fakeDependentSource struct {
	edges           map[string][]RawDependent
	directQueries   []string
	multiQueries    [][]string
	publishedAt     map[string]time.Time
	weeklyDownloads map[string]int64
}

func (f *fakeDependentSource) QueryDirectDependents(name string, yield func(RawDependent) error) error {
	f.directQueries = append(f.directQueries, name)
	return f.yield(name, yield)
}

func (f *fakeDependentSource) QueryDependentsOfNames(names []string, yield func(RawDependent) error) error {
	f.multiQueries = append(f.multiQueries, append([]string(nil), names...))
	for _, name := range names {
		if err := f.yield(name, yield); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeDependentSource) QueryPublishedAt(pkgs []PackageVersion) (map[string]time.Time, error) {
	if f.publishedAt == nil {
		return nil, nil
	}
	out := make(map[string]time.Time)
	for _, pv := range pkgs {
		if t, ok := f.publishedAt[pv.String()]; ok {
			out[pv.String()] = t
		}
	}
	return out, nil
}

func (f *fakeDependentSource) QueryWeeklyDownloads(names []string) (map[string]int64, error) {
	if f.weeklyDownloads == nil {
		return nil, nil
	}
	out := make(map[string]int64)
	for _, name := range names {
		if downloads, ok := f.weeklyDownloads[name]; ok {
			out[name] = downloads
		}
	}
	return out, nil
}

func (f *fakeDependentSource) yield(name string, yield func(RawDependent) error) error {
	for _, dep := range f.edges[name] {
		if err := yield(dep); err != nil {
			return err
		}
	}
	return nil
}

func affectedByKey(affected []AffectedPackage) map[string]AffectedPackage {
	byKey := make(map[string]AffectedPackage)
	for _, a := range affected {
		byKey[a.String()] = a
	}
	return byKey
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int)
	for _, s := range got {
		seen[s]++
	}
	for _, s := range want {
		if seen[s] == 0 {
			return false
		}
		seen[s]--
	}
	return true
}

// affected-packages.csv, the shape a previous run writes.
func TestParseCompromisedCSVAcceptsTheOutputShape(t *testing.T) {
	got, err := ParseCompromisedCSV(writeCSV(t, `package_name,vulnerable_versions
axios,"1.14.1,0.30.4"
lodash,4.17.20
@scope/pkg,"1.0.0,2.0.0"
`), NPM)
	if err != nil {
		t.Fatalf("ParseCompromisedCSV: %v", err)
	}

	want := []TargetSpec{
		{System: NPM, Name: "axios", Versions: []string{"1.14.1", "0.30.4"}},
		{System: NPM, Name: "lodash", Versions: []string{"4.17.20"}},
		{System: NPM, Name: "@scope/pkg", Versions: []string{"1.0.0", "2.0.0"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// A package genuinely named package_name on a later line is data, not a header.
func TestParseCompromisedCSVOnlySkipsALeadingHeader(t *testing.T) {
	got, err := ParseCompromisedCSV(writeCSV(t, "axios,1.14.1\npackage_name,1.0.0\n"), NPM)
	if err != nil {
		t.Fatalf("ParseCompromisedCSV: %v", err)
	}
	if len(got) != 2 || got[1].Name != "package_name" {
		t.Errorf("got %+v, want both rows kept", got)
	}
}

func TestParseCompromisedCSVMixesBothShapes(t *testing.T) {
	got, err := ParseCompromisedCSV(writeCSV(t, "axios;1.14.1,0.30.4\nlodash,4.17.20\n"), NPM)
	if err != nil {
		t.Fatalf("ParseCompromisedCSV: %v", err)
	}

	want := []TargetSpec{
		{System: NPM, Name: "axios", Versions: []string{"1.14.1", "0.30.4"}},
		{System: NPM, Name: "lodash", Versions: []string{"4.17.20"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestSaveArtifacts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "output", "2026-08-05_153056")
	result := sampleResult()
	sortByImpact(result.Affected)

	var progress strings.Builder
	if err := SaveArtifacts(result, dir, &progress); err != nil {
		t.Fatalf("SaveArtifacts: %v", err)
	}

	for _, name := range []string{"blast-radius.json", "affected-packages.csv", "paths.csv"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
		if !strings.Contains(progress.String(), name) {
			t.Errorf("progress output does not mention %s:\n%s", name, progress.String())
		}
	}

	// The command has to be copy-pasteable, so it needs the real path rather
	// than a placeholder.
	wantCommand := "blast-radius visualize " + filepath.Join(dir, "blast-radius.json")
	if !strings.Contains(progress.String(), wantCommand) {
		t.Errorf("progress output does not give the visualize command %q:\n%s", wantCommand, progress.String())
	}

	// The JSON artifact must be byte-identical to what --output json prints,
	// since that is what `visualize` consumes.
	var stdout bytes.Buffer
	if err := renderTo(result, "json", 0, &stdout); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "blast-radius.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != stdout.String() {
		t.Error("blast-radius.json differs from the --output json bytes")
	}

	// paths.csv keeps the full result, not the top-N the table shows.
	paths, err := os.ReadFile(filepath.Join(dir, "paths.csv"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(paths)).ReadAll()
	if err != nil {
		t.Fatalf("paths.csv is not valid CSV: %v", err)
	}
	if len(rows) != len(result.Affected)+1 {
		t.Errorf("paths.csv has %d rows, want header + %d", len(rows), len(result.Affected))
	}
}

func TestSaveArtifactsFailsLoudly(t *testing.T) {
	// A file where the run directory should go.
	blocked := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := SaveArtifacts(sampleResult(), filepath.Join(blocked, "run"), io.Discard)
	if err == nil {
		t.Error("got nil error, want a failure rather than silently losing the results")
	}
}
