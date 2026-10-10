package replicate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// newStreamEndStub serves a streaming prediction whose SSE body is streamBody
// and whose post-stream prediction read answers predictionBody.
func newStreamEndStub(t *testing.T, streamBody, predictionBody string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models/test/model/predictions":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"pred-end","status":"starting","urls":{"stream":"` + srv.URL + `/stream"}}`))
		case "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(streamBody))
		case "/v1/predictions/pred-end":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(predictionBody))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// drainStream returns every chunk on ch and the last error any of them carried.
func drainStream(ch <-chan core.StreamChunk) ([]core.StreamChunk, error) {
	var chunks []core.StreamChunk
	var streamErr error
	for c := range ch {
		chunks = append(chunks, c)
		if c.Error != nil {
			streamErr = c.Error
		}
	}
	return chunks, streamErr
}

func streamFrom(t *testing.T, srv *httptest.Server) <-chan core.StreamChunk {
	t.Helper()
	p, err := New("test-token", srv.URL, []string{"test/model"}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	ch, err := p.CompleteStream(t.Context(), core.Request{
		Model:    "test/model",
		Messages: []core.Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("CompleteStream() error: %v", err)
	}
	return ch
}

// TestReplicateProvider_CompleteStream_DoneForUnfinishedPredictionFails pins
// that a "done" event is not, by itself, a success. Replicate emits it when a
// prediction finishes successfully, is canceled, or produces an error, so the
// stream has to be decided by the prediction's own status — the field Complete
// already fails on. Reading only the event ended a canceled prediction's stream
// with finish_reason "stop", recorded and billed as a complete answer.
func TestReplicateProvider_CompleteStream_DoneForUnfinishedPredictionFails(t *testing.T) {
	cases := []struct {
		name       string
		prediction string
		wantInErr  string
	}{
		{
			name:       "canceled",
			prediction: `{"id":"pred-end","status":"canceled","metrics":{"input_token_count":5,"output_token_count":2}}`,
			wantInErr:  "canceled",
		},
		{
			name:       "failed",
			prediction: `{"id":"pred-end","status":"failed","error":"CUDA out of memory","metrics":{"input_token_count":5,"output_token_count":2}}`,
			wantInErr:  "CUDA out of memory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStreamEndStub(t,
				"event: output\ndata: partial\n\nevent: done\ndata: {}\n\n",
				tc.prediction)

			chunks, streamErr := drainStream(streamFrom(t, srv))

			if streamErr == nil {
				t.Fatalf("stream ended without an error for a %s prediction: %+v", tc.name, chunks)
			}
			if !strings.Contains(streamErr.Error(), tc.wantInErr) {
				t.Errorf("stream error = %q, want it to contain %q", streamErr, tc.wantInErr)
			}
			for i, c := range chunks {
				for _, choice := range c.Choices {
					if choice.FinishReason != "" {
						t.Errorf("chunk %d reports finish_reason %q for a %s prediction", i, choice.FinishReason, tc.name)
					}
				}
			}
			// The tokens were consumed whatever the outcome, so the failure
			// still carries what the prediction reports.
			last := chunks[len(chunks)-1]
			if last.Usage == nil || last.Usage.PromptTokens != 5 || last.Usage.CompletionTokens != 2 {
				t.Errorf("error chunk usage = %+v, want prompt=5 completion=2", last.Usage)
			}
		})
	}
}

// TestReplicateProvider_CompleteStream_EndBeforeDoneFails pins that a stream
// body which runs out before any terminal event is a failure. The connection
// can close before the prediction finishes — Replicate's Go client reconnects
// in exactly that case — so what was delivered is only part of the answer, and
// closing the channel cleanly recorded it as a complete one.
func TestReplicateProvider_CompleteStream_EndBeforeDoneFails(t *testing.T) {
	srv := newStreamEndStub(t,
		"event: output\ndata: Hello\n\nevent: output\ndata:  wor\n\n",
		`{"id":"pred-end","status":"processing"}`)

	chunks, streamErr := drainStream(streamFrom(t, srv))

	if streamErr == nil {
		t.Fatalf("stream that ended before its done event reported success: %+v", chunks)
	}
	if !strings.Contains(streamErr.Error(), "before its done event") {
		t.Errorf("stream error = %q, want it to name the missing done event", streamErr)
	}
	if got := chunks[0].Choices[0].Delta.Content + chunks[1].Choices[0].Delta.Content; got != "Hello wor" {
		t.Errorf("delivered content = %q, want the partial output kept", got)
	}
}

// TestReplicateProvider_CompleteStream_DoneAtEOFCompletes is the control for
// the check above: a done event that is the last thing in the body, with no
// trailing blank line, still ends the stream as a success.
func TestReplicateProvider_CompleteStream_DoneAtEOFCompletes(t *testing.T) {
	for name, body := range map[string]string{
		"with data":    "event: output\ndata: hi\n\nevent: done\ndata: {}",
		"without data": "event: output\ndata: hi\n\nevent: done\n",
	} {
		t.Run(name, func(t *testing.T) {
			srv := newStreamEndStub(t, body, `{"id":"pred-end","status":"succeeded"}`)

			chunks, streamErr := drainStream(streamFrom(t, srv))

			if streamErr != nil {
				t.Fatalf("stream error = %v, want a completed stream", streamErr)
			}
			last := chunks[len(chunks)-1]
			if len(last.Choices) != 1 || last.Choices[0].FinishReason != core.FinishReasonStop {
				t.Errorf("terminal chunk = %+v, want finish_reason stop", last)
			}
		})
	}
}
