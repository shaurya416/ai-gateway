package core

import "encoding/json"

// TranscriptionRequest mirrors the OpenAI /v1/audio/transcriptions (and
// /v1/audio/translations) multipart request. It is built from the multipart form
// by the handler, not JSON-decoded, so its fields carry no json tags. File holds
// the raw audio bytes.
type TranscriptionRequest struct {
	Model    string
	File     []byte
	Filename string
	// Language is an ISO-639-1 hint; ignored on the translations path (which
	// always targets English).
	Language string
	Prompt   string
	// ResponseFormat is json (default) | text | srt | verbose_json | vtt.
	ResponseFormat string
	Temperature    *float64
	// Translate selects /v1/audio/translations (translate to English) instead of
	// /v1/audio/transcriptions.
	Translate bool
}

// TranscriptionResponse is the gateway-standard transcription result. Text is
// the transcript regardless of the upstream response_format.
//
// Extra holds every other member of the upstream's JSON response object, served
// back unchanged beside text: verbose_json's duration, language, segments and
// words, diarized_json's speaker segments, and the usage and logprobs a json
// response carries. Decoding text alone answered verbose_json with a bare
// {"text": …}, which the OpenAI SDKs read as a verbose result with no
// timestamps — openai-python's TranscriptionVerbose requires duration and
// language — under a 200.
type TranscriptionResponse struct {
	Text  string                     `json:"text"`
	Extra map[string]json.RawMessage `json:"-"`
}

// MarshalJSON writes text together with the upstream's other members. Text is
// written from the field, so it is authoritative over any copy in Extra.
func (r TranscriptionResponse) MarshalJSON() ([]byte, error) {
	if len(r.Extra) == 0 {
		return json.Marshal(struct {
			Text string `json:"text"`
		}{r.Text})
	}
	text, err := json.Marshal(r.Text)
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(r.Extra)+1)
	for k, v := range r.Extra {
		out[k] = v
	}
	out["text"] = text
	return json.Marshal(out)
}

// UnmarshalJSON reads an upstream JSON response object: text into Text, and
// every other member into Extra.
func (r *TranscriptionResponse) UnmarshalJSON(data []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	var text string
	if raw, ok := members["text"]; ok {
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		delete(members, "text")
	}
	r.Text = text
	r.Extra = nil
	if len(members) > 0 {
		r.Extra = members
	}
	return nil
}
