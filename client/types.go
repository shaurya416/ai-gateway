package client

import (
	"encoding/json"
	"strings"
)

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Ptr returns a pointer to v, for the optional numeric and boolean request
// fields: Temperature: client.Ptr(0.2).
func Ptr[T any](v T) *T { return &v }

// ChatCompletionRequest is the body of POST /v1/chat/completions.
type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`

	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	N           *int     `json:"n,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`

	// MaxTokens and MaxCompletionTokens are both completion ceilings. When both
	// are set the gateway uses MaxCompletionTokens, as the OpenAI API does.
	MaxTokens           *int `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int `json:"max_completion_tokens,omitempty"`

	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Stop             []string `json:"stop,omitempty"`

	Tools []Tool `json:"tools,omitempty"`
	// ToolChoice is "none", "auto", "required", or an object naming a function:
	// map[string]any{"type": "function", "function": map[string]any{"name": "f"}}.
	ToolChoice        any             `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ResponseFormat    *ResponseFormat `json:"response_format,omitempty"`

	LogProbs    bool `json:"logprobs,omitempty"`
	TopLogProbs *int `json:"top_logprobs,omitempty"`

	// Stream is set by the client: ChatCompletion sends false and
	// ChatCompletionStream sends true, whatever it holds here.
	Stream bool `json:"stream,omitempty"`
	// StreamOptions applies to ChatCompletionStream only. IncludeUsage asks for
	// a final chunk carrying the token usage of the whole stream.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`

	// User is the end-user identifier. The gateway records it, and load
	// balancing and A/B tests may hash on it to keep a user on one target.
	User      string             `json:"user,omitempty"`
	LogitBias map[string]float64 `json:"logit_bias,omitempty"`
}

// StreamOptions configures a streamed chat completion.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ResponseFormat constrains the shape of the model's output.
type ResponseFormat struct {
	Type       string          `json:"type"` // "text" | "json_object" | "json_schema"
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

// Tool is a function the model may call.
type Tool struct {
	Type     string   `json:"type"` // "function"
	Function Function `json:"function"`
}

// Function describes a callable function. Parameters is its JSON Schema.
type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

// ToolCall is one function call issued by the model. On a streamed delta,
// Index names which call a fragment belongs to and Arguments arrives in pieces.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names a function and its JSON-encoded arguments.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ContentPart is one element of a multipart message: text or an image.
type ContentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is the image of an image_url content part: an https URL or a data:
// URI.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "auto" | "low" | "high"
}

// Message is one message of a conversation.
//
// Content is sent as a JSON string unless ContentParts is set, in which case
// the parts are sent as the content array and Content is ignored. An assistant
// message carrying only ToolCalls sends a null content. On a response, Content
// holds the text whichever shape the gateway answered with, and ContentParts
// holds the parts when it answered with an array.
type Message struct {
	Role             string
	Content          string
	ContentParts     []ContentPart
	Name             string
	ToolCalls        []ToolCall
	ToolCallID       string
	ReasoningContent string
}

type messageWire struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Name             string          `json:"name,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

// MarshalJSON writes the OpenAI message shape.
func (m Message) MarshalJSON() ([]byte, error) {
	w := messageWire{
		Role:             m.Role,
		Name:             m.Name,
		ToolCalls:        m.ToolCalls,
		ToolCallID:       m.ToolCallID,
		ReasoningContent: m.ReasoningContent,
	}
	var err error
	switch {
	case len(m.ContentParts) > 0:
		w.Content, err = json.Marshal(m.ContentParts)
	case m.Content == "" && len(m.ToolCalls) > 0:
		w.Content = json.RawMessage("null")
	default:
		w.Content, err = json.Marshal(m.Content)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads a message whose content is a string, an array of parts,
// or null.
func (m *Message) UnmarshalJSON(data []byte) error {
	var w struct {
		messageWire
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*m = Message{
		Role:             w.Role,
		Name:             w.Name,
		ToolCalls:        w.ToolCalls,
		ToolCallID:       w.ToolCallID,
		ReasoningContent: w.ReasoningContent,
	}
	if m.ReasoningContent == "" {
		m.ReasoningContent = w.Reasoning
	}
	if len(w.Content) == 0 || string(w.Content) == "null" {
		return nil
	}
	if w.Content[0] == '"' {
		return json.Unmarshal(w.Content, &m.Content)
	}
	if err := json.Unmarshal(w.Content, &m.ContentParts); err != nil {
		return err
	}
	var text strings.Builder
	for _, p := range m.ContentParts {
		if p.Type == "text" {
			text.WriteString(p.Text)
		}
	}
	m.Content = text.String()
	return nil
}

// SystemMessage returns a system message carrying text.
func SystemMessage(text string) Message { return Message{Role: RoleSystem, Content: text} }

// UserMessage returns a user message carrying text.
func UserMessage(text string) Message { return Message{Role: RoleUser, Content: text} }

// AssistantMessage returns an assistant message carrying text.
func AssistantMessage(text string) Message { return Message{Role: RoleAssistant, Content: text} }

// ToolMessage returns the result of the tool call named by toolCallID.
func ToolMessage(toolCallID, content string) Message {
	return Message{Role: RoleTool, ToolCallID: toolCallID, Content: content}
}

// ChatCompletion is the gateway's answer to a chat completion request.
type ChatCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	// Provider is the provider that served the request, as the gateway names it.
	Provider string   `json:"provider,omitempty"`
	Choices  []Choice `json:"choices"`
	Usage    Usage    `json:"usage"`
	// ProviderMetadata carries provider-specific top-level fields the gateway
	// was configured to pass through, such as citations.
	ProviderMetadata map[string]any `json:"provider_metadata,omitempty"`

	// Meta describes the HTTP response: status, headers and routing attribution.
	Meta ResponseMeta `json:"-"`
}

// Choice is one completion candidate.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
	// Logprobs is the log-probability object exactly as the upstream returned
	// it, present only when the request set LogProbs.
	Logprobs json.RawMessage `json:"logprobs,omitempty"`
}

// Usage is the token accounting of a request. PromptTokens includes
// CacheReadTokens and CompletionTokens includes ReasoningTokens.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`

	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// PromptTokensDetails is the OpenAI breakdown of the prompt token count.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// CompletionTokensDetails is the OpenAI breakdown of the completion token count.
