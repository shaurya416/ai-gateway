package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"go.uber.org/goleak"
)

// sseHeaders sets the headers internal/sse.Write sets on a gateway stream.
func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

// gatewayChunk renders one chunk the way internal/sse.writeChunk does:
// "data: " + the JSON encoder's output (which ends in a newline) + "\n".
func gatewayChunk(t *testing.T, chunk any) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("data: ")
	if err := json.NewEncoder(&b).Encode(chunk); err != nil {
		t.Fatal(err)
	}
	b.WriteString("\n")
	return b.String()
}

func contentChunk(id, content string) map[string]any {
	return map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": 1700000000, "model": "gpt-4o-mini",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content}}},
	}
}

func streamRequest() ChatCompletionRequest {
	return ChatCompletionRequest{Model: "gpt-4o-mini", Messages: []Message{UserMessage("hi")}}
}

func collect(t *testing.T, s *ChatCompletionStream) (string, []ChatCompletionChunk) {
	t.Helper()
	var text strings.Builder
	var chunks []ChatCompletionChunk
	for s.Next() {
		c := s.Current()
		chunks = append(chunks, c)
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	return text.String(), chunks
}

func TestStreamReadsGatewayFrames(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream = %v, want true", body["stream"])
		}
		if opts, _ := body["stream_options"].(map[string]any); opts["include_usage"] != true {
			t.Errorf("stream_options = %v", body["stream_options"])
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		setAttribution(w.Header(), "anthropic", "claude", "claude-sonnet-4-5", 1)
		sseHeaders(w)
		first := contentChunk("c1", "Hello")
		first["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "Hello"}}}
		_, _ = io.WriteString(w, gatewayChunk(t, first))
		_, _ = io.WriteString(w, gatewayChunk(t, contentChunk("c1", ", world")))
		_, _ = io.WriteString(w, gatewayChunk(t, map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "created": 1700000000, "model": "gpt-4o-mini",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		}))
		_, _ = io.WriteString(w, gatewayChunk(t, map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "created": 1700000000, "model": "gpt-4o-mini",
			"choices": []any{}, "usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
		}))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	s, err := c.ChatCompletionStream(t.Context(), ChatCompletionRequest{
		Model: "gpt-4o-mini", Messages: []Message{UserMessage("hi")},
		StreamOptions: &StreamOptions{IncludeUsage: true},
	})
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Attribution is known before the first chunk is read.
	want := Attribution{Provider: "anthropic", Target: "claude", Model: "claude-sonnet-4-5", Attempts: 1}
	if s.Meta().Attribution != want {
		t.Errorf("attribution = %+v, want %+v", s.Meta().Attribution, want)
	}

	var acc ChatCompletionAccumulator
	var text strings.Builder
	n := 0
	for s.Next() {
		chunk := s.Current()
		acc.Add(chunk)
		n++
		for _, ch := range chunk.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if n != 4 || text.String() != "Hello, world" {
		t.Errorf("read %d chunks, text %q", n, text.String())
	}
	if s.Next() {
		t.Error("Next after [DONE] returned true")
	}
	got := acc.ChatCompletion()
	if got == nil || len(got.Choices) != 1 {
		t.Fatalf("accumulated = %+v", got)
	}
	if m := got.Choices[0].Message; m.Role != RoleAssistant || m.Content != "Hello, world" || got.Choices[0].FinishReason != "stop" {
		t.Errorf("accumulated choice = %+v", got.Choices[0])
	}
	if got.Usage.TotalTokens != 8 || got.ID != "c1" || got.Object != "chat.completion" {
		t.Errorf("accumulated = %+v", got)
	}
}

// TestStreamEventGrammar feeds the reader every shape the event-stream format
// allows, under each of its three line terminators.
func TestStreamEventGrammar(t *testing.T) {
	events := []string{
		": keep-alive comment",
		"",
		"retry: 1000",
		"id: 7",
		"data:" + `{"id":"a","choices":[{"index":0,"delta":{"content":"one"}}]}`, // no space after the colon
		"",
		"",
		"event: message",
		`data: {"id":"b",`, // one event split over two data lines, joined by LF
		`data:  "choices":[{"index":0,"delta":{"content":"two"}}]}`,
		"",
		"data", // a field with no colon is a field with an empty value
		"",
		"unknown: ignored",
		`data: {"id":"c","choices":[{"index":0,"delta":{"content":"three"}}]}`,
		"",
		"data: [DONE]",
		"",
	}
	for name, eol := range map[string]string{"LF": "\n", "CRLF": "\r\n", "CR": "\r"} {
		t.Run(name, func(t *testing.T) {
			body := strings.Join(events, eol) + eol
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				sseHeaders(w)
				_, _ = io.WriteString(w, body)
			})
			s, err := c.ChatCompletionStream(t.Context(), streamRequest())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			text, chunks := collect(t, s)
			if err := s.Err(); err != nil {
				t.Fatalf("Err: %v", err)
			}
			if text != "onetwothree" || len(chunks) != 3 || chunks[1].ID != "b" {
				t.Errorf("text %q from %d chunks", text, len(chunks))
			}
		})
	}
}

