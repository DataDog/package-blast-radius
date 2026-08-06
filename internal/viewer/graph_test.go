package viewer

import (
	"net/http"
	"slices"
	"testing"
)

func graphNames(nodes []graphNode) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}

// Roots are compromised package *names*, so a package compromised at several
// versions is one node carrying all of them.
func TestAPIGraphRoots(t *testing.T) {
	srv := serveFixture(t)

	var got graphResponse
	getJSON(t, srv, "/api/graph/roots", &got)

	if got.Total != 3 {
		t.Fatalf("total = %d, want the 3 compromised packages", got.Total)
	}

	// Ranked by attributed packages: axios reached four, evil one, ghost none.
	if want := []string{"axios", "evil", "ghost"}; !slices.Equal(graphNames(got.Results), want) {
		t.Errorf("roots = %v, want %v", graphNames(got.Results), want)
	}

	axios := got.Results[0]
	if !slices.Equal(axios.Versions, []string{"1.14.1"}) {
		t.Errorf("axios versions = %v", axios.Versions)
	}
	if axios.DependentCount != 3 {
		t.Errorf("axios dependent_count = %d, want 3", axios.DependentCount)
	}
	// A compromised package is not itself an affected entry in this report.
	if axios.Affected {
		t.Error("axios should not be marked affected")
	}

	// Nothing depends on ghost, so it is a root with an empty expansion rather
	// than a missing node.
	if ghost := got.Results[2]; ghost.DependentCount != 0 {
		t.Errorf("ghost dependent_count = %d, want 0", ghost.DependentCount)
	}
}

func TestAPIGraphRootsSearch(t *testing.T) {
	srv := serveFixture(t)

	var got graphResponse
	getJSON(t, srv, "/api/graph/roots?search=EVI", &got)

	if !slices.Equal(graphNames(got.Results), []string{"evil"}) {
		t.Errorf("search=EVI gave %v, want just evil", graphNames(got.Results))
	}
}

func TestAPIGraphDependents(t *testing.T) {
	srv := serveFixture(t)

	var got graphResponse
	getJSON(t, srv, "/api/graph/dependents?package=axios", &got)

	if got.Package != "axios" {
		t.Errorf("package = %q", got.Package)
	}

	// Ordered by weekly downloads descending, with the unenriched package last
	// because its count is unknown rather than zero.
	want := []string{"tremendous", "@scope/pkg", "unenriched"}
	if !slices.Equal(graphNames(got.Results), want) {
		t.Fatalf("dependents = %v, want %v", graphNames(got.Results), want)
	}

	tremendous := got.Results[0]
	if !tremendous.Affected || tremendous.VersionCount != 2 || tremendous.MinDepth != 1 {
		t.Errorf("tremendous = %+v, want an affected depth-1 package with 2 versions", tremendous)
	}
	if tremendous.WeeklyDownloads == nil || *tremendous.WeeklyDownloads != 24400 {
		t.Errorf("tremendous downloads = %v", tremendous.WeeklyDownloads)
	}
	if last := got.Results[2]; last.WeeklyDownloads != nil {
		t.Errorf("unenriched downloads = %v, want null", *last.WeeklyDownloads)
	}

	// Expanding again walks one hop further from the compromised package.
	var second graphResponse
	getJSON(t, srv, "/api/graph/dependents?package=tremendous", &second)
	if !slices.Equal(graphNames(second.Results), []string{"deep-dep"}) {
		t.Errorf("dependents[tremendous] = %v, want deep-dep", graphNames(second.Results))
	}
}

func TestAPIGraphDependentsPaginates(t *testing.T) {
	srv := serveFixture(t)

	var page graphResponse
	getJSON(t, srv, "/api/graph/dependents?package=axios&limit=2&offset=1", &page)

	if page.Total != 3 || page.Offset != 1 {
		t.Errorf("total/offset = %d/%d, want 3/1", page.Total, page.Offset)
	}
	if want := []string{"@scope/pkg", "unenriched"}; !slices.Equal(graphNames(page.Results), want) {
		t.Errorf("page = %v, want %v", graphNames(page.Results), want)
	}
}

// A leaf is a legitimate node with nothing above it, so expanding one is an
// empty result rather than an error.
func TestAPIGraphDependentsOfALeaf(t *testing.T) {
	srv := serveFixture(t)

	var got graphResponse
	getJSON(t, srv, "/api/graph/dependents?package=deep-dep", &got)

	if got.Total != 0 || len(got.Results) != 0 {
		t.Errorf("dependents[deep-dep] = %v, want nothing", graphNames(got.Results))
	}
}

func TestAPIGraphDependentsRequiresAPackage(t *testing.T) {
	srv := serveFixture(t)

	resp, err := http.Get(srv.URL + "/api/graph/dependents")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
