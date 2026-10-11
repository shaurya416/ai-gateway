package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"
)

// maxEventBytes bounds one server-sent event. A chat completion chunk is a few
// hundred bytes; the bound only stops a broken stream from growing one event
// without limit.
const maxEventBytes = 8 << 20

// sseDone is the data of the event that ends an OpenAI-compatible stream.
const sseDone = "[DONE]"

// ErrStreamTruncated is reported when a stream's connection ends before the
// gateway sent the [DONE] event that closes every complete stream: the
// response is incomplete even though no error event arrived.
var ErrStreamTruncated = errors.New("client: stream ended before [DONE]")

// ChatCompletionStream reads a streamed chat completion, one chunk at a time.
//
// Use it like bufio.Scanner — Next, Current, then Err once Next returns
// false — or range over Chunks. Either way, defer Close: it releases the
// connection when the stream is abandoned before its end. The stream starts no
// goroutines; cancelling the context the stream was opened with ends it, and
// Err then reports the context's error.
//
// A ChatCompletionStream is not safe for concurrent use.
type ChatCompletionStream struct {
	meta    ResponseMeta
	ctx     context.Context
	cancel  context.CancelCauseFunc
	body    io.ReadCloser
	scanner *bufio.Scanner

	current ChatCompletionChunk
	err     error
	done    bool
}

// ChatCompletionStream sends a streaming chat completion request and returns
// the open stream once the gateway has answered with its headers. A gateway
// that refuses the request — a model nothing serves, a rate limit, a plugin
// denial — answers before any chunk, and that answer is returned as an
// *APIError with no stream.
//
// The routing attribution is known when the stream opens: the gateway chooses
// its target before the first chunk is written, so Meta is complete before
// the first call to Next.
func (c *Client) ChatCompletionStream(ctx context.Context, req ChatCompletionRequest, opts ...RequestOption) (*ChatCompletionStream, error) {
	req.Stream = true
	streamCtx, cancel := context.WithCancelCause(ctx)

	var openTimer *time.Timer
	if c.timeout > 0 {
		timeout := c.timeout
		openTimer = time.AfterFunc(timeout, func() {
			cancel(fmt.Errorf("stream did not open within %s: %w", timeout, context.DeadlineExceeded))
		})
	}
	stopOpenTimer := func() bool {
		return openTimer == nil || openTimer.Stop()
	}

	httpReq, err := c.newRequest(streamCtx, http.MethodPost, pathChatCompletions, &req, "text/event-stream", opts)
	if err != nil {
		stopOpenTimer()
		cancel(nil)
		return nil, err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		stopOpenTimer()
		err = transportError(streamCtx, err)
		cancel(nil)
		return nil, err
	}
	if !stopOpenTimer() {
		// The open deadline fired after the headers arrived but before it
		// could be disarmed. The context is already cancelled, so the body is
		// unreadable; report the deadline rather than hand back a dead stream.
		_ = resp.Body.Close()
		err := fmt.Errorf("client: %w", context.Cause(streamCtx))
		cancel(nil)
		return nil, err
	}

	meta := metaFrom(resp)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := newAPIError(resp, meta)
		drainAndClose(resp.Body)
		cancel(nil)
		return nil, apiErr
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		if mediaType, _, _ := mime.ParseMediaType(ct); mediaType != "text/event-stream" {
			drainAndClose(resp.Body)
			cancel(nil)
			return nil, fmt.Errorf("client: expected an event stream, got Content-Type %q", ct)
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 4096), maxEventBytes+1)
	scanner.Split((&lineSplitter{}).split)
	return &ChatCompletionStream{
		meta:    meta,
		ctx:     streamCtx,
		cancel:  cancel,
		body:    resp.Body,
		scanner: scanner,
	}, nil
}

// Meta describes the response the stream was opened with, including the
// routing attribution of the target serving it.
func (s *ChatCompletionStream) Meta() ResponseMeta { return s.meta }