// TestLineSplitterByteAtATime delivers a CRLF stream one byte per read, so
// every CR arrives with nothing after it in the buffer: the CR must end its
// line immediately and the LF that follows must not produce a blank line.
func TestLineSplitterByteAtATime(t *testing.T) {
	input := "data: a\r\n\r\ndata: b\rdata: c\r\r\n\n"
	sc := bufio.NewScanner(iotest.OneByteReader(strings.NewReader(input)))
	sc.Split((&lineSplitter{}).split)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"data: a", "", "data: b", "data: c", "", ""}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}

func TestStreamErrorEvent(t *testing.T) {
	tests := map[string]string{
		// What internal/sse.Write sends when the upstream fails mid-stream.
		"gateway error frame": `data: {"error":{"message":"upstream reset","type":"stream_error","code":"stream_error"}}` + "\n\n",
		// A named error event, as some OpenAI-compatible servers send.
		"named error event": "event: error\ndata: " + `{"error":{"message":"upstream reset","type":"stream_error","code":"stream_error"}}` + "\n\n",
	}
	for name, failure := range tests {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				setAttribution(w.Header(), "openai", "primary", "gpt-4o", 1)
				sseHeaders(w)
				_, _ = io.WriteString(w, gatewayChunk(t, contentChunk("c1", "partial")))
				_, _ = io.WriteString(w, failure)
			})
			s, err := c.ChatCompletionStream(t.Context(), streamRequest())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			text, _ := collect(t, s)
			if text != "partial" {
				t.Errorf("text = %q", text)
			}
			var streamErr *StreamError
			if !errors.As(s.Err(), &streamErr) {
				t.Fatalf("Err = %v, want *StreamError", s.Err())
			}
			if streamErr.Code != "stream_error" || streamErr.Type != "stream_error" || streamErr.Message != "upstream reset" {
				t.Errorf("stream error = %+v", streamErr)
			}
			if streamErr.Meta.Attribution.Target != "primary" {
				t.Errorf("attribution = %+v", streamErr.Meta.Attribution)
			}
		})
	}
}

func TestStreamIdleTimeoutFrame(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		_, _ = io.WriteString(w, `data: {"error":{"message":"stream timed out waiting for next chunk","type":"timeout_error","code":"stream_timeout"}}`+"\n\n")
	})
	s, err := c.ChatCompletionStream(t.Context(), streamRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for s.Next() {
		t.Error("a timeout frame was read as a chunk")
	}
	var streamErr *StreamError
	if !errors.As(s.Err(), &streamErr) || streamErr.Code != "stream_timeout" {
		t.Fatalf("Err = %v, want a stream_timeout *StreamError", s.Err())
	}
}

func TestStreamTruncated(t *testing.T) {
	tests := map[string]string{
		"no [DONE]": gatewayChunk(t, contentChunk("c1", "a")),
		// An event the connection cuts off before its blank line is never
		// dispatched — even one that would have been [DONE].
		"unterminated [DONE]": gatewayChunk(t, contentChunk("c1", "a")) + "data: [DONE]",
		"partial chunk":       gatewayChunk(t, contentChunk("c1", "a")) + `data: {"id":"c1","choi`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				sseHeaders(w)
				_, _ = io.WriteString(w, body)
			})
			s, err := c.ChatCompletionStream(t.Context(), streamRequest())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if text, _ := collect(t, s); text != "a" {
				t.Errorf("text = %q", text)
			}
			if !errors.Is(s.Err(), ErrStreamTruncated) {
				t.Fatalf("Err = %v, want ErrStreamTruncated", s.Err())
			}
		})
	}
}

