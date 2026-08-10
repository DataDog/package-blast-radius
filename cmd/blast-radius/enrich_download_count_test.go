package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadCountCacheRoundTripsCountsAndNoData(t *testing.T) {
	path := filepath.Join(t.TempDir(), downloadCountCacheArtifact)
	var buf bytes.Buffer
	w := newDownloadCountCacheWriter(&buf)

	downloads := int64(123)
	if err := w.Write("known", &downloads); err != nil {
		t.Fatalf("write known: %v", err)
	}
	if err := w.Write("missing", nil); err != nil {
		t.Fatalf("write missing: %v", err)
	}
	newDownloads := int64(456)
	if err := w.Write("known", &newDownloads); err != nil {
		t.Fatalf("write updated known: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadDownloadCountCache(path)
	if err != nil {
		t.Fatalf("loadDownloadCountCache: %v", err)
	}
	if got["known"] == nil || *got["known"] != 456 {
		t.Fatalf("known = %v, want 456", got["known"])
	}
	if v, ok := got["missing"]; !ok || v != nil {
		t.Fatalf("missing = %v (present %t), want present nil", v, ok)
	}
}

func TestDownloadCountCacheSkipsMalformedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), downloadCountCacheArtifact)
	if err := os.WriteFile(path, []byte("{not-json\n{\"name\":\"known\",\"weekly_downloads\":12}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadDownloadCountCache(path)
	if err != nil {
		t.Fatalf("loadDownloadCountCache: %v", err)
	}
	if got["known"] == nil || *got["known"] != 12 {
		t.Fatalf("known = %v, want 12", got["known"])
	}
}
