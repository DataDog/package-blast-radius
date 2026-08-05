package dataset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBucketURI(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"gs://my-bucket", "my-bucket", false},
		{"my-bucket", "my-bucket", false},
		{"gs://my-bucket/", "my-bucket", false},
		{"gs://my-bucket/some/path", "", true}, // a path would silently nest the export
		{"gs://", "", true},
		{"", "", true},
	}

	for _, tt := range tests {
		got, err := parseBucketURI(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseBucketURI(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("parseBucketURI(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestListPrefixFollowsPagination(t *testing.T) {
	fake := newFakeGCP(t)
	// A truncated listing would under-report both the stale-export check and the
	// download set, so pagination has to be followed.
	fake.objectPages = [][]gcsObject{
		{{Name: "a/npm-edges-000.parquet"}, {Name: "a/npm-edges-001.parquet"}},
		{{Name: "a/npm-edges-002.parquet"}},
	}
	c := clientForFake(fake)

	got, err := c.listPrefix(context.Background(), "bucket", "a/")
	if err != nil {
		t.Fatalf("listPrefix: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("listPrefix returned %d objects, want 3 across both pages", len(got))
	}
	if fake.listCalls != 2 {
		t.Errorf("made %d list calls, want 2", fake.listCalls)
	}
}

func TestDownloadObjectsWritesEveryShard(t *testing.T) {
	fake := newFakeGCP(t)
	for i := range 12 {
		fake.objects = append(fake.objects, gcsObject{Name: fmt.Sprintf("p/npm-edges-%03d.parquet", i)})
	}
	c := clientForFake(fake)

	dir := t.TempDir()
	if err := c.downloadObjects(context.Background(), "bucket", fake.objects, dir, newReporter(&strings.Builder{}, 1)); err != nil {
		t.Fatalf("downloadObjects: %v", err)
	}

	shards, _ := filepath.Glob(filepath.Join(dir, "npm-edges-*.parquet"))
	if len(shards) != 12 {
		t.Errorf("wrote %d shards, want 12", len(shards))
	}
	// The object name is a full path in the bucket; only the basename is kept.
	if _, err := os.Stat(filepath.Join(dir, "npm-edges-000.parquet")); err != nil {
		t.Errorf("expected the shard to be named after the object basename: %v", err)
	}
}

// An interrupted or short download must leave nothing the duckdb glob could pick
// up as a valid, empty shard.
func TestDownloadObjectsLeavesNoPartialFiles(t *testing.T) {
	fake := newFakeGCP(t)
	fake.objects = []gcsObject{{Name: "p/npm-edges-000.parquet"}}
	c := clientForFake(fake)

	// Cancel before the transfer to force the failure path.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dir := t.TempDir()
	if err := c.downloadObjects(ctx, "bucket", fake.objects, dir, newReporter(&strings.Builder{}, 1)); err == nil {
		t.Fatal("downloadObjects succeeded with a cancelled context")
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".parquet") {
			t.Errorf("left a parquet file behind after failing: %s", e.Name())
		}
		if strings.HasPrefix(e.Name(), ".partial-") {
			t.Errorf("left a temp file behind: %s", e.Name())
		}
	}
}

func TestDeleteObjectsToleratesAlreadyGone(t *testing.T) {
	fake := newFakeGCP(t)
	c := clientForFake(fake)

	objects := []gcsObject{{Name: "p/one.parquet"}, {Name: "p/two.parquet"}}
	if err := c.deleteObjects(context.Background(), "bucket", objects, newReporter(&strings.Builder{}, 1)); err != nil {
		t.Fatalf("deleteObjects: %v", err)
	}
	if got := fake.countRequests("DELETE", ".parquet"); got != 2 {
		t.Errorf("issued %d deletes, want 2", got)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
		{19 * 1024 * 1024 * 1024, "19.0 GB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.in); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
