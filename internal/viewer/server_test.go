package viewer

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

func get(t *testing.T, srv *httptest.Server, path string) (string, *http.Response) {
	t.Helper()

	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(body), resp
}

func getJSON(t *testing.T, srv *httptest.Server, path string, into any) {
	t.Helper()

	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status %s: %s", path, resp.Status, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
}

func packageNames(list []packageResponse) []string {
	out := make([]string, len(list))
	for i, p := range list {
		out[i] = p.Name
	}
	return out
}

// ---------------------------------------------------------------------------
// Filtering and sorting
// ---------------------------------------------------------------------------

func TestSelectPackages(t *testing.T) {
	s := loadFixture(t)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"defaults to downloads descending", "", []string{"tremendous", "@scope/pkg", "deep-dep", "unenriched"}},
		{"sort by name ascending", "sort=name&dir=asc", []string{"@scope/pkg", "deep-dep", "tremendous", "unenriched"}},
		{"sort by name descending", "sort=name&dir=desc", []string{"unenriched", "tremendous", "deep-dep", "@scope/pkg"}},
		{"sort by version count", "sort=version_count&dir=desc", []string{"deep-dep", "@scope/pkg", "tremendous", "unenriched"}},
		{"sort by route count", "sort=routes&dir=desc", []string{"@scope/pkg", "deep-dep", "tremendous", "unenriched"}},
		{"filter by one depth", "depth=2", []string{"deep-dep"}},
		{"filter by several depths", "depth=1&depth=2", []string{"tremendous", "@scope/pkg", "deep-dep", "unenriched"}},
		{"depth range with both bounds", "minDepth=1&maxDepth=1", []string{"tremendous", "@scope/pkg", "unenriched"}},
		{"depth range with a floor only", "minDepth=2", []string{"deep-dep"}},
		{"depth range with a ceiling only", "maxDepth=1", []string{"tremendous", "@scope/pkg", "unenriched"}},
		{"fully open depth range", "minDepth=1&maxDepth=9", []string{"tremendous", "@scope/pkg", "deep-dep", "unenriched"}},
		{"empty depth range", "minDepth=9", nil},
		{"filter by minimum downloads", "minDownloads=1000", []string{"tremendous", "@scope/pkg"}},
		{"filter by target reference", "target=evil@9.9.9", []string{"@scope/pkg"}},
		{"filter by target name", "target=evil", []string{"@scope/pkg"}},
		// The scope filter is the affected package's own scope, so it must not
		// catch the unscoped packages that merely route through @scope/pkg.
		{"filter by the affected package's scope", "scope=@scope", []string{"@scope/pkg"}},
		{"filter by a scope nothing publishes under", "scope=@nobody", nil},
		{"search matches package name", "search=deep", []string{"deep-dep"}},
		{"search is case insensitive", "search=TREMENDOUS", []string{"tremendous", "deep-dep"}},
		{"search matches an intermediate hop", "search=scope", []string{"@scope/pkg", "deep-dep"}},
		{"search matches a target", "search=evil", []string{"@scope/pkg"}},
		{"search that matches nothing", "search=zzz", nil},
		{"filters combine", "depth=1&minDownloads=1000", []string{"tremendous", "@scope/pkg"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("bad query %q: %v", tt.query, err)
			}

			var got []string
			for _, i := range s.selectPackages(parsePackageQuery(q)) {
				got = append(got, s.strings.str(s.packages[i].NameID))
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// Searching a package name has to surface that package, not the thousands of
// popular packages that merely route through it.
func TestSearchRanksNameMatchesAboveRouteMatches(t *testing.T) {
	report := &blast.BlastResult{
		Targets: []blast.PackageVersion{axiosTarget},
		Affected: []blast.AffectedPackage{
			entry("popular", "1.0.0", 900000, axiosTarget,
				step("popular", "1.0.0", "^1.0.0"), step("web3-bzz", "1.0.0", "^1.6.1")),
			entry("3f-web3-bzz", "2.0.0", 50, axiosTarget,
				step("3f-web3-bzz", "2.0.0", "^1.6.1")),
			entry("web3-bzz", "1.0.0", 400, axiosTarget,
				step("web3-bzz", "1.0.0", "^1.6.1")),
		},
		UniquePackages: 3,
		MaxDepth:       2,
	}

	s, err := loadStore(writeReport(t, report))
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}

	names := func(query string) []string {
		q, _ := url.ParseQuery(query)
		var got []string
		for _, i := range s.selectPackages(parsePackageQuery(q)) {
			got = append(got, s.strings.str(s.packages[i].NameID))
		}
		return got
	}

	// Exact name, then substring, then the route-only match.
	want := []string{"web3-bzz", "3f-web3-bzz", "popular"}
	if got := names("search=web3-bzz"); !slices.Equal(got, want) {
		t.Errorf("implicit sort gave %v, want %v", got, want)
	}

	// Picking a column is a request for that order, which relevance must
	// not override.
	wantByDownloads := []string{"popular", "web3-bzz", "3f-web3-bzz"}
	if got := names("search=web3-bzz&sort=weekly_downloads&dir=desc"); !slices.Equal(got, wantByDownloads) {
		t.Errorf("explicit sort gave %v, want %v", got, wantByDownloads)
	}
}

// A row that matched on a route has to lead with that route. deep-dep reaches
// axios through @scope/pkg on one route and through tremendous on another, and
// only the first is shown in the list.
func TestRouteMatchingTheSearchLeadsThePreview(t *testing.T) {
	s := loadFixture(t)

	firstRoute := func(search string) []string {
		resp := s.packageResponse(s.pkg("deep-dep"), search)
		if len(resp.Routes) == 0 {
			t.Fatalf("search=%q returned no routes", search)
		}
		return resp.Routes[0].Hops
	}

	if got := firstRoute("scope"); !slices.Contains(got, "@scope/pkg") {
		t.Errorf(`search=scope led with hops %v, want the @scope/pkg route`, got)
	}
	if got := firstRoute("tremendous"); !slices.Contains(got, "tremendous") {
		t.Errorf(`search=tremendous led with hops %v, want the tremendous route`, got)
	}

	// A name match needs no explaining, so the preview keeps its own order.
	unsearched := s.packageResponse(s.pkg("deep-dep"), "").Routes[0].Hops
	if got := firstRoute("deep"); !slices.Equal(got, unsearched) {
		t.Errorf("name match reordered routes: got %v, want %v", got, unsearched)
	}
}

// With no download data the viewer drops the download column and sorts by
// distance, so the implicit server-side sort has to agree with it.
func TestUnenrichedReportSortsByDistanceByDefault(t *testing.T) {
	report := &blast.BlastResult{
		Targets: []blast.PackageVersion{axiosTarget},
		Affected: []blast.AffectedPackage{
			entry("a-far", "1.0.0", notEnriched, axiosTarget,
				step("a-far", "1.0.0", "^1.0.0"), step("mid", "1.0.0", "^1.6.1")),
			entry("z-near", "1.0.0", notEnriched, axiosTarget,
				step("z-near", "1.0.0", "^1.6.1")),
		},
		UniquePackages: 2,
		MaxDepth:       2,
	}

	s, err := loadStore(writeReport(t, report))
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}

	var got []string
	for _, i := range s.selectPackages(parsePackageQuery(url.Values{})) {
		got = append(got, s.strings.str(s.packages[i].NameID))
	}
	if want := []string{"z-near", "a-far"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// An unknown download count is not a small one, so unenriched packages sink to
// the bottom whichever way the column is sorted.
func TestUnenrichedPackagesSortLastInBothDirections(t *testing.T) {
	s := loadFixture(t)

	for _, dir := range []string{"asc", "desc"} {
		q, _ := url.ParseQuery("sort=weekly_downloads&dir=" + dir)
		idx := s.selectPackages(parsePackageQuery(q))
		last := s.strings.str(s.packages[idx[len(idx)-1]].NameID)
		if last != "unenriched" {
			t.Errorf("dir=%s put %q last, want unenriched", dir, last)
		}
	}
}

// selectPackages runs concurrently for every request against one shared store,
// so it must not reorder it.
func TestSelectPackagesDoesNotMutateTheStore(t *testing.T) {
	s := loadFixture(t)

	before := make([]string, len(s.packages))
	for i := range s.packages {
		before[i] = s.strings.str(s.packages[i].NameID)
	}

	q, _ := url.ParseQuery("sort=name&dir=asc")
	s.selectPackages(parsePackageQuery(q))

	for i := range s.packages {
		if got := s.strings.str(s.packages[i].NameID); got != before[i] {
			t.Fatalf("store reordered: position %d was %q, now %q", i, before[i], got)
		}
	}
}

// ---------------------------------------------------------------------------
// /api/packages
// ---------------------------------------------------------------------------

func TestAPIPackages(t *testing.T) {
	srv := serveFixture(t)

	var body packageListResponse
	get := func(query string) {
		t.Helper()
		body = packageListResponse{}
		getJSON(t, srv, "/api/packages?"+query, &body)
	}

	get("")
	if body.Total != 4 || len(body.Results) != 4 {
		t.Fatalf("total = %d, results = %d, want 4 and 4", body.Total, len(body.Results))
	}
	if body.Results[0].Name != "tremendous" {
		t.Errorf("first result = %q, want the highest-download package", body.Results[0].Name)
	}

	deep := body.Results[slices.Index(packageNames(body.Results), "deep-dep")]
	if deep.VersionCount != 3 || deep.RoutesTotal != 2 {
		t.Errorf("deep-dep = %d versions / %d routes, want 3 and 2", deep.VersionCount, deep.RoutesTotal)
	}
	if deep.LatestRecordedVersion != "2.0.0" || deep.OldestRecordedVersion != "1.0.0" {
		t.Errorf("deep-dep bounds = %q .. %q", deep.OldestRecordedVersion, deep.LatestRecordedVersion)
	}
	// The list tier carries route shapes, never concrete paths.
	for _, r := range deep.Routes {
		if len(r.Versions) != 0 {
			t.Errorf("route %s in the list response carries %d concrete versions", r.ID, len(r.Versions))
		}
	}

	// total reports the full match count, not the page size.
	get("limit=1")
	if body.Total != 4 || len(body.Results) != 1 {
		t.Errorf("limit=1: total = %d, results = %d, want 4 and 1", body.Total, len(body.Results))
	}

	get("limit=1&offset=1")
	if len(body.Results) != 1 || body.Results[0].Name != "@scope/pkg" {
		t.Errorf("offset=1 returned %v, want @scope/pkg", packageNames(body.Results))
	}

	// offset=0 is a real value, not an unset one.
	get("limit=1&offset=0")
	if len(body.Results) != 1 || body.Results[0].Name != "tremendous" {
		t.Errorf("offset=0 returned %v, want the first page", packageNames(body.Results))
	}

	// An offset past the end is clamped rather than panicking.
	get("offset=99")
	if len(body.Results) != 0 || body.Offset != 4 {
		t.Errorf("offset=99 returned %v at offset %d", packageNames(body.Results), body.Offset)
	}
}

func TestAPIPackagesReportsUnenrichedAsNull(t *testing.T) {
	srv := serveFixture(t)

	// Decoded loosely: the point is the JSON literal, which a *int64 would hide.
	var body struct {
		Results []map[string]any `json:"results"`
	}
	getJSON(t, srv, "/api/packages?search=unenriched", &body)

	if len(body.Results) != 1 {
		t.Fatalf("got %d results", len(body.Results))
	}
	if got, ok := body.Results[0]["weekly_downloads"]; !ok || got != nil {
		t.Errorf("weekly_downloads = %#v, want null", got)
	}
}

// ---------------------------------------------------------------------------
// /api/package
// ---------------------------------------------------------------------------

func TestAPIPackageDetail(t *testing.T) {
	srv := serveFixture(t)

	var pkg packageResponse
	getJSON(t, srv, "/api/package?name=deep-dep", &pkg)

	if len(pkg.Routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(pkg.Routes))
	}

	// Without ?route= the first route is expanded, so one round trip is enough.
	first := pkg.Routes[0]
	if first.VersionsTotal != 2 || len(first.Versions) != 2 {
		t.Fatalf("first route expanded %d of %d versions, want 2 of 2", len(first.Versions), first.VersionsTotal)
	}
	if !slices.Equal(first.Hops, []string{"tremendous"}) {
		t.Errorf("first route hops = %v", first.Hops)
	}
	if pkg.Routes[1].Versions != nil {
		t.Error("the unrequested route came back expanded")
	}

	// path[0] is the affected package itself; the target is not in the array.
	steps := first.Versions[0].Steps
	want := []stepResponse{
		{Package: "deep-dep", Version: "2.0.0", Requirement: "^3.11.0"},
		{Package: "tremendous", Version: "3.11.0", Requirement: "^1.6.1"},
	}
	if !slices.Equal(steps, want) {
		t.Errorf("steps = %+v, want %+v", steps, want)
	}
	if first.Target != "axios@1.14.1" {
		t.Errorf("route target = %q", first.Target)
	}

	// Asking for the other route by its stable id expands that one instead.
	second := pkg.Routes[1]
	var byID packageResponse
	getJSON(t, srv, "/api/package?name=deep-dep&route="+second.ID, &byID)
	if len(byID.Routes[1].Versions) != 1 || byID.Routes[0].Versions != nil {
		t.Errorf("route=%s expanded the wrong route", second.ID)
	}
	if hops := byID.Routes[1].Hops; !slices.Equal(hops, []string{"@scope/pkg"}) {
		t.Errorf("second route hops = %v", hops)
	}
}

// The graph view offers an expansion control per node, and withholds it where
// nothing depends on the package. That decision is made from these counts, so
// the detail response has to carry one for the package and for every hop.
func TestAPIPackageDetailCountsDependents(t *testing.T) {
	srv := serveFixture(t)

	var pkg packageResponse
	getJSON(t, srv, "/api/package?name=deep-dep", &pkg)

	want := map[string]int{"deep-dep": 0, "tremendous": 1, "@scope/pkg": 1}
	if !maps.Equal(pkg.DependentCounts, want) {
		t.Errorf("dependent_counts = %v, want %v", pkg.DependentCounts, want)
	}

	var hop packageResponse
	getJSON(t, srv, "/api/package?name=tremendous", &hop)
	if got := hop.DependentCounts["tremendous"]; got != 1 {
		t.Errorf("deep-dep depends on tremendous, so its count should be 1, got %d", got)
	}
}

// Scoped names contain a slash, which is why the name is a query parameter and
// not a path segment.
func TestAPIPackageHandlesScopedNames(t *testing.T) {
	srv := serveFixture(t)

	var pkg packageResponse
	getJSON(t, srv, "/api/package?name="+url.QueryEscape("@scope/pkg"), &pkg)

	if pkg.Name != "@scope/pkg" {
		t.Fatalf("name = %q", pkg.Name)
	}
	if !slices.Equal(pkg.Targets, []string{"axios@1.14.1", "evil@9.9.9"}) {
		t.Errorf("targets = %v, want both", pkg.Targets)
	}
}

func TestAPIPackagePaginatesVersions(t *testing.T) {
	srv := serveFixture(t)

	var pkg packageResponse
	getJSON(t, srv, "/api/package?name=deep-dep&versionsLimit=1&versionsOffset=1", &pkg)

	route := pkg.Routes[0]
	if route.VersionsTotal != 2 || route.VersionsOffset != 1 || len(route.Versions) != 1 {
		t.Fatalf("page = %d versions at offset %d of %d", len(route.Versions), route.VersionsOffset, route.VersionsTotal)
	}
	if route.Versions[0].Version != "1.5.0" {
		t.Errorf("second version = %q, want 1.5.0", route.Versions[0].Version)
	}
}

func TestAPIPackageErrors(t *testing.T) {
	srv := serveFixture(t)

	tests := []struct {
		query string
		want  int
	}{
		{"", http.StatusBadRequest},
		{"?name=nonexistent", http.StatusNotFound},
		{"?name=deep-dep&route=nope", http.StatusNotFound},
	}
	for _, tt := range tests {
		resp, err := http.Get(srv.URL + "/api/package" + tt.query)
		if err != nil {
			t.Fatalf("GET %s: %v", tt.query, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("GET /api/package%s = %s, want %d", tt.query, resp.Status, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// /api/summary
// ---------------------------------------------------------------------------

func TestAPISummary(t *testing.T) {
	srv := serveFixture(t)

	var got summaryResponse
	getJSON(t, srv, "/api/summary", &got)

	if got.System != "NPM" || got.TotalEdges != 2915977 {
		t.Errorf("summary = %+v", got)
	}
	if got.UniquePackages != 4 || got.TotalAffected != 8 {
		t.Errorf("unique_packages = %d, total_affected = %d, want 4 and 8", got.UniquePackages, got.TotalAffected)
	}
	if got.DirectDependents != 3 {
		t.Errorf("direct_dependents = %d, want 3", got.DirectDependents)
	}
	if got.DepthCounts[1] != 3 || got.VersionDepthCounts[2] != 3 {
		t.Errorf("depth counts = %v / %v", got.DepthCounts, got.VersionDepthCounts)
	}
	if got.CombinedWeeklyDownloads == nil || *got.CombinedWeeklyDownloads != 29500 {
		t.Errorf("combined_weekly_downloads = %v, want 29500", got.CombinedWeeklyDownloads)
	}
	if got.SourcePath != "report.json" {
		t.Errorf("source_path = %q", got.SourcePath)
	}

	// A compromised target nothing depends on still belongs in the list; it is
	// the honest denominator.
	refs := make([]string, len(got.Targets))
	for i, target := range got.Targets {
		refs[i] = target.Ref
	}
	want := []string{"axios@1.14.1", "evil@9.9.9", "ghost@0.1.0"}
	if !slices.Equal(refs, want) {
		t.Errorf("targets = %v, want %v ordered by attribution", refs, want)
	}
	if got.Targets[0].AttributedPackages != 4 || got.Targets[2].AttributedPackages != 0 {
		t.Errorf("attribution = %+v", got.Targets)
	}
}

func TestAPISummaryReportsAnUnenrichedReportAsNull(t *testing.T) {
	fixture := reportFixture()
	for i := range fixture.Affected {
		fixture.Affected[i].WeeklyDownloads = -1
	}

	s, err := loadStore(writeReport(t, fixture))
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	assets, err := assetFS("")
	if err != nil {
		t.Fatalf("assetFS: %v", err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, s, "report.json", assets)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var got map[string]any
	getJSON(t, srv, "/api/summary", &got)

	if v, ok := got["combined_weekly_downloads"]; !ok || v != nil {
		t.Errorf("combined_weekly_downloads = %#v, want null", v)
	}
	if got["enriched"] != false {
		t.Errorf("enriched = %v, want false", got["enriched"])
	}
}

// ---------------------------------------------------------------------------
// /api/download
// ---------------------------------------------------------------------------

func TestAPIDownloadCSV(t *testing.T) {
	srv := serveFixture(t)

	resp, err := http.Get(srv.URL + "/api/download")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}

	rows, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("parsing CSV: %v", err)
	}

	// One row per (package, route): 1 + 2 + 2 + 1 across four packages.
	if len(rows) != 7 {
		t.Fatalf("got %d rows, want header + 6", len(rows))
	}
	wantHeader := []string{
		"affected_package", "affected_package_versions", "version_count",
		"depth", "weekly_downloads",
		"compromised_package", "compromised_package_version", "example_path",
	}
	if !slices.Equal(rows[0], wantHeader) {
		t.Errorf("header = %v, want %v", rows[0], wantHeader)
	}

	byKey := map[string][]string{}
	for _, row := range rows[1:] {
		byKey[row[0]+"|"+row[5]+"@"+row[6]] = row
	}

	tremendous := byKey["tremendous|axios@1.14.1"]
	if tremendous[1] != "3.11.0,3.9.0" || tremendous[2] != "2" {
		t.Errorf("tremendous versions = %q / %q", tremendous[1], tremendous[2])
	}
	if tremendous[4] != "24400" {
		t.Errorf("tremendous downloads = %q", tremendous[4])
	}
	if !strings.Contains(tremendous[7], "-->") {
		t.Errorf("example_path = %q, want a rendered dependency chain", tremendous[7])
	}

	// Unenriched is an empty cell, never a zero.
	if got := byKey["unenriched|axios@1.14.1"][4]; got != "" {
		t.Errorf("unenriched downloads = %q, want an empty cell", got)
	}
}

func TestServesTheEmbeddedUI(t *testing.T) {
	srv := serveFixture(t)

	body, resp := get(t, srv, "/")
	if !strings.HasPrefix(body, "<!DOCTYPE html>") {
		t.Errorf("body starts with %.40q, want an HTML document", body)
	}
	if !strings.Contains(body, `src="/js/main.js"`) {
		t.Error("the page does not load the app module")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// Every module the page pulls in has to be reachable, and a 404 on one of them
// only shows up as a blank screen in the browser.
func TestServesEveryEmbeddedAsset(t *testing.T) {
	srv := serveFixture(t)

	assets, err := assetFS("")
	if err != nil {
		t.Fatalf("assetFS: %v", err)
	}

	var served int
	err = fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, resp := get(t, srv, "/"+path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /%s = %d, want 200", path, resp.StatusCode)
		}
		if body == "" {
			t.Errorf("GET /%s served an empty body", path)
		}
		served++
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if served < 8 {
		t.Errorf("walked %d assets, want the full css/js tree", served)
	}
}

func TestAssetsDirRejectsADirectoryWithoutAnIndex(t *testing.T) {
	if _, err := assetFS(t.TempDir()); err == nil {
		t.Error("assetFS accepted a directory with no index.html")
	}
}

func TestAssetsDirOverridesTheEmbeddedCopy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!DOCTYPE html><p>from disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	assets, err := assetFS(dir)
	if err != nil {
		t.Fatalf("assetFS: %v", err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, loadFixture(t), "report.json", assets)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if body, _ := get(t, srv, "/"); !strings.Contains(body, "from disk") {
		t.Errorf("body = %q, want the on-disk index", body)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func TestIntParam(t *testing.T) {
	tests := []struct {
		in       string
		def, min int
		want     int
	}{
		{"", 7, 1, 7},
		{"42", 7, 1, 42},
		{"junk", 7, 1, 7}, // unparseable falls back to the default
		{"0", 7, 1, 1},    // below the minimum is clamped, not replaced
		{"-3", 7, 1, 1},
		{"0", 100, 0, 0}, // a genuine zero survives when zero is allowed
		{"-3", 100, 0, 0},
	}
	for _, tt := range tests {
		if got := intParam(tt.in, tt.def, tt.min); got != tt.want {
			t.Errorf("intParam(%q, %d, %d) = %d, want %d", tt.in, tt.def, tt.min, got, tt.want)
		}
	}
}

func TestFormatPathForCSV(t *testing.T) {
	if got := formatPathForCSV(nil, "axios@1.14.1"); got != "axios@1.14.1" {
		t.Errorf("empty path = %q, want the bare target", got)
	}

	path := []blast.JSONStep{
		{Package: "deep", Version: "2.0.0", Requirement: "^3.0.0"},
		{Package: "mid", Version: "3.0.0", Requirement: "^1.6.1"},
	}
	want := "deep@2.0.0 --(^3.0.0)--> mid@3.0.0 --(^1.6.1)--> axios@1.14.1"
	if got := formatPathForCSV(path, "axios@1.14.1"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestContainsLower(t *testing.T) {
	tests := []struct {
		s, sub string
		want   bool
	}{
		{"IntermediateDep", "intermediate", true},
		{"intermediate", "INTERMEDIATE", true},
		{"express", "press", true},
		{"express", "", true},
		{"a", "abc", false},
		{"express", "axios", false},
	}
	for _, tt := range tests {
		if got := containsLower(tt.s, tt.sub); got != tt.want {
			t.Errorf("containsLower(%q, %q) = %t", tt.s, tt.sub, got)
		}
	}
}
