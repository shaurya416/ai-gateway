package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestLogsStats_SeriesRequiresSince pins that a time series asked for without
// the window it divides is refused rather than answered empty.
//
// A series runs from `since` to now, so with no `since` there is no window to
// divide and the store computes none. The endpoint answered 200 all the same,
// echoing the bucket count it had been asked for beside a series of zero
// points marked not truncated: a log holding traffic read as a window that had
// held none.
func TestLogsStats_SeriesRequiresSince(t *testing.T) {
	store := stageAllStore(t)
	h, r := setupTestRouterWithLogs(store)
	adminKey := createAdminKey(t, h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/admin/logs/stats?buckets=12", "", adminKey))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("buckets without since: status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "since") {
		t.Errorf("body = %s, want the refusal to name since", w.Body.String())
	}

	// The same series over a window is served, and it holds the traffic.
	since := url.QueryEscape(time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/admin/logs/stats?buckets=12&since="+since, "", adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("buckets with since: status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var stats struct {
		Series struct {
			Points []struct {
				Requests int `json:"requests"`
			} `json:"points"`
		} `json:"series"`
	}
	if err := json.NewDecoder(w.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	requests := 0
	for _, p := range stats.Series.Points {
		requests += p.Requests
	}
	if len(stats.Series.Points) != 12 || requests != 3 {
		t.Errorf("series = %d points holding %d requests, want 12 points holding 3", len(stats.Series.Points), requests)
	}

	// Leaving buckets out asks for no series, which needs no window.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/admin/logs/stats", "", adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("no buckets: status = %d, want 200: %s", w.Code, w.Body.String())
	}
}
