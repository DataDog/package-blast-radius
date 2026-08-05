package viewer

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

func affected(name, version string, depth int, downloads int64) blast.JSONAffected {
	return blast.JSONAffected{
		Name:            name,
		Version:         version,
		Depth:           depth,
		WeeklyDownloads: downloads,
		Target:          "axios@1.14.1",
		Path:            []blast.JSONStep{{Package: name, Version: version, Requirement: "^1.0.0"}},
	}
}

func sampleDeduped() []blast.JSONAffected {
	return []blast.JSONAffected{
		affected("popular", "3.0.0", 1, 5000),
		affected("middling", "2.0.0", 2, 300),
		affected("obscure", "1.0.0", 3, 10),
		affected("unenriched", "1.0.0", 2, -1),
	}
}

func names(list []blast.JSONAffected) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.Name
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFilterAndSort(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"defaults to downloads descending", "", []string{"popular", "middling", "obscure", "unenriched"}},
		{"sort by name ascending", "sort=name&dir=asc", []string{"middling", "obscure", "popular", "unenriched"}},
		{"sort by name descending", "sort=name&dir=desc", []string{"unenriched", "popular", "obscure", "middling"}},
		{"sort by depth ascending", "sort=depth&dir=asc", []string{"popular", "middling", "unenriched", "obscure"}},
		{"filter by minimum depth", "minDepth=2", []string{"middling", "obscure", "unenriched"}},
		{"filter by maximum depth", "maxDepth=1", []string{"popular"}},
		{"filter by depth range", "minDepth=2&maxDepth=2", []string{"middling", "unenriched"}},
		{"filter by minimum downloads", "minDownloads=100", []string{"popular", "middling"}},
		{"search matches package name", "search=pop", []string{"popular"}},
		{"search is case insensitive", "search=POPULAR", []string{"popular"}},
		{"search that matches nothing", "search=zzz", []string{}},
		{"filters combine", "minDepth=2&minDownloads=5", []string{"middling", "obscure"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("bad query %q: %v", tt.query, err)
			}

			got := names(filterAndSort(sampleDeduped(), q))
			if !equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// filterAndSort must not reorder or mutate the list it is given, since it runs
// concurrently for every request against one shared slice.
func TestFilterAndSortDoesNotMutateInput(t *testing.T) {
	deduped := sampleDeduped()
	before := names(deduped)

	q, _ := url.ParseQuery("sort=name&dir=asc")
	filterAndSort(deduped, q)

	if after := names(deduped); !equal(before, after) {
		t.Errorf("input reordered: was %v, now %v", before, after)
	}
}

func TestMatchesSearchLooksThroughThePath(t *testing.T) {
	a := blast.JSONAffected{
		Name: "leaf",
		Path: []blast.JSONStep{
			{Package: "leaf", Version: "1.0.0"},
			{Package: "IntermediateDep", Version: "2.0.0"},
		},
	}

	for _, term := range []string{"leaf", "intermediate", "INTERMEDIATEDEP"} {
		if !matchesSearch(a, term) {
			t.Errorf("matchesSearch(%q) = false, want true", term)
		}
	}
	if matchesSearch(a, "absent") {
		t.Error(`matchesSearch("absent") = true, want false`)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want string // "gt", "lt", or "eq"
	}{
		{"2.0.0", "1.0.0", "gt"},
		{"1.0.0", "2.0.0", "lt"},
		{"1.0.0", "1.0.0", "eq"},
		{"1.10.0", "1.9.0", "gt"}, // numeric, not lexicographic
		{"1.0.10", "1.0.9", "gt"},
		{"v2.0.0", "1.0.0", "gt"}, // leading v is stripped
		{"1.0.1", "1.0", "gt"},    // longer wins when the prefix is equal
		{"1.0.0-beta", "1.0.0", "eq"},
	}

	for _, tt := range tests {
		got := compareVersions(tt.a, tt.b)
		var label string
		switch {
		case got > 0:
			label = "gt"
		case got < 0:
			label = "lt"
		default:
			label = "eq"
		}
		if label != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d (%s), want %s", tt.a, tt.b, got, label, tt.want)
		}
	}
}

