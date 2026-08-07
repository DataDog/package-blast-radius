package viewer

import (
	"net/http"
	"slices"
	"sort"
)

// paretoRowLimit bounds how many compromised packages the chart names. Whatever
// is left is summed into one tail row, so the chart still adds up.
const paretoRowLimit = 14

// The Overview's Pareto chart answers one question: of the compromised packages
// this report names, which ones account for the affected set?
//
// Traversal records one path per affected name@version, so every affected version
// is attributed to exactly one compromised package. The version series therefore
// partitions the report: bars do not overlap, and the rows plus the tail add up
// to all of it. The package series is nearly a partition but not quite, because
// one package can have versions attributed to different compromised packages, so
// its running total is a union rather than a sum.
type paretoRow struct {
	// Package is a "name@version" reference, the compromised version itself.
	Package string `json:"package"`
	Count   int    `json:"count"`
	// CumulativeCount is every row down to this one taken together, counting each
	// affected version or package once.
	CumulativeCount int `json:"cumulative_count"`
}

type paretoSeries struct {
	Total int `json:"total"`
	// DistinctSources counts the compromised packages that were attributed
	// anything at all. A report can name thousands nothing depended on, and they
	// belong in neither the rows nor the tail.
	DistinctSources int         `json:"distinct_sources"`
	Rows            []paretoRow `json:"rows"`
}

// paretoResponse is the ranking counted both ways: affected versions, and the
// distinct package names those versions belong to.
type paretoResponse struct {
	Versions paretoSeries `json:"versions"`
	Packages paretoSeries `json:"packages"`
}

func (s *store) handlePareto(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.paretoStats())
}

// paretoStats walks every recorded route, so it is computed on first request and
// kept rather than built during load. Startup already takes minutes on the larger
// reports, and a session that never opens the Overview should not pay for a chart
// it does not draw.
func (s *store) paretoStats() paretoResponse {
	s.paretoOnce.Do(func() { s.pareto = s.computePareto() })
	return s.pareto
}

type sourceCount struct {
	id    int32
	count int
}

func (s *store) computePareto() paretoResponse {
	versionsBy, packagesBy, totalVersions := s.countByTarget()

	versionRanks := s.rankSources(versionsBy)
	packageRanks := s.rankSources(packagesBy)

	return paretoResponse{
		Versions: paretoSeries{
			Total:           totalVersions,
			DistinctSources: len(versionsBy),
			Rows:            rows(s, versionRanks, s.versionsCoveredAt(versionRanks)),
		},
		Packages: paretoSeries{
			Total:           len(s.packages),
			DistinctSources: len(packagesBy),
			Rows:            rows(s, packageRanks, s.packagesCoveredAt(packageRanks)),
		},
	}
}

// countByTarget credits every compromised package with what depends on it, and
// counts the version total alongside.
//
// The total is counted here rather than read from meta.TotalAffected: entries the
// loader skipped for having no path never became routes, and a denominator that
// included them would make every bar under-read.
func (s *store) countByTarget() (versionsBy, packagesBy map[int32]int, totalVersions int) {
	versionsBy = make(map[int32]int)
	packagesBy = make(map[int32]int)

	// A package with several routes to the same compromised package is still one
	// affected package, so the package unit dedupes across the whole package.
	var inPackage []int32

	for i := range s.packages {
		p := &s.packages[i]
		inPackage = inPackage[:0]

		for j := range p.Routes {
			r := &p.Routes[j]
			totalVersions += len(r.Versions)
			versionsBy[r.TargetID] += len(r.Versions)

			if !slices.Contains(inPackage, r.TargetID) {
				inPackage = append(inPackage, r.TargetID)
				packagesBy[r.TargetID]++
			}
		}
	}
	return versionsBy, packagesBy, totalVersions
}

// rankSources orders compromised packages by what they account for and keeps the
// head. Ties break on the reference so two runs over one report produce the same
// chart; map iteration order alone would not.
func (s *store) rankSources(counts map[int32]int) []sourceCount {
	ranked := make([]sourceCount, 0, len(counts))
	for id, c := range counts {
		ranked = append(ranked, sourceCount{id: id, count: c})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count > ranked[j].count
		}
		return s.strings.str(ranked[i].id) < s.strings.str(ranked[j].id)
	})
	return ranked[:min(len(ranked), paretoRowLimit)]
}

// versionsCoveredAt buckets affected versions by the rank that accounts for them.
// A version has exactly one compromised package, so this is a plain tally.
func (s *store) versionsCoveredAt(ranked []sourceCount) []int {
	rankOf := rankIndex(ranked)
	covered := make([]int, len(ranked))

	for i := range s.packages {
		p := &s.packages[i]
		for j := range p.Routes {
			r := &p.Routes[j]
			if rank, ok := rankOf[r.TargetID]; ok {
				covered[rank] += len(r.Versions)
			}
		}
	}
	return covered
}

// packagesCoveredAt buckets affected packages by the best-ranked compromised
// package they are attributed to.
//
// A package can be attributed to several, so the running total is a union rather
// than a sum. A set per row would answer that at the cost of a bitset over every
// package, and is not needed: rows are already ranked, so whatever a prefix
// accounts for is accounted for by its best-ranked member. Recording each package
// against that one rank turns the unions into a prefix sum.
func (s *store) packagesCoveredAt(ranked []sourceCount) []int {
	rankOf := rankIndex(ranked)
	covered := make([]int, len(ranked))

	for i := range s.packages {
		p := &s.packages[i]
		best := -1
		for j := range p.Routes {
			if rank, ok := rankOf[p.Routes[j].TargetID]; ok && (best < 0 || rank < best) {
				best = rank
			}
		}
		if best >= 0 {
			covered[best]++
		}
	}
	return covered
}

func rankIndex(ranked []sourceCount) map[int32]int {
	rankOf := make(map[int32]int, len(ranked))
	for i, source := range ranked {
		rankOf[source.id] = i
	}
	return rankOf
}

func rows(s *store, ranked []sourceCount, coveredAt []int) []paretoRow {
	out := make([]paretoRow, 0, len(ranked))
	cumulative := 0
	for i, source := range ranked {
		cumulative += coveredAt[i]
		out = append(out, paretoRow{
			Package:         s.strings.str(source.id),
			Count:           source.count,
			CumulativeCount: cumulative,
		})
	}
	return out
}