type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ChatCompletionChunk is one event of a streamed chat completion.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	// Usage is set on the final chunk when StreamOptions.IncludeUsage was
	// requested; that chunk carries no choices.
	Usage *Usage `json:"usage,omitempty"`
}

// ChunkChoice is the increment one chunk carries for one choice.
type ChunkChoice struct {
	Index        int             `json:"index"`
	Delta        Delta           `json:"delta"`
	FinishReason string          `json:"finish_reason,omitempty"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

// Delta is the part of a message one chunk adds.
type Delta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
}

// EmbeddingRequest is the body of POST /v1/embeddings.
type EmbeddingRequest struct {
	Model string `json:"model"`
	// Input is a string, a []string, or — for a target that accepts token ids —
	// a []int64 or [][]int64.
	Input any `json:"input"`
	// EncodingFormat is "float". The gateway serves "base64" as float too.
	EncodingFormat string `json:"encoding_format,omitempty"`
	Dimensions     *int   `json:"dimensions,omitempty"`
	User           string `json:"user,omitempty"`
	// InputType is the provider-specific input role some embedding models
	// take ("search_query", "search_document", …).
	InputType string `json:"input_type,omitempty"`
}

// EmbeddingResponse is the gateway's answer to an embeddings request.
type EmbeddingResponse struct {
	Object string         `json:"object"`
	Data   []Embedding    `json:"data"`
	Model  string         `json:"model"`
	Usage  EmbeddingUsage `json:"usage"`

	// Meta describes the HTTP response: status, headers and routing attribution.
	Meta ResponseMeta `json:"-"`
}

// Embedding is the vector for one input, in the order the inputs were sent.
type Embedding struct {
	Object    string    `json:"object"`
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}

// EmbeddingUsage is the token accounting of an embeddings request.
type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ModelList is the body of GET /v1/models.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`

	// Meta describes the HTTP response.
	Meta ResponseMeta `json:"-"`
}

// Model is one advertised model. The fields after OwnedBy come from the
// gateway's model catalog and are empty for a model the catalog does not know.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	// OwnedBy is the provider of the target that serves the model.
	OwnedBy string `json:"owned_by"`

	Mode            string   `json:"mode,omitempty"`
	ContextWindow   int      `json:"context_window,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	Status          string   `json:"status,omitempty"`
	Deprecated      bool     `json:"deprecated,omitempty"`
}

// Parameter support values in Capabilities.
const (
	SupportForward     = "forward"
	SupportTranslate   = "translate"
	SupportUnsupported = "unsupported"
)

// Capabilities is the body of GET /v1/capabilities.
type Capabilities struct {
	// Providers maps a provider to its parameter profile: each OpenAI chat
	// parameter to SupportForward, SupportTranslate or SupportUnsupported.
	Providers map[string]map[string]string `json:"providers"`
	// ImageResponseFormats lists, for each provider restricted to some image
	// response formats, the ones it can produce. A provider absent here honours
	// whichever format it is asked for.
	ImageResponseFormats map[string][]string `json:"image_response_formats"`

	// Meta describes the HTTP response.
	Meta ResponseMeta `json:"-"`
}

// Support reports how provider handles the chat parameter param, and false
// when the gateway described no such provider or parameter.
func (c *Capabilities) Support(provider, param string) (string, bool) {
	profile, ok := c.Providers[provider]
	if !ok {
		return "", false
	}
	support, ok := profile[param]
	return support, ok
}
