package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/sse"
	"github.com/ferro-labs/ai-gateway/internal/streamwrap"
	"github.com/ferro-labs/ai-gateway/providers"
	"github.com/ferro-labs/ai-gateway/providers/core"
)

// This file carries the stream suite past the provider boundary. Each
// provider's native stream is driven through the gateway's own stream path —
// the meter every provider stream is wrapped in, then the SSE writer that puts
// it on the wire — and what a client receives is asserted against the OpenAI
// streaming contract, while the response the meter assembles for the
// after_request stage (the one the response cache stores and replays) is
// asserted against the same provider's unary translation.

// wireFrame is one SSE data frame the gateway wrote: a chunk, an error event,
// or the [DONE] sentinel.
type wireFrame struct {
	raw   string
	done  bool
	chunk struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int               `json:"index"`
			Delta        core.MessageDelta `json:"delta"`
			FinishReason *string           `json:"finish_reason"`
		} `json:"choices"`
		Usage *core.Usage      `json:"usage"`
		Error *json.RawMessage `json:"error"`
	}
}

// gatewayStream drives ch through streamwrap.Meter and sse.Write, as the chat
// handler does, and returns the frames written and the response the meter
// handed the after_request stage — nil when the stream did not complete.
func gatewayStream(t *testing.T, ch <-chan core.StreamChunk, model string, suppressUsage bool) ([]wireFrame, *providers.Response) {
	t.Helper()
	var assembled *providers.Response
	out := streamwrap.Meter(context.Background(), ch, time.Now(), streamwrap.MeterMeta{
		Provider:               "conformance",
		Model:                  model,
		MetricModel:            model,
		SuppressUsageForClient: suppressUsage,
		CompletionFn: func(_ context.Context, resp *providers.Response, _ streamwrap.Measurements) error {
			cp := *resp
			assembled = &cp
			return nil
		},
	})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sse.Write(context.Background(), rec, out, nil)
	}()
	select {
	case <-done:
	case <-time.After(streamDrainTimeout):
		t.Fatalf("the gateway did not finish writing the stream within %s", streamDrainTimeout)
	}

	var frames []wireFrame
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		f := wireFrame{raw: data}
		if data == core.SSEDone {
			f.done = true
		} else if err := json.Unmarshal([]byte(data), &f.chunk); err != nil {
			t.Fatalf("frame %q is not JSON: %v", data, err)
		}
		frames = append(frames, f)
	}
	return frames, assembled
}

// startStream builds provider id against srv and opens its stream for the
// canonical request.
func startStream(t *testing.T, id string, fx streamFixture, srv *httptest.Server) <-chan core.StreamChunk {
	t.Helper()
	p := buildProvider(t, id, fx.extraCfg, fx.baseURLKey, srv.URL)
	sp, ok := p.(core.StreamProvider)
	if !ok {
		t.Fatalf("provider %q does not implement core.StreamProvider", id)
	}
	ch, err := sp.CompleteStream(t.Context(), canonicalRequest(fx.model))
	if err != nil {
		t.Fatalf("CompleteStream(): %v", err)
	}
	return ch
}

// fixtureStub serves a provider's native stream fixture.
func fixtureStub(t *testing.T, id string, fx streamFixture) *httptest.Server {
	t.Helper()
	if fx.newStub != nil {
		return fx.newStub(t)
	}
	return newNativeStub(loadTestdata(t, id, "chat.stream.sse"))
}

