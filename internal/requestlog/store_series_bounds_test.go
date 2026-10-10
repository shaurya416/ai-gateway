package requestlog

import (
	"path/filepath"
	"testing"
	"time"
)

// appendSeries owns its bucket bound rather than trusting the caller to have
// checked. Stats gates the call on SeriesBuckets > 0, so today no caller can
// reach it with a hostile count — but the guard living one level up is the
// kind that a second caller silently loses. A zero divides by zero deriving
// the bucket width; a negative panics in make.
func TestAppendSeries_ClampsHostileBucketCounts(t *testing.T) {
	w, err := NewSQLiteWriter(t.Context(), filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatalf("new sqlite writer: %v", err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("close request log writer: %v", err)
		}
	})

	since := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name    string
		buckets int
		want    int
	}{
		{name: "zero", buckets: 0, want: 1},
		{name: "negative", buckets: -5, want: 1},
		{name: "far past the cap", buckets: maxSeriesBuckets * 100, want: maxSeriesBuckets},
		{name: "inside the cap", buckets: 12, want: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result StatsResult
			query := Query{SeriesBuckets: tc.buckets, Since: &since}
			if err := w.appendSeries(t.Context(), &result, "", nil, query); err != nil {
				t.Fatalf("appendSeries: %v", err)
			}
			if len(result.Series) != tc.want {
				t.Errorf("series points = %d, want %d", len(result.Series), tc.want)
			}
		})
	}
}

// TestStats_TruncatedScanPercentilesCoverTheSeriesRows is the regression test
// for the percentiles reaching one row past a truncated scan. The scan reads one
// row more than the cap, only to learn that the window held more, and that row's
// duration and time to first token were collected before the cap was checked —
// so a truncated window's percentiles counted a row the series leaves out,
// though they are documented as covering the same rows.
func TestStats_TruncatedScanPercentilesCoverTheSeriesRows(t *testing.T) {
	w, err := NewSQLiteWriter(t.Context(), filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatalf("new sqlite writer: %v", err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("close request log writer: %v", err)
		}
	})

	original := seriesScanLimit
	seriesScanLimit = 3
	t.Cleanup(func() { seriesScanLimit = original })

	// Newest first: three requests the scan keeps, then the oldest — the row
	// past the cap — carrying an outlier duration and time to first token.
	now := time.Now().UTC()
	for i, ms := range []float64{10, 20, 30, 5000} {
		entry := Entry{
			Stage: "after_request", Model: "gpt-4o", Provider: "openai",
			CreatedAt:  now.Add(-time.Duration(i+1) * time.Minute),
			DurationMs: floatPtr(ms),
			TTFTMs:     floatPtr(ms / 2),
		}
		if err := w.Write(t.Context(), entry); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	since := now.Add(-time.Hour)
	got, err := w.Stats(t.Context(), Query{Since: &since, SeriesBuckets: 4})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !got.SeriesTruncated {
		t.Fatal("a scan past the lowered cap must report truncation")
	}

	var requests int
	for _, point := range got.Series {
		requests += point.Requests
	}
	if requests != seriesScanLimit {
		t.Fatalf("series covers %d requests, want the %d the scan kept", requests, seriesScanLimit)
	}
	if got.LatencyMs == nil || got.LatencyMs.Count != requests || got.LatencyMs.Max != 30 {
		t.Fatalf("latency percentiles = %+v, want %d durations with max 30: the row past the cap is not in the series", got.LatencyMs, requests)
	}
	if got.TTFTMs == nil || got.TTFTMs.Count != requests || got.TTFTMs.Max != 15 {
		t.Fatalf("TTFT percentiles = %+v, want %d values with max 15: the row past the cap is not in the series", got.TTFTMs, requests)
	}
}
