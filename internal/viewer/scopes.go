package viewer

import (
	"net/http"
	"sort"
)

// scopeRowLimit bounds how many scopes the Overview can list. A large report
// spreads its blast radius over thousands of them, and past the largest few
// dozen the rows are ones and twos that nobody is going to act on.
const scopeRowLimit = 40

// One npm scope's share of the blast radius.
type scopeRow struct {
	Scope    string `json:"scope"`
	Packages int    `json:"packages"`
	Versions int    `json:"versions"`
	// Summed per package, so consumers shared between them are counted more
	// than once. Null when the report carries no download data.
	WeeklyDownloads *int64 `json:"weekly_downloads"`
}

// scopesResponse groups the affected set by the scope each package publishes
// under: which organisations the blast radius actually lands on, as opposed to
// which ones the compromise came from.
//
// Unscoped packages are reported beside the ranking rather than as its largest
// row. "No scope" is not an organisation, and on npm it is around half of
// everything, so as a bar it would dwarf every row that carries a finding.
type scopesResponse struct {
	Scopes   []scopeRow `json:"scopes"`
	Unscoped scopeRow   `json:"unscoped"`
	// Scopes actually touched by the report, which is more than the rows above
	// whenever the limit truncates them.
	TotalScopes int `json:"total_scopes"`
	// Affected packages under a scope, unscoped ones excluded, so the rows can
	// be read as shares of the scoped part rather than of the whole report.
	ScopedPackages int `json:"scoped_packages"`
}

func (s *store) handleScopes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.scopeStats())
}

// Computed on first request and kept, for the same reason as the Pareto
// ranking: a session that never opens the Overview should not pay for it.
func (s *store) scopeStats() scopesResponse {
	s.scopesOnce.Do(func() { s.scopes = s.computeScopes() })
	return s.scopes
}

func (s *store) computeScopes() scopesResponse {
	byScope := make(map[string]*scopeRow)
	var unscoped scopeRow

	for i := range s.packages {
		p := &s.packages[i]
		row := &unscoped
		if scope := nameScope(s.strings.str(p.NameID)); scope != "" {
			if byScope[scope] == nil {
				byScope[scope] = &scopeRow{Scope: scope}
			}
			row = byScope[scope]
		}

		row.Packages++
		row.Versions += p.VersionCount
		if p.enriched() {
			row.WeeklyDownloads = addDownloads(row.WeeklyDownloads, p.WeeklyDownloads)
		}
	}

	ranked := make([]scopeRow, 0, len(byScope))
	scoped := 0
	for _, row := range byScope {
		ranked = append(ranked, *row)
		scoped += row.Packages
	}

	// Ties break on the scope name, so two runs over one report produce the
	// same chart; map iteration order alone would not.
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Packages != ranked[j].Packages {
			return ranked[i].Packages > ranked[j].Packages
		}
		if ranked[i].Versions != ranked[j].Versions {
			return ranked[i].Versions > ranked[j].Versions
		}
		return ranked[i].Scope < ranked[j].Scope
	})

	return scopesResponse{
		Scopes:         ranked[:min(len(ranked), scopeRowLimit)],
		Unscoped:       unscoped,
		TotalScopes:    len(ranked),
		ScopedPackages: scoped,
	}
}

func addDownloads(total *int64, add int64) *int64 {
	if total == nil {
		return &add
	}
	sum := *total + add
	return &sum
}