// TestProviderStreamWireConformance asserts what a client receives from each
// provider's native stream once the gateway has put it on the wire: frames in
// the chunk shape under one id and the routed model, the assistant role on the
// first delta and nowhere else, exactly one canonical finish_reason, usage
// reported once and never before that finish, and [DONE] last. With
// stream_options.include_usage:false the same stream carries no usage and no
// frame left empty by removing it.
func TestProviderStreamWireConformance(t *testing.T) {
	for id, fx := range streamFixtures() {
		for _, suppress := range []bool{false, true} {
			name := id
			if suppress {
				name += "/include_usage=false"
			}
			t.Run(name, func(t *testing.T) {
				srv := fixtureStub(t, id, fx)
				defer srv.Close()

				frames, assembled := gatewayStream(t, startStream(t, id, fx, srv), fx.model, suppress)
				if assembled == nil {
					t.Fatal("the stream did not complete; the after_request stage never ran")
				}
				if len(frames) < 2 || !frames[len(frames)-1].done {
					t.Fatalf("frames = %d, the last must be [DONE]; got %v", len(frames), frames)
				}
				tally := tallyWireChunks(t, frames[:len(frames)-1], fx.model)
				roles, finishes, usages := tally.roles, tally.finishes, tally.usages
				if roles != 1 {
					t.Errorf("delta.role appeared %d times, want once", roles)
				}
				if finishes != 1 {
					t.Errorf("finish_reason appeared %d times, want once", finishes)
				}
				if got := tally.content; got != wantContent {
					t.Errorf("content on the wire = %q, want %q", got, wantContent)
				}
				switch {
				case suppress && usages != 0:
					t.Errorf("usage reached a client that set include_usage:false on %d frame(s)", usages)
				case !suppress && !fx.noUsage && usages != 1:
					t.Errorf("usage appeared on %d frames, want exactly one", usages)
				}

				// The assembled response is metered whatever the client opted out of.
				if !fx.noUsage && (assembled.Usage.PromptTokens != wantPromptTokens || assembled.Usage.CompletionTokens != wantCompletionTokens) {
					t.Errorf("assembled usage = %+v, want prompt=%d completion=%d", assembled.Usage, wantPromptTokens, wantCompletionTokens)
				}
			})
		}
	}
}

// wireTally counts what a one-choice stream's chunk frames carried.
type wireTally struct {
	roles, finishes, usages int
	content                 string
}

// tallyWireChunks checks each chunk frame of a one-choice stream on its own —
// the chunk shape, the stream's one id, the routed model, a canonical
// finish_reason with nothing after it, usage only after the finish — and
// returns what the frames carried between them.
func tallyWireChunks(t *testing.T, chunks []wireFrame, model string) wireTally {
	t.Helper()
	var tally wireTally
	var content strings.Builder
	finishAt := -1
	for i, f := range chunks {
		c := f.chunk
		if f.done || c.Error != nil {
			t.Fatalf("frame %d is [DONE] or an error event before the end: %s", i, f.raw)
		}
		if c.Object != "chat.completion.chunk" || c.ID == "" || c.ID != chunks[0].chunk.ID || c.Model != model {
			t.Errorf("frame %d object/id/model = %q/%q/%q, want chat.completion.chunk under the stream's one id %q and the routed model %q",
				i, c.Object, c.ID, c.Model, chunks[0].chunk.ID, model)
		}
		if c.Choices == nil || (len(c.Choices) == 0 && c.Usage == nil) {
			t.Errorf("frame %d carries no choices array, or neither a choice nor usage: %s", i, f.raw)
		}
		for _, ch := range c.Choices {
			if ch.Index != 0 {
				t.Errorf("frame %d choice index = %d, want 0 for a one-choice request", i, ch.Index)
			}
			if ch.Delta.Role != "" {
				tally.roles++
				if ch.Delta.Role != core.RoleAssistant || i != 0 {
					t.Errorf("frame %d delta.role = %q; the role belongs on the first delta only, as assistant", i, ch.Delta.Role)
				}
			}
			if finishAt >= 0 && ch.Delta.Content != "" {
				t.Errorf("frame %d carries content after the finish_reason on frame %d", i, finishAt)
			}
			content.WriteString(ch.Delta.Content)
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				tally.finishes++
				finishAt = i
				if !slices.Contains(canonicalFinishReasons, *ch.FinishReason) {
					t.Errorf("frame %d finish_reason = %q, not canonical", i, *ch.FinishReason)
				}
			}
		}
		if c.Usage != nil {
			tally.usages++
			if finishAt < 0 {
				t.Errorf("frame %d carries usage before any finish_reason", i)
			}
		}
	}
	tally.content = content.String()
	return tally
}

