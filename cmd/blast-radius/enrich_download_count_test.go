package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/DataDog/package-blast-radius/internal/blast"
)

func TestEffectiveEnrichSettingsUsesEcosystemDefaults(t *testing.T) {
	tests := []struct {
		name           string
		system         blast.Ecosystem
		rate           float64
		rateChanged    bool
		workers        int
		workersChanged bool
		wantRate       float64
		wantWorkers    int
	}{
		{
			name:        "npm defaults",
			system:      blast.NPM,
			wantRate:    1.0,
			wantWorkers: 4,
		},
		{
			name:           "explicit values win",
			system:         blast.NPM,
			rate:           1.0,
			rateChanged:    true,
			workers:        3,
			workersChanged: true,
			wantRate:       1.0,
			wantWorkers:    3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRate, gotWorkers := effectiveEnrichSettings(tt.system, tt.rate, tt.rateChanged, tt.workers, tt.workersChanged)
			if gotRate != tt.wantRate || gotWorkers != tt.wantWorkers {
				t.Fatalf("effectiveEnrichSettings() = (%v, %d), want (%v, %d)", gotRate, gotWorkers, tt.wantRate, tt.wantWorkers)
			}
		})
	}
}

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
