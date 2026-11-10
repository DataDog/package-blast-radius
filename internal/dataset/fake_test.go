package dataset

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordedRequest is one call the fake GCP API received, kept so tests can assert
// on what was and, more importantly, was not sent.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

// isBillableJob reports whether this request starts a real BigQuery job. A dry
// run is free; anything else on the jobs endpoint costs money.
func (r recordedRequest) isBillableJob() bool {
	if r.Method != "POST" {
		return false
	}
	if strings.HasSuffix(r.Path, "/queries") {
		return true
	}
	if !strings.HasSuffix(r.Path, "/jobs") {
		return false
	}
	config, _ := r.Body["configuration"].(map[string]any)
	dryRun, _ := config["dryRun"].(bool)
	return !dryRun
}

// fakeGCP stands in for the BigQuery and Cloud Storage REST APIs.
type fakeGCP struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	// Programmable behaviour. Zero values give a working happy path.
	dryRunBytes string // "" means omit the field entirely
	// dryRunBytesFor overrides dryRunBytes per query, which is how a test says
	// which snapshot dates have a partition behind them.
	dryRunBytesFor   func(sql string) string
	dryRunStatus     int
	bucketStatus     int    // 0 means 200
	bucketLocation   string // "" means US
	createBucketFail bool
	scalarValue      string
	jobState         string
	jobErrorResult   *errorProto
	objects          []gcsObject
	objectPages      [][]gcsObject // when set, served as paginated pages
	// hideObjectsUntilList serves an empty listing for the first N calls, which is
	// how the fake models objects that only exist once the extract job has run.
	hideObjectsUntilList int
	objectContent        []byte
	listCalls            int
}

func newFakeGCP(t *testing.T) *fakeGCP {
	t.Helper()
	f := &fakeGCP{
		dryRunBytes:    "1440360232386", // ~1.31 TiB
		bucketLocation: "US",
		scalarValue:    "2026-03-23",
		jobState:       "DONE",
		objectContent:  validParquetFixture(t),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGCP) options(opts Options) Options {
	opts.HTTPClient = f.server.Client()
	opts.bigQueryURL = f.server.URL + "/bigquery/v2"
	opts.storageURL = f.server.URL + "/storage/v1"
	return opts
}

func (f *fakeGCP) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func (f *fakeGCP) billableJobs() []recordedRequest {
	var out []recordedRequest
	for _, r := range f.recorded() {
		if r.isBillableJob() {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeGCP) countRequests(method, pathSuffix string) int {
	n := 0
	for _, r := range f.recorded() {
		if r.Method == method && strings.HasSuffix(r.Path, pathSuffix) {
			n++
		}
	}
	return n
}

func (f *fakeGCP) handle(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		json.Unmarshal(raw, &body)
	}

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body,
	})
	f.mu.Unlock()

	path := r.URL.Path
	switch {
	case r.Method == "POST" && strings.HasSuffix(path, "/jobs"):
		f.handleJobInsert(w, body)
	case r.Method == "GET" && strings.Contains(path, "/jobs/"):
		f.handleJobGet(w)
	case r.Method == "POST" && strings.HasSuffix(path, "/queries"):
		f.handleQuery(w)
	case r.Method == "POST" && strings.HasSuffix(path, "/datasets"):
		writeJSON(w, 200, map[string]any{"id": "dataset"})
	case r.Method == "GET" && strings.Contains(path, "/o/") && r.URL.Query().Get("alt") == "media":
		w.Write(f.objectContent)
	case r.Method == "DELETE" && strings.Contains(path, "/o/"):
		w.WriteHeader(204)
	case r.Method == "GET" && strings.HasSuffix(path, "/o"):
		f.handleList(w, r)
	case r.Method == "POST" && strings.HasSuffix(path, "/storage/v1/b"):
		f.handleBucketCreate(w)
	case r.Method == "GET" && strings.Contains(path, "/storage/v1/b/"):
		f.handleBucketGet(w)
	default:
		writeGoogleError(w, 404, "not found in fake", "notFound")
	}
}

