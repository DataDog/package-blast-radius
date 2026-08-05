package blast

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// npmStub serves the two response shapes the real registry uses: a bare
// download point for single-name requests, a name-keyed map otherwise.
type npmStub struct {
	downloads map[string]int64 // name -> count; absent means unknown
	status    int              // if non-zero, returned for every request

	mu       sync.Mutex
	requests [][]string // names per request, in arrival order
}

func (s *npmStub) start(t *testing.T) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}

		segment := strings.TrimPrefix(r.URL.Path, "/")
		names := strings.Split(segment, ",")

		s.mu.Lock()
		s.requests = append(s.requests, names)
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")

		if len(names) == 1 {
			count, ok := s.downloads[names[0]]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":"package not found"}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"downloads": count, "package": names[0]})
			return
		}

		out := make(map[string]any, len(names))
		for _, n := range names {
			count, ok := s.downloads[n]
			if !ok {
				out[n] = nil // the registry nulls unknown names rather than omitting them
				continue
			}
			out[n] = map[string]any{"downloads": count, "package": n}
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)

	previous := npmDownloadsBaseURL
	npmDownloadsBaseURL = srv.URL
	t.Cleanup(func() { npmDownloadsBaseURL = previous })
}

func (s *npmStub) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func collect(t *testing.T, names []string) map[string]int64 {
	t.Helper()

	var mu sync.Mutex
	got := make(map[string]int64)
	apply := func(name string, count int64) {
		mu.Lock()
		defer mu.Unlock()
		got[name] = count
	}

	if err := fetchNPMDownloads(context.Background(), names, 4, apply); err != nil {
		t.Fatalf("fetchNPMDownloads: %v", err)
	}
	return got
}

// A batch of exactly one unscoped name used to be decoded with the multi-name
// map shape, silently leaving the package unenriched.
func TestFetchNPMDownloadsSingleUnscopedPackage(t *testing.T) {
	stub := &npmStub{downloads: map[string]int64{"axios": 118959044}}
	stub.start(t)

	got := collect(t, []string{"axios"})

	if got["axios"] != 118959044 {
		t.Errorf("axios = %d, want 118959044", got["axios"])
	}
}

// The remainder batch has one element whenever the name count is 1 mod 128,
// which is the same single-name shape as above but reached through batching.
func TestFetchNPMDownloadsSingleElementRemainderBatch(t *testing.T) {
	names := make([]string, npmBulkMaxPackages+1)
	downloads := make(map[string]int64, len(names))
	for i := range names {
		names[i] = fmt.Sprintf("pkg%d", i)
		downloads[names[i]] = int64(i + 1)
	}

	stub := &npmStub{downloads: downloads}
	stub.start(t)

	got := collect(t, names)

	if len(got) != len(names) {
		t.Fatalf("enriched %d packages, want %d", len(got), len(names))
	}
	last := names[len(names)-1]
	if got[last] != int64(len(names)) {
		t.Errorf("%s = %d, want %d (the lone package in the remainder batch)", last, got[last], len(names))
	}
	if stub.requestCount() != 2 {
		t.Errorf("made %d requests, want 2", stub.requestCount())
	}
}

func TestFetchNPMDownloadsMultiplePackages(t *testing.T) {
	stub := &npmStub{downloads: map[string]int64{"axios": 100, "lodash": 200}}
	stub.start(t)

	got := collect(t, []string{"axios", "lodash"})

	if got["axios"] != 100 || got["lodash"] != 200 {
		t.Errorf("got %v, want axios=100 lodash=200", got)
	}
	if stub.requestCount() != 1 {
		t.Errorf("made %d requests, want 1 bulk request", stub.requestCount())
	}
}

// Scoped names are rejected by the bulk endpoint, so each needs its own request.
func TestFetchNPMDownloadsScopedPackagesGoOnePerRequest(t *testing.T) {
	stub := &npmStub{downloads: map[string]int64{
		"@scope/a": 10,
		"@scope/b": 20,
		"plain":    30,
	}}
	stub.start(t)

	got := collect(t, []string{"@scope/a", "@scope/b", "plain"})

	for name, want := range map[string]int64{"@scope/a": 10, "@scope/b": 20, "plain": 30} {
		if got[name] != want {
			t.Errorf("%s = %d, want %d", name, got[name], want)
		}
	}
	if stub.requestCount() != 3 {
		t.Errorf("made %d requests, want 3 (two scoped + one bulk)", stub.requestCount())
	}
}

