package viewer

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

const (
	defaultPageSize = 100
	maxPageSize     = 500
	// routePreviewLimit bounds how many routes a list row carries. The full
	// set is always available from /api/package; this only keeps the list
	// payload small for the rare package with hundreds of routes.
	routePreviewLimit    = 5
	defaultVersionsLimit = 100
	maxVersionsLimit     = 1000
)

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------

type targetResponse struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Ref     string `json:"ref"`
	// Attribution counts, not exhaustive impact: traversal records one path
	// per affected name@version, so a package reachable from several targets
	// is only counted against the one that reached it first.
	AttributedPackages int `json:"attributed_packages"`
	AttributedVersions int `json:"attributed_versions"`
}

type summaryResponse struct {
	Target                  string           `json:"target"`
	Targets                 []targetResponse `json:"targets"`
	System                  string           `json:"system"`
	TotalEdges              int              `json:"total_edges"`
	TotalAffected           int              `json:"total_affected"`
	UniquePackages          int              `json:"unique_packages"`
	DirectDependents        int              `json:"direct_dependents"`
	MaxDepth                int              `json:"max_depth"`
	Elapsed                 string           `json:"elapsed"`
	DepthCounts             map[int]int      `json:"depth_counts"`
	VersionDepthCounts      map[int]int      `json:"version_depth_counts"`
	CombinedWeeklyDownloads *int64           `json:"combined_weekly_downloads"`
	Enriched                bool             `json:"enriched"`
	SourcePath              string           `json:"source_path"`
}

type routeResponse struct {
	ID           string   `json:"id"`
	Hops         []string `json:"hops"`
	Depth        int      `json:"depth"`
	Target       string   `json:"target"`
	VersionCount int      `json:"version_count"`

	// Populated only by /api/package, and only for the requested route.
	Versions       []versionResponse `json:"versions,omitempty"`
	VersionsOffset int               `json:"versions_offset,omitempty"`
	VersionsTotal  int               `json:"versions_total,omitempty"`
}

type versionResponse struct {
	Version string         `json:"version"`
	Steps   []stepResponse `json:"steps"`
}

type stepResponse struct {
	Package     string `json:"package"`
	Version     string `json:"version"`
	Requirement string `json:"requirement"`
}

type packageResponse struct {
	Name string `json:"name"`
	// Bounds of the recorded version set, not a continuous affected range.
	LatestRecordedVersion string          `json:"latest_recorded_version"`
	OldestRecordedVersion string          `json:"oldest_recorded_version"`
	VersionCount          int             `json:"version_count"`
	MinDepth              int             `json:"min_depth"`
	MaxDepth              int             `json:"max_depth"`
	WeeklyDownloads       *int64          `json:"weekly_downloads"` // null when unenriched
	Targets               []string        `json:"targets"`
	RoutesTotal           int             `json:"routes_total"`
	Routes                []routeResponse `json:"routes"`

	// Populated only by /api/package: how many packages depend on the traced
	// package and on each of its hops. The graph view expands nodes upward, and
	// this saves it a request per node just to find out there is nothing there.
	DependentCounts map[string]int `json:"dependent_counts,omitempty"`
}

type packageListResponse struct {
	Total   int               `json:"total"`
	Offset  int               `json:"offset"`
	Results []packageResponse `json:"results"`
}

// ---------------------------------------------------------------------------
// Projection
// ---------------------------------------------------------------------------

func (s *store) downloadsPtr(p *packageEntry) *int64 {
	if !p.enriched() {
		return nil
	}
	v := p.WeeklyDownloads
	return &v
}

func (s *store) routeResponse(r *route) routeResponse {
	return routeResponse{
		ID:           r.ID,
		Hops:         s.strings.strs32(r.HopIDs),
		Depth:        r.depth(),
		Target:       s.strings.str(r.TargetID),
		VersionCount: len(r.Versions),
	}
}

