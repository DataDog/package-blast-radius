package dataset

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFormatCost(t *testing.T) {
	tests := []struct {
		bytes   int64
		wantTiB string
		wantUSD string
	}{
		{0, "0.00", "0.00"},
		{bytesPerTiB, "1.00", "6.25"},
		{1440360232386, "1.31", "8.19"}, // roughly one npm snapshot
		// 3.125 formats as 3.12: Go rounds half to even.
		{bytesPerTiB / 2, "0.50", "3.12"},
		{166 * bytesPerTiB, "166.00", "1037.50"}, // an unfiltered full scan
	}

	for _, tt := range tests {
		tib, usd := formatCost(tt.bytes)
		gotTiB := fmt.Sprintf("%.2f", tib)
		gotUSD := fmt.Sprintf("%.2f", usd)
		if gotTiB != tt.wantTiB || gotUSD != tt.wantUSD {
			t.Errorf("formatCost(%d) = (%s TiB, $%s), want (%s TiB, $%s)",
				tt.bytes, gotTiB, gotUSD, tt.wantTiB, tt.wantUSD)
		}
	}
}

func TestEstimateBytesDecodesTheStringEncodedCount(t *testing.T) {
	fake := newFakeGCP(t)
	fake.dryRunBytes = "1440360232386"
	c := clientForFake(fake)

	got, err := c.estimateBytes(context.Background(), "proj", "SELECT 1")
	if err != nil {
		t.Fatalf("estimateBytes: %v", err)
	}
	if got != 1440360232386 {
		t.Errorf("estimateBytes = %d, want 1440360232386", got)
	}
}

func TestEstimateBytesRejectsAMissingCount(t *testing.T) {
	fake := newFakeGCP(t)
	fake.dryRunBytes = "" // statistics.query present but the field omitted
	c := clientForFake(fake)

	if _, err := c.estimateBytes(context.Background(), "proj", "SELECT 1"); err == nil {
		t.Fatal("estimateBytes succeeded with no byte count, want an error")
	}
}

func TestEstimateBytesSurfacesGoogleErrorMessage(t *testing.T) {
	fake := newFakeGCP(t)
	fake.dryRunStatus = 403
	c := clientForFake(fake)

	_, err := c.estimateBytes(context.Background(), "proj", "SELECT 1")
	if err == nil {
		t.Fatal("estimateBytes succeeded on a 403")
	}
	// A bare "HTTP 403" would leave the user guessing; the API says why.
	if !strings.Contains(err.Error(), "dry run rejected") {
		t.Errorf("error %q should carry Google's message", err)
	}
	if !strings.Contains(err.Error(), "forbidden") && !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error %q should carry the reason", err)
	}
}

func TestDryRunSendsUseLegacySqlFalse(t *testing.T) {
	fake := newFakeGCP(t)
	c := clientForFake(fake)

	if _, err := c.estimateBytes(context.Background(), "proj", "SELECT 1"); err != nil {
		t.Fatal(err)
	}

	requests := fake.recorded()
	config := requests[0].Body["configuration"].(map[string]any)
	query := config["query"].(map[string]any)
	// BigQuery defaults useLegacySql to true, so omitting it breaks the query.
	useLegacy, present := query["useLegacySql"]
	if !present {
		t.Fatal("useLegacySql was omitted; BigQuery would default it to true")
	}
	if useLegacy != false {
		t.Errorf("useLegacySql = %v, want false", useLegacy)
	}
}

// A failed dry run must fall through to a prompt rather than aborting. This is
// the bash pipefail bug that killed the script before it could ask.
func TestPriceAndConfirmAsksWhenPricingFails(t *testing.T) {
	fake := newFakeGCP(t)
	fake.dryRunStatus = 500
	c := clientForFake(fake)

	ask, out := newTestConfirmer("y\n", false)
	err := c.priceAndConfirm(context.Background(), "proj", "SELECT 1", ask, newReporter(out, 1))
	if err != nil {
		t.Fatalf("priceAndConfirm = %v, want nil after the user said yes", err)
	}
	if !strings.Contains(out.String(), "could not be priced") {
		t.Errorf("output %q should say pricing failed", out.String())
	}
	if !strings.Contains(out.String(), "without knowing the cost") {
		t.Errorf("output %q should ask about the unknown cost", out.String())
	}
}