func TestStreamMalformedChunk(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		_, _ = io.WriteString(w, "data: {not json}\n\n")
	})
	s, err := c.ChatCompletionStream(t.Context(), streamRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Next() {
		t.Fatal("a malformed chunk was returned")
	}
	if s.Err() == nil || !strings.Contains(s.Err().Error(), "decode stream chunk") {
		t.Fatalf("Err = %v", s.Err())
	}
}

func TestStreamOpenFailures(t *testing.T) {
	t.Run("gateway refusal", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			setAttribution(w.Header(), "openai", "primary", "gpt-4o", 3)
			w.Header().Set("Retry-After", "2")
			writeGatewayError(w, http.StatusServiceUnavailable, "the upstream provider is temporarily unavailable; retry shortly", "upstream_error", "upstream_unavailable")
		})
		s, err := c.ChatCompletionStream(t.Context(), streamRequest())
		if s != nil {
			t.Fatal("a refused stream was returned")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "upstream_unavailable" || apiErr.RetryAfter != 2*time.Second || apiErr.Meta.Attribution.Attempts != 3 {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not an event stream", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"x"}`)
		})
		if _, err := c.ChatCompletionStream(t.Context(), streamRequest()); err == nil || !strings.Contains(err.Error(), "event stream") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("invalid metadata sends nothing", func(t *testing.T) {
		c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
			t.Error("a request was sent")
		})
		_, err := c.ChatCompletionStream(t.Context(), streamRequest(), WithRoutingMetadata(map[string]any{"k": []int{1}}))
		if !errors.Is(err, ErrInvalidRoutingMetadata) {
			t.Fatalf("err = %v", err)
		}
	})
}

// blockingStream opens a stream whose server sends one chunk and then holds
// the connection open until the client goes away, reporting that on gone.
func blockingStream(t *testing.T, opts ...Option) (srv *httptest.Server, transport *http.Transport, gone <-chan struct{}, c *Client) {
	t.Helper()
	done := make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		sseHeaders(w)
		_, _ = io.WriteString(w, gatewayChunk(t, contentChunk("c1", "first")))
		w.(http.Flusher).Flush()
		awaitDisconnect(r)
	}))
	transport = &http.Transport{}
	opts = append([]Option{WithHTTPClient(&http.Client{Transport: transport})}, opts...)
	c, err := New(srv.URL, "sk-test", opts...)
	if err != nil {
		t.Fatal(err)
	}
	return srv, transport, done, c
}

// TestStreamCancellationEndsStreamWithoutLeaks cancels the context a stream was
// opened with while the reader is blocked waiting for the next chunk. The read
// must return with the context's error, the server must see the client go
// away, and no goroutine may outlive the stream.
func TestStreamCancellationEndsStreamWithoutLeaks(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	srv, transport, gone, c := blockingStream(t)
	defer srv.Close()
	defer transport.CloseIdleConnections()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := c.ChatCompletionStream(ctx, streamRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Next() || s.Current().Choices[0].Delta.Content != "first" {
		t.Fatalf("first chunk not read: %v", s.Err())
	}

	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	if s.Next() {
		t.Fatal("Next returned a chunk after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancellation took %s to end the read", elapsed)
	}
	if !errors.Is(s.Err(), context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", s.Err())
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the client disconnect")
	}
	_ = s.Close()
}

// TestStreamCloseReleasesConnection abandons a stream mid-way with Close: the
// connection is released, Next stops, and Err stays nil because the caller
// chose to stop.
func TestStreamCloseReleasesConnection(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	srv, transport, gone, c := blockingStream(t)
	defer srv.Close()
	defer transport.CloseIdleConnections()

	s, err := c.ChatCompletionStream(t.Context(), streamRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Next() {
		t.Fatalf("first chunk not read: %v", s.Err())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if s.Next() {
		t.Fatal("Next after Close returned a chunk")
	}
	if s.Err() != nil {
		t.Fatalf("Err after Close = %v, want nil", s.Err())
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not release the connection")
	}
}

// TestStreamTimeoutBoundsOnlyTheOpen checks WithTimeout's contract on a
// stream: it refuses a gateway that never answers, and it does not cut off an
// open stream that runs longer than the timeout.
func TestStreamTimeoutBoundsOnlyTheOpen(t *testing.T) {
	const timeout = 100 * time.Millisecond

	t.Run("headers never arrive", func(t *testing.T) {
		c := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
			awaitDisconnect(r)
		}, WithTimeout(timeout))
		start := time.Now()
		s, err := c.ChatCompletionStream(t.Context(), streamRequest())
		if s != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stream %v, err %v; want context.DeadlineExceeded", s, err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("open timeout took %s", elapsed)
		}
	})

	t.Run("open stream outlives it", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			sseHeaders(w)
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			time.Sleep(3 * timeout)
			_, _ = io.WriteString(w, gatewayChunk(t, contentChunk("c1", "late")))
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}, WithTimeout(timeout))
		s, err := c.ChatCompletionStream(t.Context(), streamRequest())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if text, _ := collect(t, s); text != "late" || s.Err() != nil {
			t.Fatalf("text %q, err %v", text, s.Err())
		}
	})
}