// packageResponse projects a stored package for the list tier: no concrete
// paths, and at most routePreviewLimit routes.
//
// A row whose name does not contain the search matched on one of its routes,
// and the list only shows the first one. Leading with a route the query is
// nowhere in makes a real hit read as a bug, so the matching route is promoted
// to the front of the preview.
func (s *store) packageResponse(p *packageEntry, search string) packageResponse {
	promoted := make([]bool, len(p.Routes))
	order := make([]int, 0, len(p.Routes))
	if search != "" && !containsLower(s.strings.str(p.NameID), search) {
		for i := range p.Routes {
			if s.routeMatches(&p.Routes[i], search) {
				promoted[i] = true
				order = append(order, i)
			}
		}
	}
	for i := range p.Routes {
		if !promoted[i] {
			order = append(order, i)
		}
	}

	limit := min(len(order), routePreviewLimit)
	routes := make([]routeResponse, 0, limit)
	for _, i := range order[:limit] {
		routes = append(routes, s.routeResponse(&p.Routes[i]))
	}
	return packageResponse{
		Name:                  s.strings.str(p.NameID),
		LatestRecordedVersion: p.LatestVersion,
		OldestRecordedVersion: p.OldestVersion,
		VersionCount:          p.VersionCount,
		MinDepth:              p.MinDepth,
		MaxDepth:              p.MaxDepth,
		WeeklyDownloads:       s.downloadsPtr(p),
		Targets:               s.strings.strs32(p.TargetIDs),
		RoutesTotal:           len(p.Routes),
		Routes:                routes,
	}
}

// steps rebuilds one version's concrete path. path[0] is the affected package
// itself, and every step's requirement is the range it declares for whatever
// comes next, the target included.
func (s *store) steps(pkgName string, r *route, vp *versionPath) []stepResponse {
	steps := make([]stepResponse, 0, len(vp.ReqIDs))
	steps = append(steps, stepResponse{
		Package:     pkgName,
		Version:     vp.Version,
		Requirement: s.requirementAt(vp, 0),
	})
	for i, hopID := range r.HopIDs {
		steps = append(steps, stepResponse{
			Package:     s.strings.str(hopID),
			Version:     s.strings.str(vp.HopVersionIDs[i]),
			Requirement: s.requirementAt(vp, i+1),
		})
	}
	return steps
}

func (s *store) requirementAt(vp *versionPath, i int) string {
	if i >= len(vp.ReqIDs) {
		return ""
	}
	return s.strings.str(vp.ReqIDs[i])
}

// ---------------------------------------------------------------------------
// Filtering and sorting
// ---------------------------------------------------------------------------

type packageQuery struct {
	search string
	depths map[int]bool

	// Closed range on min depth. A zero bound is absent: no floor, no ceiling.
	minDepth int
	maxDepth int
	target   string
	// scope is the npm scope the affected package itself publishes under, which
	// is the downstream side: the target filter is the compromised one.
	scope        string
	minDownloads int64
	sortBy       string
	desc         bool

	// Whether the caller picked the sort column, which decides if a search can
	// reorder rows by relevance or has to leave the requested order alone.
	sortPicked bool
}

func parsePackageQuery(q url.Values) packageQuery {
	pq := packageQuery{
		search:       q.Get("search"),
		target:       q.Get("target"),
		scope:        q.Get("scope"),
		minDownloads: int64(intParam(q.Get("minDownloads"), 0, 0)),
		sortBy:       q.Get("sort"),
		desc:         q.Get("dir") != "asc",
		sortPicked:   q.Get("sort") != "",
	}
	if pq.sortBy == "" {
		pq.sortBy = "weekly_downloads"
	}

	// Distance arrives as a minDepth/maxDepth range, or as repeated
	// ?depth=1&depth=2 for an arbitrary set.
	if raw := q["depth"]; len(raw) > 0 {
		pq.depths = make(map[int]bool, len(raw))
		for _, v := range raw {
			if n, err := strconv.Atoi(v); err == nil {
				pq.depths[n] = true
			}
		}
	} else {
		pq.minDepth = intParam(q.Get("minDepth"), 0, 0)
		pq.maxDepth = intParam(q.Get("maxDepth"), 0, 0)
	}
	return pq
}

