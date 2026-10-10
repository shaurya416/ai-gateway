package requestlog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// textBackends returns the stores a text-handling test runs against: SQLite
// always, and Postgres too when FERROGW_TEST_POSTGRES_DSN names a database.
// Postgres is the backend that refuses a NUL byte or a byte sequence that is
// not UTF-8 in a TEXT value, so a run without it checks only that SQLite
// stores the same form.
func textBackends(t *testing.T) map[string]*SQLWriter {
	t.Helper()
	backends := map[string]*SQLWriter{}

	sqlite, err := NewSQLiteWriter(t.Context(), filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatalf("new sqlite writer: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlite.Close(); err != nil {
			t.Errorf("close sqlite writer: %v", err)
		}
	})
	backends["sqlite"] = sqlite

	if dsn := os.Getenv("FERROGW_TEST_POSTGRES_DSN"); dsn != "" {
		pg, err := NewPostgresWriter(t.Context(), dsn)
		if err != nil {
			t.Fatalf("new postgres writer: %v", err)
		}
		_, _ = pg.db.ExecContext(t.Context(), "DELETE FROM request_logs")
		t.Cleanup(func() {
			_, _ = pg.db.ExecContext(context.Background(), "DELETE FROM request_logs")
			if err := pg.Close(); err != nil {
				t.Errorf("close postgres writer: %v", err)
			}
		})
		backends["postgres"] = pg
	}
	return backends
}

// A request whose text carries a NUL byte, or bytes that are not UTF-8, still
// leaves its row — and the same row on every backend.
//
// The values are caller-supplied: a JSON model of "gpt-4o\u0000probe" decodes
// to a NUL, a multipart model field carries any byte, and the error naming the
// unknown model quotes it. Postgres refused the INSERT, so the on_error row of
// a request the gateway had answered was lost and the request log held no
// trace of it, while SQLite kept the raw bytes.
func TestSQLWriter_UnrepresentableTextIsStoredOnEveryBackend(t *testing.T) {
	for name, w := range textBackends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			createdAt := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)

			failed := Entry{
				TraceID:      "0af7651916cd43dd8448eb211c80319c",
				Stage:        stageOnError,
				Model:        "gpt-4o\x00probe",
				Provider:     "open\xffai",
				APIKeyID:     "key\x00id",
				UserID:       "user-\xfe42",
				SessionID:    "sess\x00-7",
				ErrorMessage: "no target serves model \"gpt-4o\x00probe\": \xfe\xff",
				CreatedAt:    createdAt,
			}
			if err := w.Write(ctx, failed); err != nil {
				t.Fatalf("write: %v", err)
			}

			// A filter naming the value the request carried selects its row.
			listed, err := w.List(ctx, Query{Model: failed.Model, Provider: failed.Provider, Stage: stageOnError})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if listed.Total != 1 || len(listed.Data) != 1 {
				t.Fatalf("listed %d rows (total %d), want the one written", len(listed.Data), listed.Total)
			}
			got := listed.Data[0]
			for _, field := range []struct{ name, got, want string }{
				{"model", got.Model, "gpt-4o\uFFFDprobe"},
				{"provider", got.Provider, "open\uFFFDai"},
				{"api_key_id", got.APIKeyID, "key\uFFFDid"},
				{"user_id", got.UserID, "user-\uFFFD42"},
				{"session_id", got.SessionID, "sess\uFFFD-7"},
				{"error_message", got.ErrorMessage, "no target serves model \"gpt-4o\uFFFDprobe\": \uFFFD"},
			} {
				if field.got != field.want {
					t.Errorf("%s = %q, want %q", field.name, field.got, field.want)
				}
			}

			stats, err := w.Stats(ctx, Query{Model: failed.Model})
			if err != nil {
				t.Fatalf("stats: %v", err)
			}
			if stats.TotalEntries != 1 || stats.ErrorEntries != 1 {
				t.Errorf("stats for the model = %d requests, %d errors; want 1 and 1", stats.TotalEntries, stats.ErrorEntries)
			}

			// A late failure recorded on a completed row takes the same form.
			completedAt := createdAt.Add(time.Second)
			if err := w.Write(ctx, Entry{
				TraceID:   "1af7651916cd43dd8448eb211c80319c",
				Stage:     stageAfterRequest,
				Model:     "gpt-4o",
				Provider:  "openai",
				CreatedAt: completedAt,
			}); err != nil {
				t.Fatalf("write completed row: %v", err)
			}
			annotated, err := w.AnnotateError(ctx, "1af7651916cd43dd8448eb211c80319c", stageAfterRequest, completedAt, "after_request plugin failed: \xff\x00")
			if err != nil || !annotated {
				t.Fatalf("annotate = %v, %v; want the completed row annotated", annotated, err)
			}
			late, err := w.List(ctx, Query{Stage: stageAfterRequest})
			if err != nil {
				t.Fatalf("list completed: %v", err)
			}
			if len(late.Data) != 1 || late.Data[0].ErrorMessage != "after_request plugin failed: \uFFFD\uFFFD" {
				t.Errorf("annotated rows = %+v, want one carrying the failure as storable text", late.Data)
			}
		})
	}
}

// Valid text, which is every value in practice, is stored as given and costs
// nothing to check.
func TestStorableText_LeavesValidTextAlone(t *testing.T) {
	for _, s := range []string{"", "gpt-4o", "モデル", "error: \"quoted\"\n\ttrace"} {
		if got := storableText(s); got != s {
			t.Errorf("storableText(%q) = %q, want it unchanged", s, got)
		}
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = storableText("claude-sonnet-4-5") }); allocs != 0 {
		t.Errorf("storableText allocated %v times on valid text, want 0", allocs)
	}
}
