package blast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// npmEnrichDefaultRate is the default request pace (req/s) for npm download
// enrichment. The npm downloads API rate-limits per IP behind Cloudflare; a
// small pace avoids the 429-avalanche that bursting causes. The pacer is
// adaptive (it cuts the rate on 429 and raises it on success), so this is just
// the starting point. Overridable via the --rate flag on `enrich-download-count`.
const npmEnrichDefaultRate = 1.0

const npmEnrichDefaultWorkers = 4

// NPMScopedPackageDefaultThreshold is the number of scoped packages we enrich
// exactly by default. Above this, callers need to opt in because npm cannot
// bulk-fetch scoped package download counts.
const NPMScopedPackageDefaultThreshold = 1000

// npmBulkMaxPackages is the most names npm's bulk downloads endpoint accepts in
// a single request.
const npmBulkMaxPackages = 128

// npmDownloadsBaseURL is a variable so tests can point it at a local server.
var npmDownloadsBaseURL = "https://api.npmjs.org/downloads/point/last-week"

// npmAuthToken, when non-empty, is sent as a Bearer token with every download
// request. The unauthenticated npm downloads API is rate-limited hard behind
// Cloudflare (HTTP 429, error 1015); a token lifts that limit. Set it via the
// NPM_TOKEN environment variable (read at package init) or SetNPMAuthToken at
// runtime (e.g. from a --npm-token flag). Tests may override it directly.
var npmAuthToken = os.Getenv("NPM_TOKEN")

// SetNPMAuthToken sets the Bearer token used for download requests. It lets a
// caller pass a token from a flag after package initialization has already
// read NPM_TOKEN. Passing "" clears it.
func SetNPMAuthToken(token string) { npmAuthToken = token }

// npmMaxRetries caps retry attempts for transient (429/5xx/transport) errors.
// A var (not a const) so tests can lower it and fail fast against a stub that
// always returns 429 instead of waiting out the full backoff.
var npmMaxRetries = 5

// errNotFound marks a package the registry has no download data for, which is
// routine here: the dependency graph contains versions that were later
// unpublished.
var errNotFound = errors.New("not found")

// enrichNPMDownloads fetches weekly download counts from the npm registry and
// populates the WeeklyDownloads field on each AffectedPackage.
func enrichNPMDownloads(ctx context.Context, affected []AffectedPackage, opts EnrichOptions) error {
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
	names = applyNPMScopedPackagePolicy(names, opts)

	var mu sync.Mutex
	apply := func(name string, count int64) {
		mu.Lock()
		defer mu.Unlock()
		for _, idx := range indexByName[name] {
			affected[idx].WeeklyDownloads = count
		}
	}

	return fetchNPMDownloads(ctx, names, opts, apply)
}