// Next advances to the next chunk and reports whether there is one. It returns
// false at the end of the stream, on an error, and after Close.
func (s *ChatCompletionStream) Next() bool {
	if s.done {
		return false
	}
	for {
		event, data, err := s.readEvent()
		if err != nil {
			s.finish(err)
			return false
		}
		if len(data) == 0 {
			continue
		}
		if event == "error" {
			s.finish(s.streamError(data))
			return false
		}
		if string(data) == sseDone {
			s.finish(nil)
			return false
		}
		var frame struct {
			ChatCompletionChunk
			Error *errorBody `json:"error"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			s.finish(fmt.Errorf("client: decode stream chunk: %w", err))
			return false
		}
		if frame.Error != nil {
			s.finish(s.streamErrorFrom(frame.Error))
			return false
		}
		s.current = frame.ChatCompletionChunk
		return true
	}
}

// Current returns the chunk the last successful call to Next read.
func (s *ChatCompletionStream) Current() ChatCompletionChunk { return s.current }

// Err returns the error that ended the stream: nil after [DONE] or Close, a
// *StreamError when the gateway reported a failure inside the stream, the
// context's error when it was cancelled, and ErrStreamTruncated when the
// connection ended early.
func (s *ChatCompletionStream) Err() error { return s.err }

// Close releases the stream. It is safe to call more than once, and after the
// stream has ended.
func (s *ChatCompletionStream) Close() error {
	s.done = true
	s.release()
	return nil
}

// Chunks ranges over the stream's chunks. A failure is yielded once, as the
// last pair, with a zero chunk:
//
//	for chunk, err := range stream.Chunks() {
//		if err != nil { return err }
//		...
//	}
//
// Breaking out of the loop leaves the stream open; Close still releases it.
func (s *ChatCompletionStream) Chunks() iter.Seq2[ChatCompletionChunk, error] {
	return func(yield func(ChatCompletionChunk, error) bool) {
		for s.Next() {
			if !yield(s.current, nil) {
				return
			}
		}
		if s.err != nil {
			yield(ChatCompletionChunk{}, s.err)
		}
	}
}

// finish ends the stream with err, which is nil for a clean end.
func (s *ChatCompletionStream) finish(err error) {
	s.done = true
	s.err = err
	s.release()
}

func (s *ChatCompletionStream) release() {
	if s.body != nil {
		_ = s.body.Close()
		s.body = nil
	}
	if s.cancel != nil {
		s.cancel(nil)
		s.cancel = nil
	}
}

// readEvent reads one server-sent event and returns its type and data, per the
// HTML event-stream format: lines end in LF, CRLF or a lone CR; a blank line
// dispatches the event; a line starting with a colon is a comment; several
// data lines join with LF; a single space after the field's colon is dropped;
// id, retry and unknown fields are ignored. An event the connection cuts off
// before its blank line is not dispatched.
func (s *ChatCompletionStream) readEvent() (string, []byte, error) {
	var (
		data    []byte
		hasData bool
		event   string
	)
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(line) == 0 {
			if !hasData {
				event = ""
				continue
			}
			return event, data, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		}
		switch string(field) {
		case "data":
			if hasData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			hasData = true
			if len(data) > maxEventBytes {
				return "", nil, fmt.Errorf("client: stream event exceeds %d bytes", maxEventBytes)
			}
		case "event":
			event = string(value)
		}
	}
	if cause := context.Cause(s.ctx); cause != nil {
		return "", nil, fmt.Errorf("client: %w", cause)
	}
	if err := s.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return "", nil, fmt.Errorf("client: stream line exceeds %d bytes", maxEventBytes)
		}
		return "", nil, fmt.Errorf("client: read stream: %w", err)
	}
	return "", nil, ErrStreamTruncated
}

func (s *ChatCompletionStream) streamError(data []byte) error {
	var envelope errorEnvelope
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Error != nil {
		return s.streamErrorFrom(envelope.Error)
	}
	var body errorBody
	if err := json.Unmarshal(data, &body); err == nil && body.Message != "" {
		return s.streamErrorFrom(&body)
	}
	return &StreamError{Message: strings.TrimSpace(string(data)), Meta: s.meta}
}

func (s *ChatCompletionStream) streamErrorFrom(b *errorBody) error {
	return &StreamError{Type: b.Type, Code: b.code(), Message: b.Message, Meta: s.meta}
}

// lineSplitter is a bufio.SplitFunc for event-stream lines, which may end in
// LF, CRLF or a lone CR. A CR at the end of the buffered input ends its line
// at once — waiting to see whether LF follows would hold back an event until
// the next byte arrived — and the LF, if it does follow, is skipped.
type lineSplitter struct {
	skipLF bool
}

func (l *lineSplitter) split(data []byte, atEOF bool) (int, []byte, error) {
	if l.skipLF && len(data) > 0 {
		l.skipLF = false
		if data[0] == '\n' {
			return 1, nil, nil
		}
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\r' {
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
			} else {
				l.skipLF = true
			}
		}
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// ChatCompletionAccumulator assembles a streamed chat completion into the
// ChatCompletion the same request would have returned unstreamed: content and
// reasoning text joined per choice, tool-call fragments joined by index, and
// the finish reason and usage of the stream.
type ChatCompletionAccumulator struct {
	id       string
	model    string
	created  int64
	usage    *Usage
	choices  map[int]*choiceState
	sawChunk bool
}

type choiceState struct {
	role         string
	content      strings.Builder
	reasoning    strings.Builder
	toolCalls    []ToolCall
	toolIndex    map[int]int // stream tool-call index → position in toolCalls
	finishReason string
}

// Add folds one chunk into the result.
func (a *ChatCompletionAccumulator) Add(chunk ChatCompletionChunk) {
	a.sawChunk = true
	if a.id == "" {
		a.id = chunk.ID
	}
	if a.model == "" {
		a.model = chunk.Model
	}
	if a.created == 0 {
		a.created = chunk.Created
	}
	if chunk.Usage != nil {
		usage := *chunk.Usage
		a.usage = &usage
	}
	if a.choices == nil {
		a.choices = make(map[int]*choiceState)
	}
	for _, choice := range chunk.Choices {
		st := a.choices[choice.Index]
		if st == nil {
			st = &choiceState{toolIndex: make(map[int]int)}
			a.choices[choice.Index] = st
		}
		if choice.Delta.Role != "" {
			st.role = choice.Delta.Role
		}
		st.content.WriteString(choice.Delta.Content)
		st.reasoning.WriteString(choice.Delta.ReasoningContent)
		for _, tc := range choice.Delta.ToolCalls {
			st.addToolCall(tc)
		}
		if choice.FinishReason != "" {
			st.finishReason = choice.FinishReason
		}
	}
}

func (st *choiceState) addToolCall(tc ToolCall) {
	pos := -1
	if tc.Index != nil {
		if p, ok := st.toolIndex[*tc.Index]; ok {
			pos = p
		}
	} else if tc.ID == "" && len(st.toolCalls) > 0 {
		// An unindexed fragment with no id continues the latest call.
		pos = len(st.toolCalls) - 1
	}
	if pos < 0 {
		st.toolCalls = append(st.toolCalls, ToolCall{})
		pos = len(st.toolCalls) - 1
		if tc.Index != nil {
			st.toolIndex[*tc.Index] = pos
		}
	}
	call := &st.toolCalls[pos]
	if tc.ID != "" {
		call.ID = tc.ID
	}
	if tc.Type != "" {
		call.Type = tc.Type
	}
	if call.Function.Name == "" {
		call.Function.Name = tc.Function.Name
	}
	call.Function.Arguments += tc.Function.Arguments
}

// ChatCompletion returns the completion assembled so far, choices in index
// order. It is nil when no chunk was added.
func (a *ChatCompletionAccumulator) ChatCompletion() *ChatCompletion {
	if !a.sawChunk {
		return nil
	}
	out := &ChatCompletion{
		ID:      a.id,
		Object:  "chat.completion",
		Created: a.created,
		Model:   a.model,
		Choices: make([]Choice, 0, len(a.choices)),
	}
	if a.usage != nil {
		out.Usage = *a.usage
	}
	indexes := make([]int, 0, len(a.choices))
	for i := range a.choices {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		st := a.choices[i]
		role := st.role
		if role == "" {
			role = RoleAssistant
		}
		msg := Message{
			Role:             role,
			Content:          st.content.String(),
			ReasoningContent: st.reasoning.String(),
		}
		if len(st.toolCalls) > 0 {
			msg.ToolCalls = append([]ToolCall(nil), st.toolCalls...)
		}
		out.Choices = append(out.Choices, Choice{Index: i, Message: msg, FinishReason: st.finishReason})
	}
	return out
}