// TestProviderStreamMatchesUnary asserts that the response the gateway
// assembles from each provider's stream — the one the after_request stage
// reads and the response cache stores and replays — is the message the same
// provider's unary translation returns: one choice, the assistant role, the
// same content and usage, a canonical finish_reason.
func TestProviderStreamMatchesUnary(t *testing.T) {
	for id, fx := range streamFixtures() {
		ufx, ok := fixtures()[id]
		if !ok {
			continue
		}
		t.Run(id, func(t *testing.T) {
			srv := fixtureStub(t, id, fx)
			defer srv.Close()
			_, assembled := gatewayStream(t, startStream(t, id, fx, srv), fx.model, false)
			if assembled == nil {
				t.Fatal("the stream did not complete")
			}

			usrv := newNativeStub(loadTestdata(t, id, "chat.response.json"))
			defer usrv.Close()
			unary, err := buildProvider(t, id, ufx.extraCfg, ufx.baseURLKey, usrv.URL).Complete(t.Context(), canonicalRequest(ufx.model))
			if err != nil {
				t.Fatalf("Complete(): %v", err)
			}
			if len(assembled.Choices) != len(unary.Choices) {
				t.Fatalf("stream assembled %d choices, unary returned %d", len(assembled.Choices), len(unary.Choices))
			}
			for i := range unary.Choices {
				s, u := assembled.Choices[i], unary.Choices[i]
				if s.Index != u.Index || s.Message.Role != u.Message.Role || s.Message.Content != u.Message.Content ||
					s.Message.ReasoningContent != u.Message.ReasoningContent || len(s.Message.ToolCalls) != len(u.Message.ToolCalls) {
					t.Errorf("choice %d: stream assembled %+v, unary returned %+v", i, s, u)
				}
				if !slices.Contains(canonicalFinishReasons, s.FinishReason) {
					t.Errorf("choice %d assembled finish_reason = %q, not canonical", i, s.FinishReason)
				}
			}
			if !fx.noUsage && (assembled.Usage.PromptTokens != unary.Usage.PromptTokens || assembled.Usage.CompletionTokens != unary.Usage.CompletionTokens) {
				t.Errorf("usage: stream assembled %+v, unary returned %+v", assembled.Usage, unary.Usage)
			}
		})
	}
}

// TestProviderStreamToolCallsMatchUnary is TestProviderStreamMatchesUnary for a
// tool-calling answer, on the providers whose testdata records a native
// tool-call stream beside a unary one. The two recordings are separate calls,
// so call ids differ; the calls themselves — names, arguments, order — and the
// finish_reason must not.
func TestProviderStreamToolCallsMatchUnary(t *testing.T) {
	for _, id := range []string{providers.NameCohere} {
		t.Run(id, func(t *testing.T) {
			fx := streamFixtures()[id]
			srv := newNativeStub(loadTestdata(t, id, "chat.tools.stream.sse"))
			defer srv.Close()
			_, assembled := gatewayStream(t, startStream(t, id, fx, srv), fx.model, false)
			if assembled == nil || len(assembled.Choices) != 1 {
				t.Fatalf("assembled = %+v, want one completed choice", assembled)
			}

			usrv := newNativeStub(loadTestdata(t, id, "chat.tools.response.json"))
			defer usrv.Close()
			ufx := fixtures()[id]
			unary, err := buildProvider(t, id, ufx.extraCfg, ufx.baseURLKey, usrv.URL).Complete(t.Context(), canonicalRequest(ufx.model))
			if err != nil {
				t.Fatalf("Complete(): %v", err)
			}

			s, u := assembled.Choices[0], unary.Choices[0]
			if s.FinishReason != core.FinishReasonToolCalls || u.FinishReason != core.FinishReasonToolCalls {
				t.Errorf("finish_reason: stream %q, unary %q, want tool_calls on both", s.FinishReason, u.FinishReason)
			}
			if len(s.Message.ToolCalls) != len(u.Message.ToolCalls) {
				t.Fatalf("stream assembled %d tool calls, unary returned %d", len(s.Message.ToolCalls), len(u.Message.ToolCalls))
			}
			for i := range u.Message.ToolCalls {
				sc, uc := s.Message.ToolCalls[i], u.Message.ToolCalls[i]
				if sc.ID == "" || sc.Type != uc.Type || sc.Function.Name != uc.Function.Name || sc.Function.Arguments != uc.Function.Arguments {
					t.Errorf("tool call %d: stream assembled %+v, unary returned %+v", i, sc, uc)
				}
			}
		})
	}
}

