package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// TestPostTranscription_FormatFidelity asserts the response format decides how
// the body is read: text/srt/vtt are taken verbatim (so a transcript that is
// itself valid JSON survives), while json/verbose_json/default unmarshal the
// {text} envelope.
func TestPostTranscription_FormatFidelity(t *testing.T) {
	tests := []struct {
		name           string
		responseFormat string
		body           string
		wantText       string
	}{
		{
			name:           "text format with JSON-shaped transcript is kept verbatim",
			responseFormat: "text",
			body:           "{}",
			wantText:       "{}",
		},
		{
			name:           "text format with plain-text transcript",
			responseFormat: "text",
			body:           "hello there",
			wantText:       "hello there",
		},
		{
			name:           "json format decodes the text envelope",
			responseFormat: "json",
			body:           `{"text":"hello"}`,
			wantText:       "hello",
		},
		{
			name:           "default format decodes the text envelope",
			responseFormat: "",
			body:           `{"text":"hello"}`,
			wantText:       "hello",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			p := TranscriptionParams{
				HTTPClient: srv.Client(),
				URL:        srv.URL,
				Headers:    map[string]string{"Authorization": "Bearer test-key"},
				Label:      "testprov",
			}
			req := core.TranscriptionRequest{
				Model:          "whisper-1",
				File:           []byte("audio"),
				ResponseFormat: tt.responseFormat,
			}

			resp, err := PostTranscription(context.Background(), p, req)
			if err != nil {
				t.Fatalf("PostTranscription: %v", err)
			}
			if resp.Text != tt.wantText {
				t.Fatalf("Text = %q, want %q", resp.Text, tt.wantText)
			}
		})
	}
}

// TestPostTranscription_JSONFormatsKeepUpstreamFields pins that the JSON formats
// are served with every member the upstream returned, not text alone.
// verbose_json is the case that matters most: openai-python types it as
// TranscriptionVerbose, which requires duration and language, and its segments
// and words are the timestamps the caller asked for. Read into {text} only,
// they were dropped under a 200, so a client found no timestamps and no error.
func TestPostTranscription_JSONFormatsKeepUpstreamFields(t *testing.T) {
	tests := []struct {
		name           string
		responseFormat string
		body           string
		wantMembers    []string
	}{
		{
			name:           "verbose_json keeps duration, language, segments and words",
			responseFormat: "verbose_json",
			body: `{"task":"transcribe","language":"english","duration":2.5,"text":"hello there",` +
				`"segments":[{"id":0,"start":0,"end":2.5,"text":"hello there"}],` +
				`"words":[{"word":"hello","start":0,"end":0.5}]}`,
			wantMembers: []string{"task", "language", "duration", "segments", "words"},
		},
		{
			name:           "json keeps usage",
			responseFormat: "json",
			body:           `{"text":"hello there","usage":{"type":"duration","seconds":3}}`,
			wantMembers:    []string{"usage"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			resp, err := PostTranscription(context.Background(), TranscriptionParams{
				HTTPClient: srv.Client(),
				URL:        srv.URL,
				Label:      "testprov",
			}, core.TranscriptionRequest{Model: "whisper-1", File: []byte("audio"), ResponseFormat: tt.responseFormat})
			if err != nil {
				t.Fatalf("PostTranscription: %v", err)
			}
			if resp.Text != "hello there" {
				t.Errorf("Text = %q, want %q", resp.Text, "hello there")
			}

			served, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var want, got map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tt.body), &want); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			if err := json.Unmarshal(served, &got); err != nil {
				t.Fatalf("served body is not a JSON object: %v (%s)", err, served)
			}
			for _, member := range append(tt.wantMembers, "text") {
				if string(got[member]) != string(want[member]) {
					t.Errorf("served %s = %s, want %s (body %s)", member, got[member], want[member], served)
				}
			}
		})
	}
}

// A transcript carrying only text is served exactly as before.
func TestPostTranscription_TextOnlyBodyIsUnchanged(t *testing.T) {
	served, err := json.Marshal(core.TranscriptionResponse{Text: "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(served) != `{"text":"hi"}` {
		t.Errorf("served %s, want {\"text\":\"hi\"}", served)
	}
}
