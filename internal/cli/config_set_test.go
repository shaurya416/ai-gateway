package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

// `admin config set` must hand the Admin API the document it was given.
//
// It decoded the file into a Go value and sent that value re-encoded, which is
// not a no-op: a key the file repeats collapses to its last occurrence. The file
// below lists a guardrail under its first "plugins" key and nothing under its
// second. `ferrogw validate` refuses it and so does PUT /admin/config, both with
// the strict decoder the config loader uses — but the command sent a body with
// one "plugins" key, the empty one, so the gateway applied a config without the
// guardrail and the command printed "Configuration updated." and exited 0.
func TestRunConfigSetSendsTheFileAsWritten(t *testing.T) {
	const file = `{
  "strategy": {"mode": "fallback"},
  "targets": [{"virtual_key": "openai"}],
  "plugins": [{"name": "word-filter", "type": "guardrail", "stage": "before_request",
               "enabled": true, "config": {"blocked_words": ["secret"]}}],
  "plugins": []
}`

	var received []byte
	srv := stubGateway(t, map[string]http.HandlerFunc{
		// Answers as the Admin API's PUT /admin/config does: the body goes
		// through config.DecodeJSONStrict, and a body it refuses is a 400.
		"/admin/config": func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			received = body
			var cfg config.Config
			if err := config.DecodeJSONStrict(body, &cfg); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{"message": "invalid request body: " + err.Error()},
				})
				return
			}
			jsonHandler(http.StatusOK, `{"status":"updated"}`)(w, r)
		},
	})

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cmd, out := newHandlerCmd(t, srv.URL, "table")
	cmd.Flags().String("file", path, "")

	err := runConfigSet(cmd, nil)

	if got := strings.Count(string(received), `"plugins"`); got != 2 {
		t.Errorf(`the request carried "plugins" %d time(s), the file twice; the body sent was:
%s`, got, received)
	}
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("a file the Admin API refuses for a repeated key must fail the command, got error %v and output:\n%s",
			err, out.String())
	}
	if strings.Contains(out.String(), "Configuration updated.") {
		t.Errorf("the command reported success for a config the gateway did not apply:\n%s", out.String())
	}
}
