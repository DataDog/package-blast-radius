package dataset

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// downloadWorkers bounds concurrent shard fetches. The payload is ~1000 objects
// totalling ~20 GB, so this is about saturating the link without opening a
// thousand sockets.
const downloadWorkers = 8

// downloadReportEvery keeps a thousand-shard download to a handful of lines.
const downloadReportEvery = 200

// gcsObject is the subset of the Cloud Storage Object resource we use.
type gcsObject struct {
	Name string `json:"name"`
	Size string `json:"size"`
}

// parseBucketURI accepts "gs://bucket" or a bare "bucket" and returns the name.
func parseBucketURI(s string) (string, error) {
	name := strings.TrimPrefix(s, "gs://")
	name = strings.Trim(name, "/")
	if name == "" {
		return "", fmt.Errorf("empty bucket name")
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("expected a bucket name without a path, got %q", s)
	}
	return name, nil
}

// bucketLocation returns the bucket's location, or an error satisfying
// isNotFound when it does not exist.
func (c *client) bucketLocation(ctx context.Context, bucket string) (string, error) {
	endpoint := fmt.Sprintf("%s/b/%s?fields=location", c.storageURL, url.PathEscape(bucket))
	var result struct {
		Location string `json:"location"`
	}
	if err := c.doJSON(ctx, "GET", endpoint, nil, &result); err != nil {
		return "", err
	}
	return result.Location, nil
}

func (c *client) createBucket(ctx context.Context, projectID, bucket string) error {
	endpoint := fmt.Sprintf("%s/b?project=%s", c.storageURL, url.QueryEscape(projectID))
	body := map[string]string{"name": bucket, "location": datasetLocation}
	if err := c.doJSON(ctx, "POST", endpoint, body, nil); err != nil {
		return fmt.Errorf("creating bucket gs://%s: %w", bucket, err)
	}
	return nil
}

// listPrefix returns every object under prefix, following pagination. The bash
// version relied on `gcloud storage ls` to page implicitly; here it is explicit,
// because a truncated listing would silently under-report both the stale-export
// check and the download set.
func (c *client) listPrefix(ctx context.Context, bucket, prefix string) ([]gcsObject, error) {
	var all []gcsObject
	pageToken := ""

	for {
		endpoint := fmt.Sprintf("%s/b/%s/o?prefix=%s&fields=items(name,size),nextPageToken&maxResults=1000",
			c.storageURL, url.PathEscape(bucket), url.QueryEscape(prefix))
		if pageToken != "" {
			endpoint += "&pageToken=" + url.QueryEscape(pageToken)
		}

		var page struct {
			Items         []gcsObject `json:"items"`
			NextPageToken string      `json:"nextPageToken"`
		}
		if err := c.doJSON(ctx, "GET", endpoint, nil, &page); err != nil {
			return nil, fmt.Errorf("listing gs://%s/%s: %w", bucket, prefix, err)
		}

		all = append(all, page.Items...)
		if page.NextPageToken == "" {
			return all, nil
		}
		pageToken = page.NextPageToken
	}
}

func (c *client) deleteObjects(ctx context.Context, bucket string, objects []gcsObject, r *reporter) error {
	for i, obj := range objects {
		endpoint := fmt.Sprintf("%s/b/%s/o/%s", c.storageURL, url.PathEscape(bucket), url.PathEscape(obj.Name))
		// A concurrent delete is not a failure for our purposes.
		if err := c.doJSON(ctx, "DELETE", endpoint, nil, nil); err != nil && !isNotFound(err) {
			return fmt.Errorf("deleting gs://%s/%s: %w", bucket, obj.Name, err)
		}
		if (i+1)%100 == 0 {
			r.field("deleted", "%d/%d objects", i+1, len(objects))
		}
	}
	return nil
}

// downloadObjects fetches every object into destDir concurrently, naming each
// file after the object's basename.
func (c *client) downloadObjects(ctx context.Context, bucket string, objects []gcsObject, destDir string, r *reporter) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", destDir, err)
	}

	start := time.Now()
	countWidth := len(strconv.Itoa(len(objects)))

	var (
		queue     = make(chan gcsObject)
		wg        sync.WaitGroup
		done      atomic.Int64
		totalSize atomic.Int64
	)
	// firstErr is set once; the remaining workers then wind down via ctx.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}

	for range downloadWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for obj := range queue {
				n, err := c.downloadObject(ctx, bucket, obj.Name, destDir)
				if err != nil {
					fail(err)
					return
				}
				totalSize.Add(n)
				finished := done.Add(1)
				last := finished == int64(len(objects))
				if finished%downloadReportEvery == 0 || last {
					line := fmt.Sprintf("%*d/%d   %8s",
						countWidth, finished, len(objects), humanBytes(totalSize.Load()))
					if last {
						line += fmt.Sprintf("   in %s", time.Since(start).Round(time.Second))
					}
					r.line("%s", line)
				}
			}
		}()
	}

feed:
	for _, obj := range objects {
		select {
		case queue <- obj:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	// A cancelled parent context stops the feed without any worker failing, so
	// the count is what proves the set is complete before the build reads it.
	if finished := done.Load(); finished != int64(len(objects)) {
		return fmt.Errorf("downloaded %d of %d shards: %w", finished, len(objects), context.Cause(ctx))
	}
	return nil
}

// downloadObject writes one object to a temporary file and renames it into place
// on success, so an interrupted run never leaves a partial shard for the DuckDB
// glob to ingest.
func (c *client) downloadObject(ctx context.Context, bucket, objectName, destDir string) (int64, error) {
	endpoint := fmt.Sprintf("%s/b/%s/o/%s?alt=media",
		c.storageURL, url.PathEscape(bucket), url.PathEscape(objectName))

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("downloading %s: %w", objectName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, fmt.Errorf("downloading %s: %w", objectName, decodeAPIError(resp, endpoint))
	}

	finalPath := filepath.Join(destDir, filepath.Base(objectName))
	tmp, err := os.CreateTemp(destDir, ".partial-*")
	if err != nil {
		return 0, fmt.Errorf("creating temp file in %s: %w", destDir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	written, err := io.Copy(tmp, resp.Body)
	if err != nil {
		tmp.Close()
		return 0, fmt.Errorf("writing %s: %w", finalPath, err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("writing %s: %w", finalPath, err)
	}

	// A short read would produce a corrupt parquet shard that duckdb accepts
	// as an empty row group, so compare against the advertised size.
	if expected, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil && expected != written {
		return 0, fmt.Errorf("%s: expected %d bytes, got %d", objectName, expected, written)
	}

	if err := os.Rename(tmpName, finalPath); err != nil {
		return 0, fmt.Errorf("renaming into %s: %w", finalPath, err)
	}
	return written, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value)
}
