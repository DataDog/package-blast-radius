package viewer

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

// notEnriched marks a weekly download count as unknown rather than zero.
// `analyze` writes -1 for every package when run without
// --enrich-with-download-count, and a genuine zero must not be confused with it.
const notEnriched int64 = -1

// versionPath is one affected version's concrete route to a compromised
// package. The intermediate package names live on the parent route, so only
// the per-hop versions and declared ranges vary between versions sharing it.
type versionPath struct {
	Version       string
	HopVersionIDs []int32 // len == len(route.HopIDs)
	ReqIDs        []int32 // len == depth; the range declared at each hop
}

// route is one distinct chain of intermediate packages to a compromised
// target. Versions whose paths traverse the same chain of package *names*
// collapse into a single route, differing only in their versionPath.
type route struct {
	ID       string  // stable, derived from the chain's content
	HopIDs   []int32 // intermediate package names; source and target excluded
	TargetID int32   // interned "axios@1.14.1"
	Versions []versionPath
}

func (r *route) depth() int { return len(r.HopIDs) + 1 }

type packageEntry struct {
	NameID          int32
	WeeklyDownloads int64 // notEnriched when unknown
	VersionCount    int
	MinDepth        int
	MaxDepth        int
	TargetIDs       []int32
	Routes          []route

	// Newest and oldest version the report actually recorded. These bound a
	// sparse set, not a continuous semver range: versions the traversal never
	// reached are simply absent.
	LatestVersion string
	OldestVersion string
}

func (p *packageEntry) enriched() bool { return p.WeeklyDownloads != notEnriched }

// targetSummary counts what a compromised package was *attributed* in this
// report. Traversal records one path per affected name@version, so these are
// attribution counts and not proof that no other target could reach a package.
type targetSummary struct {
	Name               string
	Version            string
	Ref                string // "name@version", as it appears on an affected entry
	AttributedPackages int
	AttributedVersions int
}

type store struct {
	meta     meta
	strings  *interner
	packages []packageEntry
	byName   map[string]int // package name -> index into packages

	// dependents[X] is the set of packages that depend on X. Paths run
	// affected -> target, but the graph view expands target -> outward, so the
	// adjacency is stored in the direction the reader traverses it.
	dependents map[int32][]int32

	// nodeIDs resolves a graph node's name back to its interned id. The
	// interner's own lookup map is released once the store is built, and a
	// graph node can be an intermediate hop that never appears in byName.
	nodeIDs map[string]int32

	targets            []targetSummary
	depthCounts        map[int]int // packages by min depth
	versionDepthCounts map[int]int // affected versions by their own route depth
	combinedDownloads  int64       // notEnriched when no package is enriched
	skippedPathless    int

	// The Pareto ranking and the scope breakdown are derived on first request
	// rather than at build time; see paretoStats. Everything else on the store
	// is immutable once built.
	paretoOnce sync.Once
	pareto     paretoResponse
	scopesOnce sync.Once
	scopes     scopesResponse
}

func (s *store) enriched() bool { return s.combinedDownloads != notEnriched }

func (s *store) pkg(name string) *packageEntry {
	if i, ok := s.byName[name]; ok {
		return &s.packages[i]
	}
	return nil
}

// ---------------------------------------------------------------------------
// Build
// ---------------------------------------------------------------------------

// buildKey identifies a route while the store is being built: a package plus
// the chain of names it traverses. One map for the whole build rather than a
// map per package, because the overwhelming majority of packages have exactly
// one route and would waste an allocation each.
type buildKey struct {
	pkgID int32
	chain string
}

type builder struct {
	in         *interner
	pkgs       map[int32]*packageEntry
	routes     map[buildKey]*route
	dependents map[int32]map[int32]struct{}
	targets    map[int32]*targetSummary
	targetPkgs map[buildKey]struct{} // (targetID, package name) pairs already counted
	skipped    int
}

func newBuilder(estimatedPackages int) *builder {
	return &builder{
		in:         newInterner(estimatedPackages * 4),
		pkgs:       make(map[int32]*packageEntry, estimatedPackages),
		routes:     make(map[buildKey]*route, estimatedPackages),
		dependents: make(map[int32]map[int32]struct{}, estimatedPackages),
		targets:    make(map[int32]*targetSummary),
		targetPkgs: make(map[buildKey]struct{}, estimatedPackages),
	}
}

