package viewer

import (
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// The viewer has to hold a whole report in memory, and the largest ones on
// disk are over a gigabyte. TestReportLoadCost measures what that actually
// costs so the numbers in a change's description are measured rather than
// guessed. It is skipped unless BLAST_RADIUS_BENCH_REPORT points at a report:
//
//	BLAST_RADIUS_BENCH_REPORT=keyv-compromised-packages-depth-5.json \
//	  go test ./internal/viewer -run TestReportLoadCost -v -timeout 30m
func TestReportLoadCost(t *testing.T) {
	path := os.Getenv("BLAST_RADIUS_BENCH_REPORT")
	if path == "" {
		t.Skip("set BLAST_RADIUS_BENCH_REPORT to a blast-radius JSON report")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	s, err := loadStore(path)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}

	// Without the GC the retained set is indistinguishable from garbage that
	// simply has not been collected yet, which is the number that matters.
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	routes, versions, maxRoutes, maxVersions := 0, 0, 0, 0
	for i := range s.packages {
		p := &s.packages[i]
		routes += len(p.Routes)
		versions += p.VersionCount
		maxRoutes = max(maxRoutes, len(p.Routes))
		maxVersions = max(maxVersions, p.VersionCount)
	}
	edges := 0
	for _, list := range s.dependents {
		edges += len(list)
	}

	t.Logf("report            %s (%.0f MB)", path, float64(info.Size())/(1<<20))
	t.Logf("load elapsed      %s", elapsed.Round(time.Millisecond))
	t.Logf("retained heap     %.0f MB", float64(after.HeapAlloc)/(1<<20))
	t.Logf("total allocated   %.0f MB", float64(after.TotalAlloc-before.TotalAlloc)/(1<<20))
	t.Logf("peak rss          %.0f MB", float64(maxRSSBytes())/(1<<20))
	t.Logf("packages          %d", len(s.packages))
	t.Logf("routes            %d (max %d on one package)", routes, maxRoutes)
	t.Logf("versions retained %d of %d reported (max %d on one package)", versions, s.meta.TotalAffected, maxVersions)
	t.Logf("interned strings  %d", s.strings.len())
	t.Logf("graph nodes/edges %d / %d", len(s.dependents), edges)
	t.Logf("targets           %d", len(s.targets))
	t.Logf("enriched          %t", s.enriched())

	runtime.KeepAlive(s)
}

// maxRSSBytes normalises getrusage's ru_maxrss, which is bytes on Darwin and
// kilobytes on Linux.
func maxRSSBytes() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(ru.Maxrss)
	}
	return int64(ru.Maxrss) * 1024
}