func TestIntParam(t *testing.T) {
	tests := []struct {
		in   string
		def  int
		want int
	}{
		{"", 7, 7},
		{"42", 7, 42},
		{"0", 7, 7},    // zero falls back to the default
		{"-3", 7, 7},   // so does a negative
		{"junk", 7, 7}, // and an unparseable value
	}
	for _, tt := range tests {
		if got := intParam(tt.in, tt.def); got != tt.want {
			t.Errorf("intParam(%q, %d) = %d, want %d", tt.in, tt.def, got, tt.want)
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

func reportFixture() *blast.BlastResult {
	axios := blast.PackageVersion{System: blast.NPM, Name: "axios", Version: "1.14.1"}
	return &blast.BlastResult{
		Targets:    []blast.PackageVersion{axios},
		TotalEdges: 2915977,
		Affected: []blast.AffectedPackage{
			{
				PackageVersion:  blast.PackageVersion{System: blast.NPM, Name: "tremendous", Version: "3.11.0"},
				Depth:           1,
				Path:            []blast.PathStep{{Package: "tremendous", Version: "3.11.0", Requirement: "^1.6.1"}},
				Target:          axios,
				WeeklyDownloads: 24400,
			},
			// Same package at a lower version: dedup must keep 3.11.0.
			{
				PackageVersion:  blast.PackageVersion{System: blast.NPM, Name: "tremendous", Version: "3.9.0"},
				Depth:           1,
				Path:            []blast.PathStep{{Package: "tremendous", Version: "3.9.0", Requirement: "^1.6.1"}},
				Target:          axios,
				WeeklyDownloads: 24400,
			},
			{
				PackageVersion: blast.PackageVersion{System: blast.NPM, Name: "deep-dep", Version: "2.0.0"},
				Depth:          2,
				Path: []blast.PathStep{
					{Package: "deep-dep", Version: "2.0.0", Requirement: "^3.11.0"},
					{Package: "tremendous", Version: "3.11.0", Requirement: "^1.6.1"},
				},
				Target:          axios,
				WeeklyDownloads: 100,
			},
		},
		UniquePackages: 2,
		MaxDepth:       2,
		Elapsed:        1500 * time.Millisecond,
	}
}

// The analyzer's JSON contract must load into the viewer with summary fields
// and deduped package rows intact.
func TestLoadRoundTripsAnAnalyzeReport(t *testing.T) {
	path := writeReport(t, reportFixture())

	summary, deduped, depthCounts, err := load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if summary.Target != "axios@1.14.1" {
		t.Errorf("target = %q", summary.Target)
	}
	if summary.System != "NPM" {
		t.Errorf("system = %q", summary.System)
	}
	if summary.TotalEdges != 2915977 {
		t.Errorf("total_edges = %d", summary.TotalEdges)
	}
	if summary.TotalAffected != 3 {
		t.Errorf("total_affected = %d, want 3", summary.TotalAffected)
	}
	if summary.MaxDepth != 2 {
		t.Errorf("max_depth = %d", summary.MaxDepth)
	}
	if summary.Elapsed != "1.5s" {
		t.Errorf("elapsed = %q", summary.Elapsed)
	}

	if len(deduped) != 2 {
		t.Fatalf("got %d deduped packages, want 2", len(deduped))
	}
	byName := map[string]blast.JSONAffected{}
	for _, a := range deduped {
		byName[a.Name] = a
	}
	if got := byName["tremendous"].Version; got != "3.11.0" {
		t.Errorf("tremendous kept version %q, want the highest (3.11.0)", got)
	}
	if got := byName["tremendous"].WeeklyDownloads; got != 24400 {
		t.Errorf("tremendous downloads = %d, want 24400", got)
	}
	if got := len(byName["deep-dep"].Path); got != 2 {
		t.Errorf("deep-dep path has %d hops, want 2", got)
	}
	if byName["deep-dep"].Target != "axios@1.14.1" {
		t.Errorf("deep-dep target = %q", byName["deep-dep"].Target)
	}

	if depthCounts[1] != 1 || depthCounts[2] != 1 {
		t.Errorf("depth counts = %v, want one package at each of depths 1 and 2", depthCounts)
	}
}

func TestLoadRejectsNonReportFiles(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		if _, _, _, err := load(filepath.Join(dir, "absent.json")); err == nil {
			t.Error("got nil error for a missing file")
		}
	})

	t.Run("CSV mistaken for JSON", func(t *testing.T) {
		path := filepath.Join(dir, "report.csv")
		os.WriteFile(path, []byte("package,version\naxios,1.14.1\n"), 0o600)

		_, _, _, err := load(path)
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

		if _, _, _, err := load(path); err == nil {
			t.Error("got nil error for a truncated file")
		}
	})
}

func serveFixture(t *testing.T) *httptest.Server {
	t.Helper()

	summary, deduped, depthCounts, err := load(writeReport(t, reportFixture()))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, summary, deduped, depthCounts)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAPIPackages(t *testing.T) {
	srv := serveFixture(t)

	var body struct {
		Total   int                  `json:"total"`
		Offset  int                  `json:"offset"`
		Results []blast.JSONAffected `json:"results"`
	}

	get := func(query string) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/packages?" + query)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %s", resp.Status)
		}
		body.Results = nil
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
	}

	get("")
	if body.Total != 2 || len(body.Results) != 2 {
		t.Errorf("total = %d, results = %d, want 2 and 2", body.Total, len(body.Results))
	}
	if body.Results[0].Name != "tremendous" {
		t.Errorf("first result = %q, want the highest-download package", body.Results[0].Name)
	}

	// total reports the full match count, not the page size.
	get("limit=1")
	if body.Total != 2 || len(body.Results) != 1 {
		t.Errorf("limit=1: total = %d, results = %d, want 2 and 1", body.Total, len(body.Results))
	}

	get("limit=1&offset=1")
	if len(body.Results) != 1 || body.Results[0].Name != "deep-dep" {
		t.Errorf("offset=1 returned %v, want deep-dep", names(body.Results))
	}

	// An offset past the end is clamped rather than panicking.
	get("offset=99")
	if len(body.Results) != 0 {
		t.Errorf("offset=99 returned %v, want nothing", names(body.Results))
	}
}