func applyNPMScopedPackagePolicy(names []string, opts EnrichOptions) []string {
	scoped := 0
	for _, name := range names {
		if strings.HasPrefix(name, "@") {
			scoped++
		}
	}
	threshold := opts.ScopedPackageThreshold
	if threshold <= 0 {
		threshold = NPMScopedPackageDefaultThreshold
	}
	if scoped <= threshold || opts.IncludeScopedPackages {
		return names
	}

	out := names[:0]
	for _, name := range names {
		if !strings.HasPrefix(name, "@") {
			out = append(out, name)
		}
	}
	if opts.Progress != nil {
		flag := opts.ScopedPackageOptInFlag
		if flag == "" {
			flag = "the scoped-package opt-in flag"
		}
		fmt.Fprintf(opts.Progress, "Skipping %d scoped package%s: npm rejects scoped names in bulk download lookups, and fetching them one-by-one is slow. Pass %s to include them.\n",
			scoped, plural(scoped), flag)
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// fetchNPMDownloads resolves weekly download counts for names, invoking apply
// for each name the registry has data on. Best effort: failed requests are
// tallied and reported, not aborted. Requests are paced by an adaptive rate
// limiter that cuts the pace on 429 and raises it on success, converging on
// whatever the npm downloads API's per-IP limit sustains — which is what makes
// enrichment usable against it.
func fetchNPMDownloads(ctx context.Context, names []string, opts EnrichOptions, apply func(string, int64)) error {
	batches := npmBatches(names)
	if len(batches) == 0 {
		return nil
	}
	workers := opts.Workers
	if workers < 1 {
		workers = 1
	}
	progress := opts.Progress

	client := &http.Client{Timeout: 15 * time.Second}

	var (
		wg       sync.WaitGroup
		failed   atomic.Int64
		resolved atomic.Int64
		// Rolling window of resolve timestamps for a stable, bulk-aware ETA
		// that doesn't balloon during rate-limit pauses (the cumulative rate
		// would, because resolved stalls while elapsed keeps growing).
		winMu sync.Mutex
		win   []time.Time
	)
	total := len(names)

	// Count a name as resolved exactly once after the registry returned either
	// a download count or "no data". Unknown download counts are still resolved
	// lookups for progress, ETA, and resumable callers.
	resolve := func(name string, count *int64) {
		if count != nil {
			apply(name, *count)
		}
		if opts.OnPackageResolved != nil {
			opts.OnPackageResolved(name, count)
		}
		resolved.Add(1)
		now := time.Now()
		winMu.Lock()
		win = append(win, now)
		winMu.Unlock()
	}

	// Adaptive pacer: nil when Rate <= 0 (unlimited, e.g. tests against a
	// local stub). Otherwise it self-tunes to the API's per-IP limit.
	pacer := newNPMPacer(opts.Rate, progress)
	if pacer != nil {
		pacerCtx, cancelPacer := context.WithCancel(ctx)
		defer cancelPacer()
		go pacer.run(pacerCtx)
	}

	// Progress reporter: a line every 20s while fetching.
	stopProg := make(chan struct{})
	var progWG sync.WaitGroup
	if progress != nil {
		progWG.Add(1)
		go func() {
			defer progWG.Done()
			t := time.NewTicker(20 * time.Second)
			defer t.Stop()
			start := time.Now()
			for {
				select {
				case <-stopProg:
					return
				case <-t.C:
					r := resolved.Load()
					pending := total - int(r)
					elapsed := time.Since(start)

					// Rolling 60s name-resolution rate (bulk-aware, unlike the
					// request pace), so the ETA reflects real throughput and
					// stays stable across pauses.
					cutoff := time.Now().Add(-60 * time.Second)
					winMu.Lock()
					i := 0
					for i < len(win) && win[i].Before(cutoff) {
						i++
					}
					win = win[i:]
					recent := len(win)
					winMu.Unlock()

					var eta string
					switch {
					case recent >= 5:
						rate := float64(recent) / 60.0
						eta = (time.Duration(float64(pending)/rate) * time.Second).Round(time.Second).String()
					case r > 0 && pending > 0:
						rate := float64(r) / elapsed.Seconds()
						if rate > 0 {
							eta = (time.Duration(float64(pending)/rate) * time.Second).Round(time.Second).String()
						}
					}

					pace, paused := 0.0, time.Duration(0)
					if pacer != nil {
						pace = pacer.currentRate()
						paused = pacer.pauseRemaining()
					}
					pauseStr := ""
					if paused > 0 {
						pauseStr = fmt.Sprintf(", rate-limited, pausing %s", paused.Round(time.Second))
					}
					fmt.Fprintf(progress, "  enrich: %d/%d resolved, %d pending, %d failed, pace %.2f req/s%s, elapsed %s, ETA %s\n",
						r, total, pending, failed.Load(), pace, pauseStr, elapsed.Round(time.Second), eta)
				}
			}
		}()
	}

	// Worker pipeline: a small pool covers network latency; the pacer bounds
	// the actual request rate.
	work := make(chan []string, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range work {
				if err := fetchNPMBatch(ctx, client, b, resolve, pacer); err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return
					}
					failed.Add(1)
				}
			}
		}()
	}

	for _, b := range batches {
		// The pacer gates launches: it blocks while paused (after a 429) and
		// spaces launches at the current pace. Unlimited (nil pacer) launches
		// as fast as workers accept.
		if pacer != nil {
			select {
			case <-pacer.tokens:
			case <-ctx.Done():
				close(work)
				wg.Wait()
				close(stopProg)
				progWG.Wait()
				return ctx.Err()
			}
		}
		select {
		case work <- b:
		case <-ctx.Done():
			close(work)
			wg.Wait()
			close(stopProg)
			progWG.Wait()
			return ctx.Err()
		}
	}
	close(work)
	wg.Wait()
	close(stopProg)
	progWG.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}

	if n := failed.Load(); n > 0 {
		return fmt.Errorf("%d of %d npm download requests failed", n, len(batches))
	}
	return nil
}