// midStreamFailures maps a provider to its native stream cut by the failure its
// upstream reports once the 200 is out, in that vendor's own frame shape: the
// OpenAI error envelope in place of a chunk, Anthropic's error event (the body
// testdata/error.401.json records), Gemini's error envelope sent as a frame
// (likewise), and Cohere's message-end carrying an error. Every OpenAI-wire
// provider is covered by the envelope.
func midStreamFailures(t *testing.T) map[string]string {
	t.Helper()
	const openAIError = `{"error":{"message":"The server had an error while processing your request.","type":"server_error"}}`
	out := map[string]string{
		providers.NameAnthropic: "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"conformance\"}}\n\n" +
			"event: error\ndata: " + strings.TrimSpace(loadTestdata(t, providers.NameAnthropic, "error.401.json")) + "\n\n",
		providers.NameGemini: "data: {\"responseId\":\"g\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"conformance\"}]}}]}\n\n" +
			"data: " + compactJSON(t, loadTestdata(t, providers.NameGemini, "error.401.json")) + "\n\n",
		providers.NameCohere: "event: content-delta\ndata: {\"type\":\"content-delta\",\"index\":0,\"delta\":{\"message\":{\"content\":{\"text\":\"conformance\"}}}}\n\n" +
			"event: message-end\ndata: {\"type\":\"message-end\",\"delta\":{\"error\":\"internal error\",\"finish_reason\":\"ERROR\"}}\n\n",
	}
	for _, id := range openAIWireStreamProviders() {
		fx := streamFixtures()[id]
		out[id] = "data: {\"id\":\"c\",\"model\":\"" + fx.model + "\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"conformance\"},\"finish_reason\":null}]}\n\n" +
			"data: " + openAIError + "\n\n"
	}
	return out
}

