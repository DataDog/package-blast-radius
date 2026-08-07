package blast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// npmBulkMaxPackages is the most names npm's bulk downloads endpoint accepts in
// a single request.
const npmBulkMaxPackages = 128

// npmDownloadsBaseURL is a variable so tests can point it at a local server.
var npmDownloadsBaseURL = "https://api.npmjs.org/downloads/point/last-week"

// errNotFound marks a package the registry has no download data for, which is
// routine here: the dependency graph contains versions that were later
// unpublished.
var errNotFound = errors.New("not found")

// enrichNPMDownloads fetches weekly download counts from the npm registry and
// populates the WeeklyDownloads field on each AffectedPackage.
func enrichNPMDownloads(ctx context.Context, affected []AffectedPackage, workers int) error {
	// Downloads are per package, not per version, so resolve each name once.
	indexByName := make(map[string][]int)
	for i := range affected {
		indexByName[affected[i].Name] = append(indexByName[affected[i].Name], i)
	}

	names := make([]string, 0, len(indexByName))
	for name := range indexByName {
		names = append(names, name)
	}

	var mu sync.Mutex
	apply := func(name string, count int64) {
		mu.Lock()
		defer mu.Unlock()
		for _, idx := range indexByName[name] {
			affected[idx].WeeklyDownloads = count
		}
	}

	return fetchNPMDownloads(ctx, names, workers, apply)
}

// fetchNPMDownloads resolves weekly download counts for names, invoking apply
// for each name the registry has data on. Best effort: failed requests are
// tallied and reported, not aborted.
func fetchNPMDownloads(ctx context.Context, names []string, workers int, apply func(string, int64)) error {
	batches := npmBatches(names)
	if len(batches) == 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}

	client := &http.Client{Timeout: 15 * time.Second}

	var wg sync.WaitGroup
	var failed atomic.Int64
	sem := make(chan struct{}, workers)

	for _, batch := range batches {
		wg.Add(1)
		sem <- struct{}{}
		go func(batch []string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fetchNPMBatch(ctx, client, batch, apply); err != nil {
				failed.Add(1)
			}
		}(batch)
	}
	wg.Wait()

	if n := failed.Load(); n > 0 {
		return fmt.Errorf("%d of %d npm download requests failed", n, len(batches))
	}
	return nil
}

// npmBatches groups names into requests the bulk endpoint accepts: scoped
// names go one per request (the bulk endpoint rejects them), the rest capped
// at npmBulkMaxPackages.
func npmBatches(names []string) [][]string {
	var batches [][]string
	var unscoped []string

	for _, name := range names {
		if strings.HasPrefix(name, "@") {
			batches = append(batches, []string{name})
		} else {
			unscoped = append(unscoped, name)
		}
	}

	for i := 0; i < len(unscoped); i += npmBulkMaxPackages {
		batches = append(batches, unscoped[i:min(i+npmBulkMaxPackages, len(unscoped))])
	}

	return batches
}

type npmDownloadPoint struct {
	Downloads int64 `json:"downloads"`
}

// fetchNPMBatch requests one batch. Multi-name answers come as a name-keyed
// map, single-name as a bare point, so the two shapes decode separately.
func fetchNPMBatch(ctx context.Context, client *http.Client, batch []string, apply func(string, int64)) error {
	if len(batch) == 1 {
		name := batch[0]
		var point npmDownloadPoint
		if err := fetchJSON(ctx, client, npmDownloadsURL(url.PathEscape(name)), &point); err != nil {
			return ignoreNotFound(err)
		}
		apply(name, point.Downloads)
		return nil
	}

	// Names the registry doesn't know come back as a null entry, not omitted.
	var points map[string]*npmDownloadPoint
	if err := fetchJSON(ctx, client, npmDownloadsURL(strings.Join(batch, ",")), &points); err != nil {
		return ignoreNotFound(err)
	}
	for name, point := range points {
		if point != nil {
			apply(name, point.Downloads)
		}
	}
	return nil
}

func ignoreNotFound(err error) error {
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

func npmDownloadsURL(segment string) string {
	return npmDownloadsBaseURL + "/" + segment
}

func fetchJSON(ctx context.Context, client *http.Client, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	return json.NewDecoder(resp.Body).Decode(dst)
}
