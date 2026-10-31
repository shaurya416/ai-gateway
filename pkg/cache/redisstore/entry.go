package redisstore

import (
	"encoding/json"
	"fmt"

	"github.com/ferro-labs/ai-gateway/providers/core"
)

// entryVersion is the format of a stored entry. An entry carrying any other
// version is read as a miss, so a gateway that changes the format and one
// that has not yet can share a key prefix without serving each other's bytes.
const entryVersion = 1

// entry is what one cached response is stored as.
//
// It does not reuse the response's own JSON encoding, which is the client
// wire format and loses things a cache must keep: Message carries its fields
// through a custom codec that drops Content beside ContentParts, Usage folds
// reasoning tokens into the completion count on decode, Metadata is omitted
// when empty, and OverheadMs is not encoded at all. Every field is spelled out
// here instead, with no omitempty, so nil and empty stay distinct — a client
// is served "choices": null for one and [] for the other.
type entry struct {
	V        int           `json:"v"`
	Response entryResponse `json:"response"`
}

type entryResponse struct {
	ID         string         `json:"id"`
	Object     string         `json:"object"`
	Created    int64          `json:"created"`
	Model      string         `json:"model"`
	Provider   string         `json:"provider"`
	Choices    []entryChoice  `json:"choices"`
	Usage      entryUsage     `json:"usage"`
	Metadata   map[string]any `json:"metadata"`
	OverheadMs float64        `json:"overhead_ms"`
}

type entryChoice struct {
	Index        int          `json:"index"`
	Message      entryMessage `json:"message"`
	FinishReason string       `json:"finish_reason"`
	// Logprobs is the upstream's raw object, carried as bytes (base64 in the
	// JSON) so it comes back byte for byte rather than re-compacted.
	Logprobs []byte `json:"logprobs"`
}

type entryMessage struct {
	Role             string             `json:"role"`
	Content          string             `json:"content"`
	ContentParts     []core.ContentPart `json:"content_parts"`
	Name             string             `json:"name"`
	ToolCalls        []core.ToolCall    `json:"tool_calls"`
	ToolCallID       string             `json:"tool_call_id"`
	ReasoningContent string             `json:"reasoning_content"`
}

// entryUsage is Usage without its JSON methods, so its fields encode and
// decode exactly as they stand.
type entryUsage core.Usage

// encodeEntry serialises resp for storage.
func encodeEntry(resp *core.Response) ([]byte, error) {
	e := entry{V: entryVersion, Response: entryResponse{
		ID:         resp.ID,
		Object:     resp.Object,
		Created:    resp.Created,
		Model:      resp.Model,
		Provider:   resp.Provider,
		Usage:      entryUsage(resp.Usage),
		Metadata:   resp.Metadata,
		OverheadMs: resp.OverheadMs,
	}}
	if resp.Choices != nil {
		e.Response.Choices = make([]entryChoice, len(resp.Choices))
		for i, c := range resp.Choices {
			e.Response.Choices[i] = entryChoice{
				Index:        c.Index,
				Message:      fromMessage(c.Message),
				FinishReason: c.FinishReason,
				Logprobs:     []byte(c.Logprobs),
			}
		}
	}
	return json.Marshal(e)
}

// decodeEntry reads an entry encodeEntry wrote.
func decodeEntry(b []byte) (*core.Response, error) {
	var e entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("entry is not valid JSON: %w", err)
	}
	if e.V != entryVersion {
		return nil, fmt.Errorf("entry has format version %d, this gateway reads %d", e.V, entryVersion)
	}
	r := e.Response
	resp := &core.Response{
		ID:         r.ID,
		Object:     r.Object,
		Created:    r.Created,
		Model:      r.Model,
		Provider:   r.Provider,
		Usage:      core.Usage(r.Usage),
		Metadata:   r.Metadata,
		OverheadMs: r.OverheadMs,
	}
	if r.Choices != nil {
		resp.Choices = make([]core.Choice, len(r.Choices))
		for i, c := range r.Choices {
			resp.Choices[i] = core.Choice{
				Index:        c.Index,
				Message:      c.Message.toMessage(),
				FinishReason: c.FinishReason,
				Logprobs:     core.Logprobs(c.Logprobs),
			}
		}
	}
	return resp, nil
}

func fromMessage(m core.Message) entryMessage {
	return entryMessage{
		Role:             m.Role,
		Content:          m.Content,
		ContentParts:     m.ContentParts,
		Name:             m.Name,
		ToolCalls:        m.ToolCalls,
		ToolCallID:       m.ToolCallID,
		ReasoningContent: m.ReasoningContent,
	}
}

func (m entryMessage) toMessage() core.Message {
	return core.Message{
		Role:             m.Role,
		Content:          m.Content,
		ContentParts:     m.ContentParts,
		Name:             m.Name,
		ToolCalls:        m.ToolCalls,
		ToolCallID:       m.ToolCallID,
		ReasoningContent: m.ReasoningContent,
	}
}
