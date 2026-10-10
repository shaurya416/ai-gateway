package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
)

// srtTranscript is what an upstream answers response_format=srt with, and what
// the shared OpenAI-compatible decoder hands back as Text verbatim.
const srtTranscript = "1\n00:00:00,000 --> 00:00:01,500\nHello there.\n\n"

// subtitleProvider answers every transcription with srtTranscript and records
// the response_format it was asked for.
type subtitleProvider struct{ gotFormat string }

func (*subtitleProvider) Name() string                { return "stt" }
func (*subtitleProvider) SupportsModel(m string) bool { return m == "whisper-1" }
func (*subtitleProvider) Complete(context.Context, providers.Request) (*providers.Response, error) {
	return &providers.Response{}, nil
}
func (p *subtitleProvider) Transcribe(_ context.Context, req providers.TranscriptionRequest) (*providers.TranscriptionResponse, error) {
	p.gotFormat = req.ResponseFormat
	return &providers.TranscriptionResponse{Text: srtTranscript}, nil
}

func audioUpload(t *testing.T, path, format string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("RIFF....WAVE"))
	_ = form.WriteField("model", "whisper-1")
	if format != "" {
		_ = form.WriteField("response_format", format)
	}
	_ = form.Close()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	return r
}

// TestTranscriptions_PlainTextFormatsAreTheBody pins the OpenAI contract for
// response_format text, srt and vtt: the transcript is the response body, not a
// {"text": …} object. Both reference SDKs type those three as a string result —
// openai-python returns the body as it arrived, and openai-node parses any JSON
// media type into an object — so a JSON envelope reached the caller as the
// subtitle file itself, answered 200.
func TestTranscriptions_PlainTextFormatsAreTheBody(t *testing.T) {
	for _, tc := range []struct {
		path      string
		translate bool
		format    string
	}{
		{"/v1/audio/transcriptions", false, "srt"},
		{"/v1/audio/transcriptions", false, "vtt"},
		{"/v1/audio/transcriptions", false, "text"},
		{"/v1/audio/translations", true, "srt"},
	} {
		t.Run(tc.path+"/"+tc.format, func(t *testing.T) {
			gw, err := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets:  []config.Target{{VirtualKey: "stt", Models: []string{"whisper-1"}}},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			stub := &subtitleProvider{}
			gw.RegisterProvider(stub)

			w := httptest.NewRecorder()
			Transcriptions(gw, tc.translate)(w, audioUpload(t, tc.path, tc.format))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
			}
			if stub.gotFormat != tc.format {
				t.Fatalf("provider asked for response_format %q, want %q", stub.gotFormat, tc.format)
			}
			if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "json") {
				t.Errorf("Content-Type = %q: a JSON media type is parsed into an object where the SDKs promise a string", ct)
			}
			if got := w.Body.String(); got != srtTranscript {
				t.Errorf("body = %q, want the transcript verbatim %q", got, srtTranscript)
			}
		})
	}
}

// TestTranscriptions_JSONFormatsKeepTheEnvelope pins the other half: the
// default and json formats are the {"text": …} object they always were.
func TestTranscriptions_JSONFormatsKeepTheEnvelope(t *testing.T) {
	for _, format := range []string{"", "json"} {
		t.Run("format="+format, func(t *testing.T) {
			gw, err := newTestGateway(t, config.Config{
				Strategy: config.StrategyConfig{Mode: config.ModeSingle},
				Targets:  []config.Target{{VirtualKey: "stt", Models: []string{"whisper-1"}}},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			gw.RegisterProvider(&subtitleProvider{})

			w := httptest.NewRecorder()
			Transcriptions(gw, false)(w, audioUpload(t, "/v1/audio/transcriptions", format))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var body struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not a JSON object: %v (%s)", err, w.Body.String())
			}
			if body.Text != srtTranscript {
				t.Errorf("text = %q, want %q", body.Text, srtTranscript)
			}
		})
	}
}