func compactJSON(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// TestProviderMidStreamFailureConformance asserts that an upstream failing
// after its 200 reaches the client as the gateway's error event — after the
// content already sent and in place of [DONE] — and never as a short answer
// that ended cleanly: the after_request stage, and with it the response cache,
// never sees it.
func TestProviderMidStreamFailureConformance(t *testing.T) {
	for id, body := range midStreamFailures(t) {
		t.Run(id, func(t *testing.T) {
			fx := streamFixtures()[id]
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			frames, assembled := gatewayStream(t, startStream(t, id, fx, srv), fx.model, false)
			if assembled != nil {
				t.Errorf("the failed stream completed and reached the after_request stage as %+v", assembled)
			}
			if len(frames) == 0 {
				t.Fatal("no frames written")
			}
			last := frames[len(frames)-1]
			if last.done || last.chunk.Error == nil {
				t.Fatalf("last frame = %s, want the gateway's error event; frames = %v", last.raw, frames)
			}
			for _, f := range frames {
				if f.done {
					t.Errorf("[DONE] written for a stream that failed")
				}
			}
		})
	}
}

// nativeStreamWire lists the stream fixtures whose upstream streams its own
// event schema rather than OpenAI's chat.completion.chunk frames.
var nativeStreamWire = []string{
	providers.NameAnthropic,
	providers.NameGemini,
	providers.NameCohere,
	providers.NameReplicate,
}

// openAIWireStreamProviders returns every stream fixture whose upstream streams
// OpenAI's chunk frames, sorted for a stable subtest order.
func openAIWireStreamProviders() []string {
	var ids []string
	for id := range streamFixtures() {
		if !slices.Contains(nativeStreamWire, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// TestProviderStreamChoicesKeepTheirIndex drives an n=2 answer — two choices
// interleaved, opened together and finished out of order, then the usage-only
// frame — through every OpenAI-wire provider and the gateway's stream path. On
// the wire each choice gets the assistant role once and one finish_reason under
// its own index; the assembled response holds both choices whole, in index
// order.
func TestProviderStreamChoicesKeepTheirIndex(t *testing.T) {
	for _, id := range openAIWireStreamProviders() {
		t.Run(id, func(t *testing.T) {
			fx := streamFixtures()[id]
			frame := func(choices, usage string) string {
				f := `data: {"id":"chatcmpl-n2","object":"chat.completion.chunk","created":1,"model":"` + fx.model + `","choices":[` + choices + `]`
				if usage != "" {
					f += `,"usage":` + usage
				}
				return f + "}\n\n"
			}
			body := frame(`{"index":0,"delta":{"role":"assistant","content":"conformance"},"finish_reason":null},{"index":1,"delta":{"role":"assistant","content":"second"},"finish_reason":null}`, "") +
				frame(`{"index":1,"delta":{"content":" choice"},"finish_reason":"stop"}`, "") +
				frame(`{"index":0,"delta":{"content":" ok"},"finish_reason":"stop"}`, "") +
				frame(``, `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}`) +
				"data: [DONE]\n\n"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			frames, assembled := gatewayStream(t, startStream(t, id, fx, srv), fx.model, false)
			if len(frames) == 0 || !frames[len(frames)-1].done {
				t.Fatalf("the stream did not end with [DONE]: %v", frames)
			}
			roles, finishes := map[int]int{}, map[int]int{}
			content := map[int]string{}
			for _, f := range frames[:len(frames)-1] {
				for _, ch := range f.chunk.Choices {
					if ch.Delta.Role != "" {
						roles[ch.Index]++
					}
					if ch.FinishReason != nil && *ch.FinishReason != "" {
						finishes[ch.Index]++
					}
					content[ch.Index] += ch.Delta.Content
				}
			}
			want := map[int]string{0: wantContent, 1: "second choice"}
			for idx, text := range want {
				if roles[idx] != 1 || finishes[idx] != 1 || content[idx] != text {
					t.Errorf("choice %d on the wire: role %d time(s), finish_reason %d time(s), content %q; want once, once, %q",
						idx, roles[idx], finishes[idx], content[idx], text)
				}
			}
			if assembled == nil || len(assembled.Choices) != 2 {
				t.Fatalf("assembled = %+v, want two choices", assembled)
			}
			for i, c := range assembled.Choices {
				if c.Index != i || c.Message.Content != want[i] || c.FinishReason != core.FinishReasonStop {
					t.Errorf("assembled choice %d = %+v, want index %d with %q, finished", i, c, i, want[i])
				}
			}
		})
	}
}

// TestProviderStreamLogprobsMatchUnary asserts, on every OpenAI-wire provider,
// that a streamed answer's logprobs reach the client chunk by chunk and that the
// response the gateway assembles from them — the one the response cache stores
// and replays — carries the object the same provider's unary answer carries for
// the same tokens: each chunk's entries joined in order. The entries are in the
// shape OpenAI's chat completions return (openai-go's
// ChatCompletionChunkChoiceLogprobs and ChatCompletionChoiceLogprobs: content
// and refusal lists of token entries).
func TestProviderStreamLogprobsMatchUnary(t *testing.T) {
	const (
		first  = `{"token":"conformance","logprob":-0.1,"bytes":[99],"top_logprobs":[]}`
		second = `{"token":" ok","logprob":-0.2,"bytes":[32],"top_logprobs":[]}`
	)
	for _, id := range openAIWireStreamProviders() {
		t.Run(id, func(t *testing.T) {
			fx := streamFixtures()[id]
			usage := `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}`
			stream := `data: {"id":"c","model":"` + fx.model + `","choices":[{"index":0,"delta":{"role":"assistant","content":"conformance"},"logprobs":{"content":[` + first + `],"refusal":null},"finish_reason":null}]}` + "\n\n" +
				`data: {"id":"c","model":"` + fx.model + `","choices":[{"index":0,"delta":{"content":" ok"},"logprobs":{"content":[` + second + `],"refusal":null},"finish_reason":null}]}` + "\n\n" +
				`data: {"id":"c","model":"` + fx.model + `","choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}]}` + "\n\n" +
				`data: {"id":"c","model":"` + fx.model + `","choices":[],"usage":` + usage + `}` + "\n\n" +
				"data: [DONE]\n\n"
			unaryBody := `{"id":"c","model":"` + fx.model + `","choices":[{"index":0,"message":{"role":"assistant","content":"conformance ok"},` +
				`"logprobs":{"content":[` + first + `,` + second + `],"refusal":null},"finish_reason":"stop"}],"usage":` + usage + `}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(stream))
			}))
			defer srv.Close()

			frames, assembled := gatewayStream(t, startStream(t, id, fx, srv), fx.model, false)
			carried := 0
			for _, f := range frames {
				if strings.Contains(f.raw, `"logprobs":{`) {
					carried++
				}
			}
			if carried != 2 {
				t.Errorf("%d frames carried logprobs on the wire, want 2", carried)
			}
			if assembled == nil || len(assembled.Choices) != 1 {
				t.Fatalf("assembled = %+v, want one completed choice", assembled)
			}
			if assembled.Choices[0].Logprobs == nil {
				t.Fatal("the assembled choice carries no logprobs; the stream delivered them")
			}

			usrv := newNativeStub(unaryBody)
			defer usrv.Close()
			ufx := fixtures()[id]
			unary, err := buildProvider(t, id, ufx.extraCfg, ufx.baseURLKey, usrv.URL).Complete(t.Context(), canonicalRequest(ufx.model))
			if err != nil {
				t.Fatalf("Complete(): %v", err)
			}
			if len(unary.Choices) != 1 || unary.Choices[0].Logprobs == nil {
				t.Fatalf("unary choices = %+v, want one carrying logprobs", unary.Choices)
			}
			if got, want := compactJSON(t, string(assembled.Choices[0].Logprobs)), compactJSON(t, string(unary.Choices[0].Logprobs)); got != want {
				t.Errorf("assembled logprobs = %s, unary logprobs = %s", got, want)
			}
		})
	}
}