func (f *fakeGCP) handleJobInsert(w http.ResponseWriter, body map[string]any) {
	config, _ := body["configuration"].(map[string]any)
	dryRun, _ := config["dryRun"].(bool)

	if dryRun {
		if f.dryRunStatus != 0 {
			writeGoogleError(w, f.dryRunStatus, "dry run rejected", "invalid")
			return
		}
		bytes := f.dryRunBytes
		if f.dryRunBytesFor != nil {
			query, _ := config["query"].(map[string]any)
			sql, _ := query["query"].(string)
			bytes = f.dryRunBytesFor(sql)
		}
		stats := map[string]any{}
		if bytes != "" {
			stats["totalBytesProcessed"] = bytes
		}
		writeJSON(w, 200, map[string]any{"statistics": map[string]any{"query": stats}})
		return
	}
	writeJSON(w, 200, map[string]any{
		"jobReference": map[string]any{"projectId": "p", "jobId": "job-1", "location": "US"},
	})
}

func (f *fakeGCP) handleJobGet(w http.ResponseWriter) {
	status := map[string]any{"state": f.jobState}
	if f.jobErrorResult != nil {
		status["errorResult"] = map[string]any{
			"reason": f.jobErrorResult.Reason, "message": f.jobErrorResult.Message,
		}
	}
	writeJSON(w, 200, map[string]any{"status": status})
}

func (f *fakeGCP) handleQuery(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{
		"jobComplete": true,
		"rows":        []any{map[string]any{"f": []any{map[string]any{"v": f.scalarValue}}}},
	})
}

func (f *fakeGCP) handleList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.listCalls++
	hidden := f.listCalls <= f.hideObjectsUntilList
	f.mu.Unlock()

	if hidden {
		writeJSON(w, 200, map[string]any{})
		return
	}

	if f.objectPages != nil {
		index := 0
		if token := r.URL.Query().Get("pageToken"); token != "" {
			fmt.Sscanf(token, "page-%d", &index)
		}
		page := map[string]any{"items": f.objectPages[index]}
		if index+1 < len(f.objectPages) {
			page["nextPageToken"] = fmt.Sprintf("page-%d", index+1)
		}
		writeJSON(w, 200, page)
		return
	}
	writeJSON(w, 200, map[string]any{"items": f.objects})
}

func (f *fakeGCP) handleBucketGet(w http.ResponseWriter) {
	if f.bucketStatus != 0 {
		writeGoogleError(w, f.bucketStatus, "bucket problem", "forbidden")
		return
	}
	writeJSON(w, 200, map[string]any{"location": f.bucketLocation})
}

func (f *fakeGCP) handleBucketCreate(w http.ResponseWriter) {
	if f.createBucketFail {
		writeGoogleError(w, 403, "insufficient permission to create buckets", "forbidden")
		return
	}
	writeJSON(w, 200, map[string]any{"name": "bucket"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeGoogleError(w http.ResponseWriter, status int, message, reason string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": message,
			"errors":  []any{map[string]any{"reason": reason, "message": message}},
		},
	})
}

// validParquetFixture returns the bytes of a real, tiny parquet file with a
// superset of the columns buildDatabase's edges, versions, and downloads tables
// need, so the same fixture can stand in for whichever shard a test downloads.
func validParquetFixture(t *testing.T) []byte {
	t.Helper()

	path := filepath.Join(t.TempDir(), "fixture.parquet")
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("opening in-memory duckdb: %v", err)
	}
	defer db.Close()

	stmt := fmt.Sprintf(`
		CREATE TABLE fixture(Name VARCHAR, Version VARCHAR, DepName VARCHAR, Requirement VARCHAR, PublishedAt TIMESTAMP, WeeklyDownloads BIGINT, WindowStart DATE, WindowEnd DATE);
		INSERT INTO fixture VALUES ('a', '1.0.0', 'axios', '^1.0.0', '2024-01-02 03:04:05', 1234, '2026-08-04', '2026-08-10');
		COPY fixture TO '%s' (FORMAT PARQUET);
	`, path)
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("writing parquet fixture: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading parquet fixture: %v", err)
	}
	return data
}