// npmBatches groups names into requests the bulk endpoint accepts. Unscoped
// names are returned first in bulk batches, capped at npmBulkMaxPackages, so a
// large run gets broad coverage quickly. Scoped names follow one per request
// because the bulk endpoint rejects them.
func npmBatches(names []string) [][]string {
	var batches [][]string
	var unscoped []string

	for _, name := range names {
		if strings.HasPrefix(name, "@") {
			continue
		}
		unscoped = append(unscoped, name)
	}

	for i := 0; i < len(unscoped); i += npmBulkMaxPackages {
		batches = append(batches, unscoped[i:min(i+npmBulkMaxPackages, len(unscoped))])
	}

	for _, name := range names {
		if strings.HasPrefix(name, "@") {
			batches = append(batches, []string{name})
		}
	}

	return batches
}

type npmDownloadPoint struct {
	Downloads int64 `json:"downloads"`
}

// fetchNPMBatch requests one batch. Multi-name answers come as a name-keyed
// map, single-name as a bare point, so the two shapes decode separately.
func fetchNPMBatch(ctx context.Context, client *http.Client, batch []string, resolve func(string, *int64), pacer *npmPacer) error {
	if len(batch) == 1 {
		name := batch[0]
		var point npmDownloadPoint
		if err := fetchJSON(ctx, client, npmDownloadsURL(url.PathEscape(name)), &point, pacer); err != nil {
			if errors.Is(err, errNotFound) {
				resolve(name, nil)
				return nil
			}
			return err
		}
		resolve(name, &point.Downloads)
		return nil
	}

	// Names the registry doesn't know come back as a null entry, not omitted.
	var points map[string]*npmDownloadPoint
	if err := fetchJSON(ctx, client, npmDownloadsURL(strings.Join(batch, ",")), &points, pacer); err != nil {
		if errors.Is(err, errNotFound) {
			for _, name := range batch {
				resolve(name, nil)
			}
			return nil
		}
		return err
	}
	for _, name := range batch {
		point := points[name]
		if point != nil {
			resolve(name, &point.Downloads)
			continue
		}
		resolve(name, nil)
	}
	return nil
}

func npmDownloadsURL(segment string) string {
	return npmDownloadsBaseURL + "/" + segment
}

// fetchJSON performs a GET with Bearer auth and decodes the JSON body into dst.
// It retries 429/5xx/transport errors. On a 429 it tells the pacer to cut its
// pace and pause for the bucket to recover; on a successful (non-429) response
// it nudges the pacer back up, so the rate self-tunes to the API's limit.
func fetchJSON(ctx context.Context, client *http.Client, url string, dst any, pacer *npmPacer) error {
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
			// Transport errors are often transient (connection reset, TLS
			// hiccup); retry a few times so a flaky link doesn't drop a whole
			// batch of 128 names.
			lastErr = err
			if attempt >= npmMaxRetries {
				return err
			}
			if !sleepBackoff(ctx, attempt) {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return err
			}
			continue
		}

		switch {
		case resp.StatusCode == http.StatusNotFound:
			resp.Body.Close()
			// A 404 still consumed a request slot, so it counts as success for
			// the pacer (the bucket is refilling).
			if pacer != nil {
				pacer.onSuccess()
			}
			return errNotFound
		case resp.StatusCode == http.StatusOK:
			if pacer != nil {
				pacer.onSuccess()
			}
			err := json.NewDecoder(resp.Body).Decode(dst)
			resp.Body.Close()
			return err
		case resp.StatusCode == http.StatusTooManyRequests:
			resp.Body.Close()
			lastErr = fmt.Errorf("GET %s: %s", url, resp.Status)
			if attempt >= npmMaxRetries {
				return lastErr
			}
			// Cut the pace and wait for the bucket to recover before retrying,
			// rather than backing off briefly and re-draining it at the same
			// pace (which is the 429-loop the fixed ticker used to fall into).
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			var wait time.Duration
			if pacer != nil {
				wait = pacer.on429(retryAfter)
			} else {
				wait = retryAfter
			}
			if wait <= 0 {
				wait = npmBackoffBase << attempt
				if wait > npmBackoffCap {
					wait = npmBackoffCap
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		case resp.StatusCode >= 500:
			resp.Body.Close()
			lastErr = fmt.Errorf("GET %s: %s", url, resp.Status)
			if attempt >= npmMaxRetries {
				return lastErr
			}
			if !sleepBackoff(ctx, attempt) {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return lastErr
			}
		default:
			resp.Body.Close()
			return fmt.Errorf("GET %s: %s", url, resp.Status)
		}
	}
}