func (b *builder) add(a *blast.JSONAffected) {
	// Every affected entry carries a path beginning with itself; without one
	// there is no depth and no place in the graph, so it cannot be shown.
	if len(a.Path) == 0 {
		b.skipped++
		return
	}

	nameID := b.in.id(a.Name)
	targetID := b.in.id(a.Target)
	depth := len(a.Path)

	hopIDs := make([]int32, 0, depth-1)
	hopVersionIDs := make([]int32, 0, depth-1)
	reqIDs := make([]int32, 0, depth)
	for i, step := range a.Path {
		reqIDs = append(reqIDs, b.in.id(step.Requirement))
		if i == 0 {
			continue // path[0] is the affected package itself
		}
		hopIDs = append(hopIDs, b.in.id(step.Package))
		hopVersionIDs = append(hopVersionIDs, b.in.id(step.Version))
	}

	b.recordEdges(a, targetID)

	pkg := b.pkgs[nameID]
	if pkg == nil {
		pkg = &packageEntry{NameID: nameID, WeeklyDownloads: notEnriched, MinDepth: depth, MaxDepth: depth}
		b.pkgs[nameID] = pkg
	}
	pkg.VersionCount++
	if depth < pkg.MinDepth {
		pkg.MinDepth = depth
	}
	if depth > pkg.MaxDepth {
		pkg.MaxDepth = depth
	}
	// Download counts are per package name, so any entry carries the same
	// value; taking the max skips the notEnriched sentinel.
	if a.WeeklyDownloads > pkg.WeeklyDownloads {
		pkg.WeeklyDownloads = a.WeeklyDownloads
	}
	if !slices.Contains(pkg.TargetIDs, targetID) {
		pkg.TargetIDs = append(pkg.TargetIDs, targetID)
	}

	b.countTargetAttribution(a, targetID, nameID)

	key := buildKey{pkgID: nameID, chain: routeChain(a.Target, a.Path)}
	r := b.routes[key]
	if r == nil {
		r = &route{HopIDs: hopIDs, TargetID: targetID}
		b.routes[key] = r
	}
	r.Versions = append(r.Versions, versionPath{
		Version:       a.Version,
		HopVersionIDs: hopVersionIDs,
		ReqIDs:        reqIDs,
	})
}

// recordEdges stores the reverse adjacency the graph view reads:
//
//	dependents[path[i+1]] gains path[i]
//	dependents[targetName] gains path[len-1]
func (b *builder) recordEdges(a *blast.JSONAffected, targetID int32) {
	for i := 0; i < len(a.Path)-1; i++ {
		b.addEdge(b.in.id(a.Path[i+1].Package), b.in.id(a.Path[i].Package))
	}
	last := a.Path[len(a.Path)-1].Package
	b.addEdge(b.in.id(targetName(b.in.str(targetID))), b.in.id(last))
}

func (b *builder) addEdge(dependency, dependent int32) {
	set := b.dependents[dependency]
	if set == nil {
		set = make(map[int32]struct{}, 4)
		b.dependents[dependency] = set
	}
	set[dependent] = struct{}{}
}

func (b *builder) countTargetAttribution(a *blast.JSONAffected, targetID, nameID int32) {
	t := b.targets[targetID]
	if t == nil {
		name, version := splitTargetRef(a.Target)
		t = &targetSummary{Name: name, Version: version, Ref: a.Target}
		b.targets[targetID] = t
	}
	t.AttributedVersions++
	seen := buildKey{pkgID: nameID, chain: a.Target}
	if _, ok := b.targetPkgs[seen]; !ok {
		b.targetPkgs[seen] = struct{}{}
		t.AttributedPackages++
	}
}

// routeChain is the grouping key for a route: the target plus the chain of
// intermediate package *names*. Intermediate versions are deliberately
// excluded, so express@4.21.2 -> body-parser@1.20.0 -> axios and
// express@4.20.0 -> body-parser@1.19.0 -> axios are one route "via body-parser".
func routeChain(target string, path []blast.JSONStep) string {
	var sb strings.Builder
	sb.WriteString(target)
	for _, step := range path[1:] {
		sb.WriteByte(0)
		sb.WriteString(step.Package)
	}
	return sb.String()
}

// routeID derives a route's identifier from its content so it stays stable
// across reloads of the same report. Indices would not: they leak into
// bookmarks once view state is URL-synced, and would silently repoint if
// iteration order ever changed.
func routeID(chain string) string {
	h := fnv.New64a()
	h.Write([]byte(chain))
	return fmt.Sprintf("%012x", h.Sum64()&0xffffffffffff)
}