func TestPriceAndConfirmStopsOnRefusal(t *testing.T) {
	fake := newFakeGCP(t)
	c := clientForFake(fake)

	ask, out := newTestConfirmer("n\n", false)
	err := c.priceAndConfirm(context.Background(), "proj", "SELECT 1", ask, newReporter(out, 1))
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("priceAndConfirm = %v, want ErrAborted", err)
	}
	// The figures must be shown before the question, not after.
	if !strings.Contains(out.String(), "1.31 TiB") || !strings.Contains(out.String(), "8.19") {
		t.Errorf("output %q should show the real cost", out.String())
	}
}

func TestWaitForJobFailsOnErrorResult(t *testing.T) {
	fake := newFakeGCP(t)
	fake.jobErrorResult = &errorProto{Reason: "quotaExceeded", Message: "quota exceeded"}
	c := clientForFake(fake)

	err := c.waitForJob(context.Background(), "proj", "job-1")
	if err == nil {
		t.Fatal("waitForJob succeeded on a DONE job carrying errorResult")
	}
	if !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("error %q should carry the job's message", err)
	}
}

func TestWaitForJobHonoursContextCancellation(t *testing.T) {
	fake := newFakeGCP(t)
	fake.jobState = "RUNNING" // never finishes
	c := clientForFake(fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.waitForJob(ctx, "proj", "job-1"); err == nil {
		t.Fatal("waitForJob returned nil for a cancelled context")
	}
}

func TestPollIntervalGrowsAndCaps(t *testing.T) {
	if first, second := pollInterval(0), pollInterval(1); first >= second {
		t.Errorf("interval should grow: %v then %v", first, second)
	}
	if got := pollInterval(1000); got != 15e9 {
		t.Errorf("pollInterval(1000) = %v, want it capped at 15s", got)
	}
}

func TestEdgeExportSQLIsScopedByDateAndSystem(t *testing.T) {
	sql := edgeExportSQL("NPM", "2026-03-23")

	// Without the date filter this scans ~166 TB instead of ~1.3 TB.
	if !strings.Contains(sql, "DATE(SnapshotAt) = '2026-03-23'") {
		t.Errorf("SQL is missing the snapshot date filter:\n%s", sql)
	}
	if !strings.Contains(sql, "System = 'NPM'") {
		t.Errorf("SQL is missing the system filter:\n%s", sql)
	}
	// The self-join is what restricts the result to direct dependency edges.
	for _, join := range []string{"`From`.Name = Name", "`From`.Version = Version"} {
		if !strings.Contains(sql, join) {
			t.Errorf("SQL is missing the From self-join condition %q:\n%s", join, sql)
		}
	}
}

func clientForFake(f *fakeGCP) *client {
	c := newClient(f.server.Client())
	c.bigQueryURL = f.server.URL + "/bigquery/v2"
	c.storageURL = f.server.URL + "/storage/v1"
	return c
}

// A dry run that reads nothing means the snapshot filter matched no partition.
// Prompting there would offer a free query producing an empty export, and the
// mistake would only surface minutes later at the build.
func TestPriceAndConfirmRefusesAScanThatReadsNothing(t *testing.T) {
	fake := newFakeGCP(t)
	fake.dryRunBytes = "0"
	c := clientForFake(fake)

	ask, out := newTestConfirmer("y\n", false)

	err := c.priceAndConfirm(context.Background(), "proj", "SELECT 1", ask, newReporter(out, 1))
	if !errors.Is(err, errEmptyScan) {
		t.Fatalf("priceAndConfirm = %v, want errEmptyScan", err)
	}
	if strings.Contains(out.String(), "Proceed?") {
		t.Errorf("the user was asked to run a query that reads nothing:\n%s", out.String())
	}
}

func TestFormatScanSizeKeepsSmallScansReadable(t *testing.T) {
	if got := formatScanSize(0); got != "0 B" {
		t.Errorf("formatScanSize(0) = %q, want 0 B", got)
	}
	if got := formatScanSize(2 << 30); got != "2.0 GB" {
		t.Errorf("formatScanSize(2 GiB) = %q", got)
	}
	if got := formatScanSize(1440360232386); got != "1.31 TiB" {
		t.Errorf("formatScanSize(1.31 TiB) = %q", got)
	}
}