func (s *store) matches(p *packageEntry, pq packageQuery) bool {
	if pq.depths != nil && !pq.depths[p.MinDepth] {
		return false
	}
	if pq.minDepth > 0 && p.MinDepth < pq.minDepth {
		return false
	}
	if pq.maxDepth > 0 && p.MinDepth > pq.maxDepth {
		return false
	}
	if pq.minDownloads > 0 && (!p.enriched() || p.WeeklyDownloads < pq.minDownloads) {
		return false
	}
	if pq.target != "" && !s.hasTarget(p, pq.target) {
		return false
	}
	if pq.scope != "" && nameScope(s.strings.str(p.NameID)) != pq.scope {
		return false
	}
	if pq.search != "" && !s.matchesSearch(p, pq.search) {
		return false
	}
	return true
}

// hasTarget accepts a full "name@version" reference, or just the compromised
// package's name, so the Overview can filter by target package without
// enumerating its versions.
//
// It also accepts a bare "@scope", which the Overview's scope panel links to.
// That is unambiguous: a scoped npm name always carries a slash, so an @-prefixed
// value without one cannot be a package.
func (s *store) hasTarget(p *packageEntry, want string) bool {
	scope := strings.HasPrefix(want, "@") && !strings.Contains(want, "/")

	for _, id := range p.TargetIDs {
		ref := s.strings.str(id)
		if ref == want || targetName(ref) == want {
			return true
		}
		if scope && nameScope(targetName(ref)) == want {
			return true
		}
	}
	return false
}

// nameScope is the npm scope a package name belongs to, or "" when it has none.
// It reads both sides: a compromised package's scope and an affected one's.
func nameScope(name string) string {
	if !strings.HasPrefix(name, "@") {
		return ""
	}
	if slash := strings.Index(name, "/"); slash > 0 {
		return name[:slash]
	}
	return ""
}

func (s *store) matchesSearch(p *packageEntry, search string) bool {
	if containsLower(s.strings.str(p.NameID), search) {
		return true
	}
	for i := range p.Routes {
		if s.routeMatches(&p.Routes[i], search) {
			return true
		}
	}
	return false
}

// routeMatches reports whether the search hits the route's target or any of
// the intermediate dependencies it passes through.
func (s *store) routeMatches(r *route, search string) bool {
	if containsLower(s.strings.str(r.TargetID), search) {
		return true
	}
	for _, hopID := range r.HopIDs {
		if containsLower(s.strings.str(hopID), search) {
			return true
		}
	}
	return false
}

// Relevance tiers for a search hit. A query is nearly always the name of a
// package someone has in mind, and matching a route hop pulls in every package
// that merely reaches through it, so a name match has to outrank one.
const (
	rankExactName = iota
	rankNamePrefix
	rankNameSubstring
	rankRoute
)

func (s *store) searchRank(p *packageEntry, search string) int {
	name := strings.ToLower(s.strings.str(p.NameID))
	q := strings.ToLower(search)
	switch {
	case name == q:
		return rankExactName
	case strings.HasPrefix(name, q):
		return rankNamePrefix
	case strings.Contains(name, q):
		return rankNameSubstring
	}
	return rankRoute
}

// selectPackages returns indices into s.packages rather than copies; a filter
// that matches most of a 77k-package report should not duplicate it.
func (s *store) selectPackages(pq packageQuery) []int {
	idx := make([]int, 0, len(s.packages))
	for i := range s.packages {
		if s.matches(&s.packages[i], pq) {
			idx = append(idx, i)
		}
	}

	// A report with no download data has no download column either, so the
	// viewer's implicit sort is distance ascending. Mirror it or the header
	// caret points at a column the rows are not actually ordered by.
	if !pq.sortPicked && !s.enriched() {
		pq.sortBy = "depth"
		pq.desc = false
	}

	// A search only reorders rows while the caller is on the implicit sort;
	// once a column is picked, that column is what the rows are ranked by.
	byRelevance := pq.search != "" && !pq.sortPicked

	// s.packages is already ordered by impact, so the default sort is free.
	if !byRelevance && pq.sortBy == "weekly_downloads" && pq.desc {
		return idx
	}

	// Ranks are precomputed into a lookup keyed by package index: the
	// comparator runs O(n log n) times and must not redo the string work.
	var ranks []int8
	if byRelevance {
		ranks = make([]int8, len(s.packages))
		for _, i := range idx {
			ranks[i] = int8(s.searchRank(&s.packages[i], pq.search))
		}
	}

	sort.SliceStable(idx, func(a, b int) bool {
		if byRelevance && ranks[idx[a]] != ranks[idx[b]] {
			return ranks[idx[a]] < ranks[idx[b]]
		}
		return s.lessBy(&s.packages[idx[a]], &s.packages[idx[b]], pq)
	})
	return idx
}