func (b *builder) finish(summary meta) *store {
	s := &store{
		meta:               summary,
		strings:            b.in,
		byName:             make(map[string]int, len(b.pkgs)),
		depthCounts:        make(map[int]int),
		versionDepthCounts: make(map[int]int),
		combinedDownloads:  notEnriched,
		skippedPathless:    b.skipped,
	}

	routesByPkg := make(map[int32][]route, len(b.pkgs))
	for key, r := range b.routes {
		r.ID = routeID(key.chain)
		sort.Slice(r.Versions, func(i, j int) bool {
			return blast.CompareVersions(r.Versions[i].Version, r.Versions[j].Version) > 0
		})
		routesByPkg[key.pkgID] = append(routesByPkg[key.pkgID], *r)
	}

	s.packages = make([]packageEntry, 0, len(b.pkgs))
	for nameID, pkg := range b.pkgs {
		pkg.Routes = routesByPkg[nameID]
		sortRoutes(pkg.Routes)
		disambiguateRouteIDs(pkg.Routes)
		pkg.LatestVersion, pkg.OldestVersion = versionBounds(pkg.Routes)
		s.packages = append(s.packages, *pkg)
	}

	sort.Slice(s.packages, func(i, j int) bool {
		return lessByImpact(&s.packages[i], &s.packages[j], b.in)
	})

	var combined int64
	anyEnriched := false
	for i := range s.packages {
		p := &s.packages[i]
		s.byName[b.in.str(p.NameID)] = i
		s.depthCounts[p.MinDepth]++
		for j := range p.Routes {
			s.versionDepthCounts[p.Routes[j].depth()] += len(p.Routes[j].Versions)
		}
		if p.enriched() {
			anyEnriched = true
			combined += p.WeeklyDownloads
		}
	}
	if anyEnriched {
		s.combinedDownloads = combined
	}

	s.dependents = make(map[int32][]int32, len(b.dependents))
	s.nodeIDs = make(map[string]int32, len(b.dependents))
	for dependency, set := range b.dependents {
		list := make([]int32, 0, len(set))
		for dependent := range set {
			list = append(list, dependent)
		}
		s.sortDependents(list)
		s.dependents[dependency] = list
		s.nodeIDs[b.in.str(dependency)] = dependency
	}

	s.targets = make([]targetSummary, 0, len(b.targets))
	for _, t := range b.targets {
		s.targets = append(s.targets, *t)
	}
	sort.Slice(s.targets, func(i, j int) bool {
		if s.targets[i].AttributedPackages != s.targets[j].AttributedPackages {
			return s.targets[i].AttributedPackages > s.targets[j].AttributedPackages
		}
		return s.targets[i].Ref < s.targets[j].Ref
	})

	b.in.seal()
	return s
}

// sortDependents orders a node's children the way the graph view shows them:
// biggest real-world impact first, with unknown download counts last.
func (s *store) sortDependents(list []int32) {
	sort.Slice(list, func(i, j int) bool {
		a, b := s.pkg(s.strings.str(list[i])), s.pkg(s.strings.str(list[j]))
		switch {
		case a == nil && b == nil:
			return s.strings.str(list[i]) < s.strings.str(list[j])
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return lessByImpact(a, b, s.strings)
	})
}

// lessByImpact orders packages by weekly downloads descending, then by name.
// Unenriched packages sort last: their download count is unknown, not zero.
func lessByImpact(a, b *packageEntry, in *interner) bool {
	if a.enriched() != b.enriched() {
		return a.enriched()
	}
	if a.WeeklyDownloads != b.WeeklyDownloads {
		return a.WeeklyDownloads > b.WeeklyDownloads
	}
	return in.str(a.NameID) < in.str(b.NameID)
}

// versionBounds finds the newest and oldest version across every route. Each
// route's versions are already sorted descending, so only the ends matter.
func versionBounds(routes []route) (latest, oldest string) {
	for i := range routes {
		vs := routes[i].Versions
		if len(vs) == 0 {
			continue
		}
		if latest == "" || blast.CompareVersions(vs[0].Version, latest) > 0 {
			latest = vs[0].Version
		}
		last := vs[len(vs)-1].Version
		if oldest == "" || blast.CompareVersions(last, oldest) < 0 {
			oldest = last
		}
	}
	return latest, oldest
}

func sortRoutes(routes []route) {
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].depth() != routes[j].depth() {
			return routes[i].depth() < routes[j].depth()
		}
		if len(routes[i].Versions) != len(routes[j].Versions) {
			return len(routes[i].Versions) > len(routes[j].Versions)
		}
		return routes[i].ID < routes[j].ID
	})
}

// disambiguateRouteIDs guards the content-derived IDs against a hash
// collision within one package. A 48-bit hash over at most a few hundred
// routes makes this vanishingly unlikely, but a silent collision would point
// a bookmarked route at the wrong chain.
func disambiguateRouteIDs(routes []route) {
	seen := make(map[string]int, len(routes))
	for i := range routes {
		n := seen[routes[i].ID]
		seen[routes[i].ID] = n + 1
		if n > 0 {
			routes[i].ID = fmt.Sprintf("%s-%d", routes[i].ID, n)
		}
	}
}

