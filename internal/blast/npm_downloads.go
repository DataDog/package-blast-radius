package blast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
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

// npmAuthToken, when non-empty, is sent as a Bearer token with every download
// request. The unauthenticated npm downloads API is rate-limited hard behind
// Cloudflare (HTTP 429, error 1015); a token lifts that limit. Set it via the
// NPM_TOKEN environment variable, so `analyze --enrich-with-download-count`
// benefits without a new flag. Tests in this package may override it directly.
var npmAuthToken = os.Getenv("NPM_TOKEN")

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
	// Sort so the batch order is deterministic across runs; the result is the
	// same either way (merged under a mutex), but a stable order makes test
	// assertions and debugging reproducible.
	sort.Strings(names)

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
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if npmAuthToken != "" {
			req.Header.Set("Authorization", "Bearer "+npmAuthToken)
		}

		resp, err := client.Do(req)
		if err != nil {
			// Transport errors are often transient (connection reset, TLS hiccup);
			// retry a few times before giving up so a flaky link doesn't drop a
			// whole batch of 128 names.
			lastErr = err
			if attempt >= npmMaxRetries {
				return err
			}
			if !sleepBackoff(ctx, attempt) {
				return err
			}
			continue
			}

			if resp.StatusCode == http.StatusNotFound {
				resp.Body.Close()
				return errNotFound
			}
			if resp.StatusCode == http.StatusOK {
				err := json.NewDecoder(resp.Body).Decode(dst)
				resp.Body.Close()
				return err
			}

			// 429 (Cloudflare rate limit) and 5xx are retryable. Drain and close
			// the body so the connection can be reused, then back off.
			lastErr = fmt.Errorf("GET %s: %s", url, resp.Status)
			retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			resp.Body.Close()
			if !retryable || attempt >= npmMaxRetries {
				return lastErr
			}
			if !sleepBackoff(ctx, attempt, retryAfter) {
				return lastErr
			}
		}
}

// npmMaxRetries caps retry attempts for transient (429/5xx/transport) errors.
// Beyond this the batch fails and the best-effort caller reports the loss.
const npmMaxRetries = 5

// sleepBackoff waits for an exponential backoff with jitter before the next
// retry, honoring a server-provided floor when non-zero. Returns false if the
// context was cancelled while waiting.
func sleepBackoff(ctx context.Context, attempt int, floor ...time.Duration) bool {
	backoff := npmBackoffBase << attempt // 250ms, 500ms, 1s, 2s, 4s
	if backoff > npmBackoffCap {
		backoff = npmBackoffCap
	}
	if len(floor) > 0 && floor[0] > 0 && floor[0] > backoff {
		backoff = floor[0]
	}
	// Add up to 50% jitter so concurrent workers don't retry in lockstep and
	// re-trigger the rate limit.
	backoff += time.Duration(rand.Int64N(int64(backoff) / 2))
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

const (
	npmBackoffBase = 250 * time.Millisecond
	npmBackoffCap = 8 * time.Second
)

// parseRetryAfter interprets the Retry-After header as seconds (the npm
// downloads API uses the delta-seconds form), returning zero when absent or
// unparseable.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}