// npmPacer paces request launches at a rate that self-tunes to the npm
// downloads API's per-IP limit, using additive-increase/multiplicative-decrease:
// a 429 halves the pace (and pauses briefly); a run of successes nudges it back
// up. A fixed pace either 429-avalanches (set too high) or underutilizes (set
// too low); the pacer converges on whatever the IP actually sustains, so the
// caller doesn't have to guess --rate.
type npmPacer struct {
	max        float64 // ceiling: the configured starting pace
	mu         sync.Mutex
	rate       float64 // current req/s
	pauseUntil time.Time
	successes  int // consecutive successes since the last cut
	tokens     chan struct{}
	progress   io.Writer // for the one-line "cutting pace" notice on a 429
}

const (
	npmPacerMinRate   = 0.25             // req/s floor; below this a 429 just means "stop"
	npmPacerCut       = 0.5              // on 429, rate *= cut
	npmPacerIncr      = 0.25             // req/s added after a run of successes
	npmPacerIncrEvery = 40               // successes before an additive increase
	npmPacerPause     = 20 * time.Second // brief pause on 429 so the bucket recovers before resuming at the lower pace
)

// newNPMPacer returns a pacer starting at rate, or nil for rate <= 0
// (unlimited — used by tests against a local stub with no rate limit).
func newNPMPacer(rate float64, progress io.Writer) *npmPacer {
	if rate <= 0 {
		return nil
	}
	if rate < npmPacerMinRate {
		rate = npmPacerMinRate
	}
	return &npmPacer{max: rate, rate: rate, tokens: make(chan struct{}, 1), progress: progress}
}

// run emits launch tokens at the current pace until ctx is done. While paused
// (after a 429) it emits nothing, so the launcher naturally stalls until the
// bucket recovers.
func (p *npmPacer) run(ctx context.Context) {
	for {
		p.mu.Lock()
		rate := p.rate
		pu := p.pauseUntil
		p.mu.Unlock()

		if !pu.IsZero() {
			if d := time.Until(pu); d > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(d):
				}
			}
		}

		interval := time.Duration(float64(time.Second) / rate)
		if interval <= 0 {
			interval = time.Nanosecond
		}
		select {
		case <-ctx.Done():
			return
		case p.tokens <- struct{}{}:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// on429 halves the pace, resets the success streak, and pauses briefly so the
// rate-limit bucket recovers before the next launch. It returns the wait
// duration so the request that hit the 429 can retry after the same recovery
// window instead of re-draining the bucket. retryAfter (from the Retry-After
// header) overrides the default pause when the server asks for longer.
func (p *npmPacer) on429(retryAfter time.Duration) time.Duration {
	p.mu.Lock()
	p.rate *= npmPacerCut
	if p.rate < npmPacerMinRate {
		p.rate = npmPacerMinRate
	}
	p.successes = 0
	d := npmPacerPause
	if retryAfter > d {
		d = retryAfter
	}
	p.pauseUntil = time.Now().Add(d)
	rate := p.rate
	prog := p.progress
	p.mu.Unlock()
	if prog != nil {
		fmt.Fprintf(prog, "  enrich: rate-limited (429), cutting pace to %.2f req/s, pausing %s...\n", rate, d.Round(time.Second))
	}
	return d
}

// onSuccess nudges the pace back up after a run of successes, capped at the
// starting pace. Called for any non-429 response (200 or 404): both consumed a
// request slot, so both mean the bucket is refilling.
func (p *npmPacer) onSuccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.successes++
	if p.successes >= npmPacerIncrEvery && p.rate < p.max {
		p.successes = 0
		p.rate += npmPacerIncr
		if p.rate > p.max {
			p.rate = p.max
		}
	}
}

// currentRate returns the pace for progress reporting.
func (p *npmPacer) currentRate() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rate
}

// pauseRemaining returns how long until the current 429 pause lifts, or 0.
func (p *npmPacer) pauseRemaining() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pauseUntil.IsZero() {
		return 0
	}
	if d := time.Until(p.pauseUntil); d > 0 {
		return d
	}
	return 0
}

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
	npmBackoffCap  = 8 * time.Second
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
