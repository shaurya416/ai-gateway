package httpserver_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// The server's WriteTimeout in these tests, and how long every provider and
// upstream below takes to answer: twice as long, so an answer is only ever
// produced after the timeout armed at the end of the request's headers.
const (
	answerWriteTimeout = 300 * time.Millisecond
	lateAnswerDelay    = 2 * answerWriteTimeout
)

// lateProvider serves every routed surface, and the pass-through, only after
// lateAnswerDelay — as a slow model, a long transcription or an upstream still
// working does.
type lateProvider struct{ baseURL string }

func (lateProvider) Name() string                { return "late" }
func (lateProvider) SupportsModel(m string) bool { return m == "late-model" }
func (lateProvider) ConfiguredModels() []string  { return []string{"late-model"} }

func (p lateProvider) BaseURL() string { return p.baseURL }
func (lateProvider) AuthHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer upstream-secret"}
}

// wait returns once lateAnswerDelay has passed, or the request has ended.
func wait(ctx context.Context) error {
	timer := time.NewTimer(lateAnswerDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (lateProvider) Complete(ctx context.Context, _ providers.Request) (*providers.Response, error) {
	if err := wait(ctx); err != nil {
		return nil, err
	}
	return &providers.Response{
		ID:    "late-1",
		Model: "late-model",
		Choices: []providers.Choice{{
			Message:      providers.Message{Role: "assistant", Content: "late answer"},
			FinishReason: "stop",
		}},
	}, nil
}

// CompleteStream takes lateAnswerDelay to start, as a provider waiting on its
// upstream's response headers does, and then fails with the upstream's 503
// when the prompt says so.
func (lateProvider) CompleteStream(ctx context.Context, req providers.Request) (<-chan providers.StreamChunk, error) {
	if err := wait(ctx); err != nil {
		return nil, err
	}
	if len(req.Messages) > 0 && req.Messages[0].Content == "refuse" {
		return nil, &core.HTTPStatusError{StatusCode: http.StatusServiceUnavailable, Provider: "late", Message: "overloaded"}
	}
	ch := make(chan providers.StreamChunk, 1)
	ch <- providers.StreamChunk{
		ID:      "late-1",
		Model:   "late-model",
		Choices: []providers.StreamChoice{{Delta: providers.MessageDelta{Role: "assistant", Content: "late answer"}}},
	}
	close(ch)
	return ch, nil
}

func (lateProvider) Embed(ctx context.Context, _ providers.EmbeddingRequest) (*providers.EmbeddingResponse, error) {
	if err := wait(ctx); err != nil {
		return nil, err
	}
	return &providers.EmbeddingResponse{
		Object: "list",
		Model:  "late-model",
		Data:   []providers.Embedding{{Object: "embedding", Embedding: []float64{0.5}}},
	}, nil
}

func (lateProvider) GenerateImage(ctx context.Context, _ providers.ImageRequest) (*providers.ImageResponse, error) {
	if err := wait(ctx); err != nil {
		return nil, err
	}
	return &providers.ImageResponse{Created: 1, Data: []providers.GeneratedImage{{URL: "https://example.com/late.png"}}}, nil
}

func (lateProvider) Rerank(ctx context.Context, _ providers.RerankRequest) (*providers.RerankResponse, error) {
	if err := wait(ctx); err != nil {
		return nil, err
	}
	return &providers.RerankResponse{Model: "late-model", Results: []providers.RerankResult{{Index: 0, RelevanceScore: 0.5}}}, nil
}

func (lateProvider) Moderate(ctx context.Context, _ providers.ModerationRequest) (*providers.ModerationResponse, error) {
	if err := wait(ctx); err != nil {
		return nil, err
	}
	return &providers.ModerationResponse{ID: "modr-late", Model: "late-model", Results: []core.ModerationResult{{}}}, nil
}

func (lateProvider) Transcribe(ctx context.Context, _ providers.TranscriptionRequest) (*providers.TranscriptionResponse, error) {
	if err := wait(ctx); err != nil {
		return nil, err
	}
	return &providers.TranscriptionResponse{Text: "late answer"}, nil
}

func (lateProvider) Speech(ctx context.Context, _ providers.SpeechRequest) (*providers.SpeechResponse, error) {
	if err := wait(ctx); err != nil {
		return nil, err
	}
	return &providers.SpeechResponse{Audio: []byte("late audio"), ContentType: "audio/mpeg"}, nil
}

// newLateUpstream is the upstream the pass-through and the Responses surface
// forward to. It answers after lateAnswerDelay: /responses with a Responses
// body, /empty with a 204 that carries no body at all, anything else with a
// JSON one.
func newLateUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if err := wait(r.Context()); err != nil {
			return
		}
		switch r.URL.Path {
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/responses":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_late","object":"response","output_text":"late answer","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"late answer"}`)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// lateGatewayServer serves the real router, backed by lateProvider, from a
// server whose WriteTimeout is answerWriteTimeout.
func lateGatewayServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("FERRO_MODEL_CATALOG_TIMEOUT", "0")
	upstream := newLateUpstream(t)

	// request_timeout well above the write timeout, as an operator allowing
	// long provider calls sets it.
	gw, err := newTestGateway(t, config.Config{
		Strategy:       config.StrategyConfig{Mode: config.ModeSingle},
		Targets:        []config.Target{{VirtualKey: "late", Models: []string{"late-model"}}},
		RequestTimeout: "10s",
	})
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	gw.RegisterProvider(lateProvider{baseURL: upstream.URL})

	srv := httptest.NewUnstartedServer(buildTestRouter(t, gw))
	srv.Config.WriteTimeout = answerWriteTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// lateRequest is one request whose answer is ready only after the server's
// WriteTimeout, and what the caller must read back.
type lateRequest struct {
	name       string
	method     string
	path       string
	header     map[string]string
	body       func(t *testing.T) (io.Reader, string)
	wantStatus int
	wantBody   string
}

func jsonBody(s string) func(*testing.T) (io.Reader, string) {
	return func(*testing.T) (io.Reader, string) { return strings.NewReader(s), "application/json" }
}

func audioUpload(t *testing.T) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	part, err := form.CreateFormFile("file", "speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(bytes.Repeat([]byte{0x55}, 1<<10))
	if err := form.WriteField("model", "late-model"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, form.FormDataContentType()
}

// An answer that takes longer to produce than the server's WriteTimeout still
// reaches the caller, on every route that waits on a provider. net/http arms
// WriteTimeout once it has read the request's headers, so time spent on the
// provider call was taken out of the time left to write the answer: the
// request was served and recorded as a success while the caller read EOF.
func TestRouter_AnswerReadyAfterWriteTimeoutReachesTheCaller(t *testing.T) {
	srv := lateGatewayServer(t)

	cases := []lateRequest{
		{
			name: "chat", method: http.MethodPost, path: "/v1/chat/completions",
			body:       jsonBody(`{"model":"late-model","messages":[{"role":"user","content":"hi"}]}`),
			wantStatus: http.StatusOK, wantBody: `"content":"late answer"`,
		},
		{
			name: "chat stream", method: http.MethodPost, path: "/v1/chat/completions",
			body:       jsonBody(`{"model":"late-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
			wantStatus: http.StatusOK, wantBody: `"content":"late answer"`,
		},
		{
			// The stream never starts, so its answer is the JSON error the
			// handler writes in its place.
			name: "chat stream refused", method: http.MethodPost, path: "/v1/chat/completions",
			body:       jsonBody(`{"model":"late-model","stream":true,"messages":[{"role":"user","content":"refuse"}]}`),
			wantStatus: http.StatusBadGateway, wantBody: `"error"`,
		},
		{
			name: "legacy completions", method: http.MethodPost, path: "/v1/completions",
			body:       jsonBody(`{"model":"late-model","prompt":"hi"}`),
			wantStatus: http.StatusOK, wantBody: `"text":"late answer"`,
		},
		{
			name: "embeddings", method: http.MethodPost, path: "/v1/embeddings",
			body:       jsonBody(`{"model":"late-model","input":"hi"}`),
			wantStatus: http.StatusOK, wantBody: `"embedding"`,
		},
		{
			name: "images", method: http.MethodPost, path: "/v1/images/generations",
			body:       jsonBody(`{"model":"late-model","prompt":"hi"}`),
			wantStatus: http.StatusOK, wantBody: `late.png`,
		},
		{
			name: "rerank", method: http.MethodPost, path: "/v1/rerank",
			body:       jsonBody(`{"model":"late-model","query":"q","documents":["d"]}`),
			wantStatus: http.StatusOK, wantBody: `"relevance_score":0.5`,
		},
		{
			name: "moderations", method: http.MethodPost, path: "/v1/moderations",
			body:       jsonBody(`{"model":"late-model","input":"hi"}`),
			wantStatus: http.StatusOK, wantBody: `"modr-late"`,
		},
		{
			name: "audio transcription", method: http.MethodPost, path: "/v1/audio/transcriptions",
			body:       audioUpload,
			wantStatus: http.StatusOK, wantBody: `"text":"late answer"`,
		},
		{
			name: "audio translation", method: http.MethodPost, path: "/v1/audio/translations",
			body:       audioUpload,
			wantStatus: http.StatusOK, wantBody: `"text":"late answer"`,
		},
		{
			name: "audio speech", method: http.MethodPost, path: "/v1/audio/speech",
			body:       jsonBody(`{"model":"late-model","input":"hi","voice":"alloy"}`),
			wantStatus: http.StatusOK, wantBody: `late audio`,
		},
		{
			name: "responses create", method: http.MethodPost, path: "/v1/responses",
			body:       jsonBody(`{"model":"late-model","input":"hi"}`),
			wantStatus: http.StatusOK, wantBody: `"resp_late"`,
		},
		{
			name: "pass-through", method: http.MethodPost, path: "/v1/late/answer",
			header:     map[string]string{"X-Provider": "late"},
			body:       jsonBody(`{"input":"hi"}`),
			wantStatus: http.StatusOK, wantBody: `"late answer"`,
		},
		{
			// No body byte is ever written, so the answer is only the status
			// line and headers net/http sends once the handler returns.
			name: "pass-through empty answer", method: http.MethodPost, path: "/v1/empty",
			header:     map[string]string{"X-Provider": "late"},
			body:       jsonBody(`{"input":"hi"}`),
			wantStatus: http.StatusNoContent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, contentType := tc.body(t)
			req, err := http.NewRequestWithContext(t.Context(), tc.method, srv.URL+tc.path, body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", contentType)
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}

			client := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
			defer client.Transport.(*http.Transport).CloseIdleConnections()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("an answer ready after WriteTimeout was lost on the wire: %v", err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatalf("read answer: %v (read so far: %s)", err, got)
			}
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.wantStatus, got)
			}
			if !strings.Contains(string(got), tc.wantBody) {
				t.Fatalf("body = %s, want it to contain %s", got, tc.wantBody)
			}
		})
	}
}
