package viewer

import (
	"testing"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

func TestScopesGroupsAffectedPackages(t *testing.T) {
	var resp scopesResponse
	getJSON(t, serveFixture(t), "/api/scopes", &resp)

	if len(resp.Scopes) != 1 || resp.Scopes[0].Scope != "@scope" {
		t.Fatalf("scopes = %+v, want one row for @scope", resp.Scopes)
	}

	row := resp.Scopes[0]
	if row.Packages != 1 || row.Versions != 2 {
		t.Errorf("@scope = %d packages and %d versions, want 1 and 2", row.Packages, row.Versions)
	}
	if row.WeeklyDownloads == nil || *row.WeeklyDownloads != 5000 {
		t.Errorf("@scope downloads = %v, want 5000", row.WeeklyDownloads)
	}

	// The unscoped packages are the majority here, as they are on npm. They are
	// reported beside the ranking and must never appear as a row in it.
	if resp.Unscoped.Packages != 3 || resp.Unscoped.Versions != 6 {
		t.Errorf("unscoped = %d packages and %d versions, want 3 and 6", resp.Unscoped.Packages, resp.Unscoped.Versions)
	}
	// Only the enriched ones contribute: `unenriched` has no download count.
	if resp.Unscoped.WeeklyDownloads == nil || *resp.Unscoped.WeeklyDownloads != 24500 {
		t.Errorf("unscoped downloads = %v, want 24500", resp.Unscoped.WeeklyDownloads)
	}

	if resp.TotalScopes != 1 || resp.ScopedPackages != 1 {
		t.Errorf("total_scopes = %d and scoped_packages = %d, want 1 and 1", resp.TotalScopes, resp.ScopedPackages)
	}
}

// A report with no download data at all reports no download totals, rather than
// summing the -1 sentinel into a negative one.
func TestScopesReportsUnenrichedAsNull(t *testing.T) {
	s := storeFrom(t, &blast.BlastResult{
		Targets: []blast.PackageVersion{axiosTarget},
		Affected: []blast.AffectedPackage{
			entry("@a/one", "1.0.0", -1, axiosTarget, step("@a/one", "1.0.0", "^1.6.1")),
		},
		UniquePackages: 1,
		MaxDepth:       1,
	})

	got := s.scopeStats()
	if len(got.Scopes) != 1 {
		t.Fatalf("scopes = %+v, want one row", got.Scopes)
	}
	if got.Scopes[0].WeeklyDownloads != nil {
		t.Errorf("downloads = %v, want null", *got.Scopes[0].WeeklyDownloads)
	}
}

// Scopes are ranked by affected packages, and ties break on the name so the
// chart is the same on every run.
func TestScopesRankByPackagesThenName(t *testing.T) {
	s := storeFrom(t, &blast.BlastResult{
		Targets: []blast.PackageVersion{axiosTarget},
		Affected: []blast.AffectedPackage{
			entry("@big/one", "1.0.0", 10, axiosTarget, step("@big/one", "1.0.0", "^1.6.1")),
			entry("@big/two", "1.0.0", 10, axiosTarget, step("@big/two", "1.0.0", "^1.6.1")),
			entry("@zed/one", "1.0.0", 10, axiosTarget, step("@zed/one", "1.0.0", "^1.6.1")),
			entry("@arc/one", "1.0.0", 10, axiosTarget, step("@arc/one", "1.0.0", "^1.6.1")),
		},
		UniquePackages: 4,
		MaxDepth:       1,
	})

	want := []string{"@big", "@arc", "@zed"}
	for i, scope := range want {
		if got := s.scopeStats().Scopes[i].Scope; got != scope {
			t.Errorf("rank %d = %q, want %q", i, got, scope)
		}
	}
}

// The breakdown is derived on first request and kept, so a second call must not
// recompute or double-count.
func TestScopesAreComputedOnce(t *testing.T) {
	s := loadFixture(t)
	first := s.scopeStats()
	if second := s.scopeStats(); second.Scopes[0].Packages != first.Scopes[0].Packages {
		t.Errorf("second call = %d packages, first = %d", second.Scopes[0].Packages, first.Scopes[0].Packages)
	}
}