// splitTargetRef decomposes "axios@1.14.1" and "@scope/pkg@1.2.3". Scoped npm
// names begin with @, so the split has to be on the last one.
func splitTargetRef(ref string) (name, version string) {
	if i := strings.LastIndex(ref, "@"); i > 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

// rankedTargets is every compromised version the report declares, ordered by
// how many packages it was attributed.
//
// s.targets is derived from affected entries, so a compromised version nothing
// depended on never appears there. The analyzer's own target list is the
// honest denominator, so those are folded back in with zero attribution rather
// than dropped.
func (s *store) rankedTargets() []targetSummary {
	out := slices.Clone(s.targets)
	seen := make(map[string]bool, len(s.meta.Targets))
	for _, t := range s.meta.Targets {
		ref := t.Name + "@" + t.Version
		if seen[ref] || s.targetByRef(ref) != nil {
			continue
		}
		seen[ref] = true
		out = append(out, targetSummary{Name: t.Name, Version: t.Version, Ref: ref})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].AttributedPackages != out[j].AttributedPackages {
			return out[i].AttributedPackages > out[j].AttributedPackages
		}
		return out[i].Ref < out[j].Ref
	})
	return out
}

func targetName(ref string) string {
	name, _ := splitTargetRef(ref)
	return name
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

// maybeDecompress wraps f in a gzip reader if it's gzip-compressed, detected
// by magic bytes rather than the file extension since callers may rename
// downloaded reports.
func maybeDecompress(f *os.File, path string) (io.Reader, error) {
	br := bufio.NewReader(f)
	magic, err := br.Peek(2)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("decompressing %s: %w", path, err)
		}
		return gr, nil
	}
	return br, nil
}

// loadStore stream-decodes a `blast-radius analyze --output json` file into an
// aggregated store. Entries are folded in as they arrive rather than collected
// first: these files reach a gigabyte, and holding the decoded array costs
// more than the aggregate it produces.
func loadStore(jsonPath string) (*store, error) {
	fmt.Fprintf(os.Stderr, "Loading %s...\n", jsonPath)
	start := time.Now()

	f, err := os.Open(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", jsonPath, err)
	}
	defer f.Close()

	r, err := maybeDecompress(f, jsonPath)
	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(r)

	openTok, err := dec.Token()
	if err != nil || openTok != json.Delim('{') {
		return nil, fmt.Errorf(
			"expected a JSON object at the start of %s; did you forget --output json when generating this file?", jsonPath)
	}

	var summary meta
	var b *builder

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("reading JSON token: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("expected an object key (string), got %v", tok)
		}

		switch key {
		case "target":
			err = dec.Decode(&summary.Target)
		case "targets":
			err = dec.Decode(&summary.Targets)
		case "system":
			err = dec.Decode(&summary.System)
		case "total_edges":
			err = dec.Decode(&summary.TotalEdges)
		case "total_affected":
			err = dec.Decode(&summary.TotalAffected)
		case "unique_packages":
			err = dec.Decode(&summary.UniquePackages)
		case "max_depth":
			err = dec.Decode(&summary.MaxDepth)
		case "elapsed":
			err = dec.Decode(&summary.Elapsed)
		case "affected":
			// unique_packages may not have been read yet; it only sizes the
			// initial allocations.
			b = newBuilder(max(summary.UniquePackages, 1024))
			err = streamAffected(dec, b)
		default:
			var skip json.RawMessage
			err = dec.Decode(&skip)
		}
		if err != nil {
			return nil, fmt.Errorf("decoding %q: %w", key, err)
		}
	}

	if b == nil {
		b = newBuilder(1024)
	}

	fmt.Fprintf(os.Stderr, "Aggregating...\n")
	s := b.finish(summary)

	fmt.Fprintf(os.Stderr, "Ready: %d packages, %d versions in %s\n",
		len(s.packages), summary.TotalAffected, time.Since(start).Round(time.Millisecond))
	if s.skippedPathless > 0 {
		fmt.Fprintf(os.Stderr, "  Skipped %d entries with no recorded path\n", s.skippedPathless)
	}
	for d := 1; d <= summary.MaxDepth; d++ {
		if c, ok := s.depthCounts[d]; ok {
			fmt.Fprintf(os.Stderr, "  Depth %d: %d packages\n", d, c)
		}
	}

	return s, nil
}

func streamAffected(dec *json.Decoder, b *builder) error {
	if _, err := dec.Token(); err != nil { // opening [
		return err
	}

	n := 0
	var a blast.JSONAffected
	for dec.More() {
		a = blast.JSONAffected{}
		if err := dec.Decode(&a); err != nil {
			return fmt.Errorf("entry %d: %w", n, err)
		}
		b.add(&a)
		n++
		if n%500000 == 0 {
			fmt.Fprintf(os.Stderr, "  Read %d entries...\n", n)
		}
	}

	if _, err := dec.Token(); err != nil { // closing ]
		return err
	}
	return nil
}
