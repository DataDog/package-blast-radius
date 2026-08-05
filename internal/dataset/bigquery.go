package dataset

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// BigQuery on-demand pricing. A snapshot of the npm edge table scans ~1.3 TiB.
const (
	pricePerTiB = 6.25
	bytesPerTiB = 1 << 40
)

// The deps.dev public dataset lives in the US multi-region, and an extract job
// requires its destination bucket to share that location.
const datasetLocation = "US"

// Job resource types, trimmed to the fields this package sets or reads.
// https://cloud.google.com/bigquery/docs/reference/rest/v2/Job

type tableRef struct {
	ProjectID string `json:"projectId"`
	DatasetID string `json:"datasetId"`
	TableID   string `json:"tableId"`
}

type queryConfig struct {
	Query string `json:"query"`
	// UseLegacySQL is a pointer because BigQuery defaults it to true; sending
	// false explicitly is required, and omitempty would drop it.
	UseLegacySQL     *bool     `json:"useLegacySql"`
	DestinationTable *tableRef `json:"destinationTable,omitempty"`
	WriteDisposition string    `json:"writeDisposition,omitempty"`
}

type extractConfig struct {
	SourceTable       *tableRef `json:"sourceTable"`
	DestinationURIs   []string  `json:"destinationUris"`
	DestinationFormat string    `json:"destinationFormat"`
	Compression       string    `json:"compression,omitempty"`
}

type jobConfiguration struct {
	DryRun  bool           `json:"dryRun,omitempty"`
	Query   *queryConfig   `json:"query,omitempty"`
	Extract *extractConfig `json:"extract,omitempty"`
}

type jobReference struct {
	ProjectID string `json:"projectId,omitempty"`
	JobID     string `json:"jobId,omitempty"`
	Location  string `json:"location,omitempty"`
}

type errorProto struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type jobStatus struct {
	State       string        `json:"state,omitempty"`
	ErrorResult *errorProto   `json:"errorResult,omitempty"`
	Errors      []*errorProto `json:"errors,omitempty"`
}

type queryStatistics struct {
	// TotalBytesProcessed is an int64 rendered as a JSON string.
	TotalBytesProcessed string `json:"totalBytesProcessed,omitempty"`
}

type jobStatistics struct {
	Query *queryStatistics `json:"query,omitempty"`
}

type job struct {
	JobReference  *jobReference     `json:"jobReference,omitempty"`
	Configuration *jobConfiguration `json:"configuration,omitempty"`
	Status        *jobStatus        `json:"status,omitempty"`
	Statistics    *jobStatistics    `json:"statistics,omitempty"`
}

func boolPtr(b bool) *bool { return &b }

// edgeExportSQL builds the dependency-edge query for one ecosystem and snapshot.
// The self-join on `From` keeps only edges whose parent is the row's own
// (Name, Version), which is what makes the result a direct-dependency table.
func edgeExportSQL(bqSystem, snapshotDate string) string {
	return fmt.Sprintf(
		"SELECT Name, Version, `To`.Name AS DepName, Requirement\n"+
			"FROM `bigquery-public-data.deps_dev_v1.DependencyGraphEdges`\n"+
			"WHERE DATE(SnapshotAt) = '%s'\n"+
			"  AND System = '%s'\n"+
			"  AND `From`.Name = Name AND `From`.Version = Version",
		snapshotDate, bqSystem)
}

func latestSnapshotSQL() string {
	return "SELECT FORMAT_DATE('%Y-%m-%d', DATE(MAX(SnapshotAt))) AS d\n" +
		"FROM `bigquery-public-data.deps_dev_v1.DependencyGraphEdges`\n" +
		"WHERE SnapshotAt >= TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 14 DAY)"
}

// formatCost converts a byte count to TiB and on-demand dollars. Kept pure and
// separate so the arithmetic is testable and locale-independent.
func formatCost(bytes int64) (tib, usd float64) {
	tib = float64(bytes) / bytesPerTiB
	return tib, tib * pricePerTiB
}