func (s *store) lessBy(a, b *packageEntry, pq packageQuery) bool {
	cmp := 0
	switch pq.sortBy {
	case "name":
		cmp = strings.Compare(s.strings.str(a.NameID), s.strings.str(b.NameID))
	case "depth":
		cmp = a.MinDepth - b.MinDepth
	case "version_count":
		cmp = a.VersionCount - b.VersionCount
	case "routes":
		cmp = len(a.Routes) - len(b.Routes)
	case "version":
		cmp = blast.CompareVersions(a.LatestVersion, b.LatestVersion)
	default: // weekly_downloads
		// An unknown download count is not a small one, so unenriched
		// packages sink to the bottom whichever way the column is sorted.
		if a.enriched() != b.enriched() {
			return a.enriched()
		}
		cmp = cmpInt64(a.WeeklyDownloads, b.WeeklyDownloads)
	}

	if pq.desc {
		cmp = -cmp
	}
	if cmp != 0 {
		return cmp < 0
	}
	// Name breaks ties and stays ascending in both directions, so reversing
	// the sort does not scramble rows that are equal on the chosen column.
	return s.strings.str(a.NameID) < s.strings.str(b.NameID)
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *store) handleSummary(sourcePath string) http.HandlerFunc {
	ranked := s.rankedTargets()
	targets := make([]targetResponse, 0, len(ranked))
	for _, t := range ranked {
		targets = append(targets, targetResponse(t))
	}

	resp := summaryResponse{
		Target:             s.meta.Target,
		Targets:            targets,
		System:             s.meta.System,
		TotalEdges:         s.meta.TotalEdges,
		TotalAffected:      s.meta.TotalAffected,
		UniquePackages:     len(s.packages),
		DirectDependents:   s.depthCounts[1],
		MaxDepth:           s.meta.MaxDepth,
		Elapsed:            s.meta.Elapsed,
		DepthCounts:        s.depthCounts,
		VersionDepthCounts: s.versionDepthCounts,
		Enriched:           s.enriched(),
		SourcePath:         sourcePath,
	}
	if s.enriched() {
		v := s.combinedDownloads
		resp.CombinedWeeklyDownloads = &v
	}

	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, resp)
	}
}

func (s *store) targetByRef(ref string) *targetSummary {
	for i := range s.targets {
		if s.targets[i].Ref == ref {
			return &s.targets[i]
		}
	}
	return nil
}

func (s *store) handlePackages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pq := parsePackageQuery(q)
	idx := s.selectPackages(pq)

	limit := min(intParam(q.Get("limit"), defaultPageSize, 1), maxPageSize)
	offset := intParam(q.Get("offset"), 0, 0)

	total := len(idx)
	if offset > total {
		offset = total
	}
	page := idx[offset:]
	if len(page) > limit {
		page = page[:limit]
	}

	results := make([]packageResponse, 0, len(page))
	for _, i := range page {
		results = append(results, s.packageResponse(&s.packages[i], pq.search))
	}

	writeJSON(w, packageListResponse{Total: total, Offset: offset, Results: results})
}

