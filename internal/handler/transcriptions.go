package handler

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/internal/apierror"
	"github.com/ferro-labs/ai-gateway/providers"
)

// transcriptionMultipartMemory bounds how much of the multipart form is buffered
// in memory before Go spills the rest to a temp file.
const transcriptionMultipartMemory = 10 << 20 // 10 MiB

// audioMaxRequestBytes caps the audio upload body. It is larger than the JSON
// surfaces' default because a multipart audio file is larger; OpenAI's whisper
// accepts 25 MiB. Applied here (not via the shared MaxRequestBody middleware)
// because the audio routes need a higher cap than the /v1/* group's, and the
// body must be bounded before the multipart parser reads it.
const audioMaxRequestBytes = 25 << 20 // 25 MiB

// Transcriptions handles POST /v1/audio/transcriptions (translate=false) and
// POST /v1/audio/translations (translate=true). It parses the multipart audio
// upload and routes to the first registered TranscriptionProvider that serves
// the requested model.
func Transcriptions(gw *aigateway.Gateway, translate bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, audioMaxRequestBytes)
		err := r.ParseMultipartForm(transcriptionMultipartMemory)
		// A file part larger than transcriptionMultipartMemory is spilled to a
		// temporary file. net/http removes those only for the request it
		// dispatched, and this handler holds a copy of it — every middleware
		// that adds to the context makes one — so the form parsed here is not
		// the one the server cleans up. Removed here, on every path: the form
		// can be set even when an error is returned (a malformed query string).
		if r.MultipartForm != nil {
			defer func() { _ = r.MultipartForm.RemoveAll() }()
		}
		if err != nil {
			writeAudioBodyError(w, err, "invalid multipart form")
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			writeAudioBodyError(w, err, "file is required")
			return
		}
		defer func() { _ = file.Close() }()

		model := r.FormValue("model")
		if model == "" {
			apierror.WriteOpenAI(w, http.StatusBadRequest, "model is required", "invalid_request_error", "invalid_request")
			return
		}

		// A temperature that is not a number is the caller's mistake, and it
		// is reported as one. Dropping it instead answered 200 at the
		// provider's default sampling, with nothing in the response to say the
		// value the caller set was never applied.
		var temperature *float64
		if t := r.FormValue("temperature"); t != "" {
			v, perr := strconv.ParseFloat(t, 64)
			if perr != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				apierror.WriteOpenAI(w, http.StatusBadRequest, "temperature must be a number", "invalid_request_error", "invalid_request")
				return
			}
			temperature = &v
		}

		audio, err := io.ReadAll(file)
		if err != nil {
			writeAudioBodyError(w, err, "failed to read audio file")
			return
		}

		req := providers.TranscriptionRequest{
			Model:          model,
			File:           audio,
			Filename:       header.Filename,
			Language:       r.FormValue("language"),
			Prompt:         r.FormValue("prompt"),
			ResponseFormat: r.FormValue("response_format"),
			Temperature:    temperature,
			Translate:      translate,
		}

		attribution := &aigateway.RoutingAttribution{}
		ctx := aigateway.WithRoutingAttribution(r.Context(), attribution)
		resp, err := gw.Transcribe(ctx, req)
		attribution.SetHeaders(w.Header())
		if err != nil {
			apierror.WriteRouteError(w, err)
			return
		}

		if plainTextAudioFormat(req.ResponseFormat) {
			// The transcript is the whole body for these formats, not a
			// {"text": …} envelope — Text already holds it verbatim. The media
			// type is what the SDKs key on: openai-node parses a JSON one into
			// an object where its types promise a string, and openai-python
			// hands back the body itself, so an envelope reached the caller as
			// the subtitle file or transcript it had asked for.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, resp.Text)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// plainTextAudioFormat reports whether response_format asks for the transcript
// itself rather than a JSON object: text, srt and vtt, the three formats the
// OpenAI SDKs type as a string result.
func plainTextAudioFormat(format string) bool {
	switch format {
	case "text", "srt", "vtt":
		return true
	default:
		return false
	}
}

// writeAudioBodyError maps a multipart/read failure to 413 for a body-limit hit
// or 400 otherwise.
func writeAudioBodyError(w http.ResponseWriter, err error, msg string) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		apierror.WriteOpenAI(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error", "request_too_large")
		return
	}
	apierror.WriteOpenAI(w, http.StatusBadRequest, msg, "invalid_request_error", "invalid_request")
}