// estimateBytes runs sql as a dry run, which BigQuery does not bill, and returns
// the byte count it would scan.
func (c *client) estimateBytes(ctx context.Context, projectID, sql string) (int64, error) {
	request := &job{
		JobReference: &jobReference{ProjectID: projectID, Location: datasetLocation},
		Configuration: &jobConfiguration{
			DryRun: true,
			Query:  &queryConfig{Query: sql, UseLegacySQL: boolPtr(false)},
		},
	}

	var result job
	endpoint := fmt.Sprintf("%s/projects/%s/jobs", c.bigQueryURL, url.PathEscape(projectID))
	if err := c.doJSON(ctx, "POST", endpoint, request, &result); err != nil {
		return 0, err
	}

	if result.Statistics == nil || result.Statistics.Query == nil || result.Statistics.Query.TotalBytesProcessed == "" {
		return 0, fmt.Errorf("dry run returned no byte count")
	}
	bytes, err := strconv.ParseInt(result.Statistics.Query.TotalBytesProcessed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable totalBytesProcessed %q: %w",
			result.Statistics.Query.TotalBytesProcessed, err)
	}
	return bytes, nil
}

// priceAndConfirm dry-runs sql, reports what it will cost, and requires a yes
// before the caller bills anything. When the dry run cannot produce a figure it
// still asks, rather than proceeding silently with an unknown bill.
func (c *client) priceAndConfirm(ctx context.Context, projectID, sql string, ask *confirmer, r *reporter, emptyScan error) error {
	bytes, err := c.estimateBytes(ctx, projectID, sql)
	if err != nil {
		r.field("cost", "could not be priced: %v", err)
		return ask.confirm("Run it anyway, without knowing the cost?")
	}
	// These queries filter a partitioned table, so a dry run that reads nothing
	// means no partition matched. Prompting would offer a free query that
	// produces an empty export, and the mistake would only surface at the build.
	if bytes == 0 && emptyScan != nil {
		return emptyScan
	}

	_, usd := formatCost(bytes)
	r.field("scan size", "%s", formatScanSize(bytes))
	r.field("cost", "~$%.2f   on-demand, $%.2f/TiB", usd, pricePerTiB)
	return ask.confirm("Proceed?")
}

// formatScanSize keeps a small scan from rendering as "0.00 TiB", which reads as
// nothing at all.
func formatScanSize(bytes int64) string {
	if tib := float64(bytes) / bytesPerTiB; tib >= 0.01 {
		return fmt.Sprintf("%.2f TiB", tib)
	}
	return humanBytes(bytes)
}

// ensureDataset creates the destination dataset, treating an existing one as success.
func (c *client) ensureDataset(ctx context.Context, projectID, datasetID string) error {
	endpoint := fmt.Sprintf("%s/projects/%s/datasets", c.bigQueryURL, url.PathEscape(projectID))
	body := map[string]any{
		"datasetReference": map[string]string{"projectId": projectID, "datasetId": datasetID},
		"location":         datasetLocation,
	}
	err := c.doJSON(ctx, "POST", endpoint, body, nil)
	if err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("creating dataset %s: %w", datasetID, err)
	}
	return nil
}

// insertJob starts an asynchronous job and returns its ID.
func (c *client) insertJob(ctx context.Context, projectID string, config *jobConfiguration) (string, error) {
	request := &job{
		JobReference:  &jobReference{ProjectID: projectID, Location: datasetLocation},
		Configuration: config,
	}

	var result job
	endpoint := fmt.Sprintf("%s/projects/%s/jobs", c.bigQueryURL, url.PathEscape(projectID))
	if err := c.doJSON(ctx, "POST", endpoint, request, &result); err != nil {
		return "", err
	}
	if result.JobReference == nil || result.JobReference.JobID == "" {
		return "", fmt.Errorf("job was accepted but no job ID was returned")
	}
	return result.JobReference.JobID, nil
}

// pollInterval grows from 2s to 15s so a multi-minute query is not polled
// hundreds of times.
func pollInterval(attempt int) time.Duration {
	d := time.Duration(2+attempt) * time.Second
	if d > 15*time.Second {
		return 15 * time.Second
	}
	return d
}