func TestAPISummary(t *testing.T) {
	srv := serveFixture(t)

	resp, err := http.Get(srv.URL + "/api/summary")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if got["target"] != "axios@1.14.1" || got["system"] != "NPM" {
		t.Errorf("summary = %v", got)
	}
	if got["total_edges"].(float64) != 2915977 {
		t.Errorf("total_edges = %v", got["total_edges"])
	}
	if _, ok := got["depth_counts"]; !ok {
		t.Error("summary is missing depth_counts")
	}
}

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

	if len(rows) != 3 {
		t.Fatalf("got %d rows, want header + 2", len(rows))
	}
	wantHeader := []string{"package", "version", "depth", "weekly_downloads", "target", "path"}
	for i, h := range wantHeader {
		if rows[0][i] != h {
			t.Errorf("header[%d] = %q, want %q", i, rows[0][i], h)
		}
	}
	// The CSV column order must match the analyzer's own csv output.
	if rows[1][0] != "tremendous" || rows[1][3] != "24400" {
		t.Errorf("row = %v", rows[1])
	}
	if !strings.Contains(rows[2][5], "-->") {
		t.Errorf("path column = %q, want a rendered dependency chain", rows[2][5])
	}
}

func TestServesTheEmbeddedUI(t *testing.T) {
	srv := serveFixture(t)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	if !strings.HasPrefix(body, "<!DOCTYPE html>") {
		t.Errorf("body starts with %.40q, want an HTML document", body)
	}
	if !strings.Contains(body, "/api/packages") {
		t.Error("the UI does not reference the packages API it depends on")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
}
