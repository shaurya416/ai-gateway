package httpserver_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
)

// An audio upload larger than the handler's in-memory multipart allowance is
// spilled to a temporary file. net/http removes those only for the request it
// dispatched, and every middleware that adds to the context hands the handler a
// copy, so before the handler cleaned up after itself each such upload left its
// whole file on disk for the life of the process — up to 25 MiB a request from
// any caller allowed to reach the audio routes.
//
// It runs through the real router on a real server, because that is where the
// copies are made and where the server's own cleanup runs: a recorder calling
// the handler directly would have neither.
func TestRouter_AudioUploadLeavesNoTempFiles(t *testing.T) {
	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "stub"}},
	})
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	srv := httptest.NewServer(buildTestRouter(t, gw))
	t.Cleanup(srv.Close)

	// Over the 10 MiB in-memory allowance and under the 25 MiB route cap.
	audio := bytes.Repeat([]byte{0x55}, 11<<20)

	for _, tc := range []struct {
		name   string
		path   string
		fields map[string]string
	}{
		// Parsed and routed: the stub serves no transcription model, so this
		// is answered by the router, after the form has been read.
		{name: "routed", path: "/v1/audio/transcriptions", fields: map[string]string{"model": "whisper-1"}},
		{name: "translation", path: "/v1/audio/translations", fields: map[string]string{"model": "whisper-1"}},
		// Refused after the form was parsed.
		{name: "missing model", path: "/v1/audio/transcriptions"},
		// The form is parsed and then an error is returned for the query string,
		// which leaves the spilled file behind unless the error path removes it.
		{name: "malformed query", path: "/v1/audio/transcriptions?a=%zz", fields: map[string]string{"model": "whisper-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// os.TempDir reads TMPDIR on every call, so this case's multipart
			// spill files land in a directory of its own.
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)

			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			for k, v := range tc.fields {
				if err := mw.WriteField(k, v); err != nil {
					t.Fatalf("write field: %v", err)
				}
			}
			part, err := mw.CreateFormFile("file", "speech.wav")
			if err != nil {
				t.Fatalf("create form file: %v", err)
			}
			if _, err := part.Write(audio); err != nil {
				t.Fatalf("write audio: %v", err)
			}
			if err := mw.Close(); err != nil {
				t.Fatalf("close multipart: %v", err)
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+tc.path, &body)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", mw.FormDataContentType())
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("POST %s: %v", tc.path, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("status = 200; the stub serves no transcription model")
			}

			entries, err := os.ReadDir(tmp)
			if err != nil {
				t.Fatalf("read temp dir: %v", err)
			}
			var leaked []string
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "multipart-") {
					leaked = append(leaked, e.Name())
				}
			}
			if len(leaked) > 0 {
				t.Fatalf("status %d: upload left %d temp file(s) behind: %v", resp.StatusCode, len(leaked), leaked)
			}
		})
	}
}
