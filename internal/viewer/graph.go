package viewer

import (
	"net/http"
	"sort"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

const (
	defaultGraphPageSize = 25
	maxGraphPageSize     = 200
)

// graphNode is one package in the expansion graph.
//
// Identity is a package *name*. A compromised package's versions are an
// attribute of its node rather than nodes of their own, so `axios@1.14.1` and
// `axios@1.14.2` expand into one subtree instead of two overlapping ones.
type graphNode struct {
	Name            string `json:"name"`
	WeeklyDownloads *int64 `json:"weekly_downloads"` // null when unenriched

	// How many packages depend on this one, so a node can say whether
	// expanding it will yield anything before anyone clicks it.
	DependentCount int `json:"dependent_count"`

	// Recorded affected versions. Zero for a node that only ever appears as an
	// intermediate hop, which the traversal never attributed on its own.
	VersionCount int  `json:"version_count"`
	MinDepth     int  `json:"min_depth"`
	Affected     bool `json:"affected"`

	// Compromised versions. Root nodes only.
	Versions []string `json:"versions,omitempty"`
}

type graphResponse struct {
	Package string      `json:"package,omitempty"`
	Total   int         `json:"total"`
	Offset  int         `json:"offset"`
	Results []graphNode `json:"results"`
}

func (s *store) graphNode(name string) graphNode {
	n := graphNode{Name: name}
	if id, ok := s.nodeIDs[name]; ok {
		n.DependentCount = len(s.dependents[id])
	}
	if p := s.pkg(name); p != nil {
		n.Affected = true
		n.WeeklyDownloads = s.downloadsPtr(p)
		n.VersionCount = p.VersionCount
		n.MinDepth = p.MinDepth
	}
	return n
}

// graphRoots collapses the target list to one node per compromised package
// name. The ranked list is already ordered by attributed packages, so
// first-seen order is the ranking.
func (s *store) graphRoots(search string) []graphNode {
	ranked := s.rankedTargets()
	roots := make([]graphNode, 0, len(ranked))
	at := make(map[string]int, len(ranked))

	for i := range ranked {
		t := &ranked[i]
		if search != "" && !containsLower(t.Name, search) {
			continue
		}
		pos, ok := at[t.Name]
		if !ok {
			pos = len(roots)
			at[t.Name] = pos
			roots = append(roots, s.graphNode(t.Name))
		}
		roots[pos].Versions = append(roots[pos].Versions, t.Version)
	}

	for i := range roots {
		vs := roots[i].Versions
		sort.Slice(vs, func(a, b int) bool { return blast.CompareVersions(vs[a], vs[b]) > 0 })
	}
	return roots
}

func (s *store) handleGraphRoots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	roots := s.graphRoots(q.Get("search"))
	results, total, offset := pageGraphNodes(roots, q.Get("limit"), q.Get("offset"))
	writeJSON(w, graphResponse{Total: total, Offset: offset, Results: results})
}

// handleGraphDependents expands one node into the packages that depend on it.
// An unknown name is an empty expansion rather than a 404: a leaf package is a
// legitimate node with nothing above it.
func (s *store) handleGraphDependents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("package")
	if name == "" {
		http.Error(w, "missing ?package=", http.StatusBadRequest)
		return
	}

	search := q.Get("search")
	// A missing name must not fall through to id 0, which is a real node.
	var list []int32
	if id, ok := s.nodeIDs[name]; ok {
		list = s.dependents[id]
	}

	// Already ordered by impact at build time, so the page is a slice of the
	// filtered names rather than a re-sort.
	nodes := make([]graphNode, 0, len(list))
	for _, id := range list {
		child := s.strings.str(id)
		if search != "" && !containsLower(child, search) {
			continue
		}
		nodes = append(nodes, s.graphNode(child))
	}

	results, total, offset := pageGraphNodes(nodes, q.Get("limit"), q.Get("offset"))
	writeJSON(w, graphResponse{Package: name, Total: total, Offset: offset, Results: results})
}

func pageGraphNodes(nodes []graphNode, rawLimit, rawOffset string) (page []graphNode, total, offset int) {
	limit := min(intParam(rawLimit, defaultGraphPageSize, 1), maxGraphPageSize)
	offset = intParam(rawOffset, 0, 0)
	total = len(nodes)
	if offset > total {
		offset = total
	}
	page = nodes[offset:]
	if len(page) > limit {
		page = page[:limit]
	}
	return page, total, offset
}
