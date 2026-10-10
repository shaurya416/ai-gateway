package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/requestlog"
)

// stageAllStore is a real SQLite request log holding what a request logger
// writes for three requests: a before_request row and a terminal row each, an
// hour old so a purge cut off at now covers all six.
func stageAllStore(t *testing.T) *requestlog.SQLWriter {
	t.Helper()
	store, err := requestlog.NewSQLiteWriter(t.Context(), filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatalf("open request log: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	at := time.Now().UTC().Add(-time.Hour)
	for _, trace := range []string{"a", "b", "c"} {
		for _, entry := range []requestlog.Entry{
			{TraceID: trace, Stage: "before_request", Model: "gpt-4", CreatedAt: at},
			{TraceID: trace, Stage: "after_request", Model: "gpt-4", Provider: "openai", TotalTokens: 10, CreatedAt: at},
		} {
			if err := store.Write(t.Context(), entry); err != nil {
				t.Fatalf("write %s/%s: %v", trace, entry.Stage, err)
			}
		}
	}
	return store
}

// TestLogsStageAll_PurgeAndStatsMeanEveryStage holds DELETE /admin/logs and
// GET /admin/logs/stats to the meaning GET /admin/logs gives `stage=all`: every
// stage, not a stage named "all". Both used to pass the sentinel through as a
// literal stage, so a purge scoped the way the listing is filtered matched no
// row and answered 200 {"deleted":0} — a retention purge reported as done that
// removed nothing — and the stats summary for the same filter read as a gateway
// that had served no traffic.
func TestLogsStageAll_PurgeAndStatsMeanEveryStage(t *testing.T) {
	store := stageAllStore(t)
	h, r := setupTestRouterWithLogs(store)
	adminKey := createAdminKey(t, h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, "/admin/logs/stats?stage=all", "", adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("stats: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var stats struct {
		Summary struct {
			TotalEntries int `json:"total_entries"`
		} `json:"summary"`
		ByStage map[string]dimension `json:"by_stage"`
	}
	if err := json.NewDecoder(w.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.Summary.TotalEntries != 3 {
		t.Errorf("stats?stage=all total_entries = %d, want 3: the filter that lists every row must not summarise none",
			stats.Summary.TotalEntries)
	}
	if got := stats.ByStage["before_request"].Count + stats.ByStage["after_request"].Count; got != 6 {
		t.Errorf("stats?stage=all by_stage covers %d rows, want 6: %v", got, stats.ByStage)
	}

	before := url.QueryEscape(time.Now().UTC().Format(time.RFC3339))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodDelete, "/admin/logs?stage=all&before="+before, "", adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var purge struct {
		Deleted int `json:"deleted"`
	}
	if err := json.NewDecoder(w.Body).Decode(&purge); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	if purge.Deleted != 6 {
		t.Errorf("DELETE ?stage=all deleted %d rows, want 6 (every stage before the cutoff)", purge.Deleted)
	}

	if total, _ := listLogsWith(t, r, adminKey, "/admin/logs?stage=all"); total != 0 {
		t.Errorf("%d rows survived a purge of every stage", total)
	}
}

func listLogsWith(t *testing.T, r http.Handler, key *model.APIKey, target string) (int, []requestlog.Entry) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedRequest(http.MethodGet, target, "", key))
	if w.Code != http.StatusOK {
		t.Fatalf("%s: expected 200, got %d: %s", target, w.Code, w.Body.String())
	}
	var payload struct {
		Data    []requestlog.Entry `json:"data"`
		Summary struct {
			TotalEntries int `json:"total_entries"`
		} `json:"summary"`
	}
	if err := json.NewDecoder(w.Body).Decode(&payload); err != nil {
		t.Fatalf("%s: decode: %v", target, err)
	}
	return payload.Summary.TotalEntries, payload.Data
}