// handlePackage serves one package's complete route list, plus a page of
// concrete versions for the requested route.
//
// The name arrives as a query parameter rather than a path segment because
// scoped npm names contain a slash, and an encoded %2F does not survive
// ServeMux's segment matching intact.
func (s *store) handlePackage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		http.Error(w, "missing ?name=", http.StatusBadRequest)
		return
	}

	p := s.pkg(name)
	if p == nil {
		http.Error(w, fmt.Sprintf("no package %q in this report", name), http.StatusNotFound)
		return
	}

	// The detail tier replaces the preview with every route, so it has no
	// search to order them by.
	resp := s.packageResponse(p, "")
	resp.Routes = make([]routeResponse, 0, len(p.Routes))
	for i := range p.Routes {
		resp.Routes = append(resp.Routes, s.routeResponse(&p.Routes[i]))
	}
	resp.DependentCounts = s.dependentCounts(name, resp.Routes)

	// Default to the first route so a caller that just wants "show me this
	// package" gets a usable path without a second round trip.
	wanted := q.Get("route")
	target := 0
	if wanted != "" {
		target = -1
		for i := range resp.Routes {
			if resp.Routes[i].ID == wanted {
				target = i
				break
			}
		}
		if target < 0 {
			http.Error(w, fmt.Sprintf("no route %q on package %q", wanted, name), http.StatusNotFound)
			return
		}
	}
	if len(resp.Routes) == 0 {
		writeJSON(w, resp)
		return
	}

	limit := min(intParam(q.Get("versionsLimit"), defaultVersionsLimit, 1), maxVersionsLimit)
	offset := intParam(q.Get("versionsOffset"), 0, 0)

	stored := &p.Routes[target]
	versions := stored.Versions
	if offset > len(versions) {
		offset = len(versions)
	}
	versions = versions[offset:]
	if len(versions) > limit {
		versions = versions[:limit]
	}

	out := make([]versionResponse, 0, len(versions))
	for i := range versions {
		out = append(out, versionResponse{
			Version: versions[i].Version,
			Steps:   s.steps(name, stored, &versions[i]),
		})
	}
	resp.Routes[target].Versions = out
	resp.Routes[target].VersionsOffset = offset
	resp.Routes[target].VersionsTotal = len(stored.Versions)

	writeJSON(w, resp)
}

// handleDownload streams the complete filtered set as CSV: one row per
// (affected package, route), with every recorded version listed. Versions are
// comma-joined so a downloaded file matches the shape ParseCompromisedCSV
// reads, letting one run's export drive the next.
func (s *store) handleDownload(w http.ResponseWriter, r *http.Request) {
	idx := s.selectPackages(parsePackageQuery(r.URL.Query()))

	filename := fmt.Sprintf("blast-radius-%d.csv", time.Now().Unix())
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	cw := csv.NewWriter(w)
	defer cw.Flush()

	cw.Write([]string{
		"affected_package", "affected_package_versions", "version_count",
		"depth", "weekly_downloads",
		"compromised_package", "compromised_package_version", "example_path",
	})

	for _, i := range idx {
		p := &s.packages[i]
		name := s.strings.str(p.NameID)
		downloads := ""
		if p.enriched() {
			downloads = strconv.FormatInt(p.WeeklyDownloads, 10)
		}
		for j := range p.Routes {
			route := &p.Routes[j]
			versions := make([]string, 0, len(route.Versions))
			for k := range route.Versions {
				versions = append(versions, route.Versions[k].Version)
			}
			target := s.strings.str(route.TargetID)
			// The example is the path of the first recorded version, not one that
			// holds for every version in the row.
			example := ""
			if len(route.Versions) > 0 {
				example = formatPathForCSV(
					toJSONSteps(s.steps(name, route, &route.Versions[0])), target)
			}
			targetName, targetVersion := splitTargetRef(target)
			cw.Write([]string{
				name,
				strings.Join(versions, ","),
				strconv.Itoa(len(versions)),
				strconv.Itoa(route.depth()),
				downloads,
				targetName,
				targetVersion,
				example,
			})
		}
	}
}

func toJSONSteps(steps []stepResponse) []blast.JSONStep {
	out := make([]blast.JSONStep, len(steps))
	for i, s := range steps {
		out[i] = blast.JSONStep{Package: s.Package, Version: s.Version, Requirement: s.Requirement}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