// waitForJob polls until the job reaches DONE, then converts a job-level error
// into a Go error. A DONE job with errorResult set has failed, which is the case
// a zero exit code from the bq CLI used to hide.
func (c *client) waitForJob(ctx context.Context, projectID, jobID string) error {
	endpoint := fmt.Sprintf("%s/projects/%s/jobs/%s?location=%s",
		c.bigQueryURL, url.PathEscape(projectID), url.PathEscape(jobID), datasetLocation)

	for attempt := 0; ; attempt++ {
		var result job
		if err := c.doJSON(ctx, "GET", endpoint, nil, &result); err != nil {
			return fmt.Errorf("polling job %s: %w", jobID, err)
		}

		if result.Status != nil && result.Status.State == "DONE" {
			if e := result.Status.ErrorResult; e != nil {
				return fmt.Errorf("job %s failed: %s (%s)", jobID, e.Message, e.Reason)
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval(attempt)):
		}
	}
}

// runQueryToTable executes sql and materialises the result, replacing any
// previous contents of the destination table.
func (c *client) runQueryToTable(ctx context.Context, projectID, sql string, dest *tableRef, r *reporter) error {
	jobID, err := c.insertJob(ctx, projectID, &jobConfiguration{
		Query: &queryConfig{
			Query:            sql,
			UseLegacySQL:     boolPtr(false),
			DestinationTable: dest,
			WriteDisposition: "WRITE_TRUNCATE",
		},
	})
	if err != nil {
		return fmt.Errorf("starting query job: %w", err)
	}
	return c.reportJob(ctx, projectID, jobID, "query", r)
}

// extractToGCS writes the table to GCS as sharded, Snappy-compressed parquet.
func (c *client) extractToGCS(ctx context.Context, projectID string, src *tableRef, destURI string, r *reporter) error {
	jobID, err := c.insertJob(ctx, projectID, &jobConfiguration{
		Extract: &extractConfig{
			SourceTable:       src,
			DestinationURIs:   []string{destURI},
			DestinationFormat: "PARQUET",
			Compression:       "SNAPPY",
		},
	})
	if err != nil {
		return fmt.Errorf("starting extract job: %w", err)
	}
	return c.reportJob(ctx, projectID, jobID, "extract", r)
}

// reportJob waits for a job and prints one line for it, with a heartbeat in
// between because an export query can run for minutes with nothing else to show.
func (c *client) reportJob(ctx context.Context, projectID, jobID, kind string, r *reporter) error {
	start := time.Now()
	stopHeartbeat := r.heartbeat("still running "+kind, heartbeatInterval)
	err := c.waitForJob(ctx, projectID, jobID)
	stopHeartbeat()
	if err != nil {
		return err
	}
	r.field(kind, "%s  done in %s", jobID, time.Since(start).Round(time.Second))
	return nil
}

// queryScalar runs sql and returns the single cell of the single row it produces.
// Used only for snapshot discovery.
func (c *client) queryScalar(ctx context.Context, projectID, sql string) (string, error) {
	endpoint := fmt.Sprintf("%s/projects/%s/queries", c.bigQueryURL, url.PathEscape(projectID))
	body := map[string]any{
		"query":        sql,
		"useLegacySql": false,
		"location":     datasetLocation,
		"maxResults":   1,
		// Long enough for a partitioned MAX() scan to finish inline.
		"timeoutMs": 120_000,
	}

	var result struct {
		JobComplete bool `json:"jobComplete"`
		Rows        []struct {
			F []struct {
				V any `json:"v"`
			} `json:"f"`
		} `json:"rows"`
		Errors []*errorProto `json:"errors"`
	}
	if err := c.doJSON(ctx, "POST", endpoint, body, &result); err != nil {
		return "", err
	}
	if !result.JobComplete {
		return "", fmt.Errorf("query did not complete within the timeout")
	}
	if len(result.Rows) == 0 || len(result.Rows[0].F) == 0 {
		return "", fmt.Errorf("query returned no rows")
	}
	value, ok := result.Rows[0].F[0].V.(string)
	if !ok || value == "" {
		return "", fmt.Errorf("query returned no value")
	}
	return value, nil
}