func TestStreamChunksIterator(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		_, _ = io.WriteString(w, gatewayChunk(t, contentChunk("c1", "a")))
		_, _ = io.WriteString(w, gatewayChunk(t, contentChunk("c1", "b")))
		_, _ = io.WriteString(w, `data: {"error":{"message":"boom","type":"stream_error","code":"stream_error"}}`+"\n\n")
	})
	s, err := c.ChatCompletionStream(t.Context(), streamRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	var text strings.Builder
	var last error
	for chunk, err := range s.Chunks() {
		if err != nil {
			last = err
			break
		}
		text.WriteString(chunk.Choices[0].Delta.Content)
	}
	if text.String() != "ab" {
		t.Errorf("text = %q", text.String())
	}
	var streamErr *StreamError
	if !errors.As(last, &streamErr) || streamErr.Message != "boom" {
		t.Fatalf("iterator error = %v", last)
	}
}

func TestAccumulatorJoinsToolCallsAndChoices(t *testing.T) {
	idx := func(i int) *int { return &i }
	var acc ChatCompletionAccumulator
	if acc.ChatCompletion() != nil {
		t.Fatal("an empty accumulator produced a completion")
	}
	acc.Add(ChatCompletionChunk{ID: "c", Model: "m", Created: 1, Choices: []ChunkChoice{
		{Index: 1, Delta: Delta{Role: RoleAssistant, Content: "second "}},
		{Index: 0, Delta: Delta{Role: RoleAssistant, ToolCalls: []ToolCall{
			{Index: idx(0), ID: "call_a", Type: "function", Function: FunctionCall{Name: "lookup", Arguments: `{"q":`}},
		}}},
	}})
	acc.Add(ChatCompletionChunk{Choices: []ChunkChoice{
		{Index: 0, Delta: Delta{ToolCalls: []ToolCall{
			{Index: idx(1), ID: "call_b", Type: "function", Function: FunctionCall{Name: "time", Arguments: `{}`}},
			{Index: idx(0), Function: FunctionCall{Arguments: `"go"}`}},
		}}},
		{Index: 1, Delta: Delta{Content: "choice", ReasoningContent: "hmm"}, FinishReason: "stop"},
	}})
	acc.Add(ChatCompletionChunk{Choices: []ChunkChoice{{Index: 0, FinishReason: "tool_calls"}}})
	acc.Add(ChatCompletionChunk{Choices: []ChunkChoice{}, Usage: &Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}})

	got := acc.ChatCompletion()
	if got.ID != "c" || got.Model != "m" || got.Created != 1 || got.Usage.TotalTokens != 3 || len(got.Choices) != 2 {
		t.Fatalf("completion = %+v", got)
	}
	first := got.Choices[0]
	if first.Index != 0 || first.FinishReason != "tool_calls" || len(first.Message.ToolCalls) != 2 {
		t.Fatalf("choice 0 = %+v", first)
	}
	if call := first.Message.ToolCalls[0]; call.ID != "call_a" || call.Function.Name != "lookup" || call.Function.Arguments != `{"q":"go"}` || call.Index != nil {
		t.Errorf("tool call 0 = %+v", call)
	}
	if call := first.Message.ToolCalls[1]; call.ID != "call_b" || call.Function.Arguments != `{}` {
		t.Errorf("tool call 1 = %+v", call)
	}
	second := got.Choices[1]
	if second.Index != 1 || second.Message.Content != "second choice" || second.Message.ReasoningContent != "hmm" || second.FinishReason != "stop" {
		t.Errorf("choice 1 = %+v", second)
	}
}