// Unknown names come back as null inside a bulk response; they must be skipped
// rather than recorded as zero downloads.
func TestFetchNPMDownloadsSkipsUnknownPackages(t *testing.T) {
	stub := &npmStub{downloads: map[string]int64{"known": 42}}
	stub.start(t)

	got := collect(t, []string{"known", "unpublished"})

	if got["known"] != 42 {
		t.Errorf("known = %d, want 42", got["known"])
	}
	if _, ok := got["unpublished"]; ok {
		t.Errorf("unpublished was enriched to %d, want left untouched", got["unpublished"])
	}
}

// A 404 on a single package means "no data", not a failed request.
func TestFetchNPMDownloadsUnknownSinglePackageIsNotAFailure(t *testing.T) {
	stub := &npmStub{downloads: map[string]int64{}}
	stub.start(t)

	var mu sync.Mutex
	applied := 0
	err := fetchNPMDownloads(context.Background(), []string{"@scope/gone"}, 2, func(string, int64) {
		mu.Lock()
		defer mu.Unlock()
		applied++
	})
	if err != nil {
		t.Errorf("got error %v, want nil for a 404", err)
	}
	if applied != 0 {
		t.Errorf("applied %d counts, want 0", applied)
	}
}

func TestFetchNPMDownloadsReportsFailures(t *testing.T) {
	stub := &npmStub{status: http.StatusTooManyRequests}
	stub.start(t)

	err := fetchNPMDownloads(context.Background(), []string{"a", "@s/b"}, 2, func(string, int64) {})
	if err == nil {
		t.Fatal("got nil error, want a failure report")
	}
	if !strings.Contains(err.Error(), "2 of 2") {
		t.Errorf("error = %q, want it to report 2 of 2 failed", err)
	}
}

// Enrichment must apply a per-package count to every affected version of it.
func TestEnrichNPMDownloadsAppliesToAllVersions(t *testing.T) {
	stub := &npmStub{downloads: map[string]int64{"axios": 500, "lodash": 900}}
	stub.start(t)

	affected := []AffectedPackage{
		{PackageVersion: PackageVersion{Name: "axios", Version: "1.0.0"}, WeeklyDownloads: -1},
		{PackageVersion: PackageVersion{Name: "axios", Version: "2.0.0"}, WeeklyDownloads: -1},
		{PackageVersion: PackageVersion{Name: "lodash", Version: "4.0.0"}, WeeklyDownloads: -1},
		{PackageVersion: PackageVersion{Name: "ghost", Version: "1.0.0"}, WeeklyDownloads: -1},
	}

	if err := enrichNPMDownloads(context.Background(), affected, 4); err != nil {
		t.Fatalf("enrichNPMDownloads: %v", err)
	}

	want := []int64{500, 500, 900, -1}
	for i, w := range want {
		if affected[i].WeeklyDownloads != w {
			t.Errorf("affected[%d] (%s@%s) = %d, want %d",
				i, affected[i].Name, affected[i].Version, affected[i].WeeklyDownloads, w)
		}
	}
}

func TestNPMBatches(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := npmBatches(nil); len(got) != 0 {
			t.Errorf("got %v, want no batches", got)
		}
	})

	t.Run("caps unscoped batches at the API limit", func(t *testing.T) {
		names := make([]string, npmBulkMaxPackages*2+5)
		for i := range names {
			names[i] = fmt.Sprintf("pkg%d", i)
		}

		batches := npmBatches(names)

		if len(batches) != 3 {
			t.Fatalf("got %d batches, want 3", len(batches))
		}
		for i, b := range batches {
			if len(b) > npmBulkMaxPackages {
				t.Errorf("batch %d has %d names, over the %d limit", i, len(b), npmBulkMaxPackages)
			}
		}
		if len(batches[2]) != 5 {
			t.Errorf("remainder batch has %d names, want 5", len(batches[2]))
		}
	})

	t.Run("isolates scoped names", func(t *testing.T) {
		batches := npmBatches([]string{"@a/x", "plain1", "@b/y", "plain2"})

		if len(batches) != 3 {
			t.Fatalf("got %d batches, want 3", len(batches))
		}
		for _, b := range batches {
			if len(b) > 1 && (strings.HasPrefix(b[0], "@") || strings.HasPrefix(b[len(b)-1], "@")) {
				t.Errorf("batch %v mixes a scoped name into a bulk request", b)
			}
		}
	})
}
